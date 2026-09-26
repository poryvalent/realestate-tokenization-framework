package ipfs

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acresync/orchestrator/internal/ipfsguard"
)

const (
	hash32  = "0xabcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	hash32b = "0x1111111111111111111111111111111111111111111111111111111111111111"
	addr1   = "0x000000000000000000000000000000000000dead"
	addr2   = "0x000000000000000000000000000000000000beef"
)

func validSnapshot() string {
	return fmt.Sprintf(`{
	  "docType": "SNAPSHOT",
	  "schemaVersion": 1,
	  "schemeRef": %q,
	  "periodId": 1,
	  "recordDate": "2026-09-30",
	  "takenAt": "2026-09-30T18:30:00Z",
	  "totalUnits": 500,
	  "distinctHolders": 201,
	  "merkleRoot": %q,
	  "algoVersion": 1,
	  "lines": [
	    {"leafIndex": 0, "holder": %q, "units": 25, "excluded": true,  "investorAnchor": %q},
	    {"leafIndex": 1, "holder": %q, "units": 3,  "excluded": false, "investorAnchor": %q}
	  ]
	}`, hash32, hash32b, addr1, hash32, addr2, hash32b)
}

func newTestPublisher(t *testing.T) (*Publisher, *MockProvider) {
	t.Helper()
	m, err := NewMockProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return NewPublisher(m), m
}

func TestPublishHappyPath(t *testing.T) {
	pub, mock := newTestPublisher(t)

	pin, err := pub.Publish(context.Background(), ipfsguard.DocSnapshot, []byte(validSnapshot()))
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	if pin.DerivedCID != pin.ProviderCID {
		t.Errorf("the mock pins raw blocks, so the addresses must agree: %s vs %s",
			pin.DerivedCID, pin.ProviderCID)
	}
	if !pin.CodecVerified {
		t.Error("CodecVerified must be true against a raw-block store")
	}
	if !pin.SingleBlock {
		t.Error("a two-line snapshot is far under the block ceiling")
	}
	if pin.PinCount != 1 {
		t.Errorf("PinCount = %d, want 1", pin.PinCount)
	}
	if pin.ByteSize != len(pin.CanonicalBytes) {
		t.Errorf("ByteSize %d does not match the canonical bytes length %d",
			pin.ByteSize, len(pin.CanonicalBytes))
	}

	// The digest must be SHA-256 of the canonical bytes, which is what a verifier recomputes.
	want := sha256.Sum256(pin.CanonicalBytes)
	if pin.Digest != want {
		t.Errorf("digest %x is not SHA-256 of the pinned bytes %x", pin.Digest, want)
	}

	if _, err := os.Stat(filepath.Join(mock.Dir(), pin.DerivedCID)); err != nil {
		t.Errorf("content should be stored under its own address: %v", err)
	}
}

// TestPublishedBytesAreCanonicalNotCallerBytes proves the pinned bytes are the guard's output.
//
// The caller handed in indented JSON with newlines. If those bytes were pinned, the digest would
// depend on the caller's formatting, and re-deriving the same logical document later would produce a
// different anchor. Anchoring is permanent, so that divergence would be unrecoverable.
func TestPublishedBytesAreCanonicalNotCallerBytes(t *testing.T) {
	pub, _ := newTestPublisher(t)

	raw := []byte(validSnapshot())
	pin, err := pub.Publish(context.Background(), ipfsguard.DocSnapshot, raw)
	if err != nil {
		t.Fatal(err)
	}

	if string(pin.CanonicalBytes) == string(raw) {
		t.Fatal("the fixture is indented, so canonical bytes must differ from the input")
	}
	if strings.ContainsAny(string(pin.CanonicalBytes), "\n\t") {
		t.Error("canonical output must not contain insignificant whitespace")
	}
	if sha256.Sum256(raw) == pin.Digest {
		t.Error("the digest must be over canonical bytes, not the caller's formatting")
	}
}

