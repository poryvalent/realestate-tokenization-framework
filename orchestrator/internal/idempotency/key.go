// Package idempotency derives the deterministic keys that make every mutating
// AcreSync operation replay-safe.
//
// # Why derived rather than random
//
// A client-generated UUID makes a retry safe only if the client remembers it. A
// double-clicked admin button, a retried HTTP request after a timeout, or a
// redelivered queue message will each generate a fresh UUID and each will be
// treated as a new action. For an operation like "inject this month's rent" that
// is a double distribution.
//
// Deriving the key from the business content inverts the property: the same
// logical action always produces the same key, so a duplicate is rejected by a
// unique index without the caller needing to cooperate. Two genuinely different
// actions produce different keys because their content differs.
//
// # Framing
//
// The Phase 2 specification described the input as a ':'-joined string. That is
// ambiguous: action "A" with scope "B:C" and action "A:B" with scope "C" join to
// the same byte sequence. A key collision between two distinct actions is a
// silently-skipped operation, so this implementation uses length-prefixed
// framing instead, which admits no such collision.
//
// The exact preimage is:
//
//	frame(domain) || frame(action) || frame(schemeID) || frame(scopeID) || frame(canonicalPayload)
//
// where frame(x) = uint32be(len(x)) || x, and domain is the constant
// "acresync.idempotency.v1". The key is the SHA-256 of that preimage.
//
// Bumping the domain constant deliberately invalidates every previously derived
// key, which is the correct behaviour if the derivation rule ever changes:
// old keys must not collide with new ones.
package idempotency

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"regexp"
	"strings"

	"github.com/acresync/orchestrator/internal/canonical"
)

// Domain separates AcreSync idempotency preimages from every other use of
// SHA-256 in the system. Never change this without intending to invalidate all
// existing keys.
const Domain = "acresync.idempotency.v1"

// KeyLen is the key size in bytes. It matches Solidity's bytes32 so the same
// value passes unchanged to the contract's trailing idempotencyKey parameter.
const KeyLen = 32

var (
	ErrUnknownAction   = errors.New("idempotency: unregistered action")
	ErrMissingSchemeID = errors.New("idempotency: schemeID is required")
	ErrNotAUUID        = errors.New("idempotency: identifier is not a lowercase canonical UUID")
	ErrBadKeyEncoding  = errors.New("idempotency: key must be 0x followed by 64 lowercase hex characters")
)

// uuidRe requires the lowercase canonical 8-4-4-4-12 form. Uppercase is
// rejected rather than normalised: accepting both spellings would mean the same
// logical action could derive two different keys depending on which casing the
// caller happened to pass.
var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Key is a 32-byte idempotency key.
type Key [KeyLen]byte

// Hex renders the key as 0x-prefixed lowercase hex, the form stored in Postgres
// and passed to the contract.
func (k Key) Hex() string { return "0x" + hex.EncodeToString(k[:]) }

func (k Key) String() string { return k.Hex() }

// Bytes returns a copy of the raw key.
func (k Key) Bytes() []byte {
	out := make([]byte, KeyLen)
	copy(out, k[:])
	return out
}

// IsZero reports whether the key is unset.
func (k Key) IsZero() bool {
	return k == Key{}
}

// ParseKey parses the 0x-prefixed lowercase hex form produced by Key.Hex.
func ParseKey(s string) (Key, error) {
	var k Key
	if len(s) != 2+2*KeyLen || !strings.HasPrefix(s, "0x") {
		return k, fmt.Errorf("%w: got %d characters", ErrBadKeyEncoding, len(s))
	}
	body := s[2:]
	if body != strings.ToLower(body) {
		return k, fmt.Errorf("%w: contains uppercase", ErrBadKeyEncoding)
	}
	raw, err := hex.DecodeString(body)
	if err != nil {
		return k, fmt.Errorf("%w: %v", ErrBadKeyEncoding, err)
	}
	copy(k[:], raw)
	return k, nil
}

