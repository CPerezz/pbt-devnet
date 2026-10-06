package pbtsnap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestRoot(t *testing.T) {
	root := common.HexToHash("0x5e1b7c3a9f0d2e4b6a8c0e1f3a5b7d9c2e4f6a8b0c1d3e5f7a9b2c4d6e8f0a1b")
	cases := []struct {
		name    string
		bytes   []byte
		want    common.Hash
		wantErr string
	}{
		// An empty state's snapshot is exactly the end tag and the root.
		{"empty state", append([]byte{tagEnd}, root[:]...), root, ""},
		{"records then trailer", append([]byte{0x01, 0xaa, 0x03, 0xbb, tagEnd}, root[:]...), root, ""},
		{"too short", []byte{tagEnd, 0x01}, common.Hash{}, "too short"},
		{"no end tag", append([]byte{0x06}, root[:]...), common.Hash{}, "want the end tag"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "snapshot.bin")
			if err := os.WriteFile(path, c.bytes, 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := Root(path)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("Root = %v, %v; want %v", got, err, c.want)
			}
		})
	}
}

func TestLoggedMatches(t *testing.T) {
	root := common.HexToHash("0xc45bc4aa00000000000000000000000000000000000000000000000000a7140a")
	for _, c := range []struct {
		logged string
		want   bool
	}{
		{"c45bc4..a7140a", true}, // geth's terminal format, as bintrie convert logs it
		{"c45bc4..a7140b", false},
		{root.Hex(), true},
		{"0xc45bc4aa00000000000000000000000000000000000000000000000000a7140b", false},
	} {
		if got := LoggedMatches(c.logged, root); got != c.want {
			t.Errorf("LoggedMatches(%q) = %v, want %v", c.logged, got, c.want)
		}
	}
}

func TestConvertLog(t *testing.T) {
	// The shape a real `geth bintrie convert` prints: padded messages, separators past 99,999.
	log := "INFO [10-03|08:46:48.401] Starting MPT to binary trie conversion   root=2aa6bf..1950e8 block=182,004\n" +
		"INFO [10-03|08:46:48.687] Conversion complete                      binaryRoot=6dc433..b888a3\n"
	block, root, err := ConvertLog(log)
	if err != nil || block != 182004 || root != "6dc433..b888a3" {
		t.Fatalf("ConvertLog = %d, %q, %v; want 182004, 6dc433..b888a3", block, root, err)
	}
	if _, _, err := ConvertLog("INFO Starting MPT to binary trie conversion block=5\n"); err == nil {
		t.Fatal("ConvertLog accepted a log with no completion line")
	}
}
