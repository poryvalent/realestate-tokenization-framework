// Package canonical produces a deterministic, byte-stable JSON encoding for
// AcreSync.
//
// Determinism is a correctness requirement, not a convenience. Two things in
// the system depend on it:
//
//  1. Idempotency keys are derived from the canonical encoding of an action
//     payload. If the same logical payload can encode two different ways, the
//     same business action can be executed twice under two different keys. In a
//     distribution context that means paying investors twice.
//
//  2. IPFS CIDs are content addresses over the canonical encoding. If a third
//     party cannot reproduce our byte sequence they cannot reproduce the CID,
//     and the "independently verifiable" property of the audit trail is a claim
//     rather than a fact.
//
// # Relationship to RFC 8785 (JCS)
//
// This encoder is a strict subset of RFC 8785 JSON Canonicalization Scheme:
//
//   - Object members are sorted by UTF-16 code unit sequence (JCS 3.2.3).
//   - No insignificant whitespace.
//   - Strings use JCS minimal escaping (JCS 3.2.2.2). Notably '/', '<', '>' and
//     '&' are NOT escaped, unlike Go's encoding/json default.
//   - UTF-8 output.
//
// The deliberate divergence: floating point numbers are rejected outright.
// JCS serialises numbers through ES6 Number::toString, which is defined over
// IEEE-754 doubles and therefore cannot represent every int64 exactly. Every
// monetary value in AcreSync is an integer count of paise and several exceed
// 2^53. Rather than inherit that hazard we ban floats at the type level, which
// leaves integer serialisation unambiguous and makes the output reproducible by
// any off-the-shelf JCS implementation.
//
// Byte slices, time.Time, and NaN/Inf are also rejected. Callers must convert
// to an explicit lowercase hex string or an explicit RFC 3339 string first, so
// that the wire format is a decision in the caller's code rather than a
// side effect of a library default.
package canonical

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// MaxDepth bounds recursion so a hostile or malformed document cannot exhaust
// the stack.
const MaxDepth = 32

var (
	ErrFloatNotPermitted  = errors.New("canonical: floating point values are not permitted; use an integer or an explicit string")
	ErrBytesNotPermitted  = errors.New("canonical: byte slices are not permitted; encode as a lowercase 0x-prefixed hex string")
	ErrTimeNotPermitted   = errors.New("canonical: time.Time is not permitted; format as an explicit RFC 3339 string")
	ErrUnsupportedType    = errors.New("canonical: unsupported type")
	ErrNonStringKey       = errors.New("canonical: object keys must be strings")
	ErrDuplicateKey       = errors.New("canonical: duplicate object key")
	ErrInvalidUTF8        = errors.New("canonical: string is not valid UTF-8")
	ErrNonIntegerNumber   = errors.New("canonical: number is not an integer")
	ErrMaxDepthExceeded   = errors.New("canonical: maximum nesting depth exceeded")
	ErrMalformedJSON      = errors.New("canonical: malformed JSON")
	ErrTrailingJSONTokens = errors.New("canonical: trailing tokens after top-level JSON value")
)

const hexDigits = "0123456789abcdef"

// Marshal encodes v into canonical form.
//
// Supported: nil, bool, all signed and unsigned integer kinds, string,
// json.Number (integers only), maps with string keys, slices, arrays, pointers,
// interfaces, and structs (honouring `json` tags).
//
// Rejected: float32, float64, complex, byte slices, time.Time, channels,
// functions, and unsafe pointers.
func Marshal(v any) ([]byte, error) {
	return encode(make([]byte, 0, 256), reflect.ValueOf(v), 0)
}

// MarshalJSON re-encodes an existing JSON document into canonical form.
//
// It parses at the token level rather than into a map so that duplicate object
// keys are detected and rejected. A document with duplicate keys has no single
// canonical form, so accepting one would silently break reproducibility.
//
// Numbers are decoded as json.Number and validated as integers, which preserves
// full int64 and uint64 precision that a float64 round-trip would destroy.
func MarshalJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()

	tree, err := parseValue(dec, 0)
	if err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, ErrTrailingJSONTokens
	}
	return Marshal(tree)
}

// Equal reports whether two documents have the same canonical encoding.
func Equal(a, b []byte) (bool, error) {
	ca, err := MarshalJSON(a)
	if err != nil {
		return false, err
	}
	cb, err := MarshalJSON(b)
	if err != nil {
		return false, err
	}
	return string(ca) == string(cb), nil
}

