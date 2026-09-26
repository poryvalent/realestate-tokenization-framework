// Command genvectors writes the cross-language golden vectors.
//
// The Go and Solidity implementations of the Merkle construction and the ballot leaf
// encodings have to agree byte for byte. A verifier who can prove membership
// off-chain but not on-chain has nothing useful, and a leaf layout that differs
// between the two means every anchored root is wrong in a way no single-language test
// would catch.
//
// This command emits one JSON file consumed by both sides:
//
//	Go       internal/parity/parity_test.go asserts the file matches current output
//	Solidity contracts/test/*.t.sol replays the same vectors through the library
//
// Drift in either implementation therefore fails a test rather than shipping.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/acresync/orchestrator/internal/ballot"
	"github.com/acresync/orchestrator/internal/merkle"
	"github.com/acresync/orchestrator/internal/money"
	"github.com/acresync/orchestrator/internal/snapshot"
)

// DefaultOutputPath is where the Foundry tests expect to find the vectors, relative
// to the repository root.
const DefaultOutputPath = "contracts/test/testdata/vectors.json"

type Vectors struct {
	Note     string          `json:"note"`
	Merkle   MerkleVectors   `json:"merkle"`
	Ballot   BallotVectors   `json:"ballot"`
	Snapshot SnapshotVectors `json:"snapshot"`
}

// SnapshotVectors pins down the entitlement leaf encoding.
//
// # Why this section was added later than the others
//
// AcreSyncScheme.verifyEntitlement takes the leaf as a calldata parameter instead of computing it, so
// unlike the ballot leaves there was no on-chain function holding the snapshot encoding to any
// standard. That made it the one encoding in the system a verifier could not reproduce from anything
// authoritative: nothing would reject a wrongly built leaf, it would simply fail to verify, and the
// holder would be told their entitlement does not match with no indication why.
//
// Publishing the rule as a vector closes that. The Solidity side replays it through an independent
// implementation, so the two cannot drift, and an external verifier has a concrete case to check
// their own implementation against rather than prose to interpret.
type SnapshotVectors struct {
	AlgoVersion uint32 `json:"algoVersion"`
	LeafDomain  string `json:"leafDomain"`

	EntitlementLeaf EntitlementLeafCase `json:"entitlementLeaf"`

	// Register is a small worked example: holdings in, root out, with a proof.
	Register SnapshotRegisterCase `json:"register"`
}

type EntitlementLeafCase struct {
	LeafIndex      uint32 `json:"leafIndex"`
	Holder         string `json:"holder"`
	InvestorAnchor string `json:"investorAnchor"`
	Units          uint32 `json:"units"`
	Excluded       bool   `json:"excluded"`

	// PreimageHex is included so a mismatch points at the offending byte rather than only reporting
	// that two digests differ.
	PreimageHex string `json:"preimageHex"`
	Leaf        string `json:"leaf"`

	// ExcludedVariant is the same line with the flag flipped, proving the flag is bound into the
	// leaf. Without it, a holder could be reclassified as countable after the root was anchored,
	// which is exactly the number the 200-unitholder minimum polices.
	ExcludedVariantLeaf string `json:"excludedVariantLeaf"`
}

type SnapshotRegisterCase struct {
	TotalUnits      uint32   `json:"totalUnits"`
	DistinctHolders uint32   `json:"distinctHolders"`
	LeafCount       int      `json:"leafCount"`
	Leaves          []string `json:"leaves"`
	Root            string   `json:"root"`

	ProofIndex int      `json:"proofIndex"`
	ProofLeaf  string   `json:"proofLeaf"`
	Proof      []string `json:"proof"`
}

type MerkleVectors struct {
	LeafPrefix string       `json:"leafPrefix"`
	NodePrefix string       `json:"nodePrefix"`
	HashLeaf   HashLeafCase `json:"hashLeaf"`
	HashNode   HashNodeCase `json:"hashNode"`

	// TreeCount is written explicitly because Solidity's JSON helpers cannot
	// conveniently discover array length. An explicit count also means adding a tree
	// to this generator forces the Foundry test to be updated rather than silently
	// leaving the new case unverified.
	TreeCount int        `json:"treeCount"`
	Trees     []TreeCase `json:"trees"`
}

