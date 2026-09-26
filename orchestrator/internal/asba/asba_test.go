package asba

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/acresync/orchestrator/internal/money"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now(context.Context) (time.Time, error) { return c.t, nil }

var at = time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)

const (
	schemeUUID = "6f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8"
	bidUUID    = "0123abcd-4567-89ef-0123-456789abcdef"
	acctRef    = "acct-investor-1"
	mandateRef = "mandate-1"
)

// unitPrice is ₹10 lakh, so a 3-unit bid blocks ₹30 lakh.
const unitPrice = money.Paise(100_000_000)

func newMock(t *testing.T) *Mock {
	t.Helper()
	return NewMock(fixedClock{at})
}

func blockReq(key string, units int64) BlockRequest {
	return BlockRequest{
		IdempotencyKey: key,
		BidID:          bidUUID,
		BankAccountRef: acctRef,
		AmountPaise:    money.Paise(units) * unitPrice,
		MandateRef:     mandateRef,
	}
}

// ---------------------------------------------------------------------------
// The distinction the package exists to hold
// ---------------------------------------------------------------------------

// TestBlockingDoesNotMoveMoney is the property ASBA is chosen for.
//
// The investor's balance is unchanged after a block. Only the available portion drops, because the bank
// has marked funds unavailable rather than transferring them. If this were not true, abandoning an offer
// would mean refunding hundreds of investors rather than releasing a hold.
func TestBlockingDoesNotMoveMoney(t *testing.T) {
	m := newMock(t)
	m.Fund(acctRef, money.Paise(50_000_000_00), mandateRef) // ₹50 lakh
	before := m.Available(acctRef)

	b, err := m.RequestBlock(context.Background(), blockReq("k1", 3))
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != StatusBlocked {
		t.Fatalf("status = %s, want BLOCKED", b.Status)
	}

	// Held rises by exactly the bid amount.
	if got := m.Held(acctRef); got != b.BlockedPaise {
		t.Errorf("held = %d, want %d", got, b.BlockedPaise)
	}
	// Available falls by the same.
	if got := m.Available(acctRef); got != before-b.BlockedPaise {
		t.Errorf("available = %d, want %d", got, before-b.BlockedPaise)
	}
	// Nothing was debited.
	if b.DebitedPaise != 0 {
		t.Errorf("debited = %d; a block must not move money", b.DebitedPaise)
	}
	if b.Status.MoneyMoved() {
		t.Error("MoneyMoved must be false for a block")
	}
	if !b.Status.HoldsFunds() {
		t.Error("HoldsFunds must be true for a block")
	}
}