// ---------------------------------------------------------------------------
// Encoding
// ---------------------------------------------------------------------------

var (
	jsonNumberType = reflect.TypeOf(json.Number(""))
	timeType       = reflect.TypeOf(time.Time{})
	bigFloatType   = reflect.TypeOf(big.Float{})
	bigRatType     = reflect.TypeOf(big.Rat{})
)

func encode(dst []byte, v reflect.Value, depth int) ([]byte, error) {
	if depth > MaxDepth {
		return nil, ErrMaxDepthExceeded
	}

	// An invalid Value is an untyped nil interface.
	if !v.IsValid() {
		return append(dst, "null"...), nil
	}

	switch v.Type() {
	case jsonNumberType:
		return encodeJSONNumber(dst, json.Number(v.String()))
	case timeType:
		return nil, ErrTimeNotPermitted
	case bigFloatType, bigRatType:
		return nil, ErrFloatNotPermitted
	}

	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return append(dst, "null"...), nil
		}
		return encode(dst, v.Elem(), depth)

	case reflect.Bool:
		if v.Bool() {
			return append(dst, "true"...), nil
		}
		return append(dst, "false"...), nil

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.AppendInt(dst, v.Int(), 10), nil

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.AppendUint(dst, v.Uint(), 10), nil

	case reflect.Float32, reflect.Float64:
		return nil, ErrFloatNotPermitted

	case reflect.Complex64, reflect.Complex128:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedType, v.Type())

	case reflect.String:
		return appendCanonicalString(dst, v.String())

	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return nil, ErrBytesNotPermitted
		}
		if v.IsNil() {
			return append(dst, "null"...), nil
		}
		return encodeArray(dst, v, depth)

	case reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return nil, ErrBytesNotPermitted
		}
		return encodeArray(dst, v, depth)

	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return nil, fmt.Errorf("%w: %s", ErrNonStringKey, v.Type())
		}
		if v.IsNil() {
			return append(dst, "null"...), nil
		}
		return encodeMap(dst, v, depth)

	case reflect.Struct:
		return encodeStruct(dst, v, depth)

	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedType, v.Type())
	}
}

func encodeArray(dst []byte, v reflect.Value, depth int) ([]byte, error) {
	dst = append(dst, '[')
	for i := range v.Len() {
		if i > 0 {
			dst = append(dst, ',')
		}
		var err error
		dst, err = encode(dst, v.Index(i), depth+1)
		if err != nil {
			return nil, err
		}
	}
	return append(dst, ']'), nil
}

func encodeMap(dst []byte, v reflect.Value, depth int) ([]byte, error) {
	keys := make([]string, 0, v.Len())
	for iter := v.MapRange(); iter.Next(); {
		keys = append(keys, iter.Key().String())
	}
	sortUTF16(keys)

	dst = append(dst, '{')
	for i, k := range keys {
		if i > 0 {
			dst = append(dst, ',')
		}
		var err error
		if dst, err = appendCanonicalString(dst, k); err != nil {
			return nil, err
		}
		dst = append(dst, ':')
		if dst, err = encode(dst, v.MapIndex(reflect.ValueOf(k).Convert(v.Type().Key())), depth+1); err != nil {
			return nil, err
		}
	}
	return append(dst, '}'), nil
}

type structField struct {
	name  string
	value reflect.Value
}

func encodeStruct(dst []byte, v reflect.Value, depth int) ([]byte, error) {
	t := v.Type()
	fields := make([]structField, 0, t.NumField())

	for i := range t.NumField() {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		name, omitEmpty, skip := parseJSONTag(sf)
		if skip {
			continue
		}
		fv := v.Field(i)
		if omitEmpty && fv.IsZero() {
			continue
		}
		fields = append(fields, structField{name: name, value: fv})
	}

	slices.SortFunc(fields, func(a, b structField) int {
		return compareUTF16(a.name, b.name)
	})

	// Two exported fields tagged with the same JSON name would make the output
	// order-dependent on struct declaration order.
	for i := 1; i < len(fields); i++ {
		if fields[i].name == fields[i-1].name {
			return nil, fmt.Errorf("%w: %q in %s", ErrDuplicateKey, fields[i].name, t)
		}
	}

	dst = append(dst, '{')
	for i, f := range fields {
		if i > 0 {
			dst = append(dst, ',')
		}
		var err error
		if dst, err = appendCanonicalString(dst, f.name); err != nil {
			return nil, err
		}
		dst = append(dst, ':')
		if dst, err = encode(dst, f.value, depth+1); err != nil {
			return nil, err
		}
	}
	return append(dst, '}'), nil
}

