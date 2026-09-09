package main

import (
	"embed"
	"fmt"
	"io/fs"
	"net"
	"net/http"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
)

// The page is static and polls /api/state; synthetic.js (the review-time lap
// generator) is served by `make ui-preview` only.
//
//go:embed ui/index.html ui/river.js
var uiFS embed.FS

// serveHTTP starts the live page in the background; a bind failure is a
// warn, not fatal - the JSONL stream is the monitor's real output.
func serveHTTP(addr string, document func() []byte, log *migmon.Log) {
	ui, err := fs.Sub(uiFS, "ui")
	if err != nil {
		panic(err) // the embed directive above guarantees the directory
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		doc := document()
		if doc == nil {
			http.Error(w, "no document yet", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(doc)
	})
	mux.Handle("GET /", http.FileServerFS(ui))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Emit(migmon.Event{Kind: migmon.EvWarn, Node: "monitor", Detail: fmt.Sprintf("--http %s: %v", addr, err)})
		return
	}
	go func() {
		srv := &http.Server{Handler: mux}
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Emit(migmon.Event{Kind: migmon.EvWarn, Node: "monitor", Detail: "http server: " + err.Error()})
		}
	}()
}
