package canonical

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "regenerate golden vector files")

// ---------------------------------------------------------------------------
// Rejections
// ---------------------------------------------------------------------------

func TestMarshalRejectsFloats(t *testing.T) {
	cases := []any{
		float64(1),
		float32(1),
		map[string]any{"amount": 1.5},
		[]any{1.0},
		struct{ V float64 }{V: 0},
	}
	for _, c := range cases {
		if _, err := Marshal(c); !errors.Is(err, ErrFloatNotPermitted) {
			t.Errorf("Marshal(%#v): want ErrFloatNotPermitted, got %v", c, err)
		}
	}
}

func TestMarshalRejectsByteSlicesAndTime(t *testing.T) {
	if _, err := Marshal([]byte{1, 2, 3}); !errors.Is(err, ErrBytesNotPermitted) {
		t.Errorf("byte slice: want ErrBytesNotPermitted, got %v", err)
	}
	if _, err := Marshal(map[string]any{"h": []byte{0xde, 0xad}}); !errors.Is(err, ErrBytesNotPermitted) {
		t.Errorf("nested byte slice: want ErrBytesNotPermitted, got %v", err)
	}
	if _, err := Marshal(time.Now()); !errors.Is(err, ErrTimeNotPermitted) {
		t.Errorf("time.Time: want ErrTimeNotPermitted, got %v", err)
	}
}

func TestMarshalJSONRejectsDuplicateKeys(t *testing.T) {
	raw := []byte(`{"a":1,"a":2}`)
	if _, err := MarshalJSON(raw); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("want ErrDuplicateKey, got %v", err)
	}
}

func TestMarshalJSONRejectsNonIntegerNumbers(t *testing.T) {
	for _, raw := range []string{`{"a":1.5}`, `{"a":1e3}`, `{"a":1.0}`, `{"a":-2.5}`, `{"a":1E-3}`} {
		if _, err := MarshalJSON([]byte(raw)); !errors.Is(err, ErrNonIntegerNumber) {
			t.Errorf("MarshalJSON(%s): want ErrNonIntegerNumber, got %v", raw, err)
		}
	}
}

func TestMaxDepthEnforced(t *testing.T) {
	deep := strings.Repeat(`{"a":`, MaxDepth+5) + `1` + strings.Repeat(`}`, MaxDepth+5)
	if _, err := MarshalJSON([]byte(deep)); !errors.Is(err, ErrMaxDepthExceeded) {
		t.Fatalf("want ErrMaxDepthExceeded, got %v", err)
	}
}

