// Package merkle builds the Merkle trees whose roots AcreSync anchors on-chain.
//
// Two trees matter: the frozen bid book, anchored before the ballot seed is
// committed, and the record-date register snapshot, anchored with each
// distribution period. In both cases the root is the entire basis for a third
// party's ability to verify a claim without trusting us, so the construction has
// to be specified exactly rather than left to whatever a library happens to do.
//
// # Hash choice
//
// SHA-256, not keccak256. That is a deliberate deviation from Ethereum
// convention. AcreSync already uses SHA-256 for idempotency keys, IPFS content
// digests and investor anchors, and a verifier who has to implement one hash
// function has a materially easier job than one juggling two. Solidity exposes
// sha256() as a precompile, so the on-chain cost difference across a proof of
// depth ten is a few hundred gas inside a view function, which is immaterial.
//
// # Domain separation
//
// Leaves are hashed as sha256(0x00 || data) and internal nodes as
// sha256(0x01 || left || right). Without the distinct prefixes an attacker can
// present an internal node as though it were a leaf, because the node's hash is a
// valid hash of 64 bytes of "leaf data". That is the standard second-preimage
// attack on naive Merkle trees, and the one-byte prefix closes it.
//
// # Sorted pairs
//
// Sibling hashes are concatenated in ascending byte order, so a proof carries no
// direction bits and verification is a fold. The known cost is that a tree with
// duplicate leaves becomes ambiguous, so New rejects duplicates outright rather
// than accepting input it cannot represent faithfully.
//
// # Odd levels
//
// An unpaired node is promoted unchanged to the next level rather than hashed
// with itself. Duplicating a node would make a tree with n leaves and one with a
// particular n+1 leaves produce the same root, which is a forgery surface.
package merkle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

const (
	// LeafPrefix domain-separates leaf hashes.
	LeafPrefix byte = 0x00
	// NodePrefix domain-separates internal node hashes.
	NodePrefix byte = 0x01
)

// Size is the digest length in bytes.
const Size = sha256.Size

var (
	ErrNoLeaves        = errors.New("merkle: cannot build a tree with no leaves")
	ErrDuplicateLeaf   = errors.New("merkle: duplicate leaf")
	ErrIndexOutOfRange = errors.New("merkle: leaf index out of range")
)

// Hash is a 32-byte SHA-256 digest.
type Hash [Size]byte

// Hex renders the digest as 0x-prefixed lowercase hex, the form used in the
// database, in pinned documents, and in contract calldata.
func (h Hash) Hex() string { return "0x" + hex.EncodeToString(h[:]) }

func (h Hash) String() string { return h.Hex() }

// IsZero reports whether the digest is unset.
func (h Hash) IsZero() bool { return h == Hash{} }

// ParseHash parses the 0x-prefixed lowercase hex form.
func ParseHash(s string) (Hash, error) {
	var h Hash
	if len(s) != 2+2*Size || s[:2] != "0x" {
		return h, fmt.Errorf("merkle: expected 0x followed by %d hex characters, got %q", 2*Size, s)
	}
	raw, err := hex.DecodeString(s[2:])
	if err != nil {
		return h, fmt.Errorf("merkle: %w", err)
	}
	// Lowercase only, so one digest has exactly one spelling and a content address
	// derived from it is reproducible.
	if hex.EncodeToString(raw) != s[2:] {
		return h, fmt.Errorf("merkle: hex must be lowercase: %q", s)
	}
	copy(h[:], raw)
	return h, nil
}

// HashLeaf computes sha256(0x00 || data).
func HashLeaf(data []byte) Hash {
	h := sha256.New()
	h.Write([]byte{LeafPrefix})
	h.Write(data)
	var out Hash
	copy(out[:], h.Sum(nil))
	return out
}

// HashNode computes sha256(0x01 || min(a,b) || max(a,b)).
//
// Exported because it is part of the published specification: an independent verifier
// needs both the leaf and node rules to reproduce a root, and the Solidity library is
// held to this exact behaviour by the shared golden vectors.
func HashNode(a, b Hash) Hash {
	lo, hi := a, b
	if bytes.Compare(a[:], b[:]) > 0 {
		lo, hi = b, a
	}
	h := sha256.New()
	h.Write([]byte{NodePrefix})
	h.Write(lo[:])
	h.Write(hi[:])
	var out Hash
	copy(out[:], h.Sum(nil))
	return out
}

