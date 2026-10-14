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
		if migmon.ClientType(r.nodes[c.Node].EL) == "geth" && r.gethSwap == "convert" {
			st.MinHead = prod.Anchor + convertLead
			st.Head, _ = r.elClients[1].HeadNumber(ctx)
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

// gethFlags builds the import flags geth's wrapper reads from pbt-swap/flags: the import
// mode names a producer's snapshot and preimages, both staged in datadir/pbt-swap/; the
// convert mode (self-conversion: geth's own persisted state) drops the snapshot flag and
// names only the producer's preimages. Either way --force replaces the tree geth seeded
// from genesis, and the import/conversion runs in the background while geth keeps
// following the chain.
//
// A conversion reads the state geth last persisted, which must be at or past the anchor.
// pathdb persists only when its write buffer (256 MiB) fills, which this devnet's traffic
// never does in a lap (geth logs layers=241 at head 241: still genesis). --history.state=8
// makes it persist whenever it trails the head by 128+8 blocks.
func gethFlags(datadir, anchorHash string, convert bool) string {
	if convert {
		return fmt.Sprintf("--pbt.import.preimages=%s/pbt-swap/preimages.bin\n--pbt.import.anchor=%s\n--pbt.import.force\n--history.state=8\n",
			datadir, anchorHash)
	}
	return fmt.Sprintf(
		"--pbt.import.snapshot=%s/pbt-swap/pbt-snapshot.bin\n--pbt.import.preimages=%s/pbt-swap/preimages.bin\n--pbt.import.anchor=%s\n--pbt.import.force\n",
		datadir, datadir, anchorHash)
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
	// A converting geth reads its own state: only the producer's preimages go in.
	convert := client == "geth" && r.gethSwap == "convert"
	if !convert {
		if err := copyFile(filepath.Join(artDir, "pbt-snapshot.bin"), filepath.Join(local, "pbt-snapshot.bin")); err != nil {
			return fail(err.Error())
		}
	}
	if err := copyFile(filepath.Join(artDir, "preimages.bin"), filepath.Join(local, "preimages.bin")); err != nil {
		return fail(err.Error())
	}
	switch client {
	case "geth":
		// geth imports, or converts its own state, in the background while it keeps
		// following the chain; force replaces the tree it seeded from genesis.
		if convert {
			r.ev.update(func() { sw.Mode = "convert" })
		}
		if err := os.WriteFile(filepath.Join(local, "flags"), []byte(gethFlags(datadir, prod.AnchorHash, convert)), 0o644); err != nil {
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
				// The import runs in the background of a node that keeps following,
				// so a refused or failed one is read from the client, not a marker.
				prog, err := r.progress(el)
				if err == nil && prog.Phase == migmon.PhaseImportFailed {
					detail := "import failed"
					if prog.Binary != nil && prog.Binary.Error != "" {
						detail += ": " + prog.Binary.Error
					}
					return fail(detail)
				}
				if caughtUp, err := r.migrationCaughtUp(prog, err); err == nil && caughtUp {
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

// progress reads one debug_migrationProgress report.
func (r *runner) progress(el migmon.Client) (migmon.MigrationProgress, error) {
	raw, err := el.Progress(ctx)
	if err != nil {
		return migmon.MigrationProgress{}, err
	}
	return migmon.DecodeProgress(raw)
}

// migrationCaughtUp reports the binary cursor of a progress report within
// CaughtUpLag of the chain tip, node 1's head. The consumer's own head is no
// reference: right after the restart it is as stale as the cursor.
func (r *runner) migrationCaughtUp(prog migmon.MigrationProgress, progErr error) (bool, error) {
	if progErr != nil || prog.Binary == nil {
		return false, progErr
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
