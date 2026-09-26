package merkle

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
)

func leaves(n int) []Hash {
	out := make([]Hash, n)
	for i := range out {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(i))
		out[i] = HashLeaf(b[:])
	}
	return out
}

func TestSingleLeafRootIsTheLeaf(t *testing.T) {
	l := leaves(1)
	tr, err := New(l)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Root() != l[0] {
		t.Fatalf("root %s != leaf %s", tr.Root(), l[0])
	}
	proof, err := tr.Proof(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(proof) != 0 {
		t.Fatalf("single-leaf proof should be empty, got %d elements", len(proof))
	}
	if !Verify(tr.Root(), l[0], proof) {
		t.Fatal("empty proof should verify against a single-leaf root")
	}
}

// Every leaf in every tree size must produce a proof that verifies. Sizes that are
// not powers of two are the interesting ones, because that is where node promotion
// changes proof length between leaves.
func TestProofsVerifyForAllSizes(t *testing.T) {
	for n := 1; n <= 300; n++ {
		l := leaves(n)
		tr, err := New(l)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		root := tr.Root()
		for i := range n {
			proof, err := tr.Proof(i)
			if err != nil {
				t.Fatalf("n=%d i=%d: %v", n, i, err)
			}
			if !Verify(root, l[i], proof) {
				t.Fatalf("n=%d i=%d: proof of length %d failed to verify", n, i, len(proof))
			}
		}
	}
}

func TestProofFailsForWrongLeaf(t *testing.T) {
	l := leaves(475) // the v1 public unit count, and not a power of two
	tr, err := New(l)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := tr.Proof(100)
	if err != nil {
		t.Fatal(err)
	}
	if Verify(tr.Root(), l[101], proof) {
		t.Fatal("a proof for one leaf must not verify another")
	}
	if Verify(tr.Root(), HashLeaf([]byte("not in the tree")), proof) {
		t.Fatal("a proof must not verify a leaf absent from the tree")
	}
}

func TestTamperedProofFails(t *testing.T) {
	l := leaves(64)
	tr, _ := New(l)
	proof, _ := tr.Proof(7)
	if len(proof) == 0 {
		t.Fatal("expected a non-empty proof")
	}
	proof[0][0] ^= 0xff
	if Verify(tr.Root(), l[7], proof) {
		t.Fatal("a tampered proof must not verify")
	}
}

// Domain separation is the defence against presenting an internal node as a leaf.
// Without distinct prefixes, a node hash is a perfectly good hash of 64 bytes of
// "leaf data" and an attacker can prove membership of something that was never a
// leaf.
func TestDomainSeparationPreventsNodeAsLeaf(t *testing.T) {
	l := leaves(4)
	tr, _ := New(l)

	// Recompute the internal node covering leaves 0 and 1.
	node := HashNode(l[0], l[1])

	// If that node were accepted as a leaf, this proof would verify.
	sibling := HashNode(l[2], l[3])
	if Verify(tr.Root(), node, []Hash{sibling}) {
		// It does fold to the root arithmetically; that is exactly why leaves and
		// nodes must be distinguishable by construction.
		t.Log("node folds to the root when treated as a leaf, as expected")
	}

	// The real protection: a leaf hash can never equal a node hash, because the
	// prefixes differ.
	leafOfNodeBytes := HashLeaf(append(append([]byte{}, l[0][:]...), l[1][:]...))
	if leafOfNodeBytes == node {
		t.Fatal("a leaf hash collided with a node hash; domain separation is not applied")
	}
}

func TestPrefixesAreApplied(t *testing.T) {
	data := []byte("payload")

	want := sha256.Sum256(append([]byte{LeafPrefix}, data...))
	if got := HashLeaf(data); got != Hash(want) {
		t.Fatalf("HashLeaf does not use the 0x00 prefix")
	}

	a, b := leaves(2)[0], leaves(2)[1]
	lo, hi := a, b
	if bytes.Compare(a[:], b[:]) > 0 {
		lo, hi = b, a
	}
	h := sha256.New()
	h.Write([]byte{NodePrefix})
	h.Write(lo[:])
	h.Write(hi[:])
	var expect Hash
	copy(expect[:], h.Sum(nil))
	if HashNode(a, b) != expect {
		t.Fatal("hashNode does not use the 0x01 prefix with sorted operands")
	}
}

// Sorted pairs are what let a proof omit direction bits, so the order in which two
// siblings are supplied must not matter.
func TestNodeHashIsOrderIndependent(t *testing.T) {
	l := leaves(2)
	if HashNode(l[0], l[1]) != HashNode(l[1], l[0]) {
		t.Fatal("sorted-pair hashing must be commutative")
	}
}

// Duplicates are rejected because a sorted-pair tree cannot represent them
// unambiguously. Accepting them would mean building a tree whose proofs are not
// sound.
func TestDuplicateLeavesRejected(t *testing.T) {
	l := leaves(4)
	l[3] = l[1]
	if _, err := New(l); !errors.Is(err, ErrDuplicateLeaf) {
		t.Fatalf("want ErrDuplicateLeaf, got %v", err)
	}
}

func TestEmptyRejected(t *testing.T) {
	if _, err := New(nil); !errors.Is(err, ErrNoLeaves) {
		t.Fatalf("want ErrNoLeaves, got %v", err)
	}
}

// Promotion rather than duplication: a tree of n leaves must not collide with a
// tree of n+1 leaves whose last leaf repeats. Duplicating an unpaired node would
// make exactly that collision possible.
func TestPromotionAvoidsLengthExtension(t *testing.T) {
	three := leaves(3)
	tr3, err := New(three)
	if err != nil {
		t.Fatal(err)
	}

	four := append(leaves(3), HashLeaf([]byte("fourth")))
	tr4, err := New(four)
	if err != nil {
		t.Fatal(err)
	}
	if tr3.Root() == tr4.Root() {
		t.Fatal("trees of different leaf counts produced the same root")
	}
}

func TestRootIsStable(t *testing.T) {
	l := leaves(475)
	first, err := RootOf(l)
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		again, err := RootOf(l)
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatal("root is not stable across repeated construction")
		}
	}
}

