package main

import (
	"strings"
	"testing"
)

func TestGethFlags(t *testing.T) {
	const datadir, hash = "/data/geth/execution-data", "0xanchor"

	t.Run("import: names both the snapshot and the preimages", func(t *testing.T) {
		flags := gethFlags(datadir, hash, false)
		for _, want := range []string{
			"--pbt.import.snapshot=" + datadir + "/pbt-swap/pbt-snapshot.bin",
			"--pbt.import.preimages=" + datadir + "/pbt-swap/preimages.bin",
			"--pbt.import.anchor=" + hash,
			"--pbt.import.force",
		} {
			if !strings.Contains(flags, want) {
				t.Fatalf("flags %q missing %q", flags, want)
			}
		}
	})

	t.Run("convert: no snapshot flag, only preimages and anchor", func(t *testing.T) {
		flags := gethFlags(datadir, hash, true)
		if strings.Contains(flags, "--pbt.import.snapshot") {
			t.Fatalf("convert mode flags %q must not name a snapshot", flags)
		}
		for _, want := range []string{
			"--pbt.import.preimages=" + datadir + "/pbt-swap/preimages.bin",
			"--pbt.import.anchor=" + hash,
			"--pbt.import.force",
		} {
			if !strings.Contains(flags, want) {
				t.Fatalf("flags %q missing %q", flags, want)
			}
		}
	})
}