// TestReleasingABlockCostsNothing is why an abort is cheap right up to settlement.
func TestReleasingABlockCostsNothing(t *testing.T) {
	m := newMock(t)
	opening := money.Paise(50_000_000_00)
	m.Fund(acctRef, opening, mandateRef)

	b, err := m.RequestBlock(context.Background(), blockReq("k1", 3))
	if err != nil {
		t.Fatal(err)
	}

	released, err := m.Release(context.Background(), "rk1", b.BankRef)
	if err != nil {
		t.Fatal(err)
	}

	if released.Status != StatusUnblocked {
		t.Fatalf("status = %s, want UNBLOCKED", released.Status)
	}
	if released.DebitedPaise != 0 {
		t.Errorf("debited = %d, want 0", released.DebitedPaise)
	}
	if released.ReleasedPaise != b.BlockedPaise {
		t.Errorf("released %d, blocked %d", released.ReleasedPaise, b.BlockedPaise)
	}

	// The investor is exactly where they started.
	if got := m.Available(acctRef); got != opening {
		t.Errorf("available = %d, want the original %d; releasing a block must leave the investor "+
			"whole", got, opening)
	}
	if got := m.Held(acctRef); got != 0 {
		t.Errorf("held = %d, want 0", got)
	}
	if err := released.Reconcile(); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// The reconciliation identity
// ---------------------------------------------------------------------------

// TestPartialAllotmentDebitsAndReleasesExactly is the arithmetic every bid depends on.
//
//	debited + released == blocked
//
// with no tolerance. A shortfall leaves an investor's money frozen with no claim against it; an excess
// releases more than was held.
func TestPartialAllotmentDebitsAndReleasesExactly(t *testing.T) {
	m := newMock(t)
	m.Fund(acctRef, money.Paise(50_000_000_00), mandateRef)
	ctx := context.Background()

	// Bid for 5 units, allotted 2.
	b, err := m.RequestBlock(ctx, blockReq("k1", 5))
	if err != nil {
		t.Fatal(err)
	}

	debit := money.Paise(2) * unitPrice
	settled, err := m.Settle(ctx, SettleRequest{
		IdempotencyKey: "sk1", BankRef: b.BankRef, DebitPaise: debit,
	})
	if err != nil {
		t.Fatal(err)
	}

	if settled.Status != StatusDebited {
		t.Fatalf("status = %s, want DEBITED", settled.Status)
	}
	if settled.DebitedPaise != debit {
		t.Errorf("debited = %d, want %d", settled.DebitedPaise, debit)
	}
	if want := b.BlockedPaise - debit; settled.ReleasedPaise != want {
		t.Errorf("released = %d, want %d", settled.ReleasedPaise, want)
	}
	if err := settled.Reconcile(); err != nil {
		t.Fatal(err)
	}

	// The account lost exactly the debited amount and nothing stays held.
	if got := m.Held(acctRef); got != 0 {
		t.Errorf("held = %d after settlement, want 0", got)
	}
	if got := m.Available(acctRef); got != money.Paise(50_000_000_00)-debit {
		t.Errorf("available = %d, want the opening balance less %d", got, debit)
	}
}

// TestReconcileRejectsEveryBrokenShape covers the ways a block record can lie.
func TestReconcileRejectsEveryBrokenShape(t *testing.T) {
	blocked := money.Paise(3) * unitPrice
	ts := at

	cases := map[string]*Block{
		"partial block": {
			BidID: "b", Status: StatusBlocked, BlockedAt: &ts,
			RequestedPaise: blocked, BlockedPaise: blocked - 1,
		},
		"blocked but already debited": {
			BidID: "b", Status: StatusBlocked, BlockedAt: &ts,
			RequestedPaise: blocked, BlockedPaise: blocked, DebitedPaise: 1,
		},
		"blocked with no timestamp": {
			BidID: "b", Status: StatusBlocked,
			RequestedPaise: blocked, BlockedPaise: blocked,
		},
		"debit exceeds block": {
			BidID: "b", Status: StatusDebited,
			RequestedPaise: blocked, BlockedPaise: blocked,
			DebitedPaise: blocked + 1, ReleasedPaise: 0,
		},
		"halves do not sum": {
			BidID: "b", Status: StatusDebited,
			RequestedPaise: blocked, BlockedPaise: blocked,
			DebitedPaise: 1, ReleasedPaise: 1,
		},
		"unblocked but debited": {
			BidID: "b", Status: StatusUnblocked,
			RequestedPaise: blocked, BlockedPaise: blocked,
			DebitedPaise: 1, ReleasedPaise: blocked - 1,
		},
		"debited but took nothing": {
			BidID: "b", Status: StatusDebited,
			RequestedPaise: blocked, BlockedPaise: blocked,
			DebitedPaise: 0, ReleasedPaise: blocked,
		},
		"failed but records movement": {
			BidID: "b", Status: StatusFailed, FailureCode: "x",
			RequestedPaise: blocked, BlockedPaise: blocked,
		},
		"failed with no code": {
			BidID: "b", Status: StatusFailed, RequestedPaise: blocked,
		},
		"requested but records movement": {
			BidID: "b", Status: StatusRequested,
			RequestedPaise: blocked, DebitedPaise: 1,
		},
	}

	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			if err := b.Reconcile(); err == nil {
				t.Fatalf("%+v should not reconcile", b)
			}
		})
	}
}

