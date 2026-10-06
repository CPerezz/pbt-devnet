package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
	"github.com/CPerezz/pbt-devnet/internal/pbtsnap"
)

var ctx = context.Background()

// runProducers waits for the shared export gate, then runs both producers concurrently;
// a gate that never opens before the fork fails both without ever touching a client.
func (r *runner) runProducers() {
	if !r.waitExportGate() {
		for _, p := range r.cfg.Producers {
			r.failProducer(p.Node, "fork reached before export conditions were met")
		}
		return
	}
	var wg sync.WaitGroup
	for _, p := range r.cfg.Producers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch p.Kind {
			case "geth-convert":
				r.gethConvert(p.Node)
			case "erigon-export":
				r.erigonExport(p.Node)
			}
		}()
	}
	wg.Wait()

	// A restarted producer published new ports. The shadow roots are read only now, well
	// after the anchor, so every EL has had time to execute it.
	if err := r.resolveELs(); err != nil {
		say("re-resolving EL clients: %v", err)
	}
	for _, p := range r.ev.Producers {
		if p.Status != "ok" {
			continue
		}
		shadows := r.shadowRoots(p.AnchorHash)
		r.ev.update(func() { p.ShadowRoots = shadows })
	}
}

// waitExportGate polls until no partition is applied, the chaos-free gap ahead is long
// enough, and every validating EL answers - or the fork arrives first.
func (r *runner) waitExportGate() bool {
	notBefore := r.genesis + r.cfg.ExportAfterSeconds
	for {
		now := time.Now().Unix()
		if now > r.fork {
			return false
		}
		st := exportState{
			Now: now, NotBefore: notBefore, Partitions: r.partitions(),
			Gap:  chaosFree(r.dump, time.Unix(now, 0), gapHorizon),
			Down: r.unhealthyELs(0),
		}
		if b := exportBlocker(st); b == "" {
			return true
		} else {
			say("export waiting: %s", b)
		}
		time.Sleep(10 * time.Second)
	}
}

// unhealthyELs returns the service names of validating ELs (excluding one, 0 = none) not
// answering eth_blockNumber.
func (r *runner) unhealthyELs(exclude int) []string {
	var down []string
	for i := 1; i <= r.validating; i++ {
		if i == exclude {
			continue
		}
		if _, err := r.elClients[i].HeadNumber(ctx); err != nil {
			down = append(down, r.nodes[i].EL)
		}
	}
	return down
}

// shadowRoots asks debug_shadowStateRoot(hash) of every EL; "" covers a null, an error, or
// a client with no introspection surface.
func (r *runner) shadowRoots(hash string) map[string]string {
	out := map[string]string{}
	for i := 1; i <= r.total; i++ {
		root, err := r.elClients[i].ShadowRoot(ctx, hash)
		if err != nil {
			root = ""
		}
		out[r.nodes[i].EL] = root
	}
	return out
}

// erigonMeta is the part of pbt-snapshot.meta.json the lap uses.
type erigonMeta struct {
	Block     migmon.FlexUint64 `json:"block"`
	BlockHash string            `json:"blockHash"`
	StateRoot string            `json:"stateRoot"`
	PbtRoot   string            `json:"pbtRoot"`
}

func parseMeta(raw []byte) (erigonMeta, error) {
	var m erigonMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, fmt.Errorf("pbt-snapshot.meta.json: %w", err)
	}
	if m.Block == 0 || m.BlockHash == "" || m.PbtRoot == "" {
		return m, fmt.Errorf("pbt-snapshot.meta.json lacks block, blockHash or pbtRoot: %s", raw)
	}
	return m, nil
}

func (r *runner) failProducer(node int, detail string) {
	p := r.producer(node)
	r.ev.update(func() {
		p.Status = "failed"
		p.Detail = detail
	})
	say("producer node-%d failed: %s", node, detail)
	r.postLifecycle(node, migmon.StepExportFailed, 0, "", detail)
}

