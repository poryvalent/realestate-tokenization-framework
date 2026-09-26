// Package ipfsguard is the fail-closed gate that every document must pass before
// it is pinned to IPFS.
//
// # Why this exists
//
// Publishing to IPFS is irrevocable in a way that publishing to a private bucket
// is not. Content is addressed by its hash, so a document can be re-pinned by
// anyone who has ever fetched it. There is no delete. A personal identifier that
// reaches IPFS once has reached it permanently, and under India's DPDP Act an
// erasure request against that data is impossible to honour.
//
// The AcreSync architecture already keeps personal data off-chain and behind
// HMAC anchors. This package defends the one remaining path by which it could
// leak: an orchestrator change that adds a field to a serialised struct, where
// the field happens to carry a name, a PAN, or a bank reference, and nobody
// notices because the pinning code marshals whatever it is given.
//
// # How it fails closed
//
// Validation is an allowlist over both field names and value formats. Any key
// not declared in the schema is a violation. Any value that does not match its
// declared machine format is a violation. There is no free-text field type at
// all, so there is nowhere for prose to hide. Adding a field to a pinned
// document requires editing schema.go, which is a reviewable code change rather
// than an emergent side effect.
//
// A residual pattern scan for Indian identifier shapes runs on top of the
// allowlist as defence in depth. The allowlist is the guarantee; the scan is the
// smoke detector.
package ipfsguard

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/acresync/orchestrator/internal/canonical"
)

// MaxDocumentBytes caps the canonical document size. A runaway document is a
// denial-of-service vector against the pinning service and against every
// verifier who later downloads it.
const MaxDocumentBytes = 8 << 20 // 8 MiB

// DefaultMaxArrayLen applies when a Field declares no MaxLen.
const DefaultMaxArrayLen = 200_000

var (
	ErrUnknownDocType = errors.New("ipfsguard: unknown document type")
	ErrTooLarge       = errors.New("ipfsguard: document exceeds maximum size")
	ErrNotAnObject    = errors.New("ipfsguard: document root must be a JSON object")
	ErrUnknownField   = errors.New("ipfsguard: field is not on the allowlist")
	ErrMissingField   = errors.New("ipfsguard: required field is absent")
	ErrArrayTooLong   = errors.New("ipfsguard: array exceeds declared maximum length")
	ErrNotAnArray     = errors.New("ipfsguard: expected an array")
	ErrPIIDetected    = errors.New("ipfsguard: value matches a personal identifier pattern")
	ErrBadSchema      = errors.New("ipfsguard: malformed schema declaration")
)

// Violation is one specific reason a document was rejected, located by JSON path.
type Violation struct {
	Path string
	Err  error
}

func (v Violation) String() string { return v.Path + ": " + v.Err.Error() }

// ValidationError aggregates every violation found in a document.
//
// Validation collects all violations rather than stopping at the first. An
// operator fixing a rejected document should see the complete list in one pass,
// and a fail-closed gate that reports one problem at a time invites the habit of
// fixing symptoms until it goes quiet.
type ValidationError struct {
	Doc        DocType
	Violations []Violation
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "ipfsguard: %s document rejected with %d violation(s):", e.Doc, len(e.Violations))
	for _, v := range e.Violations {
		b.WriteString("\n  - ")
		b.WriteString(v.String())
	}
	return b.String()
}

func (e *ValidationError) Unwrap() []error {
	out := make([]error, len(e.Violations))
	for i, v := range e.Violations {
		out[i] = v.Err
	}
	return out
}

// ---------------------------------------------------------------------------
// Personal identifier patterns
// ---------------------------------------------------------------------------

// These three are scanned across the whole canonical document because they
// cannot false-positive against the formats the allowlist permits. PAN and IFSC
// both require uppercase letters, and no allowlisted value contains an uppercase
// letter except closed-enum members, none of which contain digits. An email
// requires '@', which no allowlisted format admits.
var documentWidePII = []struct {
	name string
	re   *regexp.Regexp
}{
	{"PAN", regexp.MustCompile(`\b[A-Z]{5}[0-9]{4}[A-Z]\b`)},
	{"email address", regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)},
	{"IFSC code", regexp.MustCompile(`\b[A-Z]{4}0[A-Z0-9]{6}\b`)},
}

// These two are scanned only on kinds where the scan is sound. A twelve-digit
// run occurs naturally inside a 64-character hex digest and inside the final
// group of a UUID, so scanning those kinds would produce noise rather than
// signal. See Kind.isScannableForNumericPII.
var numericPII = []struct {
	name string
	re   *regexp.Regexp
}{
	{"Aadhaar number", regexp.MustCompile(`\b\d{4}\s?\d{4}\s?\d{4}\b`)},
	{"Indian mobile number", regexp.MustCompile(`\b(?:\+91[\-\s]?)?[6-9]\d{9}\b`)},
}

// ---------------------------------------------------------------------------
// Entry points
// ---------------------------------------------------------------------------