func parseJSONTag(sf reflect.StructField) (name string, omitEmpty, skip bool) {
	tag, ok := sf.Tag.Lookup("json")
	if !ok {
		return sf.Name, false, false
	}
	if tag == "-" {
		return "", false, true
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	if name == "" {
		name = sf.Name
	}
	for _, opt := range parts[1:] {
		if opt == "omitempty" {
			omitEmpty = true
		}
	}
	return name, omitEmpty, false
}

func encodeJSONNumber(dst []byte, n json.Number) ([]byte, error) {
	s := n.String()
	if s == "" {
		return nil, ErrMalformedJSON
	}
	if !isIntegerLiteral(s) {
		return nil, fmt.Errorf("%w: %s", ErrNonIntegerNumber, s)
	}
	// Normalise via big.Int so that "-0", "007" and "1e3"-free integer forms
	// all collapse to a single representation.
	bi, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNonIntegerNumber, s)
	}
	return append(dst, bi.String()...), nil
}

// isIntegerLiteral rejects fractions and exponents. An exponent is rejected even
// when it denotes an integer (1e3) because accepting it would mean two spellings
// of the same value, which is exactly what canonicalisation must prevent.
func isIntegerLiteral(s string) bool {
	if strings.ContainsAny(s, ".eE") {
		return false
	}
	i := 0
	if s[0] == '-' || s[0] == '+' {
		i = 1
	}
	if i >= len(s) {
		return false
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// appendCanonicalString applies RFC 8785 section 3.2.2.2 minimal escaping.
func appendCanonicalString(dst []byte, s string) ([]byte, error) {
	if !utf8.ValidString(s) {
		return nil, ErrInvalidUTF8
	}
	dst = append(dst, '"')
	for _, r := range s {
		switch r {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			if r < 0x20 {
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[r>>4], hexDigits[r&0x0F])
			} else {
				dst = utf8.AppendRune(dst, r)
			}
		}
	}
	return append(dst, '"'), nil
}

// ---------------------------------------------------------------------------
// UTF-16 code unit ordering (RFC 8785 section 3.2.3)
// ---------------------------------------------------------------------------

// compareUTF16 orders strings by their UTF-16 code unit sequence. This differs
// from Go's byte-wise string comparison for code points above U+FFFF: a
// surrogate pair starts at 0xD800, which sorts below characters in the
// U+E000..U+FFFF range, whereas their UTF-8 bytes sort the other way.
func compareUTF16(a, b string) int {
	au := utf16.Encode([]rune(a))
	bu := utf16.Encode([]rune(b))
	for i := 0; i < len(au) && i < len(bu); i++ {
		if au[i] != bu[i] {
			if au[i] < bu[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(au) < len(bu):
		return -1
	case len(au) > len(bu):
		return 1
	}
	return 0
}

func sortUTF16(keys []string) {
	slices.SortFunc(keys, compareUTF16)
}

// ---------------------------------------------------------------------------
// Token-level parsing with duplicate key detection
// ---------------------------------------------------------------------------

func parseValue(dec *json.Decoder, depth int) (any, error) {
	if depth > MaxDepth {
		return nil, ErrMaxDepthExceeded
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedJSON, err)
	}
	return parseFromToken(dec, tok, depth)
}

func parseFromToken(dec *json.Decoder, tok json.Token, depth int) (any, error) {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := make(map[string]any)
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, fmt.Errorf("%w: %v", ErrMalformedJSON, err)
				}
				key, ok := kt.(string)
				if !ok {
					return nil, ErrNonStringKey
				}
				if _, exists := obj[key]; exists {
					return nil, fmt.Errorf("%w: %q", ErrDuplicateKey, key)
				}
				val, err := parseValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				obj[key] = val
			}
			if _, err := dec.Token(); err != nil { // consume '}'
				return nil, fmt.Errorf("%w: %v", ErrMalformedJSON, err)
			}
			return obj, nil

		case '[':
			arr := make([]any, 0, 8)
			for dec.More() {
				val, err := parseValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			if _, err := dec.Token(); err != nil { // consume ']'
				return nil, fmt.Errorf("%w: %v", ErrMalformedJSON, err)
			}
			return arr, nil
		}
		return nil, ErrMalformedJSON

	case json.Number, string, bool, nil:
		return tok, nil
	}
	return nil, ErrMalformedJSON
}
