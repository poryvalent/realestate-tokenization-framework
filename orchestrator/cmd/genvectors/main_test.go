package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The Go half of the drift guard.
//
// The Solidity tests replay the committed vector file, so they would keep passing
// forever against a stale file even if the Go implementation changed underneath. This
// test closes that gap: if Go output no longer matches the committed file, Go CI fails
// and the operator is told to regenerate and to consider what the change invalidates.
func TestCommittedVectorsMatchCurrentOutput(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, filepath.FromSlash(DefaultOutputPath))

	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading committed vectors: %v\n\nRegenerate with: go run ./cmd/genvectors", err)
	}

	built, err := Build()
	if err != nil {
		t.Fatal(err)
	}
	current, err := json.MarshalIndent(built, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	current = append(current, '\n')

	if string(committed) == string(current) {
		return
	}

	// Report which top-level sections moved, which is more useful than a diff of a
	// 14KB file when the real question is what got invalidated.
	var a, b map[string]any
	_ = json.Unmarshal(committed, &a)
	_ = json.Unmarshal(current, &b)

	changed := []string{}
	for _, section := range []string{"merkle", "ballot", "snapshot"} {
		ja, _ := json.Marshal(a[section])
		jb, _ := json.Marshal(b[section])
		if string(ja) != string(jb) {
			changed = append(changed, section)
		}
	}

	t.Fatalf(
		"committed vectors do not match current Go output; changed sections: %v\n\n"+
			"Regenerate with:  go run ./cmd/genvectors\n\n"+
			"Then consider what the change invalidated. These vectors define the leaf\n"+
			"encodings, the Merkle construction and the seed derivation. A change to any of\n"+
			"them means every root and seed AcreSync has anchored can no longer be reproduced\n"+
			"by a verifier following the published rules, so the algorithm version must be\n"+
			"incremented in the same commit.",
		changed)
}

// Guards against the vector file silently losing coverage, which would leave the
// Solidity parity tests passing while checking almost nothing.
func TestVectorCoverage(t *testing.T) {
	v, err := Build()
	if err != nil {
		t.Fatal(err)
	}

	if v.Merkle.TreeCount != len(v.Merkle.Trees) {
		t.Errorf("TreeCount %d disagrees with %d trees", v.Merkle.TreeCount, len(v.Merkle.Trees))
	}
	if v.Merkle.TreeCount < 5 {
		t.Errorf("only %d trees; the set should cover a single leaf, a power of two, "+
			"an odd count that forces promotion, and the 475-unit offer size", v.Merkle.TreeCount)
	}

	var sawPublicOffer bool
	totalProofs := 0
	for _, tree := range v.Merkle.Trees {
		if tree.ProofCount != len(tree.Proofs) {
			t.Errorf("tree %s: ProofCount %d disagrees with %d proofs",
				tree.Name, tree.ProofCount, len(tree.Proofs))
		}
		if tree.Root == "" {
			t.Errorf("tree %s has no root", tree.Name)
		}
		totalProofs += tree.ProofCount
		if tree.LeafCount == 475 {
			sawPublicOffer = true
		}
	}
	if !sawPublicOffer {
		t.Error("no 475-leaf tree; that is the real public offer size and the case that " +
			"exercises node promotion at scale")
	}
	if totalProofs < 10 {
		t.Errorf("only %d proofs across all trees", totalProofs)
	}

	// Sorted pairing is what lets a proof omit direction bits.
	if v.Merkle.HashNode.Out != v.Merkle.HashNode.Swapped {
		t.Error("hashNode vector is not order independent; sorted pairing is broken")
	}

	if v.Ballot.BidLeaf.Leaf == "" || v.Ballot.AllotmentLeaf.Leaf == "" {
		t.Error("ballot leaf vectors are incomplete")
	}
	if v.Ballot.FinalSeed.Out == "" || v.Ballot.FinalSeed.Commitment == "" || v.Ballot.FinalSeed.RankKeyOut == "" {
		t.Error("seed, commitment or rank key vector is missing")
	}
	if v.Ballot.Golden.UnitsAllotted != 475 {
		t.Errorf("golden draw allotted %d units, expected the full 475", v.Ballot.Golden.UnitsAllotted)
	}
	if v.Ballot.Golden.DistinctAllottees < 200 {
		t.Errorf("golden draw reached %d holders, below the statutory floor of 200",
			v.Ballot.Golden.DistinctAllottees)
	}
	if len(v.Ballot.OutcomeCodes) != 4 {
		t.Errorf("expected 4 outcome codes, got %d", len(v.Ballot.OutcomeCodes))
	}
}

// Determinism, because a generator that varied between runs would make the drift guard
// fire constantly and train everyone to regenerate without reading the message.
func TestBuildIsDeterministic(t *testing.T) {
	first, err := Build()
	if err != nil {
		t.Fatal(err)
	}
	a, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}

	for range 20 {
		again, err := Build()
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(again)
		if err != nil {
			t.Fatal(err)
		}
		if string(a) != string(b) {
			t.Fatal("Build is not deterministic")
		}
	}
}
