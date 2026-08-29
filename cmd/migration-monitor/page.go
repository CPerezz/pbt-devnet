package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
)

//go:embed page.html
var pageFS embed.FS

// pageTemplate is parsed once at package init, so a template typo fails the
// build's test run rather than surfacing only when a handler first fires.
var pageTemplate = template.Must(template.New("page.html").Funcs(template.FuncMap{
	"abbrev": abbrevRoot,
}).ParseFS(pageFS, "page.html"))

// abbrevRoot shortens a shadow root to what fits a matrix cell. Ten
// characters plus the ellipsis is enough to eyeball a match at a glance and
// still short enough that twenty rows fit on one screen.
func abbrevRoot(root string) string {
	const n = 10
	if len(root) <= n {
		return root
	}
	return root[:n] + "\u2026"
}

// serveHTTP starts the live status server in the background. It never
// blocks and never takes the monitor down: a bind failure is a warn event,
// not a fatal one, because an operator who forgot to check the page should
// not also lose the JSONL record.
func serveHTTP(addr string, s *snapshot, log *migmon.Log) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", apiStateHandler(s, log))
	mux.HandleFunc("GET /", pageHandler(s, log))

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Emit(migmon.Event{Kind: migmon.EvWarn, Detail: fmt.Sprintf("--http %s: %v", addr, err)})
		return
	}
	go func() {
		srv := &http.Server{Handler: mux}
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Emit(migmon.Event{Kind: migmon.EvWarn, Detail: fmt.Sprintf("http server: %v", err)})
		}
	}()
}

// apiStateHandler serves the snapshot as JSON. Field names are the machine
// surface a lap driver tails, so this is a thin, stable encode - all the
// judgment (verdicts, cell status) already happened when the snapshot was
// built.
func apiStateHandler(s *snapshot, log *migmon.Log) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(s.view(time.Now())); err != nil {
			log.Emit(migmon.Event{Kind: migmon.EvWarn, Detail: fmt.Sprintf("encode /api/state: %v", err)})
		}
	}
}

// pageHandler renders the auto-refreshing status page.
func pageHandler(s *snapshot, log *migmon.Log) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := pageTemplate.Execute(w, s.view(time.Now())); err != nil {
			log.Emit(migmon.Event{Kind: migmon.EvWarn, Detail: fmt.Sprintf("render page: %v", err)})
		}
	}
}