// Tree is an immutable Merkle tree over an ordered set of leaves.
type Tree struct {
	// levels[0] is the leaves, levels[len-1] is the single-element root level.
	levels [][]Hash
}

// New builds a tree over leaves, which must be non-empty and unique.
//
// Leaf order is significant and is the caller's responsibility: the bid book is
// ordered by leaf index and the register snapshot by leaf index, both of which are
// recorded in the pinned document so a verifier can reconstruct the same tree.
func New(leaves []Hash) (*Tree, error) {
	if len(leaves) == 0 {
		return nil, ErrNoLeaves
	}

	seen := make(map[Hash]int, len(leaves))
	for i, l := range leaves {
		if prior, dup := seen[l]; dup {
			return nil, fmt.Errorf("%w: index %d repeats index %d (%s)", ErrDuplicateLeaf, i, prior, l.Hex())
		}
		seen[l] = i
	}

	level := make([]Hash, len(leaves))
	copy(level, leaves)
	levels := [][]Hash{level}

	for len(level) > 1 {
		next := make([]Hash, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			if i+1 == len(level) {
				// Promote the unpaired node unchanged.
				next = append(next, level[i])
				continue
			}
			next = append(next, HashNode(level[i], level[i+1]))
		}
		levels = append(levels, next)
		level = next
	}

	return &Tree{levels: levels}, nil
}

// Root returns the tree root. For a single-leaf tree the root is that leaf.
func (t *Tree) Root() Hash { return t.levels[len(t.levels)-1][0] }

// LeafCount returns the number of leaves.
func (t *Tree) LeafCount() int { return len(t.levels[0]) }

// Depth returns the number of levels above the leaves.
func (t *Tree) Depth() int { return len(t.levels) - 1 }

// Leaf returns the leaf at index.
func (t *Tree) Leaf(index int) (Hash, error) {
	if index < 0 || index >= len(t.levels[0]) {
		return Hash{}, fmt.Errorf("%w: %d (have %d leaves)", ErrIndexOutOfRange, index, len(t.levels[0]))
	}
	return t.levels[0][index], nil
}

// Proof returns the sibling hashes needed to recompute the root from the leaf at
// index, ordered from the leaf level upward.
//
// A promoted node contributes no sibling, so proof length varies between leaves in
// a tree whose leaf count is not a power of two. That is expected and Verify
// handles it, because verification folds the proof rather than walking a fixed
// depth.
func (t *Tree) Proof(index int) ([]Hash, error) {
	if index < 0 || index >= len(t.levels[0]) {
		return nil, fmt.Errorf("%w: %d (have %d leaves)", ErrIndexOutOfRange, index, len(t.levels[0]))
	}

	proof := make([]Hash, 0, t.Depth())
	pos := index
	for level := 0; level < len(t.levels)-1; level++ {
		nodes := t.levels[level]
		var sibling int
		if pos%2 == 0 {
			sibling = pos + 1
		} else {
			sibling = pos - 1
		}
		if sibling < len(nodes) {
			proof = append(proof, nodes[sibling])
		}
		// A promoted node keeps its index halved the same way a hashed pair does.
		pos /= 2
	}
	return proof, nil
}

// Verify recomputes a root from a leaf and its proof.
//
// This is the exact algorithm the Solidity verifier must implement, and the pair
// of implementations is cross-checked in the contract tests at M2.
func Verify(root, leaf Hash, proof []Hash) bool {
	computed := leaf
	for _, sibling := range proof {
		computed = HashNode(computed, sibling)
	}
	return computed == root
}

// RootOf builds a tree and returns only its root, for callers that need no proofs.
func RootOf(leaves []Hash) (Hash, error) {
	t, err := New(leaves)
	if err != nil {
		return Hash{}, err
	}
	return t.Root(), nil
}
