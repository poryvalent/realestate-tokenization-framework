package idempotency

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/acresync/orchestrator/internal/canonical"
	"github.com/acresync/orchestrator/internal/money"
)

var update = flag.Bool("update", false, "regenerate golden vector files")

const (
	schemeA = "6f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8"
	schemeB = "0123abcd-4567-89ef-0123-456789abcdef"
	scopeA  = "11111111-2222-3333-4444-555555555555"
	scopeB  = "99999999-8888-7777-6666-555555555555"
)

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func TestDeriveRejectsUnknownAction(t *testing.T) {
	_, err := Derive(Input{Action: "NOT_A_REAL_ACTION", SchemeID: schemeA})
	if !errors.Is(err, ErrUnknownAction) {
		t.Fatalf("want ErrUnknownAction, got %v", err)
	}
}

func TestDeriveRequiresSchemeID(t *testing.T) {
	if _, err := Derive(Input{Action: ActionAdvanceClock}); !errors.Is(err, ErrMissingSchemeID) {
		t.Fatalf("want ErrMissingSchemeID, got %v", err)
	}
}

func TestDeriveRequiresCanonicalUUIDs(t *testing.T) {
	cases := []struct{ name, scheme, scope string }{
		{"uppercase scheme", "6F1A2B3C-4D5E-6F70-8192-A3B4C5D6E7F8", ""},
		{"no dashes", "6f1a2b3c4d5e6f708192a3b4c5d6e7f8", ""},
		{"not a uuid", "scheme-1", ""},
		{"uppercase scope", schemeA, "11111111-2222-3333-4444-55555555555A"},
		{"short scope", schemeA, "1111"},
	}
	for _, c := range cases {
		_, err := Derive(Input{Action: ActionAdvanceClock, SchemeID: c.scheme, ScopeID: c.scope})
		if !errors.Is(err, ErrNotAUUID) {
			t.Errorf("%s: want ErrNotAUUID, got %v", c.name, err)
		}
	}
}