// TestReconcileAllCatchesOffsettingErrors is why per-block checks exist alongside the aggregate.
//
// Two investors wrong in opposite directions produce totals that balance perfectly. Only the per-block
// pass finds them.
func TestReconcileAllCatchesOffsettingErrors(t *testing.T) {
	blocked := money.Paise(3) * unitPrice

	good := &Block{
		BidID: "ok", Status: StatusDebited,
		RequestedPaise: blocked, BlockedPaise: blocked,
		DebitedPaise: blocked, ReleasedPaise: 0,
	}
	// Over-released by one paise.
	overA := &Block{
		BidID: "a", Status: StatusDebited,
		RequestedPaise: blocked, BlockedPaise: blocked,
		DebitedPaise: blocked - 2, ReleasedPaise: 3,
	}
	// Under-released by one paise, so the totals cancel out.
	underB := &Block{
		BidID: "b", Status: StatusDebited,
		RequestedPaise: blocked, BlockedPaise: blocked,
		DebitedPaise: 2, ReleasedPaise: blocked - 3,
	}

	sumDebited := overA.DebitedPaise + underB.DebitedPaise + good.DebitedPaise
	sumReleased := overA.ReleasedPaise + underB.ReleasedPaise + good.ReleasedPaise
	if sumDebited+sumReleased != 3*blocked {
		t.Fatalf("the fixture must have offsetting errors that balance in aggregate, got %d against %d",
			sumDebited+sumReleased, 3*blocked)
	}

	if _, _, _, err := ReconcileAll([]*Block{good, overA, underB}); !errors.Is(err, ErrNotReconciled) {
		t.Fatalf("offsetting per-block errors must still be caught, got %v", err)
	}
}

func TestReconcileAllOnAHealthySet(t *testing.T) {
	m := newMock(t)
	ctx := context.Background()

	var blocks []*Block
	for i := 0; i < 5; i++ {
		acct := fmt.Sprintf("acct-%d", i)
		m.Fund(acct, money.Paise(50_000_000_00), "mandate")

		b, err := m.RequestBlock(ctx, BlockRequest{
			IdempotencyKey: fmt.Sprintf("k%d", i),
			BidID:          fmt.Sprintf("bid-%d", i),
			BankAccountRef: acct,
			AmountPaise:    money.Paise(int64(i+1)) * unitPrice,
			MandateRef:     "mandate",
		})
		if err != nil {
			t.Fatal(err)
		}

		// Allot half, rounded down, so several are partial and one wins nothing.
		debit := money.Paise(int64(i/2)) * unitPrice
		s, err := m.Settle(ctx, SettleRequest{
			IdempotencyKey: fmt.Sprintf("s%d", i), BankRef: b.BankRef, DebitPaise: debit,
		})
		if err != nil {
			t.Fatal(err)
		}
		blocks = append(blocks, s)
	}

	blocked, debited, released, err := ReconcileAll(blocks)
	if err != nil {
		t.Fatal(err)
	}
	if debited+released != blocked {
		t.Fatalf("aggregate broken: %d + %d != %d", debited, released, blocked)
	}
	if blocked == 0 {
		t.Fatal("the fixture blocked nothing")
	}
}

// ---------------------------------------------------------------------------
// All or nothing
// ---------------------------------------------------------------------------