// gethConvert runs the node-6 producer: freeze the head, convert the stopped datadir in a
// throwaway container, restart the node, and check the artifacts match the frozen head.
func (r *runner) gethConvert(node int) {
	r.lock.begin()
	defer r.lock.end()
	p := r.producer(node)
	n := r.nodes[node]
	artDir := filepath.Join(r.out, "artifacts", fmt.Sprintf("node-%d", node))
	os.MkdirAll(artDir, 0o755)

	r.ev.update(func() { p.StartedAt = time.Now().Unix() })
	r.postLifecycle(node, migmon.StepExporting, 0, "", "")

	if err := r.serviceStop(n.CL); err != nil {
		r.failProducer(node, fmt.Sprintf("stopping CL: %v", err))
		return
	}
	downFrom := time.Now().Unix()

	hdr, err := r.elClients[node].HeaderByTag(ctx, "latest")
	if err != nil || hdr == nil {
		r.failProducer(node, fmt.Sprintf("reading frozen head: %v", err))
		return
	}

	if err := r.serviceStop(n.EL); err != nil {
		r.failProducer(node, fmt.Sprintf("stopping EL: %v", err))
		return
	}

	cid, err := r.containerID(n.EL)
	if err != nil {
		r.failProducer(node, err.Error())
		return
	}
	convCID, err := sh("docker", "create", "pbt-geth:local",
		"--datadir", "/tmp/execution-data", "bintrie", "convert", "--force",
		"--snapshot-out", "/tmp/pbt-snapshot.bin", "--preimages-out", "/tmp/preimages.bin")
	if err != nil {
		r.failProducer(node, fmt.Sprintf("creating converter: %v", err))
		return
	}
	defer r.dockerRM(convCID)
	if err := streamDatadir(cid, "/data/geth/execution-data", convCID, "/tmp"); err != nil {
		r.failProducer(node, err.Error())
		return
	}

	if err := r.serviceStart(n.EL); err != nil {
		r.failProducer(node, fmt.Sprintf("starting EL: %v", err))
		return
	}
	if err := r.serviceStart(n.CL); err != nil {
		r.failProducer(node, fmt.Sprintf("starting CL: %v", err))
		return
	}
	downTo := time.Now().Unix()

	// Combined: geth logs its conversion progress to stderr.
	convertLog, convErr := shCombined("docker", "start", "-a", convCID)
	os.WriteFile(filepath.Join(artDir, "convert.log"), []byte(convertLog), 0o644)
	if convErr != nil {
		r.failProducer(node, fmt.Sprintf("converter: %v", convErr))
		return
	}

	block, binaryRoot, err := pbtsnap.ConvertLog(convertLog)
	if err != nil {
		r.failProducer(node, err.Error())
		return
	}
	if err := r.dockerCP(convCID+":/tmp/pbt-snapshot.bin", filepath.Join(artDir, "pbt-snapshot.bin")); err != nil {
		r.failProducer(node, err.Error())
		return
	}
	if err := r.dockerCP(convCID+":/tmp/preimages.bin", filepath.Join(artDir, "preimages.bin")); err != nil {
		r.failProducer(node, err.Error())
		return
	}

	if block != hdr.Number {
		r.failProducer(node, fmt.Sprintf("convert log anchor block=%d, frozen head was %d", block, hdr.Number))
		return
	}
	pbtRoot, err := pbtsnap.Root(filepath.Join(artDir, "pbt-snapshot.bin"))
	if err != nil {
		r.failProducer(node, err.Error())
		return
	}
	if !pbtsnap.LoggedMatches(binaryRoot, pbtRoot) {
		r.failProducer(node, fmt.Sprintf("convert log binaryRoot=%s, snapshot trailer pbtRoot=%s", binaryRoot, pbtRoot.Hex()))
		return
	}

	r.ev.update(func() {
		p.Status, p.Detail = "ok", ""
		p.Anchor, p.AnchorHash, p.AnchorStateRoot = hdr.Number, hdr.Hash, hdr.Root
		p.PbtRoot = pbtRoot.Hex()
		p.DownFrom, p.DownTo = downFrom, downTo
		p.ExportedAt = time.Now().Unix()
	})
	r.postLifecycle(node, migmon.StepExported, hdr.Number, hdr.Hash, "")
}

// erigonExport runs the node-2 producer: a live snapshot export, no downtime.
func (r *runner) erigonExport(node int) {
	r.lock.begin()
	defer r.lock.end()
	p := r.producer(node)
	n := r.nodes[node]
	artDir := filepath.Join(r.out, "artifacts", fmt.Sprintf("node-%d", node))
	os.MkdirAll(artDir, 0o755)

	r.ev.update(func() { p.StartedAt = time.Now().Unix() })
	r.postLifecycle(node, migmon.StepExporting, 0, "", "")

	cid, err := r.containerID(n.EL)
	if err != nil {
		r.failProducer(node, err.Error())
		return
	}

	var out string
	var runErr error
	for try := 1; try <= 3; try++ {
		out, runErr = shCombined("docker", "exec", cid, "sh", "-c",
			"rm -rf /tmp/pbt-export && erigon --datadir /data/erigon/execution-data snapshots export-pbt --out /tmp/pbt-export")
		if runErr == nil {
			break
		}
		say("erigon export try %d/3 failed: %v", try, runErr)
	}
	os.WriteFile(filepath.Join(artDir, "export.log"), []byte(out), 0o644)
	if runErr != nil {
		r.failProducer(node, fmt.Sprintf("snapshots export-pbt: %v", runErr))
		return
	}

	metaRaw, err := r.dockerExec(cid, "cat", "/tmp/pbt-export/pbt-snapshot.meta.json")
	if err != nil {
		r.failProducer(node, err.Error())
		return
	}
	meta, err := parseMeta([]byte(metaRaw))
	if err != nil {
		r.failProducer(node, err.Error())
		return
	}

	for _, f := range [][2]string{
		{"pbt-snapshot.bin", "pbt-snapshot.bin"},
		{"framed.bin", "preimages.bin"}, // the name the importers are handed
		{"pbt-snapshot.meta.json", "pbt-snapshot.meta.json"},
	} {
		if err := r.dockerCP(cid+":/tmp/pbt-export/"+f[0], filepath.Join(artDir, f[1])); err != nil {
			r.failProducer(node, fmt.Sprintf("copying %s: %v", f[0], err))
			return
		}
	}

	r.ev.update(func() {
		p.Status, p.Detail = "ok", ""
		p.Anchor, p.AnchorHash, p.AnchorStateRoot = uint64(meta.Block), meta.BlockHash, meta.StateRoot
		p.PbtRoot = meta.PbtRoot
		p.ExportedAt = time.Now().Unix()
	})
	r.postLifecycle(node, migmon.StepExported, uint64(meta.Block), meta.BlockHash, "")
}