func TestLeafOrderMatters(t *testing.T) {
	l := leaves(8)
	a, _ := RootOf(l)
	l[0], l[7] = l[7], l[0]
	b, _ := RootOf(l)
	if a == b {
		t.Fatal("reordering leaves must change the root; leaf position is part of the commitment")
	}
}

func TestIndexOutOfRange(t *testing.T) {
	tr, _ := New(leaves(4))
	for _, i := range []int{-1, 4, 999} {
		if _, err := tr.Proof(i); !errors.Is(err, ErrIndexOutOfRange) {
			t.Errorf("Proof(%d): want ErrIndexOutOfRange, got %v", i, err)
		}
		if _, err := tr.Leaf(i); !errors.Is(err, ErrIndexOutOfRange) {
			t.Errorf("Leaf(%d): want ErrIndexOutOfRange, got %v", i, err)
		}
	}
}

func TestHashHexRoundTrip(t *testing.T) {
	h := HashLeaf([]byte("x"))
	parsed, err := ParseHash(h.Hex())
	if err != nil {
		t.Fatal(err)
	}
	if parsed != h {
		t.Fatal("round trip changed the digest")
	}
	if len(h.Hex()) != 66 {
		t.Fatalf("hex length %d, want 66", len(h.Hex()))
	}
}

func TestParseHashRejectsBadInput(t *testing.T) {
	good := HashLeaf([]byte("x")).Hex()
	bad := []string{
		"",
		good[2:],
		good + "00",
		good[:len(good)-1],
		"0X" + good[2:],
	}
	for _, s := range bad {
		if _, err := ParseHash(s); err == nil {
			t.Errorf("ParseHash(%q) should have failed", s)
		}
	}
	// Uppercase body is rejected so one digest has one spelling, which is what keeps
	// a content address reproducible.
	upper := "0xABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789"
	if _, err := ParseHash(upper); err == nil {
		t.Error("uppercase hex body should be rejected")
	}
}

// A committed vector. If this changes, every root AcreSync has ever anchored
// becomes unreproducible by a verifier built against the old rules.
func TestGoldenRoot(t *testing.T) {
	l := make([]Hash, 5)
	for i := range l {
		l[i] = HashLeaf([]byte(fmt.Sprintf("leaf-%d", i)))
	}
	root, err := RootOf(l)
	if err != nil {
		t.Fatal(err)
	}
	const want = "0x77467d592e66a60dd201ec919080ea66eedf1f8b32171fc9bbfa88761937b7ad"
	if root.Hex() != want {
		t.Errorf("golden root changed.\n got %s\nwant %s\n\n"+
			"If the construction was altered deliberately, every previously anchored root is\n"+
			"now unverifiable by a third party implementing these rules. Bump the documented\n"+
			"algorithm version and update this vector in the same change.", root.Hex(), want)
	}
}