// Input is the full set of inputs to key derivation.
type Input struct {
	// Action is the operation being performed. Must be registered in AllActions.
	Action Action

	// SchemeID is the scheme the action belongs to. Required, and required to be
	// a lowercase canonical UUID.
	SchemeID string

	// ScopeID narrows the action within the scheme: an offer, a period, a bid, an
	// entitlement. Empty for scheme-level actions such as ADVANCE_CLOCK. When
	// present it must be a lowercase canonical UUID.
	ScopeID string

	// Payload is the business content that distinguishes this action from
	// another of the same type in the same scope. It is encoded with the
	// canonical encoder, so it must contain no floats, no byte slices, and no
	// time.Time values.
	//
	// Choosing what belongs here is a design decision with consequences. For
	// INJECT_RENT the payload carries the lease, the receipt timestamp and the
	// amount, so re-posting an identical receipt is rejected while a genuinely
	// different receipt is accepted. For ANCHOR_PERIOD it carries the NDCF
	// figures and the statement hash, so re-anchoring identical data is a no-op
	// while changed data derives a new key and is then caught by the contract's
	// own periodAnchored guard.
	Payload any
}

// Derive computes the idempotency key for an action.
//
// Derivation is total: given the same Input it always returns the same Key, on
// any machine, in any process, in any order of map iteration. That property is
// what TestDeriveIsStableAcrossMapOrdering and the golden vector file exist to
// defend.
func Derive(in Input) (Key, error) {
	var zero Key

	if !in.Action.Valid() {
		return zero, fmt.Errorf("%w: %q", ErrUnknownAction, in.Action)
	}
	if in.SchemeID == "" {
		return zero, ErrMissingSchemeID
	}
	if !uuidRe.MatchString(in.SchemeID) {
		return zero, fmt.Errorf("%w: schemeID %q", ErrNotAUUID, in.SchemeID)
	}
	if in.ScopeID != "" && !uuidRe.MatchString(in.ScopeID) {
		return zero, fmt.Errorf("%w: scopeID %q", ErrNotAUUID, in.ScopeID)
	}

	payload, err := canonical.Marshal(in.Payload)
	if err != nil {
		return zero, fmt.Errorf("idempotency: canonicalising payload for %s: %w", in.Action, err)
	}

	h := sha256.New()
	writeFrame(h, []byte(Domain))
	writeFrame(h, []byte(in.Action))
	writeFrame(h, []byte(in.SchemeID))
	writeFrame(h, []byte(in.ScopeID))
	writeFrame(h, payload)

	var k Key
	copy(k[:], h.Sum(nil))
	return k, nil
}

// MustDerive is Derive for use in tests and static initialisation.
func MustDerive(in Input) Key {
	k, err := Derive(in)
	if err != nil {
		panic(fmt.Sprintf("idempotency: MustDerive: %v", err))
	}
	return k
}

// Preimage returns the exact bytes hashed by Derive.
//
// This exists so the derivation rule can be reproduced and audited by a third
// party, and so a key mismatch during an incident can be diagnosed by comparing
// preimages rather than by guessing.
func Preimage(in Input) ([]byte, error) {
	payload, err := canonical.Marshal(in.Payload)
	if err != nil {
		return nil, err
	}
	var buf []byte
	buf = appendFrame(buf, []byte(Domain))
	buf = appendFrame(buf, []byte(in.Action))
	buf = appendFrame(buf, []byte(in.SchemeID))
	buf = appendFrame(buf, []byte(in.ScopeID))
	buf = appendFrame(buf, payload)
	return buf, nil
}

func writeFrame(h hash.Hash, b []byte) {
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(b)))
	h.Write(lenBuf[:])
	h.Write(b)
}

func appendFrame(dst, b []byte) []byte {
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(b)))
	dst = append(dst, lenBuf[:]...)
	return append(dst, b...)
}