// A float in a payload must be a hard error, not silently formatted. Two runs
// producing "0.1" and "0.10000000000000001" would derive two keys for one action.
func TestDeriveRejectsFloatPayload(t *testing.T) {
	_, err := Derive(Input{
		Action:   ActionInjectRent,
		SchemeID: schemeA,
		ScopeID:  scopeA,
		Payload:  map[string]any{"amount": 1234.56},
	})
	if !errors.Is(err, canonical.ErrFloatNotPermitted) {
		t.Fatalf("want ErrFloatNotPermitted, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Determinism
// ---------------------------------------------------------------------------

func TestDeriveIsStableAcrossMapOrdering(t *testing.T) {
	in := Input{
		Action:   ActionAnchorPeriod,
		SchemeID: schemeA,
		ScopeID:  scopeA,
		Payload: map[string]any{
			"ndcfPaise":        money.Paise(30_000_000_000),
			"distributedPaise": money.Paise(28_500_000_000),
			"distributionBps":  9500,
			"statementHash":    "0xabcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
			"snapshotRoot":     "0xfedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
			"recordDate":       "2026-09-30",
		},
	}
	first := MustDerive(in)
	for i := range 2000 {
		if got := MustDerive(in); got != first {
			t.Fatalf("iteration %d: %s != %s", i, got.Hex(), first.Hex())
		}
	}
}

func TestDistinctInputsProduceDistinctKeys(t *testing.T) {
	base := Input{Action: ActionInjectRent, SchemeID: schemeA, ScopeID: scopeA,
		Payload: map[string]any{"leaseId": scopeB, "amountPaise": money.Paise(100)}}

	variants := map[string]Input{
		"different action":   {Action: ActionDraftNDCF, SchemeID: schemeA, ScopeID: scopeA, Payload: base.Payload},
		"different scheme":   {Action: ActionInjectRent, SchemeID: schemeB, ScopeID: scopeA, Payload: base.Payload},
		"different scope":    {Action: ActionInjectRent, SchemeID: schemeA, ScopeID: scopeB, Payload: base.Payload},
		"empty scope":        {Action: ActionInjectRent, SchemeID: schemeA, ScopeID: "", Payload: base.Payload},
		"different amount":   {Action: ActionInjectRent, SchemeID: schemeA, ScopeID: scopeA, Payload: map[string]any{"leaseId": scopeB, "amountPaise": money.Paise(101)}},
		"nil payload":        {Action: ActionInjectRent, SchemeID: schemeA, ScopeID: scopeA, Payload: nil},
		"empty map payload":  {Action: ActionInjectRent, SchemeID: schemeA, ScopeID: scopeA, Payload: map[string]any{}},
	}

	baseKey := MustDerive(base)
	seen := map[Key]string{baseKey: "base"}

	for name, v := range variants {
		k := MustDerive(v)
		if prior, dup := seen[k]; dup {
			t.Errorf("%q collides with %q: both derive %s", name, prior, k.Hex())
		}
		seen[k] = name
	}
}

// Identical payload content must derive an identical key regardless of whether it
// arrived as a struct or as a map. Otherwise a refactor from one to the other
// silently permits a replay of an action already performed.
func TestStructAndMapPayloadsAgree(t *testing.T) {
	type rent struct {
		LeaseID     string      `json:"leaseId"`
		AmountPaise money.Paise `json:"amountPaise"`
		ReceivedAt  string      `json:"receivedAt"`
	}
	viaStruct := MustDerive(Input{Action: ActionInjectRent, SchemeID: schemeA, ScopeID: scopeA,
		Payload: rent{LeaseID: scopeB, AmountPaise: 2_500_000_000, ReceivedAt: "2026-09-01T00:00:00Z"}})

	viaMap := MustDerive(Input{Action: ActionInjectRent, SchemeID: schemeA, ScopeID: scopeA,
		Payload: map[string]any{
			"leaseId":     scopeB,
			"amountPaise": money.Paise(2_500_000_000),
			"receivedAt":  "2026-09-01T00:00:00Z",
		}})

	if viaStruct != viaMap {
		t.Fatalf("struct and map payloads disagree:\n struct %s\n    map %s", viaStruct.Hex(), viaMap.Hex())
	}
}

// ---------------------------------------------------------------------------
// Framing
// ---------------------------------------------------------------------------

// The reason the Phase 2 ':'-join was replaced. With a raw separator, moving a
// boundary between adjacent fields produces the same preimage; with length
// prefixes it cannot. Demonstrated on payload content, since the action and UUID
// fields are format-constrained.
func TestFramingIsUnambiguous(t *testing.T) {
	a := MustDerive(Input{Action: ActionCorrectHolding, SchemeID: schemeA, ScopeID: scopeA,
		Payload: map[string]any{"a": "x", "b": "yz"}})
	b := MustDerive(Input{Action: ActionCorrectHolding, SchemeID: schemeA, ScopeID: scopeA,
		Payload: map[string]any{"a": "xy", "b": "z"}})
	if a == b {
		t.Fatal("boundary shift between adjacent values produced the same key")
	}
}

func TestPreimageLayout(t *testing.T) {
	in := Input{Action: ActionAdvanceClock, SchemeID: schemeA, ScopeID: "", Payload: map[string]any{}}
	pre, err := Preimage(in)
	if err != nil {
		t.Fatal(err)
	}

	payload, err := canonical.Marshal(in.Payload)
	if err != nil {
		t.Fatal(err)
	}

	want := 0
	for _, part := range [][]byte{[]byte(Domain), []byte(in.Action), []byte(in.SchemeID), []byte(in.ScopeID), payload} {
		want += 4 + len(part)
	}
	if len(pre) != want {
		t.Fatalf("preimage length %d, want %d", len(pre), want)
	}

	// First frame is the domain tag, length-prefixed big-endian.
	if n := binary.BigEndian.Uint32(pre[:4]); int(n) != len(Domain) {
		t.Fatalf("first frame length = %d, want %d", n, len(Domain))
	}
	if string(pre[4:4+len(Domain)]) != Domain {
		t.Fatalf("first frame = %q, want %q", pre[4:4+len(Domain)], Domain)
	}
}

// The domain tag exists so that changing the derivation rule cannot produce a key
// that collides with one derived under the old rule.
func TestDomainTagIsVersioned(t *testing.T) {
	if Domain != "acresync.idempotency.v1" {
		t.Fatalf("Domain changed to %q. This invalidates every key ever derived and every "+
			"unique index entry that depends on them. If intentional, update this test and "+
			"document the migration.", Domain)
	}
}

// ---------------------------------------------------------------------------
// Key encoding
// ---------------------------------------------------------------------------

func TestKeyHexRoundTrip(t *testing.T) {
	k := MustDerive(Input{Action: ActionPause, SchemeID: schemeA})
	parsed, err := ParseKey(k.Hex())
	if err != nil {
		t.Fatal(err)
	}
	if parsed != k {
		t.Fatal("round trip changed the key")
	}
	if len(k.Hex()) != 66 {
		t.Fatalf("hex length %d, want 66 (0x + 64)", len(k.Hex()))
	}
}

func TestParseKeyRejectsBadEncodings(t *testing.T) {
	good := MustDerive(Input{Action: ActionPause, SchemeID: schemeA}).Hex()
	bad := []string{
		"",
		good[2:],               // missing 0x
		good + "00",            // too long
		good[:len(good)-1],     // too short
		"0x" + "G" + good[3:],  // non-hex
		"0X" + good[2:],        // uppercase prefix
	}
	for _, s := range bad {
		if _, err := ParseKey(s); !errors.Is(err, ErrBadKeyEncoding) {
			t.Errorf("ParseKey(%q): want ErrBadKeyEncoding, got %v", s, err)
		}
	}
	// Uppercase body is rejected so a key has exactly one spelling.
	upper := "0x" + "ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789"
	if _, err := ParseKey(upper); !errors.Is(err, ErrBadKeyEncoding) {
		t.Errorf("uppercase body should be rejected, got %v", err)
	}
}

func TestZeroKey(t *testing.T) {
	var k Key
	if !k.IsZero() {
		t.Error("zero value should report IsZero")
	}
	if MustDerive(Input{Action: ActionPause, SchemeID: schemeA}).IsZero() {
		t.Error("a derived key should never be zero")
	}
}

// ---------------------------------------------------------------------------
// Golden vectors
// ---------------------------------------------------------------------------

type vector struct {
	Name     string          `json:"name"`
	Action   Action          `json:"action"`
	SchemeID string          `json:"schemeId"`
	ScopeID  string          `json:"scopeId"`
	Payload  json.RawMessage `json:"payload"`
	Key      string          `json:"key"`
}

// The M0 gate for key derivation. These hex values are committed and must never
// change silently: a unique index in Postgres and the contract's usedKeys mapping
// both already contain keys derived under this rule.
func TestGoldenVectors(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "idempotency_vectors.json")

	type spec struct {
		name     string
		action   Action
		schemeID string
		scopeID  string
		payload  string
	}
	specs := []spec{
		{"advance_clock", ActionAdvanceClock, schemeA, "", `{"to":"2026-10-01T00:00:00Z"}`},
		{"empty_payload", ActionPause, schemeA, "", `{}`},
		{"null_payload", ActionUnpause, schemeA, "", `null`},
		{"inject_rent", ActionInjectRent, schemeA, scopeA,
			`{"leaseId":"99999999-8888-7777-6666-555555555555","receivedAt":"2026-09-01T00:00:00Z","receivedAmountPaise":2500000000}`},
		{"anchor_period", ActionAnchorPeriod, schemeA, scopeA,
			`{"periodSeq":1,"recordDate":"2026-09-30","ndcfPaise":30000000000,"distributedPaise":28500000000,"distributionBps":9500,"statementHash":"0xabcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"}`},
		{"commit_seed", ActionCommitSeed, schemeA, scopeB,
			`{"commitment":"0x1111111111111111111111111111111111111111111111111111111111111111","targetBlock":8123456}`},
		{"settle_batch", ActionSettleBatch, schemeA, scopeB,
			`{"cursorFrom":0,"holders":["0x000000000000000000000000000000000000dead","0x000000000000000000000000000000000000beef"],"units":[3,1]}`},
		{"finalise_settlement", ActionFinaliseSettlement, schemeA, scopeB, `{"expectedHolders":200,"expectedUnits":475}`},
		{"reverse_period", ActionReversePeriod, schemeA, scopeA,
			`{"reason":"DUPLICATE_INJECTION","narrativeHash":"0x2222222222222222222222222222222222222222222222222222222222222222"}`},
	}

	if *update {
		vecs := make([]vector, 0, len(specs))
		for _, s := range specs {
			var payload any
			if err := json.Unmarshal([]byte(s.payload), &payload); err != nil {
				t.Fatalf("%s: %v", s.name, err)
			}
			// Route through canonical.MarshalJSON so integers keep full precision.
			cb, err := canonical.MarshalJSON([]byte(s.payload))
			if err != nil {
				t.Fatalf("%s: %v", s.name, err)
			}
			var tree any
			if err := unmarshalUseNumber(cb, &tree); err != nil {
				t.Fatalf("%s: %v", s.name, err)
			}
			k, err := Derive(Input{Action: s.action, SchemeID: s.schemeID, ScopeID: s.scopeID, Payload: tree})
			if err != nil {
				t.Fatalf("%s: %v", s.name, err)
			}
			vecs = append(vecs, vector{
				Name: s.name, Action: s.action, SchemeID: s.schemeID, ScopeID: s.scopeID,
				Payload: json.RawMessage(s.payload), Key: k.Hex(),
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
		t.Fatalf("reading golden vectors (run `go test ./internal/idempotency -update` to create): %v", err)
	}
	var vecs []vector
	if err := json.Unmarshal(blob, &vecs); err != nil {
		t.Fatal(err)
	}
	if len(vecs) != len(specs) {
		t.Fatalf("golden file has %d vectors, test defines %d; regenerate with -update", len(vecs), len(specs))
	}
	for _, v := range vecs {
		cb, err := canonical.MarshalJSON(v.Payload)
		if err != nil {
			t.Errorf("%s: %v", v.Name, err)
			continue
		}
		var tree any
		if err := unmarshalUseNumber(cb, &tree); err != nil {
			t.Errorf("%s: %v", v.Name, err)
			continue
		}
		got, err := Derive(Input{Action: v.Action, SchemeID: v.SchemeID, ScopeID: v.ScopeID, Payload: tree})
		if err != nil {
			t.Errorf("%s: %v", v.Name, err)
			continue
		}
		if got.Hex() != v.Key {
			t.Errorf("%s:\n got %s\nwant %s", v.Name, got.Hex(), v.Key)
		}
	}
}

func unmarshalUseNumber(data []byte, v *any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}