type HashLeafCase struct {
	PreimageHex string `json:"preimageHex"`
	Out         string `json:"out"`
}

type HashNodeCase struct {
	A   string `json:"a"`
	B   string `json:"b"`
	Out string `json:"out"`
	// Swapped proves the sorted-pair property: argument order must not matter.
	Swapped string `json:"swapped"`
}

type TreeCase struct {
	Name       string      `json:"name"`
	LeafCount  int         `json:"leafCount"`
	Leaves     []string    `json:"leaves"`
	Root       string      `json:"root"`
	ProofCount int         `json:"proofCount"`
	Proofs     []ProofCase `json:"proofs"`
}

type ProofCase struct {
	Index int      `json:"index"`
	Leaf  string   `json:"leaf"`
	Proof []string `json:"proof"`
}

type BallotVectors struct {
	AlgoVersion   uint32            `json:"algoVersion"`
	BidLeaf       BidLeafCase       `json:"bidLeaf"`
	AllotmentLeaf AllotmentLeafCase `json:"allotmentLeaf"`
	FinalSeed     FinalSeedCase     `json:"finalSeed"`
	Golden        GoldenCase        `json:"golden"`
	OutcomeCodes  map[string]byte   `json:"outcomeCodes"`
}

type BidLeafCase struct {
	LeafIndex uint32 `json:"leafIndex"`
	// BidRef is the canonical form used everywhere else: 32 lowercase hex characters,
	// no prefix, as stored in the database and pinned to IPFS.
	BidRef string `json:"bidRef"`
	// BidRefBytes is the same 16 bytes with an 0x prefix, because Solidity's JSON
	// helpers require one to parse a value as bytes.
	BidRefBytes       string `json:"bidRefBytes"`
	InvestorAnchor    string `json:"investorAnchor"`
	UnitsBid          uint32 `json:"unitsBid"`
	PricePerUnitPaise uint64 `json:"pricePerUnitPaise"`
	Leaf              string `json:"leaf"`
}

type AllotmentLeafCase struct {
	LeafIndex      uint32 `json:"leafIndex"`
	InvestorAnchor string `json:"investorAnchor"`
	UnitsAllotted  uint32 `json:"unitsAllotted"`
	OutcomeCode    byte   `json:"outcomeCode"`
	BallotRank     uint32 `json:"ballotRank"`
	Leaf           string `json:"leaf"`
}

type FinalSeedCase struct {
	Secret          string `json:"secret"`
	Commitment      string `json:"commitment"`
	TargetBlockHash string `json:"targetBlockHash"`
	BidbookRoot     string `json:"bidbookRoot"`
	Out             string `json:"out"`
	// RankKey pins the ranking derivation, which is what makes a single bid's
	// published position independently checkable.
	RankKeyAnchor    string `json:"rankKeyAnchor"`
	RankKeyLeafIndex uint32 `json:"rankKeyLeafIndex"`
	RankKeyOut       string `json:"rankKeyOut"`
}

type GoldenCase struct {
	BidderCount       int    `json:"bidderCount"`
	BidbookRoot       string `json:"bidbookRoot"`
	FinalSeed         string `json:"finalSeed"`
	ResultRoot        string `json:"resultRoot"`
	UnitsAllotted     uint32 `json:"unitsAllotted"`
	DistinctAllottees uint32 `json:"distinctAllottees"`
}

func main() {
	out := flag.String("out", "", "output path (default: nearest "+DefaultOutputPath+")")
	flag.Parse()

	path := *out
	if path == "" {
		root, err := repoRoot()
		if err != nil {
			fail(err)
		}
		path = filepath.Join(root, filepath.FromSlash(DefaultOutputPath))
	}

	v, err := Build()
	if err != nil {
		fail(err)
	}

	blob, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fail(err)
	}
	blob = append(blob, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fail(err)
	}
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		fail(err)
	}
	fmt.Printf("wrote %d bytes to %s\n", len(blob), path)
}

