package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/acresync/orchestrator/internal/config"
	"github.com/acresync/orchestrator/internal/store"
)

// operatorServer builds a server with the operator surface over a rolled-back transaction.
func operatorServer(t *testing.T) (*Server, context.Context, pgx.Tx) {
	t.Helper()

	ctx := context.Background()
	tx, err := apiPool(t).Begin(ctx)
	if err != nil {
		t.Fatalf("beginning a transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })

	srv := New(Deps{
		Env:           config.EnvLocal,
		Wall:          fixedClock{at: tokenNow},
		SessionSecret: testSecret,
		Schemes:       store.NewSchemes(tx),
		Outbox:        store.NewOutbox(tx),
	})
	return srv, ctx, tx
}

// outboxSeq keeps every seeded nonce and transaction hash distinct.
var outboxSeq atomic.Int64

// seedOutboxEntry inserts a chain queue entry.
//
// The constraints are real and coupled: CONFIRMED requires a tx hash, a block number, a confirmed_at and at
// least five confirmations, which is the reorg depth the system treats as final.
func seedOutboxEntry(t *testing.T, ctx context.Context, q store.Querier, schemeID, fn, status string, createdAt time.Time) string {
	t.Helper()

	confirmed := status == "CONFIRMED"
	needsTx := confirmed || status == "BROADCAST" || status == "PENDING" || status == "CONFIRMING"

	// CONFIRMING must name the block it was mined in, not just CONFIRMED: without it a confirmation count would
	// be recorded against no block and the reorg check would have nothing to compare against.
	needsBlock := confirmed || status == "CONFIRMING"

	// outbox_one_tx_per_nonce is a unique index, correctly: two transactions sharing a nonce is exactly the
	// double-send a nonce exists to prevent. So every seeded entry gets its own nonce and hash.
	seq := outboxSeq.Add(1)

	var txHash any
	if needsTx {
		txHash = fmt.Sprintf("0x%064x", seq)
	}

	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO chain_outbox (
			scheme_id, target_contract, function_name,
			payload_json, payload_hash, calldata, idempotency_key,
			nonce, tx_hash, status, confirmations, block_number,
			confirmed_at, environment_tag, created_at
		) VALUES (
			$1, '0xa656a42974b40cf64f32e758abb0689a2a178391', $2,
			'{}'::jsonb, $3, decode('00','hex'), $4,
			CASE WHEN $5 THEN $10::bigint ELSE NULL END,
			$6, $7::outbox_status,
			CASE WHEN $8 THEN 5 ELSE 0 END,
			CASE WHEN $11 THEN 1234567 ELSE NULL END,
			CASE WHEN $8 THEN $9::timestamptz ELSE NULL END,
			'SEPOLIA_SIM', $9::timestamptz
		) RETURNING id`,
		schemeID, fn, apiIdem("payload"), apiIdem("outbox"),
		needsTx, txHash, status, confirmed, createdAt, seq, needsBlock).Scan(&id)
	if err != nil {
		t.Fatalf("seeding a %s outbox entry: %v", status, err)
	}
	return id
}

// TestOutboxRequiresAnOperator covers the guard.
func TestOutboxRequiresAnOperator(t *testing.T) {
	srv, ctx, tx := operatorServer(t)
	investorID, _, _ := seedAPIInvestor(t, ctx, tx)

	t.Run("no token", func(t *testing.T) {
		rec := getAs(t, srv, "/v1/admin/outbox", "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("an investor token", func(t *testing.T) {
		rec := getAs(t, srv, "/v1/admin/outbox", investorToken(t, srv, investorID))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; an investor has no business in the chain queue", rec.Code)
		}
	})
}

// TestEveryOperatorRoleCanReadTheOutbox is a deliberate decision, so it is pinned.
//
// The queue is the operator's own infrastructure and holds no investor identity. A trustee asking why an approval
// has not landed needs the same view as the manager who queued it.
func TestEveryOperatorRoleCanReadTheOutbox(t *testing.T) {
	srv, ctx, tx := operatorServer(t)
	schemeID := seedAPIScheme(t, ctx, tx)
	seedOutboxEntry(t, ctx, tx, schemeID, "anchorBidbook", "CONFIRMED", tokenNow.Add(-time.Hour))

	for _, role := range AllRoles() {
		t.Run(string(role), func(t *testing.T) {
			rec := getAs(t, srv, "/v1/admin/outbox", tokenFor(t, srv, operatorClaims(role)))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s was refused: %d %s", role, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestOutboxServesTheContractsFields covers the payload.
func TestOutboxServesTheContractsFields(t *testing.T) {
	doc := loadContract(t)
	srv, ctx, tx := operatorServer(t)
	schemeID := seedAPIScheme(t, ctx, tx)
	seedOutboxEntry(t, ctx, tx, schemeID, "anchorBidbook", "CONFIRMED", tokenNow.Add(-time.Hour))

	rec := getAs(t, srv, "/v1/admin/outbox", tokenFor(t, srv, operatorClaims(RoleManager)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d. body: %s", rec.Code, rec.Body.String())
	}

	// Captured once. decodeBody drains rec.Body, so a second read would see an empty buffer and report the
	// response as not being JSON.
	raw := rec.Body.Bytes()

	decoded := decodeBody(t, rec)
	items, ok := decoded["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("items = %v, want the seeded entry", decoded["items"])
	}

	entry := items[0].(map[string]any)
	for _, field := range []string{"id", "functionName", "status", "environmentTag"} {
		if _, present := entry[field]; !present {
			t.Errorf("the contract requires %q and it is absent", field)
		}
	}
	if entry["functionName"] != "anchorBidbook" {
		t.Errorf("functionName = %v", entry["functionName"])
	}
	if entry["status"] != "CONFIRMED" {
		t.Errorf("status = %v", entry["status"])
	}
	if int64(entry["confirmations"].(float64)) != 5 {
		t.Errorf("confirmations = %v, want the 5 the schema demands of a CONFIRMED entry", entry["confirmations"])
	}

	// And it satisfies the published schema. Re-decoded with decodeJSON: decodeBody above uses plain
	// json.Unmarshal, which turns every number into a float64 and would defeat the validator's precision check.
	precise, ok := decodeJSON(t, raw).(map[string]any)
	if !ok {
		t.Fatal("the response is not an object")
	}
	preciseItems, ok := precise["items"].([]any)
	if !ok {
		t.Fatalf("items is %T", precise["items"])
	}

	v := &validator{doc: doc}
	for i, item := range preciseItems {
		v.check(t, "items["+itoa(i)+"]", item, schemaNamed(t, doc, "OutboxEntry"))
	}
	if len(v.violations) > 0 {
		sort.Strings(v.violations)
		t.Errorf("an outbox entry does not satisfy the contract:\n  %s\n\nbody: %s",
			strings.Join(v.violations, "\n  "), rec.Body.String())
	}
	if len(v.undeclared) > 0 {
		sort.Strings(v.undeclared)
		t.Errorf("an outbox entry carries fields the contract does not publish:\n  %s",
			strings.Join(v.undeclared, "\n  "))
	}
}

// TestOutboxNeverPublishesTheIdempotencyKeyOrCalldata is the reason for a separate query.
//
// The relayer's own reads select the payload, the calldata and the idempotency key because it needs them to sign
// and to rebuild after a reorg. Publishing the idempotency key would hand a caller the one input that lets them
// collide with a pending action; publishing calldata and payloads would put large blobs in every poll.
func TestOutboxNeverPublishesTheIdempotencyKeyOrCalldata(t *testing.T) {
	srv, ctx, tx := operatorServer(t)
	schemeID := seedAPIScheme(t, ctx, tx)
	seedOutboxEntry(t, ctx, tx, schemeID, "anchorBidbook", "QUEUED", tokenNow.Add(-time.Hour))

	body := strings.ToLower(getAs(t, srv, "/v1/admin/outbox", tokenFor(t, srv, operatorClaims(RoleManager))).Body.String())

	for _, term := range []string{"idempotency", "idemkey", "calldata", "payload", "payloadhash", "gasprice"} {
		if strings.Contains(body, term) {
			t.Errorf("the outbox response carries %q, which is the relayer's business and not a caller's: %s", term, body)
		}
	}
}

// TestAnUnsignedEntryHasANullNonce is the zero-versus-absent distinction.
//
// A nonce of zero is the first transaction an account ever sends. Reporting an unsigned entry that way would be a
// specific false statement rather than a vague one.
func TestAnUnsignedEntryHasANullNonce(t *testing.T) {
	srv, ctx, tx := operatorServer(t)
	schemeID := seedAPIScheme(t, ctx, tx)
	seedOutboxEntry(t, ctx, tx, schemeID, "anchorBidbook", "QUEUED", tokenNow.Add(-time.Hour))

	decoded := decodeBody(t, getAs(t, srv, "/v1/admin/outbox", tokenFor(t, srv, operatorClaims(RoleManager))))
	entry := decoded["items"].([]any)[0].(map[string]any)

	if v, present := entry["nonce"]; !present || v != nil {
		t.Errorf("nonce = %v, want null on an unsigned entry", v)
	}
	if v, present := entry["txHash"]; !present || v != nil {
		t.Errorf("txHash = %v, want null on an unsigned entry", v)
	}
	if v, present := entry["blockNumber"]; !present || v != nil {
		t.Errorf("blockNumber = %v, want null before inclusion", v)
	}
}

// TestOutboxIsNewestFirst matches what an operator is looking for.
func TestOutboxIsNewestFirst(t *testing.T) {
	srv, ctx, tx := operatorServer(t)
	schemeID := seedAPIScheme(t, ctx, tx)

	seedOutboxEntry(t, ctx, tx, schemeID, "oldest", "QUEUED", tokenNow.Add(-3*time.Hour))
	seedOutboxEntry(t, ctx, tx, schemeID, "newest", "QUEUED", tokenNow.Add(-time.Minute))
	seedOutboxEntry(t, ctx, tx, schemeID, "middle", "QUEUED", tokenNow.Add(-2*time.Hour))

	decoded := decodeBody(t, getAs(t, srv, "/v1/admin/outbox", tokenFor(t, srv, operatorClaims(RoleManager))))
	items := decoded["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("got %d entries, want 3", len(items))
	}

	want := []string{"newest", "middle", "oldest"}
	for i, expected := range want {
		got := items[i].(map[string]any)["functionName"]
		if got != expected {
			t.Errorf("position %d is %v, want %q", i, got, expected)
		}
	}
}

// TestOutboxFiltersAreValidatedNotIgnored stops a mistyped filter reading as a quiet queue.
func TestOutboxFiltersAreValidatedNotIgnored(t *testing.T) {
	srv, ctx, tx := operatorServer(t)
	schemeID := seedAPIScheme(t, ctx, tx)
	seedOutboxEntry(t, ctx, tx, schemeID, "anchorBidbook", "QUEUED", tokenNow)
	token := tokenFor(t, srv, operatorClaims(RoleManager))

	t.Run("a malformed schemeId is a 400", func(t *testing.T) {
		rec := getAs(t, srv, "/v1/admin/outbox?schemeId=not-a-uuid", token)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400. body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("an unknown status is a 400", func(t *testing.T) {
		rec := getAs(t, srv, "/v1/admin/outbox?status=INVENTED", token)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; an unknown status would otherwise return nothing and read as a quiet queue", rec.Code)
		}
	})

	t.Run("a known status filters", func(t *testing.T) {
		seedOutboxEntry(t, ctx, tx, schemeID, "confirmedOne", "CONFIRMED", tokenNow)

		rec := getAs(t, srv, "/v1/admin/outbox?status=CONFIRMED", token)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d. body: %s", rec.Code, rec.Body.String())
		}
		items := decodeBody(t, rec)["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("got %d entries, want only the CONFIRMED one", len(items))
		}
		if items[0].(map[string]any)["status"] != "CONFIRMED" {
			t.Errorf("the filter let through %v", items[0].(map[string]any)["status"])
		}
	})

	t.Run("a scheme filter isolates", func(t *testing.T) {
		other := seedAPIScheme(t, ctx, tx)
		seedOutboxEntry(t, ctx, tx, other, "otherScheme", "QUEUED", tokenNow)

		rec := getAs(t, srv, "/v1/admin/outbox?schemeId="+other, token)
		items := decodeBody(t, rec)["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("got %d entries, want only the other scheme's", len(items))
		}
		if items[0].(map[string]any)["functionName"] != "otherScheme" {
			t.Errorf("the scheme filter let through %v", items[0].(map[string]any)["functionName"])
		}
	})
}

// TestEmptyOutboxIsAnArray keeps a fresh deployment safe to render.
func TestEmptyOutboxIsAnArray(t *testing.T) {
	srv, _, _ := operatorServer(t)

	rec := getAs(t, srv, "/v1/admin/outbox", tokenFor(t, srv, operatorClaims(RoleCompliance)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d. body: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"items":null`) {
		t.Fatalf("an empty queue is null rather than []: %s", rec.Body.String())
	}
}

// TestEveryOutboxStatusRoundTrips is the guard against the Go list and the SQL enum drifting.
//
// store.AllOutboxStatuses is a hand-written list, because outbox.Status has no Valid method. A hand-written list
// is exactly the thing that goes stale, so every value is written into the column and read back.
func TestEveryOutboxStatusRoundTrips(t *testing.T) {
	srv, ctx, tx := operatorServer(t)
	schemeID := seedAPIScheme(t, ctx, tx)
	token := tokenFor(t, srv, operatorClaims(RoleManager))

	for _, status := range store.AllOutboxStatuses() {
		t.Run(string(status), func(t *testing.T) {
			// CANCELLED requires no tx hash and a cancelled_at, so it is seeded separately.
			if string(status) == "CANCELLED" || string(status) == "DEAD_LETTER" || string(status) == "REORGED" || string(status) == "FAILED" {
				seedTerminalOutboxEntry(t, ctx, tx, schemeID, "fn"+string(status), string(status))
			} else {
				seedOutboxEntry(t, ctx, tx, schemeID, "fn"+string(status), string(status), tokenNow)
			}

			rec := getAs(t, srv, "/v1/admin/outbox?status="+string(status), token)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %q does not round-trip: %d %s", status, rec.Code, rec.Body.String())
			}
			items := decodeBody(t, rec)["items"].([]any)
			if len(items) == 0 {
				t.Fatalf("status %q returned nothing, so the Go list and the SQL enum disagree", status)
			}
		})
	}
}