// TestInsufficientFundsFailsRatherThanPartiallyBlocking mirrors the schema constraint.
//
// A partial block is not a smaller valid bid. The bid is for a specific number of units at a specific
// price, so reserving less leaves it unpayable by exactly the shortfall, and admitting it to the book
// defers the failure to debit time, after the cap table is anchored.
func TestInsufficientFundsFailsRatherThanPartiallyBlocking(t *testing.T) {
	m := newMock(t)
	// Enough for two units, bidding for three.
	m.Fund(acctRef, money.Paise(2)*unitPrice, mandateRef)

	b, err := m.RequestBlock(context.Background(), blockReq("k1", 3))
	if err != nil {
		t.Fatal(err)
	}

	if b.Status != StatusFailed {
		t.Fatalf("status = %s, want FAILED", b.Status)
	}
	if b.FailureCode != "insufficient_funds" {
		t.Errorf("failure code = %q", b.FailureCode)
	}
	if b.BlockedPaise != 0 {
		t.Errorf("blocked %d; a partial block must never be recorded", b.BlockedPaise)
	}
	if b.Status.HoldsFunds() {
		t.Error("a failed block holds no funds")
	}
	if got := m.Held(acctRef); got != 0 {
		t.Errorf("held = %d, want 0", got)
	}
	if err := b.Reconcile(); err != nil {
		t.Fatal(err)
	}
}