// Build assembles the vectors. Exported so the parity test can compare against the
// committed file without shelling out.
func Build() (*Vectors, error) {
	v := &Vectors{
		Note: "Cross-language golden vectors. Generated by orchestrator/cmd/genvectors. " +
			"Consumed by Go (internal/parity) and Solidity (contracts/test). " +
			"A change here invalidates every root AcreSync has anchored: bump the algorithm " +
			"version in the same commit.",
	}

	// ----- Merkle -----

	leafPreimage := []byte("acresync-parity-payload")
	a := merkle.HashLeaf([]byte("sibling-a"))
	b := merkle.HashLeaf([]byte("sibling-b"))

	v.Merkle = MerkleVectors{
		LeafPrefix: "0x00",
		NodePrefix: "0x01",
		HashLeaf: HashLeafCase{
			PreimageHex: "0x" + hex.EncodeToString(leafPreimage),
			Out:         merkle.HashLeaf(leafPreimage).Hex(),
		},
		HashNode: HashNodeCase{
			A:       a.Hex(),
			B:       b.Hex(),
			Out: merkle.HashNode(a, b).Hex(),
			// Proves the sorted-pair property: swapping the operands must not change the
			// result, which is what lets a proof omit direction bits.
			Swapped: merkle.HashNode(b, a).Hex(),
		},
	}

	// Sizes chosen to exercise the interesting shapes: a single leaf, a power of two,
	// an odd count that forces promotion, and the real 475-unit public offer size.
	for _, spec := range []struct {
		name  string
		count int
		proof []int
	}{
		{"single", 1, []int{0}},
		{"four", 4, []int{0, 1, 2, 3}},
		{"five", 5, []int{0, 1, 2, 3, 4}},
		{"seven", 7, []int{0, 3, 6}},
		{"public475", 475, []int{0, 1, 237, 473, 474}},
	} {
		leaves := make([]merkle.Hash, spec.count)
		for i := range leaves {
			leaves[i] = merkle.HashLeaf([]byte(fmt.Sprintf("leaf-%d", i)))
		}
		tree, err := merkle.New(leaves)
		if err != nil {
			return nil, fmt.Errorf("building %s tree: %w", spec.name, err)
		}

		tc := TreeCase{Name: spec.name, LeafCount: spec.count, Root: tree.Root().Hex()}

		// Full leaf list only for the small trees; the 475-leaf list would bloat the
		// file without adding coverage, since the proofs already pin the construction.
		if spec.count <= 8 {
			for _, l := range leaves {
				tc.Leaves = append(tc.Leaves, l.Hex())
			}
		}

		for _, idx := range spec.proof {
			proof, err := tree.Proof(idx)
			if err != nil {
				return nil, fmt.Errorf("%s proof %d: %w", spec.name, idx, err)
			}
			pc := ProofCase{Index: idx, Leaf: leaves[idx].Hex()}
			for _, p := range proof {
				pc.Proof = append(pc.Proof, p.Hex())
			}
			if pc.Proof == nil {
				pc.Proof = []string{}
			}
			tc.Proofs = append(tc.Proofs, pc)
		}
		tc.ProofCount = len(tc.Proofs)
		v.Merkle.Trees = append(v.Merkle.Trees, tc)
	}
	v.Merkle.TreeCount = len(v.Merkle.Trees)

	// ----- Ballot -----

	bid := ballot.Bid{
		LeafIndex:         7,
		BidRef:            "0123456789abcdef0123456789abcdef",
		InvestorAnchor:    merkle.HashLeaf([]byte("investor-7")),
		UnitsBid:          3,
		PricePerUnitPaise: money.MinUnitPricePaise,
	}
	bidLeaf, err := ballot.BidLeaf(bid)
	if err != nil {
		return nil, err
	}

	alloc := ballot.Allocation{
		LeafIndex:      7,
		InvestorAnchor: merkle.HashLeaf([]byte("investor-7")),
		UnitsAllotted:  2,
		Outcome:        ballot.OutcomePartial,
		BallotRank:     19,
	}
	allotLeaf, err := ballot.AllotmentLeaf(alloc)
	if err != nil {
		return nil, err
	}

	secret := repeatHash(0x7a)
	blockHash := repeatHash(0x8b)

	// The golden draw, matching TestGoldenResult in the ballot package.
	units := make([]uint32, 600)
	for i := range units {
		units[i] = uint32(i%5) + 1
	}
	bids := make([]ballot.Bid, len(units))
	for i, u := range units {
		bids[i] = ballot.Bid{
			LeafIndex:         uint32(i),
			BidRef:            deterministicBidRef(i),
			InvestorAnchor:    deterministicAnchor(i),
			UnitsBid:          u,
			PricePerUnitPaise: money.MinUnitPricePaise,
		}
	}
	bookRoot, err := ballot.BidbookRoot(bids)
	if err != nil {
		return nil, err
	}
	finalSeed := ballot.DeriveFinalSeed(secret, blockHash, bookRoot)
	result, err := ballot.Run(bids, ballot.V1Params(), finalSeed)
	if err != nil {
		return nil, err
	}

	v.Ballot = BallotVectors{
		AlgoVersion: ballot.AlgoVersion,
		BidLeaf: BidLeafCase{
			LeafIndex:         bid.LeafIndex,
			BidRef:            bid.BidRef,
			BidRefBytes:       "0x" + bid.BidRef,
			InvestorAnchor:    bid.InvestorAnchor.Hex(),
			UnitsBid:          bid.UnitsBid,
			PricePerUnitPaise: uint64(bid.PricePerUnitPaise),
			Leaf:              bidLeaf.Hex(),
		},
		AllotmentLeaf: AllotmentLeafCase{
			LeafIndex:      alloc.LeafIndex,
			InvestorAnchor: alloc.InvestorAnchor.Hex(),
			UnitsAllotted:  alloc.UnitsAllotted,
			OutcomeCode:    1, // PARTIAL
			BallotRank:     alloc.BallotRank,
			Leaf:           allotLeaf.Hex(),
		},
		FinalSeed: FinalSeedCase{
			Secret:           secret.Hex(),
			Commitment:       ballot.Commitment(secret).Hex(),
			TargetBlockHash:  blockHash.Hex(),
			BidbookRoot:      bookRoot.Hex(),
			Out:              finalSeed.Hex(),
			RankKeyAnchor:    bid.InvestorAnchor.Hex(),
			RankKeyLeafIndex: bid.LeafIndex,
			RankKeyOut:       ballot.RankKey(finalSeed, bid.InvestorAnchor, bid.LeafIndex).Hex(),
		},
		Golden: GoldenCase{
			BidderCount:       len(bids),
			BidbookRoot:       bookRoot.Hex(),
			FinalSeed:         finalSeed.Hex(),
			ResultRoot:        result.ResultRoot.Hex(),
			UnitsAllotted:     result.UnitsAllotted,
			DistinctAllottees: result.DistinctAllottees,
		},
		OutcomeCodes: map[string]byte{
			"FULL":          0,
			"PARTIAL":       1,
			"NIL_BALLOT":    2,
			"NIL_TECHNICAL": 3,
		},
	}

	if err := buildSnapshotVectors(v); err != nil {
		return nil, err
	}

	return v, nil
}

