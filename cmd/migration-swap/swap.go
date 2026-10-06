package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/CPerezz/pbt-devnet/internal/migmon"
)

// epochSeconds is the offline profile's epoch: 32 slots of 6 s (args/migration-offline.yaml).
const epochSeconds = 192

// gatePoll is how often an export or swap gate re-checks its conditions.
const gatePoll = 10 * time.Second

// runSwaps drains the consumer queue in config order, strictly one at a time. A skipped swap
// never took its node down. A failed or timed-out one may have left its node's EL off the
// head, so the next swap waits until it is back: the stake rule is about validators
// attesting, not about that node's PBT catching up.
func (r *runner) runSwaps() {
	prevNode := 0
	anyFailed := false
	for _, c := range r.cfg.Consumers {
		sw := r.swap(c.Node)
		prod := r.producer(sw.Producer)
		if prod.Status != "ok" {
			r.skipSwap(sw, fmt.Sprintf("producer node-%d did not export", sw.Producer))
			continue
		}
		if !r.waitSwapGate(c, sw, prod, prevNode, anyFailed) {
			continue
		}
		prevNode = 0
		if status := r.swapBody(c, sw, prod); status == "failed" || status == "timeout" {
			prevNode, anyFailed = c.Node, true
		}
	}
}

// elBehind reports node n's EL more than CaughtUpLag blocks behind node 1's, or not answering.
func (r *runner) elBehind(n int) bool {
	ref, err1 := r.elClients[1].HeadNumber(ctx)
	head, err2 := r.elClients[n].HeadNumber(ctx)
	return err1 != nil || err2 != nil || head+migmon.CaughtUpLag < ref
}

func (r *runner) skipSwap(sw *migmon.SwapRecord, detail string) {
	r.ev.update(func() { sw.Status, sw.Detail = "skipped", detail })
	say("swap node-%d skipped: %s", sw.Node, detail)
	r.postLifecycle(sw.Node, migmon.StepSkipped, 0, "", detail)
}

// waitSwapGate polls every 10s until every swap precondition holds; true means proceed,
// false means the swap was already finished as skipped (too late / reorged anchor).
func (r *runner) waitSwapGate(c migmon.OfflineConsumer, sw *migmon.SwapRecord, prod *migmon.ProducerRecord, prevNode int, anyFailed bool) bool {
	for {
		now := time.Now().Unix()
		fin, _ := r.elClients[1].HeaderByTag(ctx, "finalized")
		var canon string
		if fin != nil && fin.Number >= prod.Anchor {
			if h, err := r.elClients[1].HeaderByNumber(ctx, prod.Anchor); err == nil && h != nil {
				canon = h.Hash
			}
		}
		st := swapState{
			Now: now, Fork: r.fork, Expected: c.ExpectedSeconds, Margin: r.cfg.MarginSeconds,
			Anchor: prod.Anchor, AnchorHash: prod.AnchorHash,
			Finalized: fin, Canonical: canon, EpochSeconds: epochSeconds,
			Down:       r.unhealthyOthers(c.Node),
			Partitions: r.partitions(),
			Gap:        chaosFree(r.dump, time.Unix(now, 0), gapHorizon),
			PrevBehind: prevNode != 0 && r.elBehind(prevNode), PrevFailed: anyFailed,
		}
		v, reason := decideSwap(st)
		switch v {
		case proceed:
			return true
		case skip:
			r.skipSwap(sw, reason)
			return false
		case reorged:
			r.ev.update(func() { prod.Status, prod.Detail = "failed", reason })
			say("producer node-%d failed: %s", prod.Node, reason)
			r.skipSwap(sw, reason)
			return false
		}
		say("swap node-%d waiting: %s", c.Node, reason)
		time.Sleep(gatePoll)
	}
}

// unhealthyOthers returns the service names of every OTHER validating EL/CL failing its
// health check: EL eth_blockNumber, CL /eth/v1/node/health.
func (r *runner) unhealthyOthers(exclude int) []string {
	down := r.unhealthyELs(exclude)
	for i := 1; i <= r.validating; i++ {
		if i == exclude {
			continue
		}
		if url, ok := r.clURLs[i]; ok && !beaconHealthy(url) {
			down = append(down, r.nodes[i].CL)
		}
	}
	return down
}

