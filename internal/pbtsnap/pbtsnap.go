// Package pbtsnap reads what the lap needs from an EIP-8347 PBT snapshot and
// from geth's conversion log, without decoding records: the pbtRoot trailer
// (EIPs#12403) and the block and root `geth bintrie convert` reports. The
// clients' importers check everything else.
package pbtsnap

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// tagEnd closes the record stream; pbtRoot[32] follows it and ends the file.
const tagEnd = 0x07

// Root returns the pbtRoot a snapshot file claims.
func Root(path string) (common.Hash, error) {
	f, err := os.Open(path)
	if err != nil {
		return common.Hash{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return common.Hash{}, err
	}
	var tail [1 + common.HashLength]byte
	if st.Size() < int64(len(tail)) {
		return common.Hash{}, fmt.Errorf("%s: %d bytes, too short for the end tag and pbtRoot", path, st.Size())
	}
	if _, err := f.ReadAt(tail[:], st.Size()-int64(len(tail))); err != nil {
		return common.Hash{}, err
	}
	if tail[0] != tagEnd {
		return common.Hash{}, fmt.Errorf("%s: byte %#x before the trailer, want the end tag %#x", path, tail[0], tagEnd)
	}
	return common.BytesToHash(tail[1:]), nil
}

// geth pads log messages and writes integers past 99,999 with thousands separators.
var (
	convertBlock = regexp.MustCompile(`Starting MPT to binary trie conversion\s.*\bblock=([0-9,]+)`)
	convertRoot  = regexp.MustCompile(`Conversion complete\s+binaryRoot=([0-9a-fA-Fx.]+)`)
)

// ConvertLog returns the block `geth bintrie convert` converted and the binary root it logged.
func ConvertLog(log string) (block uint64, root string, err error) {
	m := convertBlock.FindStringSubmatch(log)
	if m == nil {
		return 0, "", fmt.Errorf("convert log has no conversion start line")
	}
	if block, err = strconv.ParseUint(strings.ReplaceAll(m[1], ",", ""), 10, 64); err != nil {
		return 0, "", err
	}
	r := convertRoot.FindStringSubmatch(log)
	if r == nil {
		return 0, "", fmt.Errorf("convert log has no completion line")
	}
	return block, r[1], nil
}

// LoggedMatches reports whether a root as geth logged it names root. geth's terminal log
// abbreviates a hash to its first and last three bytes ("c45bc4..a7140a").
func LoggedMatches(logged string, root common.Hash) bool {
	if strings.Contains(logged, "..") {
		return logged == root.TerminalString()
	}
	return strings.EqualFold(logged, root.Hex())
}