// TestSecondBlockCannotReserveAlreadyHeldMoney covers an investor backing two bids with one balance.
func TestSecondBlockCannotReserveAlreadyHeldMoney(t *testing.T) {
	m := newMock(t)
	m.Fund(acctRef, money.Paise(3)*unitPrice, mandateRef)
	ctx := context.Background()

	first, err := m.RequestBlock(ctx, blockReq("k1", 2))
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != StatusBlocked {
		t.Fatalf("first block: %s", first.Status)
	}

	second := blockReq("k2", 2)
	second.BidID = "another-bid"
	got, err := m.RequestBlock(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFailed {
		t.Fatalf("the second block should fail: only one unit of headroom remains, got %s", got.Status)
	}
}

func TestMandateIsRequired(t *testing.T) {
	m := newMock(t)
	m.Fund(acctRef, money.Paise(50_000_000_00), mandateRef)

	t.Run("wrong mandate", func(t *testing.T) {
		r := blockReq("k1", 1)
		r.MandateRef = "not-the-mandate"
		b, err := m.RequestBlock(context.Background(), r)
		if err != nil {
			t.Fatal(err)
		}
		if b.Status != StatusFailed || b.FailureCode != "mandate_not_found" {
			t.Fatalf("status %s code %q; a bank will not freeze funds without an authorised instruction",
				b.Status, b.FailureCode)
		}
	})

	t.Run("revoked mandate", func(t *testing.T) {
		m2 := newMock(t)
		m2.Fund(acctRef, money.Paise(50_000_000_00), mandateRef)
		m2.RevokeMandate(acctRef)

		b, err := m2.RequestBlock(context.Background(), blockReq("k1", 1))
		if err != nil {
			t.Fatal(err)
		}
		if b.Status != StatusFailed {
			t.Fatalf("status = %s, want FAILED", b.Status)
		}
	})
}

// ---------------------------------------------------------------------------
// Idempotency
// ---------------------------------------------------------------------------

// TestRetryAfterTransportFailureDoesNotDoubleBlock is the money-critical case.
//
// Less damaging than a double payment because nothing moves, but it freezes twice the bid amount and the
// investor discovers it as an unexplained shortfall in their available balance.
func TestRetryAfterTransportFailureDoesNotDoubleBlock(t *testing.T) {
	m := newMock(t)
	m.Fund(acctRef, money.Paise(50_000_000_00), mandateRef)
	ctx := context.Background()

	req := blockReq("k-transport", 3)

	first, err := m.RequestBlock(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	heldAfterFirst := m.Held(acctRef)

	// The caller saw a timeout and retries blind with the identical request.
	second, err := m.RequestBlock(ctx, req)
	if err != nil {
		t.Fatalf("a retry with the same key must succeed: %v", err)
	}

	if second.BankRef != first.BankRef {
		t.Fatalf("the retry created a second block: %s then %s", first.BankRef, second.BankRef)
	}
	if m.Count() != 1 {
		t.Fatalf("%d blocks exist, want 1", m.Count())
	}
	if got := m.Held(acctRef); got != heldAfterFirst {
		t.Fatalf("held rose from %d to %d; the investor's money was frozen twice",
			heldAfterFirst, got)
	}
}

// TestIdempotencyResolvedBeforeInjectedFailure covers the ordering that makes the above work.
func TestIdempotencyResolvedBeforeInjectedFailure(t *testing.T) {
	m := newMock(t)
	m.Fund(acctRef, money.Paise(50_000_000_00), mandateRef)
	ctx := context.Background()
	req := blockReq("k-order", 2)

	first, err := m.RequestBlock(ctx, req)
	if err != nil {
		t.Fatal(err)
	}

	m.FailNextBlock(errors.New("connection reset"))
	again, err := m.RequestBlock(ctx, req)
	if err != nil {
		t.Fatalf("an existing block must be returned even when the next call is armed to fail: %v", err)
	}
	if again.BankRef != first.BankRef {
		t.Fatal("the retry must return the original block")
	}
}

func TestReusedKeyWithDifferentAmountRejected(t *testing.T) {
	m := newMock(t)
	m.Fund(acctRef, money.Paise(50_000_000_00), mandateRef)
	ctx := context.Background()

	if _, err := m.RequestBlock(ctx, blockReq("k-reuse", 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RequestBlock(ctx, blockReq("k-reuse", 3)); !errors.Is(err, ErrIdempotencyReuse) {
		t.Fatalf("want ErrIdempotencyReuse, got %v", err)
	}

	other := blockReq("k-reuse", 2)
	other.BidID = "different-bid"
	if _, err := m.RequestBlock(ctx, other); !errors.Is(err, ErrIdempotencyReuse) {
		t.Fatalf("a different bid under the same key must be rejected, got %v", err)
	}
}

// TestSettleIsRetryable covers a lost settlement response.
func TestSettleIsRetryable(t *testing.T) {
	m := newMock(t)
	m.Fund(acctRef, money.Paise(50_000_000_00), mandateRef)
	ctx := context.Background()

	b, err := m.RequestBlock(ctx, blockReq("k1", 4))
	if err != nil {
		t.Fatal(err)
	}

	debit := money.Paise(2) * unitPrice
	req := SettleRequest{IdempotencyKey: "sk1", BankRef: b.BankRef, DebitPaise: debit}

	first, err := m.Settle(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	balanceAfter := m.Available(acctRef)

	second, err := m.Settle(ctx, req)
	if err != nil {
		t.Fatalf("a settlement retry must be safe: %v", err)
	}
	if second.DebitedPaise != first.DebitedPaise {
		t.Errorf("debited changed on retry: %d then %d", first.DebitedPaise, second.DebitedPaise)
	}
	if got := m.Available(acctRef); got != balanceAfter {
		t.Fatalf("the retry debited again: balance %d then %d", balanceAfter, got)
	}
}

func TestDeriveKeysAreDistinctAndStable(t *testing.T) {
	blockKey, err := DeriveBlockKey(schemeUUID, bidUUID, money.Paise(300_000_000))
	if err != nil {
		t.Fatal(err)
	}
	settleKey, err := DeriveSettleKey(schemeUUID, bidUUID, money.Paise(200_000_000))
	if err != nil {
		t.Fatal(err)
	}
	if blockKey == settleKey {
		t.Fatal("a block and a settlement on the same bid must not share a key")
	}

	again, err := DeriveBlockKey(schemeUUID, bidUUID, money.Paise(300_000_000))
	if err != nil {
		t.Fatal(err)
	}
	if again != blockKey {
		t.Fatal("keys must be deterministic")
	}

	// A corrected allotment must change the settlement key, or the provider would return the stale
	// instruction and the wrong amount would stand.
	corrected, err := DeriveSettleKey(schemeUUID, bidUUID, money.Paise(100_000_000))
	if err != nil {
		t.Fatal(err)
	}
	if corrected == settleKey {
		t.Fatal("changing the debit amount must change the key")
	}
}

// ---------------------------------------------------------------------------
// The state graph
// ---------------------------------------------------------------------------

func TestStatusEnumMatchesPostgres(t *testing.T) {
	pg := []string{"REQUESTED", "BLOCKED", "DEBITED", "UNBLOCKED", "FAILED"}
	for _, n := range pg {
		if !BlockStatus(n).Valid() {
			t.Errorf("%s is in the asba_block_status enum but unmapped here", n)
		}
	}
	if len(AllStatuses()) != len(pg) {
		t.Errorf("AllStatuses has %d entries, the enum has %d", len(AllStatuses()), len(pg))
	}
}

func TestTerminalStatesAreTerminal(t *testing.T) {
	for _, s := range []BlockStatus{StatusDebited, StatusUnblocked, StatusFailed} {
		if !s.IsTerminal() {
			t.Errorf("%s should be terminal", s)
		}
		for _, to := range AllStatuses() {
			if s == to {
				continue
			}
			if s.CanTransitionTo(to) {
				t.Errorf("%s must not transition to %s: the bank has acted", s, to)
			}
		}
	}
}

func TestFailedBlockIsNotRevivable(t *testing.T) {
	// A retry is a new request with its own key, not a revival. Allowing REQUESTED again from FAILED
	// would make the idempotency key's meaning ambiguous.
	if StatusFailed.CanTransitionTo(StatusRequested) || StatusFailed.CanTransitionTo(StatusBlocked) {
		t.Fatal("a failed block must be terminal")
	}
}

func TestOnlyBlockedHoldsFunds(t *testing.T) {
	for _, s := range AllStatuses() {
		if got, want := s.HoldsFunds(), s == StatusBlocked; got != want {
			t.Errorf("%s.HoldsFunds() = %v, want %v", s, got, want)
		}
	}
}

func TestOnlyDebitedMovedMoney(t *testing.T) {
	for _, s := range AllStatuses() {
		if got, want := s.MoneyMoved(), s == StatusDebited; got != want {
			t.Errorf("%s.MoneyMoved() = %v, want %v", s, got, want)
		}
	}
}

func TestCheckTransition(t *testing.T) {
	if err := CheckTransition(StatusRequested, StatusBlocked); err != nil {
		t.Error(err)
	}
	if err := CheckTransition(StatusRequested, StatusDebited); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("a block cannot be debited before it exists, got %v", err)
	}
	if err := CheckTransition(StatusBlocked, StatusBlocked); err != nil {
		t.Errorf("restating a status should be a no-op, got %v", err)
	}
	if err := CheckTransition(BlockStatus("HELD"), StatusBlocked); !errors.Is(err, ErrUnknownStatus) {
		t.Errorf("want ErrUnknownStatus, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Validation and errors
// ---------------------------------------------------------------------------

func TestBlockRequestValidation(t *testing.T) {
	cases := map[string]func(*BlockRequest){
		"no idempotency key": func(r *BlockRequest) { r.IdempotencyKey = "" },
		"no bid":             func(r *BlockRequest) { r.BidID = "" },
		"no account":         func(r *BlockRequest) { r.BankAccountRef = "" },
		"no mandate":         func(r *BlockRequest) { r.MandateRef = "" },
		"zero amount":        func(r *BlockRequest) { r.AmountPaise = 0 },
		"negative amount":    func(r *BlockRequest) { r.AmountPaise = -1 },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := blockReq("k", 1)
			mutate(&r)
			if err := r.Validate(); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("want ErrInvalidRequest, got %v", err)
			}
		})
	}

	if err := blockReq("k", 1).Validate(); err != nil {
		t.Fatalf("the fixture must be valid or every case above is vacuous: %v", err)
	}
}

func TestSettleRefusesToDebitMoreThanBlocked(t *testing.T) {
	m := newMock(t)
	m.Fund(acctRef, money.Paise(50_000_000_00), mandateRef)
	ctx := context.Background()

	b, err := m.RequestBlock(ctx, blockReq("k1", 2))
	if err != nil {
		t.Fatal(err)
	}

	_, err = m.Settle(ctx, SettleRequest{
		IdempotencyKey: "sk1", BankRef: b.BankRef, DebitPaise: b.BlockedPaise + 1,
	})
	if !errors.Is(err, ErrDebitExceedsBlock) {
		t.Fatalf("want ErrDebitExceedsBlock, got %v", err)
	}
}

func TestSettleRequiresABlockedBlock(t *testing.T) {
	m := newMock(t)
	m.Fund(acctRef, money.Paise(1), mandateRef) // too little, so the block fails
	ctx := context.Background()

	b, err := m.RequestBlock(ctx, blockReq("k1", 3))
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != StatusFailed {
		t.Fatalf("expected a failed block, got %s", b.Status)
	}

	_, err = m.Settle(ctx, SettleRequest{IdempotencyKey: "sk1", BankRef: b.BankRef, DebitPaise: 0})
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("settling a failed block must be refused, got %v", err)
	}
}

func TestFetchUnknownBlock(t *testing.T) {
	m := newMock(t)
	if _, err := m.Fetch(context.Background(), "ASBA-nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestFetchExistsBecauseNotificationsAreNotGuaranteed covers the polling path.
func TestFetchReadsBackTheCurrentState(t *testing.T) {
	m := newMock(t)
	m.Fund(acctRef, money.Paise(50_000_000_00), mandateRef)
	ctx := context.Background()

	b, err := m.RequestBlock(ctx, blockReq("k1", 2))
	if err != nil {
		t.Fatal(err)
	}

	got, err := m.Fetch(ctx, b.BankRef)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusBlocked || got.BlockedPaise != b.BlockedPaise {
		t.Errorf("fetched %+v", got)
	}

	m.FailNextFetch(errors.New("gateway timeout"))
	if _, err := m.Fetch(ctx, b.BankRef); err == nil {
		t.Error("an armed fetch failure should surface")
	}
	if _, err := m.Fetch(ctx, b.BankRef); err != nil {
		t.Errorf("the following fetch must recover: %v", err)
	}
}

func TestRefusalIsDistinctFromTransportFailure(t *testing.T) {
	m := newMock(t)
	m.Fund(acctRef, money.Paise(50_000_000_00), mandateRef)
	ctx := context.Background()

	// A refusal is an answer: it produces a FAILED block with a code and no error.
	m.RefuseNextBlock("account_frozen")
	b, err := m.RequestBlock(ctx, blockReq("k1", 1))
	if err != nil {
		t.Fatalf("a refusal is not an error: %v", err)
	}
	if b.Status != StatusFailed || b.FailureCode != "account_frozen" {
		t.Fatalf("status %s code %q", b.Status, b.FailureCode)
	}

	// A transport failure leaves the outcome unknown and surfaces as an error.
	m.FailNextBlock(errors.New("connection reset"))
	if _, err := m.RequestBlock(ctx, blockReq("k2", 1)); err == nil {
		t.Fatal("a transport failure must surface as an error, since the bank may have acted")
	}
}

func TestContextCancellation(t *testing.T) {
	m := newMock(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := m.RequestBlock(ctx, blockReq("k", 1)); !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
}

func TestErrorMessagesNameTheConsequence(t *testing.T) {
	// The mandate message should explain why a bank refuses, not just that it did.
	r := blockReq("k", 1)
	r.MandateRef = ""
	err := r.Validate()
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "the investor authorised") {
		t.Errorf("the mandate error should explain the requirement, got: %v", err)
	}
}