func datadirFor(client string) string {
	switch client {
	case "geth":
		return "/data/geth/execution-data"
	case "nethermind":
		return "/data/nethermind/execution-data"
	}
	return ""
}

// swapBody stages the consumer's pbt-swap/ dir on the host, stops it, copies the dir in, restarts it, and polls to
// caught_up or failure; it always tries to copy the container's evidence back out.
func (r *runner) swapBody(c migmon.OfflineConsumer, sw *migmon.SwapRecord, prod *migmon.ProducerRecord) string {
	r.lock.begin()
	defer r.lock.end()

	n := r.nodes[c.Node]
	client := migmon.ClientType(n.EL)
	datadir := datadirFor(client)
	cid, cidErr := r.containerID(n.EL)
	defer func() {
		if cidErr == nil {
			r.copySwapEvidence(c.Node, cid, datadir)
		}
	}()
	fail := func(detail string) string {
		r.ev.update(func() { sw.Status, sw.Detail = "failed", detail })
		say("swap node-%d failed: %s", c.Node, detail)
		r.postLifecycle(c.Node, migmon.StepFailed, prod.Anchor, prod.AnchorHash, detail)
		return "failed"
	}
	if cidErr != nil {
		return fail(cidErr.Error())
	}
	if datadir == "" {
		return fail(fmt.Sprintf("no known datadir for client %q", client))
	}

	staging := filepath.Join(r.out, "staging", fmt.Sprintf("node-%d", c.Node))
	local := filepath.Join(staging, "pbt-swap")
	os.RemoveAll(staging)
	os.MkdirAll(local, 0o755)
	defer os.RemoveAll(staging)

	artDir := filepath.Join(r.out, "artifacts", fmt.Sprintf("node-%d", prod.Node))
	if err := copyFile(filepath.Join(artDir, "pbt-snapshot.bin"), filepath.Join(local, "pbt-snapshot.bin")); err != nil {
		return fail(err.Error())
	}
	if err := copyFile(filepath.Join(artDir, "preimages.bin"), filepath.Join(local, "preimages.bin")); err != nil {
		return fail(err.Error())
	}
	switch client {
	case "geth":
		if err := os.WriteFile(filepath.Join(local, "pending"), []byte(prod.AnchorHash), 0o644); err != nil {
			return fail(err.Error())
		}
	case "nethermind":
		if err := os.WriteFile(filepath.Join(local, "wipe"), nil, 0o644); err != nil {
			return fail(err.Error())
		}
		flags := fmt.Sprintf(
			"--Pbt.MigrationSnapshotPath=%s/pbt-swap/pbt-snapshot.bin\n--Pbt.MigrationPreimagesPath=%s/pbt-swap/preimages.bin\n--Pbt.MigrationAnchor=%d\n",
			datadir, datadir, prod.Anchor)
		if err := os.WriteFile(filepath.Join(local, "flags"), []byte(flags), 0o644); err != nil {
			return fail(err.Error())
		}
	}

	if r.fault == "corrupt-preimages" && client == "geth" {
		if fi, err := os.Stat(filepath.Join(local, "preimages.bin")); err == nil {
			os.Truncate(filepath.Join(local, "preimages.bin"), fi.Size()/2)
		}
	}

	fin, _ := r.elClients[1].HeaderByTag(ctx, "finalized")
	var finalizedAtStop uint64
	if fin != nil {
		finalizedAtStop = fin.Number
	}
	r.postLifecycle(c.Node, migmon.StepDisconnecting, prod.Anchor, prod.AnchorHash, "")
	if err := r.serviceStop(n.EL); err != nil {
		return fail(fmt.Sprintf("stopping EL: %v", err))
	}
	stopAt := time.Now().Unix()
	r.ev.update(func() {
		sw.Anchor, sw.AnchorHash = prod.Anchor, prod.AnchorHash
		sw.FinalizedAtStop, sw.StopAt = finalizedAtStop, stopAt
	})

	if err := r.dockerCP(local, cid+":"+datadir); err != nil {
		return fail(fmt.Sprintf("copying pbt-swap in: %v", err))
	}

	r.postLifecycle(c.Node, migmon.StepImporting, prod.Anchor, prod.AnchorHash, "")
	if err := r.serviceStart(n.EL); err != nil {
		// Non-fatal: kurtosis start can time out on readiness while the container did come up.
		say("swap node-%d: start error, polling anyway: %v", c.Node, err)
	}
	startAt := time.Now().Unix()
	r.ev.update(func() { sw.StartAt = startAt })

	deadline := time.Unix(stopAt, 0).Add(time.Duration(c.TimeoutSeconds) * time.Second)
	reachable := false
	for {
		running, err := r.containerRunning(cid)
		if err != nil {
			say("swap node-%d: docker inspect: %v", c.Node, err)
		} else if !running {
			return fail("container is not running")
		}
		// The wrapper finishes the import before the client opens its RPC, so the marker is
		// final by the time the node answers; a failed import that falls back to the online
		// seed still catches up, and must not read as a good swap.
		if _, err := r.dockerExec(cid, "test", "-e", datadir+"/pbt-swap/failed"); err == nil {
			return fail("pbt-swap/failed marker present")
		}
		if url, err := r.portURL(n.EL, "rpc"); err == nil {
			el := migmon.NewClient(n.EL, url)
			r.elClients[c.Node] = el
			if !reachable {
				if _, err := el.HeadNumber(ctx); err == nil {
					reachable = true
					r.ev.update(func() { sw.ReachableAt = time.Now().Unix() })
					r.postLifecycle(c.Node, migmon.StepReconnecting, prod.Anchor, prod.AnchorHash, "")
				}
			}
			if reachable {
				if caughtUp, err := r.migrationCaughtUp(el); err == nil && caughtUp {
					r.ev.update(func() { sw.CaughtUpAt, sw.Status, sw.Detail = time.Now().Unix(), "ok", "" })
					r.postLifecycle(c.Node, migmon.StepCaughtUp, prod.Anchor, prod.AnchorHash, "")
					return "ok"
				}
			}
		}
		if time.Now().After(deadline) {
			r.ev.update(func() {
				sw.Status, sw.Detail = "timeout", fmt.Sprintf("no caught_up within %ds of stop", c.TimeoutSeconds)
			})
			say("swap node-%d timed out", c.Node)
			r.postLifecycle(c.Node, migmon.StepTimeout, prod.Anchor, prod.AnchorHash, "")
			return "timeout"
		}
		time.Sleep(gatePoll)
	}
}