// buildSnapshotVectors emits the entitlement leaf case and a small worked register.
func buildSnapshotVectors(v *Vectors) error {
	const holderHex = "0x000000000000000000000000000000000000dead"

	holder, err := snapshot.ParseAddress(holderHex)
	if err != nil {
		return err
	}
	anchor := repeatHash(0xa7)

	const (
		leafIndex = uint32(3)
		units     = uint32(25)
	)

	leaf := snapshot.EntitlementLeaf(leafIndex, holder, anchor, units, false)
	flipped := snapshot.EntitlementLeaf(leafIndex, holder, anchor, units, true)

	v.Snapshot = SnapshotVectors{
		AlgoVersion: snapshot.AlgoVersion,
		LeafDomain:  snapshot.LeafDomain,
		EntitlementLeaf: EntitlementLeafCase{
			LeafIndex:           leafIndex,
			Holder:              holderHex,
			InvestorAnchor:      anchor.Hex(),
			Units:               units,
			Excluded:            false,
			PreimageHex:         "0x" + hex.EncodeToString(snapshot.LeafPreimage(leafIndex, holder, anchor, units, false)),
			Leaf:                leaf.Hex(),
			ExcludedVariantLeaf: flipped.Hex(),
		},
	}

	// A worked register, deliberately not a power of two so odd-node promotion is exercised. One
	// excluded holder is included so the vector shows totalUnits and distinctHolders diverging, which
	// is the distinction most likely to be implemented wrongly by a third party.
	holdings := []snapshot.Holding{
		{InvestorID: "11111111-1111-4111-8111-111111111111", WalletAddress: "0x0000000000000000000000000000000000000001",
			InvestorAnchor: repeatHash(0x11), Units: 25, ExcludedFromHolderCount: true},
		{InvestorID: "22222222-2222-4222-8222-222222222222", WalletAddress: "0x0000000000000000000000000000000000000002",
			InvestorAnchor: repeatHash(0x22), Units: 3},
		{InvestorID: "33333333-3333-4333-8333-333333333333", WalletAddress: "0x0000000000000000000000000000000000000003",
			InvestorAnchor: repeatHash(0x33), Units: 7},
		{InvestorID: "44444444-4444-4444-8444-444444444444", WalletAddress: "0x0000000000000000000000000000000000000004",
			InvestorAnchor: repeatHash(0x44), Units: 1},
		{InvestorID: "55555555-5555-4555-8555-555555555555", WalletAddress: "0x0000000000000000000000000000000000000005",
			InvestorAnchor: repeatHash(0x55), Units: 14},
	}

	recordDate := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	snap, err := snapshot.Build(snapshot.BuildInput{
		SchemeRef:  repeatHash(0x5c),
		PeriodID:   1,
		RecordDate: recordDate,
		PeriodEnd:  recordDate,
		TakenAt:    time.Date(2026, 9, 30, 18, 30, 0, 0, time.UTC),
		Holdings:   holdings,
		// RequireMinimumHolders is false: this is a five-line fixture, not a live register.
	})
	if err != nil {
		return err
	}

	const proofIndex = 2
	proof, err := snap.Proof(proofIndex)
	if err != nil {
		return err
	}

	leaves := make([]string, 0, len(snap.Lines))
	for _, l := range snap.Lines {
		leaves = append(leaves, l.Leaf.Hex())
	}
	proofHex := make([]string, 0, len(proof))
	for _, p := range proof {
		proofHex = append(proofHex, p.Hex())
	}

	v.Snapshot.Register = SnapshotRegisterCase{
		TotalUnits:      snap.TotalUnits,
		DistinctHolders: snap.DistinctHolders,
		LeafCount:       len(snap.Lines),
		Leaves:          leaves,
		Root:            snap.MerkleRoot.Hex(),
		ProofIndex:      proofIndex,
		ProofLeaf:       snap.Lines[proofIndex].Leaf.Hex(),
		Proof:           proofHex,
	}

	return nil
}

func repeatHash(b byte) merkle.Hash {
	var h merkle.Hash
	for i := range h {
		h[i] = b
	}
	return h
}

// These mirror the helpers in ballot_test.go so the golden draw here reproduces the
// one pinned by TestGoldenResult.
func deterministicAnchor(i int) merkle.Hash {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(i)+0x9e3779b97f4a7c15)
	return merkle.Hash(sha256.Sum256(buf[:]))
}

func deterministicBidRef(i int) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(i))
	h := sha256.Sum256(buf[:])
	return hex.EncodeToString(h[:ballot.BidRefLen])
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for range 8 {
		if _, err := os.Stat(filepath.Join(dir, "contracts")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", errors.New("could not locate a contracts directory; pass -out")
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "genvectors: %v\n", err)
	os.Exit(1)
}