// ValidateAndCanonicalize validates raw against the schema for dt and returns the
// canonical bytes that should be pinned.
//
// Returning the canonical bytes rather than accepting the caller's is deliberate.
// It is the only way to guarantee that the bytes which were validated are the
// bytes which get pinned, and that the resulting CID is reproducible by a third
// party running an RFC 8785 canonicaliser over the same logical content.
func ValidateAndCanonicalize(dt DocType, raw []byte) ([]byte, error) {
	schema, ok := SchemaFor(dt)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownDocType, dt)
	}

	// canonical.MarshalJSON rejects duplicate keys, floating point numbers, and
	// non-integer numeric literals before the schema is even consulted.
	canonicalBytes, err := canonical.MarshalJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("ipfsguard: %s: %w", dt, err)
	}
	if len(canonicalBytes) > MaxDocumentBytes {
		return nil, fmt.Errorf("%w: %d bytes exceeds %d", ErrTooLarge, len(canonicalBytes), MaxDocumentBytes)
	}

	dec := json.NewDecoder(strings.NewReader(string(canonicalBytes)))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("ipfsguard: %s: decoding canonical form: %w", dt, err)
	}

	root, ok := tree.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w (got %T)", ErrNotAnObject, tree)
	}

	var violations []Violation
	walkObject(schema, root, "$", &violations)
	scanDocumentWide(canonicalBytes, &violations)

	if len(violations) > 0 {
		slices.SortFunc(violations, func(a, b Violation) int { return strings.Compare(a.Path, b.Path) })
		return nil, &ValidationError{Doc: dt, Violations: violations}
	}
	return canonicalBytes, nil
}

// Validate reports whether raw is a permissible document of type dt.
func Validate(dt DocType, raw []byte) error {
	_, err := ValidateAndCanonicalize(dt, raw)
	return err
}

// Digest returns the SHA-256 of the canonical document bytes.
//
// This is the 32-byte value anchored on-chain. A CIDv1 is 36 bytes and does not
// fit a bytes32, so the contract stores this digest and the full CID is
// reconstructed off-chain from the known multibase, codec and hash-function
// prefixes. The reconstruction rule belongs in the published verifier
// specification.
func Digest(canonicalBytes []byte) [32]byte {
	return sha256.Sum256(canonicalBytes)
}

// DigestHex renders Digest as 0x-prefixed lowercase hex.
func DigestHex(canonicalBytes []byte) string {
	d := Digest(canonicalBytes)
	return fmt.Sprintf("0x%x", d)
}

// ---------------------------------------------------------------------------
// Walk
// ---------------------------------------------------------------------------

func walkObject(schema Schema, obj map[string]any, path string, vio *[]Violation) {
	// Fail closed on anything not declared.
	for key := range obj {
		if _, declared := schema[key]; !declared {
			*vio = append(*vio, Violation{
				Path: path + "." + key,
				Err:  fmt.Errorf("%w: %q", ErrUnknownField, key),
			})
		}
	}

	names := make([]string, 0, len(schema))
	for name := range schema {
		names = append(names, name)
	}
	slices.Sort(names)

	for _, name := range names {
		field := schema[name]
		value, present := obj[name]
		childPath := path + "." + name

		if !present {
			if field.Required {
				*vio = append(*vio, Violation{
					Path: childPath,
					Err:  fmt.Errorf("%w: %q", ErrMissingField, name),
				})
			}
			continue
		}
		walkField(field, value, childPath, vio)
	}
}

func walkField(field Field, value any, path string, vio *[]Violation) {
	switch field.Kind {
	case KindObject:
		if field.Object == nil {
			*vio = append(*vio, Violation{Path: path, Err: fmt.Errorf("%w: object field has no nested schema", ErrBadSchema)})
			return
		}
		child, ok := value.(map[string]any)
		if !ok {
			*vio = append(*vio, Violation{Path: path, Err: fmt.Errorf("%w (got %T)", ErrNotAnObject, value)})
			return
		}
		walkObject(field.Object, child, path, vio)

	case KindArray:
		if field.Elem == nil {
			*vio = append(*vio, Violation{Path: path, Err: fmt.Errorf("%w: array field has no element schema", ErrBadSchema)})
			return
		}
		items, ok := value.([]any)
		if !ok {
			*vio = append(*vio, Violation{Path: path, Err: fmt.Errorf("%w (got %T)", ErrNotAnArray, value)})
			return
		}
		limit := field.MaxLen
		if limit <= 0 {
			limit = DefaultMaxArrayLen
		}
		if len(items) > limit {
			*vio = append(*vio, Violation{Path: path, Err: fmt.Errorf("%w: %d exceeds %d", ErrArrayTooLong, len(items), limit)})
			return
		}
		for i, item := range items {
			walkField(*field.Elem, item, fmt.Sprintf("%s[%d]", path, i), vio)
		}

	default:
		if err := checkScalar(field.Kind, field.Enum, value); err != nil {
			*vio = append(*vio, Violation{Path: path, Err: err})
			return
		}
		if field.Kind.isScannableForNumericPII() {
			if s, ok := value.(string); ok {
				scanNumericPII(s, path, vio)
			}
		}
	}
}

func scanNumericPII(s, path string, vio *[]Violation) {
	for _, p := range numericPII {
		if p.re.MatchString(s) {
			*vio = append(*vio, Violation{
				Path: path,
				Err:  fmt.Errorf("%w: looks like an %s", ErrPIIDetected, p.name),
			})
		}
	}
}

func scanDocumentWide(canonicalBytes []byte, vio *[]Violation) {
	for _, p := range documentWidePII {
		if loc := p.re.FindIndex(canonicalBytes); loc != nil {
			// The matched text is deliberately not included in the error. If it
			// really is a personal identifier, copying it into a log line is the
			// same leak in a different place.
			*vio = append(*vio, Violation{
				Path: fmt.Sprintf("$ (byte offset %d)", loc[0]),
				Err:  fmt.Errorf("%w: document contains something shaped like a %s", ErrPIIDetected, p.name),
			})
		}
	}
}