// Two fields resolving to the same JSON name would make output depend on struct
// declaration order, so it is rejected. Expressed as an untagged field colliding
// with a tagged one, because `go vet` already catches two identical json tags and
// the encoder must catch the case vet does not.
func TestCollidingFieldNamesRejected(t *testing.T) {
	type bad struct {
		X int
		Y int `json:"X"`
	}
	if _, err := Marshal(bad{1, 2}); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("want ErrDuplicateKey for colliding field names, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Integer precision
// ---------------------------------------------------------------------------

// The reason floats are banned rather than tolerated. A float64 round-trip
// silently corrupts every integer above 2^53, and AcreSync's paise amounts and
// 1e18-scaled ratios live above that line.
func TestIntegerPrecisionAbove2Pow53(t *testing.T) {
	const exact = int64(9007199254740993) // 2^53 + 1
	got, err := Marshal(map[string]any{"v": exact})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"v":9007199254740993}`; string(got) != want {
		t.Fatalf("got %s, want %s", got, want)
	}

	// Demonstrate the corruption the ban prevents.
	var viaFloat map[string]float64
	if err := json.Unmarshal([]byte(`{"v":9007199254740993}`), &viaFloat); err != nil {
		t.Fatal(err)
	}
	if int64(viaFloat["v"]) == exact {
		t.Fatal("expected float64 decoding to lose precision; environment differs from assumption")
	}

	// And that our own path does not.
	round, err := MarshalJSON([]byte(`{"v":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(round) != `{"v":9007199254740993}` {
		t.Fatalf("MarshalJSON lost precision: %s", round)
	}
}

func TestMaxInt64AndUint64(t *testing.T) {
	got, err := Marshal(map[string]any{
		"i": int64(-9223372036854775808),
		"u": uint64(18446744073709551615),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"i":-9223372036854775808,"u":18446744073709551615}`
	if string(got) != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestNegativeZeroNormalises(t *testing.T) {
	got, err := MarshalJSON([]byte(`{"v":-0}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"v":0}`; string(got) != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

// ---------------------------------------------------------------------------
// Key ordering
// ---------------------------------------------------------------------------

// The case that separates a correct JCS implementation from a plausible one.
//
// U+1F600 encodes in UTF-16 as the surrogate pair D83D DE00, and in UTF-8 as
// F0 9F 98 80. U+E000 encodes as the single code unit E000 and as UTF-8 EE 80 80.
//
// By UTF-16 code unit order, D83D < E000, so the emoji sorts first. By UTF-8 byte
// order, EE < F0, so U+E000 sorts first. RFC 8785 mandates the UTF-16 ordering, so
// a naive sort.Strings here produces a document that a conformant verifier will
// hash differently.
func TestKeyOrderingUsesUTF16CodeUnits(t *testing.T) {
	const emoji = "\U0001F600"    // surrogate pair D83D DE00
	const privateUse = "\uE000"   // single code unit E000

	got, err := Marshal(map[string]any{privateUse: 1, emoji: 2})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"` + emoji + `":2,"` + privateUse + `":1}`
	if string(got) != want {
		t.Fatalf("UTF-16 ordering not applied.\n got: %s\nwant: %s", got, want)
	}

	if compareUTF16(emoji, privateUse) >= 0 {
		t.Error("compareUTF16 should order the surrogate pair before U+E000")
	}
	if !(privateUse < emoji) {
		t.Error("expected Go byte-wise comparison to disagree; the test has lost its point")
	}
}

func TestKeyOrderingBasic(t *testing.T) {
	got, err := Marshal(map[string]any{"b": 1, "a": 2, "C": 3, "aa": 4, "": 5})
	if err != nil {
		t.Fatal(err)
	}
	// Empty string first, then uppercase C (0x43), then a, aa, b.
	if want := `{"":5,"C":3,"a":2,"aa":4,"b":1}`; string(got) != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

// Go randomises map iteration order, so a stable output across many attempts is
// evidence the sort is doing the work rather than luck.
func TestDeterministicAcrossMapIterationOrder(t *testing.T) {
	m := map[string]any{}
	for _, k := range []string{"zeta", "alpha", "mid", "b", "Q", "aa", "AA", "_x", "1", "9"} {
		m[k] = k
	}
	first, err := Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2000 {
		got, err := Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(first) {
			t.Fatalf("iteration %d differed:\n got %s\nwant %s", i, got, first)
		}
	}
}

// ---------------------------------------------------------------------------
// String escaping
// ---------------------------------------------------------------------------

func TestStringEscapingIsJCSMinimal(t *testing.T) {
	cases := []struct{ in, want string }{
		{`plain`, `"plain"`},
		{`a"b`, `"a\"b"`},
		{`a\b`, `"a\\b"`},
		{"tab\there", `"tab\there"`},
		{"nl\nhere", `"nl\nhere"`},
		{"cr\rhere", `"cr\rhere"`},
		{"bs\bhere", `"bs\bhere"`},
		{"ff\fhere", `"ff\fhere"`},
		{"ctrl\x01here", `"ctrl\u0001here"`},
		{"ctrl\x1fhere", `"ctrl\u001fhere"`},

		// Go's encoding/json escapes these three by default (HTML escaping) and
		// RFC 8785 requires that it must not. If this regresses, every CID we
		// have published becomes unreproducible by a conformant verifier.
		{`a<b>c&d`, `"a<b>c&d"`},

		// The solidus is never escaped.
		{`a/b`, `"a/b"`},

		// Non-ASCII passes through as UTF-8, not as \uXXXX.
		{"naïve ₹", `"naïve ₹"`},
	}
	for _, c := range cases {
		got, err := Marshal(c.in)
		if err != nil {
			t.Fatalf("Marshal(%q): %v", c.in, err)
		}
		if string(got) != c.want {
			t.Errorf("Marshal(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestEncodingJSONWouldHaveDifferedOnHTMLChars(t *testing.T) {
	std, err := json.Marshal(`a<b>c&d`)
	if err != nil {
		t.Fatal(err)
	}
	if string(std) == `"a<b>c&d"` {
		t.Skip("encoding/json no longer HTML-escapes by default; the divergence note can be revisited")
	}
	ours, err := Marshal(`a<b>c&d`)
	if err != nil {
		t.Fatal(err)
	}
	if string(ours) == string(std) {
		t.Fatal("our encoder should not HTML-escape")
	}
}

// ---------------------------------------------------------------------------
// Structural
// ---------------------------------------------------------------------------

func TestNilAndEmpty(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, `null`},
		{(*int)(nil), `null`},
		{[]any(nil), `null`},
		{map[string]any(nil), `null`},
		{[]any{}, `[]`},
		{map[string]any{}, `{}`},
		{true, `true`},
		{false, `false`},
	}
	for _, c := range cases {
		got, err := Marshal(c.in)
		if err != nil {
			t.Fatalf("Marshal(%#v): %v", c.in, err)
		}
		if string(got) != c.want {
			t.Errorf("Marshal(%#v) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestStructTags(t *testing.T) {
	type inner struct {
		Z int `json:"z"`
	}
	type outer struct {
		Beta   string `json:"beta"`
		Alpha  int    `json:"alpha"`
		Hidden int    `json:"-"`
		Omit   string `json:"omit,omitempty"`
		Nested inner  `json:"nested"`
		NoTag  int
	}
	got, err := Marshal(outer{Beta: "b", Alpha: 1, Hidden: 99, Nested: inner{Z: 7}, NoTag: 3})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"NoTag":3,"alpha":1,"beta":"b","nested":{"z":7}}`
	if string(got) != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestEqual(t *testing.T) {
	a := []byte(`{"b":2,"a":1}`)
	b := []byte(` { "a" : 1 , "b" : 2 } `)
	eq, err := Equal(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if !eq {
		t.Fatal("documents differing only in key order and whitespace should be canonically equal")
	}
}

func TestTrailingTokensRejected(t *testing.T) {
	if _, err := MarshalJSON([]byte(`{"a":1} {"b":2}`)); !errors.Is(err, ErrTrailingJSONTokens) {
		t.Fatalf("want ErrTrailingJSONTokens, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Golden vectors
// ---------------------------------------------------------------------------

type vector struct {
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
	Expected string          `json:"expected"`
}

// The M0 gate: canonical output must be byte-identical to a committed reference
// set. Any change in the encoder that alters output for existing inputs breaks
// this test, which is the point. Regenerate deliberately with -update, and treat
// a diff in the golden file as an incident rather than a chore: every previously
// published CID depends on these bytes.
func TestGoldenVectors(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "canonical_vectors.json")

	inputs := []struct {
		name  string
		input string
	}{
		{"empty_object", `{}`},
		{"empty_array", `[]`},
		{"key_order", `{"zeta":1,"Alpha":2,"beta":3,"":4}`},
		{"nested", `{"b":{"d":1,"c":2},"a":[3,2,1]}`},
		{"large_integers", `{"max_i64":9223372036854775807,"min_i64":-9223372036854775808,"above_2p53":9007199254740993}`},
		{"paise_amounts", `{"ndcfPaise":30000000000,"distributedPaise":28500000000,"distributionBps":9500}`},
		{"string_escapes", `{"quote":"a\"b","backslash":"a\\b","tab":"a\tb","ctrl":"a\u0001b","solidus":"a/b","html":"a<b>&c"}`},
		{"unicode", `{"rupee":"₹","naive":"naïve","emoji":"\ud83d\ude00"}`},
		{"literals", `{"t":true,"f":false,"n":null}`},
		{"whitespace_stripped", "{\n  \"a\" : 1,\n  \"b\" : [ 1, 2 ]\n}"},
		{"negative_zero", `{"v":-0}`},
		{"deep_nest", `{"a":{"b":{"c":{"d":{"e":1}}}}}`},
	}

	if *update {
		vecs := make([]vector, 0, len(inputs))
		for _, in := range inputs {
			out, err := MarshalJSON([]byte(in.input))
			if err != nil {
				t.Fatalf("generating %s: %v", in.name, err)
			}
			vecs = append(vecs, vector{
				Name:     in.name,
				Input:    json.RawMessage(in.input),
				Expected: string(out),
			})
		}
		blob, err := json.MarshalIndent(vecs, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(blob, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %d vectors to %s", len(vecs), path)
		return
	}

	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden vectors (run `go test ./internal/canonical -update` to create): %v", err)
	}
	var vecs []vector
	if err := json.Unmarshal(blob, &vecs); err != nil {
		t.Fatal(err)
	}
	if len(vecs) != len(inputs) {
		t.Fatalf("golden file has %d vectors, test defines %d; regenerate with -update", len(vecs), len(inputs))
	}
	for _, v := range vecs {
		got, err := MarshalJSON(v.Input)
		if err != nil {
			t.Errorf("%s: %v", v.Name, err)
			continue
		}
		if string(got) != v.Expected {
			t.Errorf("%s:\n got %s\nwant %s", v.Name, got, v.Expected)
		}
	}
}
