package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/acresync/orchestrator/internal/asba"
	"github.com/acresync/orchestrator/internal/clock"
	"github.com/acresync/orchestrator/internal/config"
	"github.com/acresync/orchestrator/internal/ipfs"
	"github.com/acresync/orchestrator/internal/merkle"
	"github.com/acresync/orchestrator/internal/store"
)

// The write-path harness.
//
// Every mutation test runs against real Postgres inside a transaction that is rolled back. The server is
// handed that transaction as its TxBeginner, so each request's own transaction is a savepoint inside it: the
// request commits exactly as it would in production, and the test still leaves nothing behind.

// businessNow is where the simulated calendar stands for these tests: inside the fixture offer's bidding
// window, which opens on 1 July and closes on 4 July.
var businessNow = time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

// fakeChain is a chain whose head and block hashes the test sets.
type fakeChain struct {
	head   uint64
	hashes map[uint64]merkle.Hash
}

func (f *fakeChain) Head(context.Context) (uint64, error) { return f.head, nil }

func (f *fakeChain) BlockHash(_ context.Context, n uint64) (merkle.Hash, error) {
	return f.hashes[n], nil
}

type writeHarness struct {
	srv   *Server
	ctx   context.Context
	tx    pgx.Tx
	clock *movableClock
	chain *fakeChain
	bank  *asba.Sandbox
}

// movableClock is a business clock a test can advance.
type movableClock struct{ at time.Time }

func (m *movableClock) Now(context.Context) (time.Time, error) { return m.at, nil }

func newWriteHarness(t *testing.T) *writeHarness {
	t.Helper()

	ctx := context.Background()
	tx, err := apiPool(t).Begin(ctx)
	if err != nil {
		t.Fatalf("beginning a transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })

	h := &writeHarness{ctx: ctx, tx: tx, clock: &movableClock{at: businessNow},
		chain: &fakeChain{hashes: map[uint64]merkle.Hash{}}}
	h.bank = asba.NewSandbox(h.clock)

	pinDir := t.TempDir()
	provider, err := ipfs.NewMockProvider(pinDir)
	if err != nil {
		t.Fatal(err)
	}

	h.srv = New(Deps{
		Env:           config.EnvLocal,
		Wall:          fixedClock{at: tokenNow},
		SessionSecret: testSecret,
		Schemes:       store.NewSchemes(tx),
		Offers:        store.NewOffers(tx),
		Periods:       store.NewPeriods(tx),
		Me:            store.NewMe(tx),
		Outbox:        store.NewOutbox(tx),
		DB:            tx,
		BusinessClock: func(string) clock.Business { return h.clock },
		Chain:         h.chain,
		ASBA:          h.bank,
		Publisher:     ipfs.NewPublisher(provider),
		SeedPepper:    []byte("test pepper that never leaves this process"),
	})
	return h
}

// send issues a request with a token, an idempotency key and a body.
func (h *writeHarness) send(t *testing.T, method, path, token, idemKey string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var raw []byte
	switch v := body.(type) {
	case nil:
	case string:
		raw = []byte(v)
	default:
		var err error
		if raw, err = json.Marshal(v); err != nil {
			t.Fatal(err)
		}
	}

	req := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
	if len(raw) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idemKey != "" {
		req.Header.Set(idempotencyKeyHeader, idemKey)
	}
	rec := httptest.NewRecorder()
	chain(h.srv, withRequestID).ServeHTTP(rec, req)
	return rec
}

func (h *writeHarness) operator(t *testing.T, role Role) string {
	t.Helper()
	return tokenFor(t, h.srv, operatorClaims(role))
}

func (h *writeHarness) investor(t *testing.T, investorID string) string {
	t.Helper()
	return investorToken(t, h.srv, investorID)
}

// idem makes a distinct Idempotency-Key within the contract's 16..128 bounds.
func idem(label string) string {
	return "test-" + label + "-" + apiUniq("k")
}

// expect fails the test unless the response has the given status.
func expect(t *testing.T, rec *httptest.ResponseRecorder, status int) map[string]any {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d. body: %s", rec.Code, status, rec.Body.String())
	}
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("the body is not a JSON object: %v\n%s", err, rec.Body.String())
		}
	}
	return out
}

// expectCode fails the test unless the response is an error with the given status and contract code.
func expectCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) string {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d. body: %s", rec.Code, status, rec.Body.String())
	}
	env := decodeError(t, rec.Result())
	if env.Error.Code != code {
		t.Fatalf("code = %q, want %q. message: %s", env.Error.Code, code, env.Error.Message)
	}
	return env.Error.Message
}

// fixtureTerms are valid terms for the fixture scheme: 475 public units, a 200-holder floor, a 25-unit cap.
func fixtureTerms(opens time.Time) map[string]any {
	return map[string]any{
		"unitsOnOffer":         475,
		"minBidUnits":          1,
		"maxBidUnits":          25,
		"minSubscriptionUnits": 428,
		"minDistinctHolders":   200,
		"priceBandLowerPaise":  100000000,
		"priceBandUpperPaise":  105000000,
		"opensAt":              opens.Format(time.RFC3339),
		"closesAt":             opens.Add(72 * time.Hour).Format(time.RFC3339),
		"allotmentDueAt":       opens.Add(96 * time.Hour).Format(time.RFC3339),
	}
}

// seedBidder creates an investor who may bid on a scheme: verified KYC, an anchor, and accounts.
func seedBidder(t *testing.T, h *writeHarness, schemeID string) (investorID, dematID, bankID string) {
	t.Helper()

	investorID, dematID, bankID = seedAPIInvestor(t, h.ctx, h.tx)
	seedAPIKYC(t, h.ctx, h.tx, investorID, "VERIFIED")

	anchor := sha256.Sum256([]byte("anchor:" + investorID))
	var id string
	if err := h.tx.QueryRow(h.ctx, `
		INSERT INTO investor_anchors (investor_id, scheme_id, anchor_hash, pepper_key_id)
		VALUES ($1, $2, $3, 'test-pepper') RETURNING id`,
		investorID, schemeID, anchor[:]).Scan(&id); err != nil {
		t.Fatalf("seeding an investor anchor: %v", err)
	}
	seedWallet(t, h, investorID)
	return investorID, dematID, bankID
}

// seedWallet gives an investor an active wallet, the register address settlement credits.
func seedWallet(t *testing.T, h *writeHarness, investorID string) string {
	t.Helper()
	sum := sha256.Sum256([]byte("wallet:" + investorID))
	addr := "0x" + hex.EncodeToString(sum[:20])
	if _, err := h.tx.Exec(h.ctx, `INSERT INTO wallets (investor_id, address) VALUES ($1, $2)`, investorID, addr); err != nil {
		t.Fatalf("seeding a wallet: %v", err)
	}
	return addr
}

// count runs a count query in the harness transaction.
func (h *writeHarness) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := h.tx.QueryRow(h.ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	return n
}

func (h *writeHarness) offerStatus(t *testing.T, offerID string) string {
	t.Helper()
	var s string
	if err := h.tx.QueryRow(h.ctx, `SELECT status::text FROM offers WHERE id = $1`, offerID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// statusRecorder doubles here only to keep http imported for helpers above.
var _ = http.StatusOK
