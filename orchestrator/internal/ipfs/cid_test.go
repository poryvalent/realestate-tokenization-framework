package ipfs

import (
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	refcid "github.com/ipfs/go-cid"
	refmh "github.com/multiformats/go-multihash"
)

// TestCIDMatchesReferenceImplementation is the test that makes the derivation rule credible.
//
// Everything else about the CID code could be self-consistent and still wrong: a hand-rolled
// encoder checked only against its own decoder will agree with itself about an incorrect prefix or
// a wrong base32 alphabet forever. The claim in cid.go is that anybody can take a digest off-chain
// and compute an address that resolves on public IPFS infrastructure, and that claim is about
// agreement with the wider ecosystem, not about internal consistency.
//
// So the derivation is checked against github.com/ipfs/go-cid, the implementation IPFS itself uses.
// The dependency is test-only; production code stays free of it, because the point is to verify a
// four-byte prefix and a base32 alphabet, not to take on a multiformats dependency tree to write
// them out.
//
// This also confirms something the fixed-width prefix quietly assumes. CIDv1 varint-encodes the
// version and codec, and the code emits them as single bytes. That is correct only because 0x01 and
// 0x55 are both below 0x80, where varint encoding is the identity. Agreement with the reference
// encoder proves it rather than leaving it as a comment.
func TestCIDMatchesReferenceImplementation(t *testing.T) {
	inputs := [][]byte{
		{},
		[]byte("hello world"),
		[]byte(`{"docType":"SNAPSHOT"}`),
		[]byte(strings.Repeat("a", 1024)),
		[]byte(strings.Repeat("snapshot line ", 5000)),
		{0x00},
		{0xff, 0xfe, 0xfd},
	}

	for _, in := range inputs {
		digest := sha256.Sum256(in)

		// The reference path: wrap the digest as a sha2-256 multihash, build a CIDv1 with the raw
		// codec, and render it in the default base32 encoding.
		mh, err := refmh.Encode(digest[:], refmh.SHA2_256)
		if err != nil {
			t.Fatalf("reference multihash encode: %v", err)
		}
		want := refcid.NewCidV1(refcid.Raw, mh).String()

		got := CIDFromDigest(digest)
		if got != want {
			t.Fatalf("derivation disagrees with go-cid for %d-byte input:\n  ours:      %s\n  reference: %s",
				len(in), got, want)
		}
	}
}

// TestReferenceCIDsDecodeBackToTheirDigest checks the reverse direction against the same library, so
// a verifier who has a CID from an external tool recovers the digest AcreSync anchored.
func TestReferenceCIDsDecodeBackToTheirDigest(t *testing.T) {
	for _, in := range [][]byte{[]byte("a"), []byte("hello world"), {}} {
		digest := sha256.Sum256(in)
		mh, err := refmh.Encode(digest[:], refmh.SHA2_256)
		if err != nil {
			t.Fatal(err)
		}
		external := refcid.NewCidV1(refcid.Raw, mh).String()

		got, err := DigestFromCID(external)
		if err != nil {
			t.Fatalf("a CID produced by go-cid must decode: %v", err)
		}
		if got != digest {
			t.Fatalf("digest round trip failed: got %x want %x", got, digest)
		}
	}
}

func TestCIDRoundTrip(t *testing.T) {
	digest := sha256.Sum256([]byte("acresync"))
	cid := CIDFromDigest(digest)

	back, err := DigestFromCID(cid)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if back != digest {
		t.Fatalf("got %x want %x", back, digest)
	}
}