// TestLogicallyIdenticalDocumentsGetOneAddress is the property the whole scheme rests on.
//
// Two services building the same snapshot will not emit identical text. If key order or spacing
// changed the address, the anchored root and the pinned document could disagree while both being
// "correct", and there would be no way to tell which spelling the chain committed to.
func TestLogicallyIdenticalDocumentsGetOneAddress(t *testing.T) {
	pub, mock := newTestPublisher(t)
	ctx := context.Background()

	a := validSnapshot()

	// Same content, docType moved to the end and whitespace collapsed.
	reordered := strings.Replace(a, `"docType": "SNAPSHOT",`, ``, 1)
	reordered = strings.TrimSpace(reordered)
	reordered = reordered[:len(reordered)-1] + `, "docType": "SNAPSHOT"}`
	reordered = strings.ReplaceAll(reordered, "\n", " ")
	reordered = strings.ReplaceAll(reordered, "\t", "")

	p1, err := pub.Publish(ctx, ipfsguard.DocSnapshot, []byte(a))
	if err != nil {
		t.Fatal(err)
	}
	p2, err := pub.Publish(ctx, ipfsguard.DocSnapshot, []byte(reordered))
	if err != nil {
		t.Fatalf("the reordered document must still validate: %v", err)
	}

	if p1.Digest != p2.Digest {
		t.Fatalf("reordering keys changed the digest:\n  %x\n  %x", p1.Digest, p2.Digest)
	}
	if p1.DerivedCID != p2.DerivedCID {
		t.Fatalf("one logical document produced two addresses: %s and %s", p1.DerivedCID, p2.DerivedCID)
	}

	// Content addressing means the second pin lands on the same key rather than creating a copy.
	if got := mock.PinCountFor(p1.DerivedCID); got != 2 {
		t.Errorf("PinCountFor = %d, want 2 pins of one address", got)
	}
	entries, _ := os.ReadDir(mock.Dir())
	if len(entries) != 1 {
		t.Errorf("store holds %d objects, want 1", len(entries))
	}
}

// TestPIINeverReachesTheStore is the DPDP guarantee, and it asserts absence rather than an error.
//
// A returned error is not sufficient evidence here. The requirement is that nothing was written,
// because IPFS has no delete: anyone who fetched the content could re-pin it, so a personal
// identifier that lands once has landed permanently and an erasure request cannot be honoured. So
// the test checks the directory is empty afterwards.
func TestPIINeverReachesTheStore(t *testing.T) {
	cases := map[string]string{
		"PAN":   `"totalUnits": 500, "pan": "ABCDE1234F",`,
		"email": `"totalUnits": 500, "contact": "investor@example.com",`,
		"IFSC":  `"totalUnits": 500, "bank": "HDFC0001234",`,
		"name":  `"totalUnits": 500, "investorName": "ACME",`,
	}

	for name, injection := range cases {
		t.Run(name, func(t *testing.T) {
			pub, mock := newTestPublisher(t)

			raw := strings.Replace(validSnapshot(), `"totalUnits": 500,`, injection, 1)
			if raw == validSnapshot() {
				t.Fatal("fixture substitution did not apply; the test would pass vacuously")
			}

			if _, err := pub.Publish(context.Background(), ipfsguard.DocSnapshot, []byte(raw)); err == nil {
				t.Fatal("publishing a document carrying a personal identifier must fail")
			}

			entries, err := os.ReadDir(mock.Dir())
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("%d objects written; a rejected document must never be stored, because "+
					"IPFS content cannot be withdrawn once published", len(entries))
			}
		})
	}
}

func TestEmptyDocumentRejected(t *testing.T) {
	pub, _ := newTestPublisher(t)
	if _, err := pub.Publish(context.Background(), ipfsguard.DocSnapshot, nil); !errors.Is(err, ErrEmptyDocument) {
		t.Fatalf("want ErrEmptyDocument, got %v", err)
	}
}

func TestUnknownDocTypeRejected(t *testing.T) {
	pub, mock := newTestPublisher(t)

	_, err := pub.Publish(context.Background(), ipfsguard.DocType("PAYOUT_INSTRUCTION"), []byte(`{"a":1}`))
	if err == nil {
		t.Fatal("an unregistered document type must be refused")
	}
	if entries, _ := os.ReadDir(mock.Dir()); len(entries) != 0 {
		t.Error("nothing should be stored for an unknown document type")
	}
}

