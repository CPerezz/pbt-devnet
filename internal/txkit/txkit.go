// Package txkit holds the EVM bytecode builders that make transactions land on the
// parts of the state EIP-8297 changed: code-zone chunks, the header stem, dedicated
// storage stems, and their deletion.
//
// It is shared by the hammer, which generates continuous traffic, and by pbtchaos,
// which needs the same shapes on a branch that is about to be reorged out. The
// builders are subtle in ways that have each been a bug once — SSTORE's operand
// order, STOP having to be byte 0, the factory's 31-byte prefix — so they live in one
// place rather than being copied.
//
// All bytecode is straight-line with no jumps, so it needs no compiler.
package txkit

import (
	"crypto/rand"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

// ReaderSeeded are the slots the reader contract writes in its own initcode. They
// straddle 64 so callread and the access-list workload touch both the header stem and
// dedicated storage stems.
var ReaderSeeded = []uint64{1, 2, 3, 5, 8, 13, 64, 65, 96, 200}

// ZeroizeSlots returns the batch a single zeroize or revert transaction works over:
// three in the header stem, then a full group above it so its stem goes from populated
// to empty in one go.
func ZeroizeSlots() []uint64 {
	out := []uint64{1, 2, 3}
	for i := uint64(64); i < 72; i++ {
		out = append(out, i)
	}
	return out
}

func RandomAddress() (common.Address, error) {
	var addr common.Address
	if _, err := rand.Read(addr[:]); err != nil {
		return addr, err
	}
	return addr, nil
}

// SlotKey turns a slot number into its 32-byte big-endian key.
func SlotKey(n uint64) common.Hash {
	return common.BigToHash(new(big.Int).SetUint64(n))
}

// Push32 emits PUSH32 followed by a full word. Every value is pushed at full width so
// the builders below never have to reason about operand sizes.
func Push32(h common.Hash) []byte {
	return append([]byte{0x7f}, h[:]...)
}

// SStore emits PUSH32 value, PUSH32 key, SSTORE. SSTORE pops the key first, so the value
// has to be pushed first.
func SStore(key, val common.Hash) []byte {
	out := Push32(val)
	out = append(out, Push32(key)...)
	return append(out, 0x55)
}

// SeedCode writes each slot to a non-zero value derived from it.
func SeedCode(slots []uint64) []byte {
	var out []byte
	for _, s := range slots {
		out = append(out, SStore(SlotKey(s), SlotKey(s+1))...)
	}
	return out
}

// ZeroizeInitCode writes every slot non-zero and then stores zero over all of them,
// inside one transaction, returning empty code.
//
// The order matters: all the writes first, then all the deletions. Interleaving them
// would only ever have one leaf live at a time, and never take a stem from fully
// populated to empty — which is the transition that makes a group collapse.
func ZeroizeInitCode(slots []uint64) []byte {
	out := SeedCode(slots)
	for _, s := range slots {
		out = append(out, SStore(SlotKey(s), common.Hash{})...)
	}
	return append(out, 0x60, 0x00, 0x60, 0x00, 0xf3) // PUSH1 0 PUSH1 0 RETURN
}

// StorageInitCode returns initcode that performs `n` unrolled SSTOREs and then
// returns zero-length code. Straight-line, no jumps: PUSH32 value, PUSH32 key,
// SSTORE, repeated, then PUSH1 0 PUSH1 0 RETURN.
func StorageInitCode(n int) []byte {
	code := make([]byte, 0, n*67+5)
	for i := 0; i < n; i++ {
		var key, val common.Hash
		// 24 random leading bytes keep successive slots far apart in the tree
		// instead of sharing a stem.
		if _, err := rand.Read(key[:24]); err != nil {
			panic(err)
		}
		key[31] = byte(i)
		val[31] = byte(i + 1)
		code = append(code, SStore(key, val)...)
	}
	code = append(code, 0x60, 0x00, 0x60, 0x00, 0xf3) // PUSH1 0 PUSH1 0 RETURN
	return code
}

// ReaderRuntime SLOADs the slot named by the first word of calldata and returns it.
//
//	PUSH1 0 | CALLDATALOAD | SLOAD | PUSH1 0 | MSTORE | PUSH1 32 | PUSH1 0 | RETURN
func ReaderRuntime() []byte {
	return []byte{0x60, 0x00, 0x35, 0x54, 0x60, 0x00, 0x52, 0x60, 0x20, 0x60, 0x00, 0xf3}
}

// WriterRuntime stores calldata word 1 at calldata word 0. The value is pushed before
// the key because SSTORE pops the key first.
//
//	PUSH1 32 | CALLDATALOAD | PUSH1 0 | CALLDATALOAD | SSTORE | STOP
func WriterRuntime() []byte {
	return []byte{0x60, 0x20, 0x35, 0x60, 0x00, 0x35, 0x55, 0x00}
}

// ExtcodeInitCode reads the size of `target` `reps` times and then copies its whole code
// into memory, returning nothing. Straight-line, and deliberately cheap in gas relative
// to the number of code-zone leaves it makes the client touch.
func ExtcodeInitCode(target common.Address, codeLen, reps int) []byte {
	var out []byte
	for i := 0; i < reps; i++ {
		out = append(out, 0x73)              // PUSH20 target
		out = append(out, target.Bytes()...) //
		out = append(out, 0x3b, 0x50)        // EXTCODESIZE, POP
	}
	// EXTCODECOPY pops address, destOffset, offset, length — so push them in reverse.
	out = append(out, Push32(SlotKey(uint64(codeLen)))...) // length
	out = append(out, 0x60, 0x00)                          // PUSH1 0  (offset in the code)
	out = append(out, 0x60, 0x00)                          // PUSH1 0  (memory destination)
	out = append(out, 0x73)                                // PUSH20 target
	out = append(out, target.Bytes()...)                   //
	out = append(out, 0x3c)                                // EXTCODECOPY
	return append(out, 0x60, 0x00, 0x60, 0x00, 0xf3)       // return empty code
}

// DeployCodeAfter returns `prefix`, then a CODECOPY/RETURN preamble, then the runtime
// blob. The prefix runs first and is a chance to initialise storage before the code is
// returned; the preamble's offset is corrected for its length.
//
// The preamble is exactly 15 bytes, so the blob starts at len(prefix)+15.
func DeployCodeAfter(prefix, runtime []byte) []byte {
	n := len(runtime)
	if n > 0xffff {
		panic("runtime blob too large for a PUSH2 length")
	}
	const preambleLen = 15
	off := len(prefix) + preambleLen
	if off > 0xffff {
		panic("prefix too large for a PUSH2 offset")
	}
	preamble := []byte{
		0x61, byte(n >> 8), byte(n), // PUSH2 len
		0x61, byte(off >> 8), byte(off), // PUSH2 offset of the blob in this code
		0x60, 0x00, // PUSH1 0  (memory destination)
		0x39,                        // CODECOPY
		0x61, byte(n >> 8), byte(n), // PUSH2 len
		0x60, 0x00, // PUSH1 0
		0xf3, // RETURN
	}
	if len(preamble) != preambleLen {
		panic(fmt.Sprintf("preamble is %d bytes, the offset says %d", len(preamble), preambleLen))
	}
	out := make([]byte, 0, len(prefix)+preambleLen+n)
	out = append(out, prefix...)
	out = append(out, preamble...)
	return append(out, runtime...)
}

// FactoryDestructInitCode returns initcode that deploys a child WITH REAL CODE and
// then destroys it in the same transaction.
//
// The obvious shape — `PUSH20 addr; SELFDESTRUCT` as the creation transaction's own
// initcode — deploys nothing, because SELFDESTRUCT halts before any RETURN. It cost
// 215,814 gas against 210,588 for a bare empty CREATE, i.e. it wrote no code at all,
// so the workload never produced the orphaned code-zone leaves it was meant to.
//
// A factory fixes that: the child returns `codeSize` bytes of code, so code-zone
// leaves are written, and the parent then calls it so it self-destructs. Post
// EIP-6780 SELFDESTRUCT only deletes when the account was created in the same
// transaction — which is exactly this case — so the account goes away while its code
// chunks remain, with nothing reference-counting them.
//
// Parent layout (31-byte prefix, then the child initcode it copies out of itself):
//
//	PUSH2 len | PUSH2 31 | PUSH1 0 | CODECOPY      copy child initcode to memory
//	PUSH2 len | PUSH1 0  | PUSH1 0 | CREATE        deploy it -> address
//	PUSH1 0 ×5 | DUP6 | GAS | CALL | STOP          call it -> SELFDESTRUCT
func FactoryDestructInitCode(beneficiary common.Address, codeSize int) []byte {
	// Child runtime: destroy on any call, followed by inert filler that exists only
	// to occupy code-zone leaves.
	childRuntime := append([]byte{0x73}, beneficiary.Bytes()...) // PUSH20 beneficiary
	childRuntime = append(childRuntime, 0xff)                    // SELFDESTRUCT
	for len(childRuntime) < codeSize {
		childRuntime = append(childRuntime, 0xfe) // INVALID: never reached, never executed
	}
	childInit := DeployCodeAfter(nil, childRuntime)

	n := len(childInit)
	const prefixLen = 31
	out := []byte{
		0x61, byte(n >> 8), byte(n), // PUSH2 len
		0x61, 0x00, prefixLen, // PUSH2 31  (offset of childInit in this code)
		0x60, 0x00, // PUSH1 0   (memory destination)
		0x39,                        // CODECOPY
		0x61, byte(n >> 8), byte(n), // PUSH2 len
		0x60, 0x00, // PUSH1 0   (memory offset)
		0x60, 0x00, // PUSH1 0   (value)
		0xf0,       // CREATE     -> [addr]
		0x60, 0x00, // retLength
		0x60, 0x00, // retOffset
		0x60, 0x00, // argsLength
		0x60, 0x00, // argsOffset
		0x60, 0x00, // value
		0x85, // DUP6 -> copy addr to the top
		0x5a, // GAS
		0xf1, // CALL
		0x00, // STOP
	}
	if len(out) != prefixLen {
		panic(fmt.Sprintf("factory prefix is %d bytes, the PUSH2 offset says %d", len(out), prefixLen))
	}
	return append(out, childInit...)
}

// PatternCode builds a deterministic runtime blob that is safe to CALL: byte 0 is
// STOP, so execution halts immediately and the rest is inert data.
//
// The terminator has to be FIRST. Filling with PUSH0 and putting a STOP at the end
// produced code that overflowed the 1024-slot stack after ~1024 bytes and reverted,
// burning all forwarded gas — so the contracts could be deployed but never called,
// and the code-zone read path went untested.
//
// fill selects the body, which decides the code hash: the same fill from different
// senders shares code-zone leaves, a different fill does not.
func PatternCode(n int, fill byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = fill
	}
	if n > 0 {
		out[0] = 0x00 // STOP
	}
	return out
}
