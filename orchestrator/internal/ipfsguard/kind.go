package ipfsguard

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"time"
)

// Kind is the declared value type of an allowlisted field.
//
// There is deliberately no free-text kind. Every string value in a pinned
// document must match a fixed machine format: hex, address, UUID, ISO date,
// opaque reference, or a member of a closed enum. That is what makes the
// document structurally incapable of carrying personal data.
//
// If a human-readable narrative is needed for an audit record, its SHA-256 goes
// in the document and the text stays in Postgres under access control. IPFS is
// not a place to put anything you might later need to retract.
type Kind int

const (
	KindInvalid Kind = iota
	KindObject
	KindArray
	KindBytes32Hex
	KindAddress
	KindUint32
	KindUint64
	KindBool
	KindDateISO
	KindDateTimeISO
	KindUUID
	KindOpaqueRef
	KindEnum
)

var kindNames = map[Kind]string{
	KindInvalid:     "invalid",
	KindObject:      "object",
	KindArray:       "array",
	KindBytes32Hex:  "bytes32hex",
	KindAddress:     "address",
	KindUint32:      "uint32",
	KindUint64:      "uint64",
	KindBool:        "bool",
	KindDateISO:     "date",
	KindDateTimeISO: "datetime",
	KindUUID:        "uuid",
	KindOpaqueRef:   "opaqueref",
	KindEnum:        "enum",
}

func (k Kind) String() string {
	if n, ok := kindNames[k]; ok {
		return n
	}
	return fmt.Sprintf("kind(%d)", int(k))
}

// isScannableForNumericPII reports whether values of this kind can be safely
// pattern-scanned for bare 12-digit and 10-digit identifiers.
//
// The Aadhaar and mobile patterns are \b-anchored, and where the false positives
// actually arise is more specific than "long digit runs":
//
//   A 0x-prefixed digest is one contiguous run of word characters, so there is no
//   word boundary before its digits and the patterns cannot match inside it. Hex
//   kinds are therefore safe in practice. They are still excluded here, because
//   relying on the absence of a boundary that the format happens to provide is a
//   fragile reason to scan something that cannot carry personal data anyway.
//
//   A UUID is the genuine hazard. Its groups are dash-delimited, so each group
//   carries word boundaries on both sides, and the final group is exactly twelve
//   characters. A UUID ending in twelve digits matches the Aadhaar pattern
//   precisely. Scanning UUIDs would produce recurring false alarms, which trains
//   operators to dismiss the alarm and is worse than not scanning at all.
//
// So the scan runs only on enums and date formats: kinds that are short, closed,
// and cannot legitimately contain a long delimited digit group.
func (k Kind) isScannableForNumericPII() bool {
	switch k {
	case KindEnum, KindDateISO, KindDateTimeISO:
		return true
	default:
		return false
	}
}

var (
	ErrFormat = errors.New("ipfsguard: value does not match declared format")

	bytes32Re   = regexp.MustCompile(`^0x[0-9a-f]{64}$`)
	addressRe   = regexp.MustCompile(`^0x[0-9a-f]{40}$`)
	uuidRe      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	opaqueRefRe = regexp.MustCompile(`^[0-9a-f]{32}$`)
	dateRe      = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// checkScalar validates a decoded JSON value against a declared kind.
//
// All hex forms are required to be lowercase. EIP-55 mixed-case checksummed
// addresses are rejected on purpose: two spellings of the same address would
// produce two different CIDs for the same logical document, and reproducibility
// of the content address is the entire reason this file exists.
func checkScalar(k Kind, enumValues []string, v any) error {
	switch k {
	case KindBool:
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%w: expected bool, got %T", ErrFormat, v)
		}
		return nil

	case KindUint32:
		return checkUnsigned(v, math.MaxUint32)

	case KindUint64:
		return checkUnsigned(v, math.MaxUint64)

	case KindBytes32Hex:
		return checkPattern(bytes32Re, v, "0x-prefixed 64-character lowercase hex")

	case KindAddress:
		return checkPattern(addressRe, v, "0x-prefixed 40-character lowercase hex")

	case KindUUID:
		return checkPattern(uuidRe, v, "lowercase canonical UUID")

	case KindOpaqueRef:
		return checkPattern(opaqueRefRe, v, "32-character lowercase hex opaque reference")

	case KindDateISO:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%w: expected string date, got %T", ErrFormat, v)
		}
		if !dateRe.MatchString(s) {
			return fmt.Errorf("%w: expected YYYY-MM-DD, got %q", ErrFormat, s)
		}
		if _, err := time.Parse("2006-01-02", s); err != nil {
			return fmt.Errorf("%w: %q is not a real calendar date", ErrFormat, s)
		}
		return nil

	case KindDateTimeISO:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%w: expected string datetime, got %T", ErrFormat, v)
		}
		// UTC with a literal Z only. A numeric offset would be a second spelling
		// of the same instant.
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return fmt.Errorf("%w: %q is not RFC 3339: %v", ErrFormat, s, err)
		}
		if s != t.UTC().Format("2006-01-02T15:04:05Z") {
			return fmt.Errorf("%w: %q must be UTC in exactly YYYY-MM-DDTHH:MM:SSZ form", ErrFormat, s)
		}
		return nil

	case KindEnum:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%w: expected enum string, got %T", ErrFormat, v)
		}
		for _, allowed := range enumValues {
			if s == allowed {
				return nil
			}
		}
		return fmt.Errorf("%w: %q is not one of %v", ErrFormat, s, enumValues)

	default:
		return fmt.Errorf("%w: kind %s is not a scalar", ErrFormat, k)
	}
}

func checkPattern(re *regexp.Regexp, v any, want string) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("%w: expected string (%s), got %T", ErrFormat, want, v)
	}
	if !re.MatchString(s) {
		return fmt.Errorf("%w: expected %s, got %q", ErrFormat, want, s)
	}
	return nil
}

func checkUnsigned(v any, max uint64) error {
	n, ok := v.(json.Number)
	if !ok {
		return fmt.Errorf("%w: expected an integer, got %T", ErrFormat, v)
	}
	s := n.String()
	u, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: %q is not a non-negative integer", ErrFormat, s)
	}
	if u > max {
		return fmt.Errorf("%w: %s exceeds maximum %d", ErrFormat, s, max)
	}
	return nil
}