func TestVerifyRoundTripPasses(t *testing.T) {
	pub, _ := newTestPublisher(t)
	ctx := context.Background()

	pin, err := pub.Publish(ctx, ipfsguard.DocSnapshot, []byte(validSnapshot()))
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.VerifyRoundTrip(ctx, pin); err != nil {
		t.Fatalf("round trip must succeed immediately after pinning: %v", err)
	}
}

// TestVerifyRoundTripCatchesCorruption exercises the branch that separates a loud failure from a
// silent one. Without this check, a gateway serving altered bytes would look identical to a working
// setup until an outside party tried to verify an anchor and could not.
func TestVerifyRoundTripCatchesCorruption(t *testing.T) {
	pub, mock := newTestPublisher(t)
	ctx := context.Background()

	pin, err := pub.Publish(ctx, ipfsguard.DocSnapshot, []byte(validSnapshot()))
	if err != nil {
		t.Fatal(err)
	}

	altered := strings.Replace(string(pin.CanonicalBytes), `"units":3`, `"units":4`, 1)
	if altered == string(pin.CanonicalBytes) {
		altered = string(pin.CanonicalBytes) + " "
	}
	if err := mock.Corrupt(pin.ProviderCID, []byte(altered)); err != nil {
		t.Fatal(err)
	}

	err = pub.VerifyRoundTrip(ctx, pin)
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("want ErrDigestMismatch, got %v", err)
	}
}

// TestVerifyRoundTripCatchesUnretrievableContent covers a pin that reported success but whose
// content cannot be fetched. Those are different claims and only the second one is auditable.
func TestVerifyRoundTripCatchesUnretrievableContent(t *testing.T) {
	pub, mock := newTestPublisher(t)
	ctx := context.Background()

	pin, err := pub.Publish(ctx, ipfsguard.DocSnapshot, []byte(validSnapshot()))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(mock.Dir(), pin.ProviderCID)); err != nil {
		t.Fatal(err)
	}

	if err := pub.VerifyRoundTrip(ctx, pin); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// lyingProvider reports an address for content it was never given.
type lyingProvider struct {
	substitute []byte
}

func (l *lyingProvider) Name() string { return "LIAR" }

func (l *lyingProvider) Pin(ctx context.Context, dt ipfsguard.DocType, canonical []byte) (*Pin, error) {
	d := sha256.Sum256(l.substitute)
	return &Pin{
		DocType:     dt,
		Digest:      d,
		DerivedCID:  CIDFromDigest(d),
		ProviderCID: CIDFromDigest(d),
		Provider:    l.Name(),
	}, nil
}

func (l *lyingProvider) Fetch(ctx context.Context, cid string) ([]byte, error) {
	return l.substitute, nil
}

// TestProviderCannotInfluenceTheAnchoredDigest is the trust boundary.
//
// The digest goes on-chain permanently, so a provider that could steer it could make the ledger
// commit to a document AcreSync never produced. The Publisher recomputes the digest locally and
// treats a provider's raw CID disagreeing with it as fatal rather than as a codec quirk.
func TestProviderCannotInfluenceTheAnchoredDigest(t *testing.T) {
	pub := NewPublisher(&lyingProvider{substitute: []byte(`{"docType":"SOMETHING_ELSE"}`)})

	_, err := pub.Publish(context.Background(), ipfsguard.DocSnapshot, []byte(validSnapshot()))
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("a provider reporting a raw CID for other content must be rejected, got %v", err)
	}
}

// dagPbProvider models a service that wraps content, as Pinata may.
type dagPbProvider struct{ stored map[string][]byte }

func (d *dagPbProvider) Name() string { return "DAGPB" }

func (d *dagPbProvider) Pin(ctx context.Context, dt ipfsguard.DocType, canonical []byte) (*Pin, error) {
	if d.stored == nil {
		d.stored = map[string][]byte{}
	}
	// A plausible dag-pb style address: not derivable from the document digest.
	wrapper := sha256.Sum256(append([]byte("unixfs-wrapper"), canonical...))
	cid := "bafybei" + CIDFromDigest(wrapper)[7:]
	d.stored[cid] = canonical
	return &Pin{DocType: dt, ProviderCID: cid, Provider: d.Name(), PinCount: 1}, nil
}

