// Package snapshot freezes the unitholder register at a record date and commits to it.
//
// # Why the leaf encoding is defined here and not in the contract
//
// AcreSyncScheme.verifyEntitlement takes the leaf as a calldata parameter rather than computing it.
// That keeps the contract generic, and it means the snapshot document is the authority on how a leaf
// is built. It also means the convention in this file is load-bearing in a way a contract function
// would not be: nothing on-chain will reject a wrongly encoded leaf, it will simply fail to verify,
// and the investor will be told their entitlement does not match with no explanation of why.
//
// The ballot encodings are protected from that by BallotEncoding.sol, shared golden vectors and a
// Solidity parity test. The snapshot leaf had none of those, so the same protection is built here:
// the rule is stated in one place, exported for verification tooling, published inside the pinned
// document, and cross-checked against an independent Solidity implementation.
package snapshot

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/acresync/orchestrator/internal/merkle"
)

// LeafDomain separates snapshot entitlement leaves from every other hash in the system.
//
// Versioned because the encoding cannot change without invalidating every root already anchored. A
// v2 would be a new domain, and both would coexist while old periods remain verifiable.
const LeafDomain = "acresync.snapshot.entitlement.v1"

// AlgoVersion is recorded in the document so a verifier knows which rule to apply.
const AlgoVersion = 1

// AddressLen is the byte length of an Ethereum address.
const AddressLen = 20

var (
	ErrBadAddress = errors.New("snapshot: wallet address must be 0x followed by 40 lowercase hex digits")
	ErrZeroUnits  = errors.New("snapshot: a snapshot line must hold at least one unit")
	ErrZeroAnchor = errors.New("snapshot: investor anchor must be set")
)

// EntitlementLeaf computes the Merkle leaf for one snapshot line.
//
// # The encoding
//
//	sha256( 0x00 || "acresync.snapshot.entitlement.v1"
//	              || leafIndex      big-endian uint32, 4 bytes
//	              || holder         raw address bytes,  20 bytes
//	              || investorAnchor                     32 bytes
//	              || units          big-endian uint32,  4 bytes
//	              || excluded       0x00 or 0x01,        1 byte )
//
// The leading 0x00 is MerkleLib's leaf prefix, which is what stops an internal node being replayed as
// a leaf.
//
// # Why every field is fixed width
//
// A variable-length encoding without separators lets two different lines produce one preimage: a line
// with leafIndex 1 and 23 units could encode identically to one with leafIndex 12 and 3 units. Fixed
// widths make the encoding injective, so distinct lines always produce distinct leaves.
//
// # Why the holder address is included
//
// The investor anchor is an HMAC under a key held in KMS, so an investor cannot compute their own
// anchor. If the anchor were the only identity in the leaf, building your own proof would require
// AcreSync to tell you a value only AcreSync can derive, which defeats the purpose of anchoring the
// root at all. The wallet address is public and known to its owner, so including it means a holder can
// locate their line in the published document by an identifier they already have.
//
// The anchor is kept alongside it because the anchor is the stable identity across periods while a
// wallet can change on transfer. Committing to both means a wallet rotation cannot quietly reassign an
// entitlement to a different investor.
//
// # Why the excluded flag is included
//
// It classifies whether the line counts toward the statutory minimum of 200 unitholders. Leaving it
// out of the leaf would allow reclassifying the investment manager as a countable holder after the
// root was anchored, which is precisely the number the minimum exists to police.
func EntitlementLeaf(leafIndex uint32, holder [AddressLen]byte, investorAnchor [32]byte, units uint32, excluded bool) merkle.Hash {
	buf := make([]byte, 0, len(LeafDomain)+4+AddressLen+32+4+1)

	buf = append(buf, LeafDomain...)
	buf = binary.BigEndian.AppendUint32(buf, leafIndex)
	buf = append(buf, holder[:]...)
	buf = append(buf, investorAnchor[:]...)
	buf = binary.BigEndian.AppendUint32(buf, units)

	// A single byte, not a Go bool cast, so the on-the-wire value is explicit and matches Solidity's
	// abi.encodePacked of a bool, which emits one byte of 0x00 or 0x01.
	if excluded {
		buf = append(buf, 0x01)
	} else {
		buf = append(buf, 0x00)
	}

	return merkle.HashLeaf(buf)
}

// LeafPreimage returns the bytes hashed by EntitlementLeaf.
//
// Exported so verification tooling and the parity tests can compare preimages rather than only
// digests. When two implementations disagree, matching digests tell you nothing about where, while
// the preimages show the offending byte immediately.
func LeafPreimage(leafIndex uint32, holder [AddressLen]byte, investorAnchor [32]byte, units uint32, excluded bool) []byte {
	buf := make([]byte, 0, len(LeafDomain)+4+AddressLen+32+4+1)
	buf = append(buf, LeafDomain...)
	buf = binary.BigEndian.AppendUint32(buf, leafIndex)
	buf = append(buf, holder[:]...)
	buf = append(buf, investorAnchor[:]...)
	buf = binary.BigEndian.AppendUint32(buf, units)
	if excluded {
		buf = append(buf, 0x01)
	} else {
		buf = append(buf, 0x00)
	}
	return buf
}

// ParseAddress converts the canonical stored form to raw bytes.
//
// Strict about case. The database constrains wallet_address to lowercase hex, and the field allowlist
// rejects mixed case, so that one address has exactly one spelling. Accepting a checksummed address
// here would let the same holder produce two different leaves for the same holding.
func ParseAddress(s string) ([AddressLen]byte, error) {
	var out [AddressLen]byte

	if len(s) != 2+2*AddressLen || s[0] != '0' || s[1] != 'x' {
		return out, fmt.Errorf("%w: got %q", ErrBadAddress, s)
	}
	for i := 0; i < AddressLen; i++ {
		hi, err := hexNibble(s[2+2*i])
		if err != nil {
			return out, fmt.Errorf("%w: got %q", ErrBadAddress, s)
		}
		lo, err := hexNibble(s[3+2*i])
		if err != nil {
			return out, fmt.Errorf("%w: got %q", ErrBadAddress, s)
		}
		out[i] = hi<<4 | lo
	}
	return out, nil
}

func hexNibble(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	default:
		// Uppercase A-F deliberately rejected rather than accepted and folded.
		return 0, fmt.Errorf("%w: %q is not a lowercase hex digit", ErrBadAddress, rune(c))
	}
}

// FormatAddress renders raw address bytes in the canonical stored form.
func FormatAddress(a [AddressLen]byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 2+2*AddressLen)
	out[0], out[1] = '0', 'x'
	for i, b := range a {
		out[2+2*i] = digits[b>>4]
		out[3+2*i] = digits[b&0x0f]
	}
	return string(out)
}