// seedTerminalOutboxEntry inserts an entry in one of the off-path statuses.
func seedTerminalOutboxEntry(t *testing.T, ctx context.Context, q store.Querier, schemeID, fn, status string) string {
	t.Helper()

	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO chain_outbox (
			scheme_id, target_contract, function_name,
			payload_json, payload_hash, calldata, idempotency_key,
			status, last_error, cancelled_at, dead_lettered_at, environment_tag
		) VALUES (
			$1, '0xa656a42974b40cf64f32e758abb0689a2a178391', $2,
			'{}'::jsonb, $3, decode('00','hex'), $4,
			$5::outbox_status,
			'the relayer reported a revert',
			CASE WHEN $5 = 'CANCELLED' THEN now() ELSE NULL END,
			CASE WHEN $5 = 'DEAD_LETTER' THEN now() ELSE NULL END,
			'SEPOLIA_SIM'
		) RETURNING id`,
		schemeID, fn, apiIdem("payload"), apiIdem("outbox"), status).Scan(&id)
	if err != nil {
		t.Fatalf("seeding a %s outbox entry: %v", status, err)
	}
	return id
}

// TestOutboxRouteIsAbsentWithoutItsStore mirrors the other gated surfaces.
func TestOutboxRouteIsAbsentWithoutItsStore(t *testing.T) {
	srv := New(Deps{Env: config.EnvLocal, Wall: fixedClock{at: tokenNow}, SessionSecret: testSecret})

	rec := getAs(t, srv, "/v1/admin/outbox", tokenFor(t, srv, operatorClaims(RoleManager)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 with no outbox store", rec.Code)
	}
}

// itoa avoids importing strconv for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