// migrationCaughtUp reads debug_migrationProgress and reports the binary cursor within
// CaughtUpLag of the chain tip, node 1's head. The consumer's own head is no reference:
// right after the restart it is as stale as the cursor.
func (r *runner) migrationCaughtUp(el migmon.Client) (bool, error) {
	raw, err := el.Progress(ctx)
	if err != nil {
		return false, err
	}
	prog, err := migmon.DecodeProgress(raw)
	if err != nil || prog.Binary == nil {
		return false, err
	}
	tip, err := r.elClients[1].HeadNumber(ctx)
	if err != nil {
		return false, err
	}
	return uint64(prog.Binary.Cursor)+migmon.CaughtUpLag >= tip, nil
}

// copySwapEvidence copies the consumer's pbt-swap/ dir contents and its docker logs to
// $OUT/artifacts/consumer-node-<n>/, best-effort: a copy failure is logged, not fatal.
func (r *runner) copySwapEvidence(node int, cid, datadir string) {
	evDir := filepath.Join(r.out, "artifacts", fmt.Sprintf("consumer-node-%d", node))
	os.MkdirAll(evDir, 0o755)
	if err := r.dockerCP(cid+":"+datadir+"/pbt-swap/.", evDir); err != nil {
		say("swap node-%d: copying pbt-swap evidence: %v", node, err)
	}
	logs, err := shCombined("docker", "logs", cid)
	if err != nil {
		say("swap node-%d: docker logs: %v", node, err)
		return
	}
	if err := os.WriteFile(filepath.Join(evDir, "docker.log"), []byte(logs), 0o644); err != nil {
		say("swap node-%d: writing docker.log: %v", node, err)
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