func (d *dagPbProvider) Fetch(ctx context.Context, cid string) ([]byte, error) {
	b, ok := d.stored[cid]
	if !ok {
		return nil, ErrNotFound
	}
	return b, nil
}

// TestDifferentCodecDegradesTheClaimWithoutBreakingVerification is the Pinata contingency.
//
// If the pinning service addresses content differently, the derived CID will not resolve. That
// weakens a convenience claim, so it is recorded as CodecVerified false rather than failing the
// pin. What must not weaken is verification itself: the anchored digest still matches the document,
// and the round trip still proves it through the provider's own address.
func TestDifferentCodecDegradesTheClaimWithoutBreakingVerification(t *testing.T) {
	pub := NewPublisher(&dagPbProvider{})
	ctx := context.Background()

	pin, err := pub.Publish(ctx, ipfsguard.DocSnapshot, []byte(validSnapshot()))
	if err != nil {
		t.Fatalf("a different codec is not an error: %v", err)
	}

	if pin.CodecVerified {
		t.Error("CodecVerified must be false when the provider address is not the derived one")
	}
	if pin.Digest != sha256.Sum256(pin.CanonicalBytes) {
		t.Error("the anchored digest must still be SHA-256 of the document")
	}
	if err := pub.VerifyRoundTrip(ctx, pin); err != nil {
		t.Fatalf("verification must still hold via the provider address: %v", err)
	}
}

func TestPinFailurePropagates(t *testing.T) {
	pub, mock := newTestPublisher(t)

	sentinel := errors.New("pinning service unavailable")
	mock.FailNextPin(sentinel)

	if _, err := pub.Publish(context.Background(), ipfsguard.DocSnapshot, []byte(validSnapshot())); !errors.Is(err, sentinel) {
		t.Fatalf("want the underlying failure, got %v", err)
	}

	// Recovery on the next attempt, since a transient network failure must not poison the provider.
	if _, err := pub.Publish(context.Background(), ipfsguard.DocSnapshot, []byte(validSnapshot())); err != nil {
		t.Fatalf("the following attempt must succeed: %v", err)
	}
}

// TestFetchRejectsTraversal covers CIDs reaching the mock from stored records or operator input.
// A CID is a path component in the filesystem provider, so anything not shaped like one is refused
// before it can be joined to a path.
func TestFetchRejectsTraversal(t *testing.T) {
	_, mock := newTestPublisher(t)

	secret := filepath.Join(filepath.Dir(mock.Dir()), "secret.txt")
	if err := os.WriteFile(secret, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{
		"../secret.txt",
		"..\\secret.txt",
		filepath.Join("..", "secret.txt"),
		"/etc/passwd",
		"",
	} {
		if _, err := mock.Fetch(context.Background(), bad); err == nil {
			t.Errorf("Fetch(%q) must be refused", bad)
		}
	}
}

func TestContextCancellationRespected(t *testing.T) {
	pub, _ := newTestPublisher(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := pub.Publish(ctx, ipfsguard.DocSnapshot, []byte(validSnapshot())); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// TestSingleBlockCeilingIsReported checks the flag that marks where the derivation stops holding.
func TestSingleBlockCeilingIsReported(t *testing.T) {
	m, err := NewMockProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	small := []byte(strings.Repeat("x", 1024))
	pin, err := m.Pin(context.Background(), ipfsguard.DocSnapshot, small)
	if err != nil {
		t.Fatal(err)
	}
	if !pin.SingleBlock {
		t.Error("1 KiB is a single block")
	}

	big := []byte(strings.Repeat("x", SingleBlockMaxBytes+1))
	pin, err = m.Pin(context.Background(), ipfsguard.DocSnapshot, big)
	if err != nil {
		t.Fatal(err)
	}
	if pin.SingleBlock {
		t.Errorf("%d bytes exceeds the %d ceiling and must be flagged, because a multi-block "+
			"document gets a dag-pb root whose digest is not the document hash",
			len(big), SingleBlockMaxBytes)
	}
}
