// Command gengenesis emits the genesis.json for a PBT (EIP-8297) devnet.
//
// The genesis is generated rather than hand-written because it has to carry the
// four system contracts' bytecode verbatim, and because the tree packs balances
// into a 16-byte BASIC_DATA field — both are things a transcription gets wrong
// silently. Everything here is read from the go-ethereum checkout this module
// points at, so the output can never drift from the client it configures.
//
// Shape follows eth/catalyst/pbt_test.go's pbtGenesis, which is the only known
// genesis this branch has actually produced and imported blocks on.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// prefundedKeys are the standard local-development keys. spamoor, tx-fuzz and the
// hammer all draw from these, so they are funded generously — but well inside the
// 16 bytes the tree gives a balance.
var prefunded = []string{
	"0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266", // ac0974be...ff80
	"0x70997970C51812dc3A010C7d01b50e0d17dc79C8", // 59c6995e...690d
	"0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC", // 5de4111a...5a1d
	"0x90F79bf6EB2c4f870365E785982E1f101E93b906", // 7c852118...6ba6
	"0x15d34AAf54267DB7D7c367839AAf71A00a2C6A65", // 47e179ec...a4f6
	"0x9965507D1a55bcC2695C58ba16FB37d819B0A4dc", // 8b3a350c...f20a
}

func main() {
	out := flag.String("out", "genesis.json", "path to write genesis.json to")
	chainID := flag.Int64("chainid", 1337, "chain id")
	gasLimit := flag.Uint64("gaslimit", 30_000_000, "genesis gas limit")
	timestamp := flag.Uint64("timestamp", 0, "genesis timestamp (0 = leave at zero)")
	balanceEth := flag.Int64("balance", 1_000_000, "prefunded balance per account, in ether")
	flag.Parse()

	// A balance must fit the tree's 16-byte BASIC_DATA field. 1e6 ETH = 1e24 wei,
	// which is ~2^80 — comfortably inside 2^128. The dev-mode faucet balance of
	// 2^256-9 is what trips bintrie.ErrBalanceOverflow, so never go near it.
	balance := new(big.Int).Mul(big.NewInt(*balanceEth), big.NewInt(params.Ether))
	if balance.BitLen() > 128 {
		fmt.Fprintf(os.Stderr, "balance %s needs %d bits; the tree gives it 128\n", balance, balance.BitLen())
		os.Exit(1)
	}

	zero := uint64(0)
	config := &params.ChainConfig{
		ChainID:             big.NewInt(*chainID),
		HomesteadBlock:      big.NewInt(0),
		EIP150Block:         big.NewInt(0),
		EIP155Block:         big.NewInt(0),
		EIP158Block:         big.NewInt(0),
		ByzantiumBlock:      big.NewInt(0),
		ConstantinopleBlock: big.NewInt(0),
		PetersburgBlock:     big.NewInt(0),
		IstanbulBlock:       big.NewInt(0),
		BerlinBlock:         big.NewInt(0),
		LondonBlock:         big.NewInt(0),
		MergeNetsplitBlock:  big.NewInt(0),

		TerminalTotalDifficulty: big.NewInt(0),

		ShanghaiTime: &zero,
		CancunTime:   &zero,
		PragueTime:   &zero,
		OsakaTime:    &zero,
		// Amsterdam is mandatory: params.ChainConfig.CheckConfigForkOrder rejects the
		// binary tree without it, and requires binaryTrieTime no earlier than
		// amsterdamTime. BogotaTime stays nil — Bogota brings EIP-7805/8141, which
		// would make any divergence harder to attribute to the tree.
		AmsterdamTime: &zero,

		// The binary tree is a timestamp fork, scheduled here at genesis. The key is
		// shared with besu, so one genesis.json serves both clients. It used to be a
		// `"pbt": true` boolean; a config still carrying that decodes fork-less and
		// yields a merkle-patricia chain without any error.
		BinaryTrieTime: &zero,

		DepositContractAddress: params.MainnetChainConfig.DepositContractAddress,

		BlobScheduleConfig: &params.BlobScheduleConfig{
			Cancun: params.DefaultCancunBlobConfig,
			Prague: params.DefaultPragueBlobConfig,
			BPO1:   params.DefaultBPO1BlobConfig,
			BPO2:   params.DefaultBPO2BlobConfig,
		},
	}

	alloc := types.GenesisAlloc{
		// Block processing from Cancun onwards calls into these unconditionally.
		// Nonce 1 marks them as deployed contracts rather than touched EOAs.
		params.BeaconRootsAddress:        {Nonce: 1, Code: params.BeaconRootsCode, Balance: common.Big0},
		params.HistoryStorageAddress:     {Nonce: 1, Code: params.HistoryStorageCode, Balance: common.Big0},
		params.WithdrawalQueueAddress:    {Nonce: 1, Code: params.WithdrawalQueueCode, Balance: common.Big0},
		params.ConsolidationQueueAddress: {Nonce: 1, Code: params.ConsolidationQueueCode, Balance: common.Big0},
	}
	for _, addr := range prefunded {
		alloc[common.HexToAddress(addr)] = types.Account{Balance: balance}
	}

	genesis := &core.Genesis{
		Config:     config,
		Timestamp:  *timestamp,
		Difficulty: common.Big0,
		GasLimit:   *gasLimit,
		BaseFee:    big.NewInt(params.InitialBaseFee),
		Alloc:      alloc,
	}

	// Fail here rather than at node boot if the config is self-inconsistent.
	if err := config.CheckConfigForkOrder(); err != nil {
		fmt.Fprintf(os.Stderr, "chain config rejected: %v\n", err)
		os.Exit(1)
	}

	blob, err := json.MarshalIndent(genesis, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal: %v\n", err)
		os.Exit(1)
	}
	blob = append(blob, '\n')
	if err := os.WriteFile(*out, blob, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", *out, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "wrote %s (chainid %d, binaryTrieTime=0, amsterdamTime=0)\n", *out, *chainID)

	// Compute and print the genesis root as the tree commits it. Feed this to the
	// driver as --expected-genesis-root (or expected_genesis_root in an args file):
	// it is the client-agnostic check that a node really is on the binary tree, rather
	// than silently having fallen back to the merkle-patricia trie.
	block := genesis.ToBlock()
	fmt.Fprintf(os.Stderr, "genesis block hash:  %s\n", block.Hash())
	fmt.Fprintf(os.Stderr, "expected_genesis_root: %s\n", block.Root())
}
