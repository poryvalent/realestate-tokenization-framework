// Package ipfs publishes AcreSync's evidence documents and records their content addresses.
//
// # The problem this package solves
//
// The contracts anchor a 32-byte SHA-256 digest, because a CIDv1 is 36 bytes and does not fit a
// bytes32. So the on-chain record alone is not enough to fetch a document: something has to get a
// verifier from "this digest is anchored" to "here is the file". That mapping is the CID
// reconstruction rule below, and it has to be published rather than kept internal, or the
// "independently verifiable" claim reduces to "verifiable by us".
//
// # Why the digest and not the CID
//
// Storing the full CID as dynamic bytes would cost more gas and, worse, would let two encodings of
// the same content disagree. A raw digest has exactly one form. The CID is then derived
// deterministically from it, so the anchored value is canonical and the fetchable address is a
// function of it.
package ipfs

import (
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
)

// The CIDv1 reconstruction rule.
//
// A CIDv1 is <multibase><version><multicodec><multihash>, and the multihash is
// <hash-function><length><digest>. AcreSync fixes every field except the digest:
//
//	multibase      'b'   base32 lowercase, RFC 4648, no padding
//	version        0x01  CIDv1
//	multicodec     0x55  raw, because the pinned bytes are the content, not a dag-pb wrapper
//	hash function  0x12  sha2-256
//	digest length  0x20  32 bytes
//
// Fixing the codec to raw matters. dag-pb would wrap the content in a protobuf node, so the digest
// anchored on-chain would be the hash of the wrapper rather than of the document, and a verifier
// hashing the file they downloaded would get a different value. With raw, the anchored digest is
// exactly SHA-256 of the canonical bytes, which is the only version anyone can reproduce without
// implementing IPFS internals.
const (
	multibasePrefix   = 'b'
	cidVersion1       = 0x01
	multicodecRaw     = 0x55
	multihashSHA2_256 = 0x12
	digestLength      = 0x20
)

// base32Lower is RFC 4648 base32 with the lowercase alphabet and no padding, which is what the
// 'b' multibase prefix denotes.
var base32Lower = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

var (
	ErrBadCID       = errors.New("ipfs: not a CIDv1 raw sha2-256 content address")
	ErrDigestLength = errors.New("ipfs: digest must be 32 bytes")
)

// CIDFromDigest derives the content address for a SHA-256 digest.
//
// This is the published rule. Given a digest read from an on-chain anchor, anyone can compute the
// CID and fetch the document from any gateway, then hash it and confirm they got the bytes the
// contract committed to.
func CIDFromDigest(digest [32]byte) string {
	buf := make([]byte, 0, 4+len(digest))
	buf = append(buf, cidVersion1, multicodecRaw, multihashSHA2_256, digestLength)
	buf = append(buf, digest[:]...)
	return string(multibasePrefix) + base32Lower.EncodeToString(buf)
}

// DigestFromCID recovers the digest from a CID, rejecting anything that is not the exact shape
// AcreSync publishes.
//
// Strict on purpose. A CIDv0, a dag-pb CID, or a different hash function would all decode to
// something digest-shaped while not being the value the contract anchored, and silently accepting
// one would mean comparing a document against a commitment it was never measured against.
func DigestFromCID(cid string) ([32]byte, error) {
	var out [32]byte

	if len(cid) == 0 || cid[0] != multibasePrefix {
		return out, fmt.Errorf("%w: expected the base32 multibase prefix 'b', got %q", ErrBadCID, cid)
	}
	body := cid[1:]
	if body != strings.ToLower(body) {
		return out, fmt.Errorf("%w: base32 body must be lowercase", ErrBadCID)
	}

	raw, err := base32Lower.DecodeString(body)
	if err != nil {
		return out, fmt.Errorf("%w: %v", ErrBadCID, err)
	}
	if len(raw) != 4+32 {
		return out, fmt.Errorf("%w: decoded %d bytes, expected 36", ErrBadCID, len(raw))
	}
	if raw[0] != cidVersion1 {
		return out, fmt.Errorf("%w: version is 0x%02x, expected CIDv1", ErrBadCID, raw[0])
	}
	if raw[1] != multicodecRaw {
		return out, fmt.Errorf("%w: multicodec is 0x%02x, expected raw (0x55). A dag-pb CID commits "+
			"to a wrapper rather than to the document bytes", ErrBadCID, raw[1])
	}
	if raw[2] != multihashSHA2_256 {
		return out, fmt.Errorf("%w: hash function is 0x%02x, expected sha2-256 (0x12)", ErrBadCID, raw[2])
	}
	if raw[3] != digestLength {
		return out, fmt.Errorf("%w: digest length is %d, expected 32", ErrBadCID, raw[3])
	}

	copy(out[:], raw[4:])
	return out, nil
}

// CIDVersion is recorded alongside a pin so a future change to the rule is detectable in stored
// data rather than only in code.
const CIDVersion = 1

// Multicodec is recorded for the same reason.
const Multicodec = "raw"

// GatewayURL builds a fetch URL for a CID.
func GatewayURL(gateway, cid string) string {
	return strings.TrimRight(gateway, "/") + "/ipfs/" + cid
}