// TestDagPbCIDRejected is the rejection that protects the anchor.
//
// A dag-pb CID commits to a UnixFS node wrapping the content, so its digest is not SHA-256 of the
// document. Accepting one where a raw CID is expected would mean comparing a document against a
// commitment it was never measured against, and the comparison would fail with no indication that
// the encoding was the reason.
func TestDagPbCIDRejected(t *testing.T) {
	digest := sha256.Sum256([]byte("acresync"))
	mh, err := refmh.Encode(digest[:], refmh.SHA2_256)
	if err != nil {
		t.Fatal(err)
	}
	dagpb := refcid.NewCidV1(refcid.DagProtobuf, mh).String()

	_, decErr := DigestFromCID(dagpb)
	if !errors.Is(decErr, ErrBadCID) {
		t.Fatalf("a dag-pb CID must be rejected, got %v", decErr)
	}
	// The message has to name the codec. An operator seeing only "bad CID" would start by
	// suspecting corruption or a truncated string, when the actual cause is an encoding choice made
	// by the pinning service, which is a completely different investigation.
	if !strings.Contains(decErr.Error(), "dag-pb") {
		t.Errorf("the error should name dag-pb so the cause is obvious, got: %v", decErr)
	}
}

// TestCIDv0Rejected guards the legacy encoding, which is base58 dag-pb and carries no version byte.
func TestCIDv0Rejected(t *testing.T) {
	digest := sha256.Sum256([]byte("acresync"))
	mh, e := refmh.Encode(digest[:], refmh.SHA2_256)
	if e != nil {
		t.Fatal(e)
	}
	v0 := refcid.NewCidV0(mh).String()

	if _, e := DigestFromCID(v0); !errors.Is(e, ErrBadCID) {
		t.Fatalf("a CIDv0 must be rejected, got %v", e)
	}
}

// TestNonSHA256CIDRejected covers a correctly formed CIDv1 raw address using a different hash.
// It is the right shape and the wrong commitment, which is the dangerous combination.
func TestNonSHA256CIDRejected(t *testing.T) {
	sum := sha256.Sum256([]byte("acresync"))
	mh, e := refmh.Encode(sum[:], refmh.SHA3_256)
	if e != nil {
		t.Fatal(e)
	}
	other := refcid.NewCidV1(refcid.Raw, mh).String()

	if _, e := DigestFromCID(other); !errors.Is(e, ErrBadCID) {
		t.Fatalf("a non-sha2-256 multihash must be rejected, got %v", e)
	}
}

func TestMalformedCIDsRejected(t *testing.T) {
	digest := sha256.Sum256([]byte("acresync"))
	valid := CIDFromDigest(digest)

	cases := map[string]string{
		"empty":               "",
		"no multibase prefix": valid[1:],
		"wrong multibase":     "z" + valid[1:],
		"uppercase body":      "b" + strings.ToUpper(valid[1:]),
		"truncated":           valid[:len(valid)-4],
		"extra characters":    valid + "aaaa",
		"not base32":          "b!!!!!!!!",
	}

	for name, cid := range cases {
		t.Run(name, func(t *testing.T) {
			if _, e := DigestFromCID(cid); e == nil {
				t.Fatalf("%q must be rejected", cid)
			}
		})
	}
}

// TestUppercaseRejected exists separately because a base32 decoder that accepted both cases would
// give one digest two spellings, and a verifier comparing CID strings would see a mismatch on
// identical content.
func TestUppercaseRejected(t *testing.T) {
	digest := sha256.Sum256([]byte("acresync"))
	upper := "b" + strings.ToUpper(CIDFromDigest(digest)[1:])

	if _, e := DigestFromCID(upper); !errors.Is(e, ErrBadCID) {
		t.Fatalf("an uppercase body must be rejected, got %v", e)
	}
}

func TestGatewayURL(t *testing.T) {
	cid := CIDFromDigest(sha256.Sum256([]byte("x")))
	want := "https://gw.example/ipfs/" + cid

	if got := GatewayURL("https://gw.example", cid); got != want {
		t.Errorf("got %s want %s", got, want)
	}
	if got := GatewayURL("https://gw.example/", cid); got != want {
		t.Errorf("a trailing slash must not double up: got %s", got)
	}
}
