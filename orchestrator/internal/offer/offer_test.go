package offer

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/acresync/orchestrator/internal/ballot"
	"github.com/acresync/orchestrator/internal/merkle"
	"github.com/acresync/orchestrator/internal/money"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

var (
	opensAt  = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	closesAt = time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	dueAt    = time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	nowOpen  = time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
)

func hash(b byte) merkle.Hash {
	var h merkle.Hash
	for i := range h {
		h[i] = b
	}
	return h
}

func confirmed() *AnchorRef {
	return &AnchorRef{OutboxID: "ob-1", Status: "CONFIRMED", Confirmations: MinAnchorConfirmations}
}

// v1Terms mirrors the locked first scheme: 475 public units, 200 holder floor.
func v1Terms() Terms {
	t := TermsFromBallotParams(ballot.V1Params())
	t.OpensAt, t.ClosesAt, t.AllotmentDueAt = opensAt, closesAt, dueAt
	return t
}

// ready is evidence for an offer that has satisfied everything up to settlement.
func ready() Evidence {
	return Evidence{
		Terms: v1Terms(),
		Now:   nowOpen,

		Book: BookState{
			BidCount: 900, InBookCount: 880, FundsBlockedCount: 880,
			UnitsBid: 1760, DistinctBidders: 880, PendingValidation: 0,
		},

		Feasible:          true,
		FeasibilityReason: "feasible: 880 eligible bids",

		Ceremony: Ceremony{
			BidbookRoot:      hash(0x11),
			BidbookAnchor:    confirmed(),
			SeedCommitment:   hash(0x22),
			CommitmentAnchor: confirmed(),
			TargetBlock:      900_100,
			TargetBlockHash:  hash(0x33),
			SecretRevealed:   true,
			FinalSeed:        hash(0x44),
			ResultRoot:       hash(0x55),
			ResultAnchor:     confirmed(),
			Attempt:          1,
		},

		Settlement: Settlement{
			Begun:             true,
			ExpectedHolders:   201,
			ExpectedUnits:     500,
			CreditedHolders:   201,
			CreditedUnits:     500,
			AllotmentFileHash: hash(0x66),
			IMUnitsRecorded:   true,
		},

		BidbookPinned:       true,
		AllotmentFilePinned: true,
		AllocationsRecorded: 880,
	}
}

// ---------------------------------------------------------------------------
// The graph
// ---------------------------------------------------------------------------

func TestStatusEnumMatchesPostgres(t *testing.T) {
	pg := []string{
		"CONFIGURED", "OPEN", "CLOSED", "BOOK_FROZEN", "FEASIBILITY_CHECKED",
		"BIDBOOK_ANCHORED", "SEED_COMMITTED", "SEED_REVEALED", "BALLOT_DRAWN",
		"ALLOTMENT_FINALISED", "SETTLED", "ABORTED",
	}
	for _, n := range pg {
		if !Status(n).Valid() {
			t.Errorf("%s is in the offer_status enum but unmapped here", n)
		}
	}
	if len(AllStatuses()) != len(pg) {
		t.Errorf("AllStatuses has %d entries, the enum has %d", len(AllStatuses()), len(pg))
	}
}

func TestHappyPathWalksToSettled(t *testing.T) {
	ev := ready()

	cur := StatusConfigured
	var visited []Status
	for {
		visited = append(visited, cur)
		next, ok := NextExpected(cur)
		if !ok {
			break
		}
		if err := Guard(cur, next, ev); err != nil {
			t.Fatalf("%s -> %s refused with complete evidence: %v", cur, next, err)
		}
		cur = next
		if len(visited) > 20 {
			t.Fatal("the happy path does not terminate")
		}
	}

	if cur != StatusSettled {
		t.Fatalf("the happy path ended at %s, want SETTLED", cur)
	}
	if len(visited) != 11 {
		t.Errorf("walked %d states (%v), want 11", len(visited), visited)
	}
}

// TestAbortReachableFromEveryPreSettlementState is the ASBA dividend made explicit.
//
// Blocked funds never left the investor's account, so abandoning an offer costs an unblock rather than a
// refund. That is why an abort edge exists everywhere before settlement: an offer that fails its holder
// floor at the feasibility check needs a legal way to stop, and without these edges it would have none.
func TestAbortReachableFromEveryPreSettlementState(t *testing.T) {
	ev := ready()
	ev.AbortReason = "holder floor unreachable"

	for _, s := range AllStatuses() {
		if s.IsTerminal() {
			continue
		}
		if !s.CanTransitionTo(StatusAborted) {
			t.Errorf("%s must be able to abort", s)
		}
		if err := Guard(s, StatusAborted, ev); err != nil {
			t.Errorf("abort from %s refused: %v", s, err)
		}
		if !s.IsAbortable() {
			t.Errorf("%s should report as abortable", s)
		}
	}
}

// TestSettledCannotAbort is the boundary where remedies change character.
func TestSettledCannotAbort(t *testing.T) {
	if StatusSettled.CanTransitionTo(StatusAborted) {
		t.Fatal("a settled offer must not be abortable: units are in Demat accounts")
	}
	if StatusSettled.IsAbortable() {
		t.Error("IsAbortable must be false once units are issued")
	}
	if !StatusSettled.UnitsIssued() {
		t.Error("UnitsIssued must be true for SETTLED")
	}

	ev := ready()
	ev.AbortReason = "changed my mind"
	err := Guard(StatusSettled, StatusAborted, ev)
	if err == nil {
		t.Fatal("aborting from SETTLED must be refused")
	}
	if !errors.Is(err, ErrIllegalTransition) && !errors.Is(err, ErrUnitsIssued) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestAbortRequiresAReason(t *testing.T) {
	ev := ready()
	ev.AbortReason = ""
	if err := Guard(StatusOpen, StatusAborted, ev); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("want ErrPreconditionFailed, got %v", err)
	}
}

func TestTerminalStatesAreTerminal(t *testing.T) {
	for _, s := range []Status{StatusSettled, StatusAborted} {
		if !s.IsTerminal() {
			t.Errorf("%s should be terminal", s)
		}
		for _, to := range AllStatuses() {
			if s == to {
				continue
			}
			if s.CanTransitionTo(to) {
				t.Errorf("%s must not transition to %s", s, to)
			}
		}
	}
}

func TestUnknownStatusRejected(t *testing.T) {
	if err := CheckTransition(StatusOpen, Status("WITHDRAWN")); !errors.Is(err, ErrUnknownStatus) {
		t.Fatalf("want ErrUnknownStatus, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// The feasibility gate before anchoring
// ---------------------------------------------------------------------------

// TestInfeasibleOfferCannotAnchor is the ordering that keeps a doomed offer out of the permanent record.
//
// Anchoring is the first irreversible public act. Testing the floors beforehand means an offer that
// cannot list is abandoned quietly. Anchoring first would leave the bid book permanently committed, the
// draw run, and the failure discovered with the evidence already immutable.
func TestInfeasibleOfferCannotAnchor(t *testing.T) {
	ev := ready()
	ev.Feasible = false
	ev.FeasibilityReason = "fully subscribed in rupees but only 51 distinct bidders"

	err := Guard(StatusFeasibilityChecked, StatusBidbookAnchored, ev)
	if !errors.Is(err, ErrNotFeasible) {
		t.Fatalf("want ErrNotFeasible, got %v", err)
	}
	// The reason has to survive into the error, or an operator sees "not feasible" and cannot tell an
	// undersubscription from a holder shortfall. Those need different commercial responses.
	if !strings.Contains(err.Error(), "51 distinct bidders") {
		t.Errorf("the specific finding should reach the operator, got: %v", err)
	}
}

// TestFeasibilityMustBeCheckedAgainstAFrozenBook covers the ordering that makes the check meaningful.
func TestFeasibilityMustBeCheckedAgainstAFrozenBook(t *testing.T) {
	for _, s := range []Status{StatusConfigured, StatusOpen, StatusClosed} {
		if s.BookIsFixed() {
			t.Errorf("%s must not report the book as fixed", s)
		}
	}
	for _, s := range []Status{
		StatusBookFrozen, StatusFeasibilityChecked, StatusBidbookAnchored,
		StatusSeedCommitted, StatusSeedRevealed, StatusBallotDrawn,
		StatusAllotmentFinalised, StatusSettled,
	} {
		if !s.BookIsFixed() {
			t.Errorf("%s must report the book as fixed", s)
		}
	}

	// And the graph does not allow feasibility to be reached from an unfrozen state.
	if StatusClosed.CanTransitionTo(StatusFeasibilityChecked) {
		t.Error("CLOSED must not lead straight to FEASIBILITY_CHECKED; the book has to be frozen first")
	}
}

// ---------------------------------------------------------------------------
// Freezing the book
// ---------------------------------------------------------------------------

// TestFreezeRequiresEveryInBookBidToHoldFunds covers the gap between validated and funded.
//
// A bid in the book without a confirmed block could be allotted units it cannot pay for, and the
// shortfall would surface at debit time, after the cap table was anchored.
func TestFreezeRequiresEveryInBookBidToHoldFunds(t *testing.T) {
	ev := ready()
	ev.Book = BookState{BidCount: 900, InBookCount: 880, FundsBlockedCount: 879}

	err := Guard(StatusClosed, StatusBookFrozen, ev)
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("want ErrPreconditionFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "879 of 880") {
		t.Errorf("the shortfall should be stated, got: %v", err)
	}
}

// TestFreezeRefusedWhileValidationOutstanding covers why CLOSED and BOOK_FROZEN are separate states.
//
// Closing stops intake; validation continues. A bid submitted before the deadline can still fail its
// funds block or KYC afterwards, so the membership of the book is not final at close.
func TestFreezeRefusedWhileValidationOutstanding(t *testing.T) {
	ev := ready()
	ev.Book = BookState{BidCount: 900, InBookCount: 880, FundsBlockedCount: 880, PendingValidation: 3}

	err := Guard(StatusClosed, StatusBookFrozen, ev)
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("want ErrPreconditionFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "still changing") {
		t.Errorf("the reason should explain the book is still moving, got: %v", err)
	}
}

func TestFreezeRefusedWithAnEmptyBook(t *testing.T) {
	ev := ready()
	ev.Book = BookState{BidCount: 12, InBookCount: 0, FundsBlockedCount: 0}
	if err := Guard(StatusClosed, StatusBookFrozen, ev); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("want ErrPreconditionFailed, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Commit-reveal ordering
// ---------------------------------------------------------------------------

// TestSeedCannotBeCommittedBeforeTheBookIsAnchored mirrors the Postgres trigger.
//
// The draw has to be bound to a bid set nobody can still change. A commitment made first would let the
// book move underneath it, and because the book root feeds the seed, the resulting draw would be
// unverifiable rather than merely suspect.
func TestSeedCannotBeCommittedBeforeTheBookIsAnchored(t *testing.T) {
	ev := ready()
	ev.Ceremony.BidbookRoot = merkle.Hash{}

	if err := Guard(StatusBidbookAnchored, StatusSeedCommitted, ev); !errors.Is(err, ErrCeremonyOrder) {
		t.Fatalf("want ErrCeremonyOrder, got %v", err)
	}
}

// TestCommitmentRequiresAConfirmedBookAnchor is the reorg gate on the draw.
func TestCommitmentRequiresAConfirmedBookAnchor(t *testing.T) {
	for conf := 0; conf < MinAnchorConfirmations; conf++ {
		ev := ready()
		ev.Ceremony.BidbookAnchor = &AnchorRef{OutboxID: "ob", Status: "CONFIRMED", Confirmations: conf}

		if err := Guard(StatusBidbookAnchored, StatusSeedCommitted, ev); !errors.Is(err, ErrAnchorNotConfirmed) {
			t.Fatalf("%d confirmations must block the commitment, got %v", conf, err)
		}
	}

	ev := ready()
	if err := Guard(StatusBidbookAnchored, StatusSeedCommitted, ev); err != nil {
		t.Fatalf("%d confirmations must suffice: %v", MinAnchorConfirmations, err)
	}
}

// TestSecretCannotBeRevealedBeforeTheCommitmentIsOnChain is the anti-grinding guarantee.
//
// If the plaintext could exist before the commitment was confirmed, an operator who disliked a draw could
// claim a different secret was always intended. The commitment proves otherwise only if it demonstrably
// predates the reveal, which is why both this guard and a Postgres trigger enforce the ordering.
func TestSecretCannotBeRevealedBeforeTheCommitmentIsOnChain(t *testing.T) {
	ev := ready()
	ev.Ceremony.CommitmentAnchor = &AnchorRef{OutboxID: "ob", Status: "CONFIRMING", Confirmations: 2}

	err := Guard(StatusSeedCommitted, StatusSeedRevealed, ev)
	if !errors.Is(err, ErrCeremonyOrder) {
		t.Fatalf("want ErrCeremonyOrder, got %v", err)
	}
	if !strings.Contains(err.Error(), "different secret was always intended") {
		t.Errorf("the error should explain the attack it prevents, got: %v", err)
	}
}

func TestRevealRequiresATargetBlockAndItsHash(t *testing.T) {
	t.Run("no target block", func(t *testing.T) {
		ev := ready()
		ev.Ceremony.TargetBlock = 0
		if err := Guard(StatusSeedCommitted, StatusSeedRevealed, ev); !errors.Is(err, ErrCeremonyOrder) {
			t.Fatalf("want ErrCeremonyOrder, got %v", err)
		}
	})

	t.Run("target block hash not yet available", func(t *testing.T) {
		ev := ready()
		ev.Ceremony.TargetBlockHash = merkle.Hash{}
		if err := Guard(StatusSeedCommitted, StatusSeedRevealed, ev); !errors.Is(err, ErrPreconditionFailed) {
			t.Fatalf("want ErrPreconditionFailed, got %v", err)
		}
	})
}

// TestAttemptsAreBoundedAtThree matches the contract and the CHECK constraint.
func TestAttemptsAreBoundedAtThree(t *testing.T) {
	ev := ready()
	ev.Ceremony.Attempt = 4

	err := Guard(StatusBidbookAnchored, StatusSeedCommitted, ev)
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("want ErrPreconditionFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "trustee escalation") {
		t.Errorf("the error should name the escalation path, got: %v", err)
	}
}

// TestRecommitIsASelfTransition records why there is no separate state for it.
//
// A recommit reuses the stored commitment and moves only the target block, so the offer is still waiting
// to reveal. Adding a state would put something in the enum that has to be explained; a self-transition
// is the honest model.
func TestRecommitIsASelfTransition(t *testing.T) {
	if err := CheckTransition(StatusSeedCommitted, StatusSeedCommitted); err != nil {
		t.Fatalf("a recommit must be a legal self-transition: %v", err)
	}
	if err := Guard(StatusSeedCommitted, StatusSeedCommitted, Evidence{}); err != nil {
		t.Errorf("a self-transition should need no fresh evidence: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The draw
// ---------------------------------------------------------------------------

// TestEveryBidNeedsAnAllocation covers the losing bidders.
//
// A book of 900 bids for 475 units produces 900 allocations, most of them nil. A losing bidder needs a
// published outcome to check their own rank against; recording only the winners would make the draw
// unfalsifiable for exactly the people most motivated to check it.
func TestEveryBidNeedsAnAllocation(t *testing.T) {
	ev := ready()
	ev.AllocationsRecorded = 475 // winners only

	err := Guard(StatusSeedRevealed, StatusBallotDrawn, ev)
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("want ErrPreconditionFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "including a nil one") {
		t.Errorf("the error should explain nil outcomes are required, got: %v", err)
	}
}

func TestDrawRequiresARevealedSecretAndASeed(t *testing.T) {
	t.Run("secret not revealed", func(t *testing.T) {
		ev := ready()
		ev.Ceremony.SecretRevealed = false
		if err := Guard(StatusSeedRevealed, StatusBallotDrawn, ev); !errors.Is(err, ErrCeremonyOrder) {
			t.Fatalf("want ErrCeremonyOrder, got %v", err)
		}
	})

	t.Run("no final seed", func(t *testing.T) {
		ev := ready()
		ev.Ceremony.FinalSeed = merkle.Hash{}
		if err := Guard(StatusSeedRevealed, StatusBallotDrawn, ev); !errors.Is(err, ErrPreconditionFailed) {
			t.Fatalf("want ErrPreconditionFailed, got %v", err)
		}
	})
}

func TestFinalisingRequiresAConfirmedResultAnchorAndAPinnedFile(t *testing.T) {
	t.Run("result anchor unconfirmed", func(t *testing.T) {
		ev := ready()
		ev.Ceremony.ResultAnchor = &AnchorRef{Status: "CONFIRMING", Confirmations: 1}
		if err := Guard(StatusBallotDrawn, StatusAllotmentFinalised, ev); !errors.Is(err, ErrAnchorNotConfirmed) {
			t.Fatalf("want ErrAnchorNotConfirmed, got %v", err)
		}
	})

	t.Run("allotment file not pinned", func(t *testing.T) {
		ev := ready()
		ev.AllotmentFilePinned = false
		err := Guard(StatusBallotDrawn, StatusAllotmentFinalised, ev)
		if !errors.Is(err, ErrPreconditionFailed) {
			t.Fatalf("want ErrPreconditionFailed, got %v", err)
		}
		if !strings.Contains(err.Error(), "reproduce") {
			t.Errorf("the reason should be that nobody could reproduce the draw, got: %v", err)
		}
	})
}

func TestAnchoringRequiresAPinnedBidbook(t *testing.T) {
	ev := ready()
	ev.BidbookPinned = false
	err := Guard(StatusFeasibilityChecked, StatusBidbookAnchored, ev)
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("want ErrPreconditionFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "nobody can fetch") {
		t.Errorf("unexpected message: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Settlement
// ---------------------------------------------------------------------------

// TestSettlementMirrorsTheContractsFiveChecks refuses before a transaction is signed.
//
// finaliseSettlement reverts on any of five conditions, and a revert costs gas while naming a Solidity
// selector. Refusing here names the missing quantity instead.
func TestSettlementMirrorsTheContractsFiveChecks(t *testing.T) {
	cases := map[string]func(*Evidence){
		"settlement not begun":    func(e *Evidence) { e.Settlement.Begun = false },
		"not bound to a file":     func(e *Evidence) { e.Settlement.AllotmentFileHash = merkle.Hash{} },
		"units short":             func(e *Evidence) { e.Settlement.CreditedUnits = 499 },
		"holders short":           func(e *Evidence) { e.Settlement.CreditedHolders = 200 },
		"manager units not on chain": func(e *Evidence) { e.Settlement.IMUnitsRecorded = false },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			ev := ready()
			mutate(&ev)
			if err := Guard(StatusAllotmentFinalised, StatusSettled, ev); !errors.Is(err, ErrSettlementIncomplete) {
				t.Fatalf("want ErrSettlementIncomplete, got %v", err)
			}
		})
	}
}

// TestManagerUnitsAreEasyToForget is the specific omission that would revert on-chain.
//
// The manager never bids, so its units are recorded directly rather than allotted by ballot. A settlement
// assembled purely from ballot output is short by exactly the manager's holding, and finaliseSettlement
// fails on its third check with a mismatch that does not obviously point at the cause.
func TestManagerUnitsAreEasyToForget(t *testing.T) {
	ev := ready()
	ev.Settlement.IMUnitsRecorded = false

	err := Guard(StatusAllotmentFinalised, StatusSettled, ev)
	if err == nil {
		t.Fatal("a settlement without the manager's subscription must be refused")
	}
	if !strings.Contains(err.Error(), "not allotted by ballot") {
		t.Errorf("the error should explain why it is missing, got: %v", err)
	}
}

// TestSettlementRefusedBelowTheHolderFloor is the statutory check.
func TestSettlementRefusedBelowTheHolderFloor(t *testing.T) {
	ev := ready()
	ev.Settlement.ExpectedHolders = 199
	ev.Settlement.CreditedHolders = 199

	if err := Guard(StatusAllotmentFinalised, StatusSettled, ev); !errors.Is(err, ErrNotFeasible) {
		t.Fatalf("want ErrNotFeasible, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Pause
// ---------------------------------------------------------------------------

func TestPauseBlocksTheLifecycleButNeverAnAbort(t *testing.T) {
	ev := ready()
	ev.Paused = true
	ev.AbortReason = "emergency"

	if err := Guard(StatusFeasibilityChecked, StatusBidbookAnchored, ev); !errors.Is(err, ErrPaused) {
		t.Errorf("anchoring while paused must be refused, got %v", err)
	}
	if err := Guard(StatusOpen, StatusAborted, ev); err != nil {
		t.Errorf("aborting while paused must work: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Terms and the feasibility arithmetic
// ---------------------------------------------------------------------------

func TestV1TermsAreValid(t *testing.T) {
	if err := v1Terms().Validate(); err != nil {
		t.Fatalf("the locked v1 terms must validate, or every negative case is vacuous: %v", err)
	}
}

// TestBidCapCannotMakeTheHolderFloorUnreachable is the arithmetic the schema also enforces.
//
// Reaching 200 holders needs 199 others holding at least one unit each, so the largest a single bid can
// be within 475 units is 276. A cap above that produces an offer that can be fully subscribed in rupees
// and still be unlistable, and the only way to find out would be a failed draw after anchoring.
func TestBidCapCannotMakeTheHolderFloorUnreachable(t *testing.T) {
	terms := v1Terms()

	if got := terms.MaxBidHeadroom(); got != 276 {
		t.Fatalf("headroom = %d, want 276 (475 units less 199 other holders)", got)
	}

	terms.MaxBidUnits = 277
	err := terms.Validate()
	if err == nil {
		t.Fatal("a cap of 277 leaves only 198 units for 199 other holders and must be refused")
	}
	if !strings.Contains(err.Error(), "276") {
		t.Errorf("the error should state the largest permissible cap, got: %v", err)
	}

	// Exactly at the boundary is acceptable.
	terms.MaxBidUnits = 276
	if err := terms.Validate(); err != nil {
		t.Errorf("a cap of exactly 276 must be permitted: %v", err)
	}
}

func TestTermsRejectSubStatutoryValues(t *testing.T) {
	cases := map[string]func(*Terms){
		"holder floor below 200":   func(t *Terms) { t.MinDistinctHolders = 199 },
		"price below ten lakh":     func(t *Terms) { t.PriceBandLowerPaise = money.MinUnitPricePaise - 1 },
		"inverted price band":      func(t *Terms) { t.PriceBandUpperPaise = t.PriceBandLowerPaise - 1 },
		"zero units":               func(t *Terms) { t.UnitsOnOffer = 0 },
		"zero minimum bid":         func(t *Terms) { t.MinBidUnits = 0 },
		"max below min":            func(t *Terms) { t.MaxBidUnits = 0 },
		"closes before it opens":   func(t *Terms) { t.ClosesAt = t.OpensAt.Add(-time.Hour) },
		"allotment before close":   func(t *Terms) { t.AllotmentDueAt = t.ClosesAt.Add(-time.Hour) },
		"holder floor above units": func(t *Terms) { t.UnitsOnOffer = 150; t.MinDistinctHolders = 200 },
		"zero min subscription":    func(t *Terms) { t.MinSubscriptionUnits = 0 },
	}

	for name, mutate := range cases {
		t.Run(name, func(t2 *testing.T) {
			terms := v1Terms()
			mutate(&terms)
			if err := terms.Validate(); !errors.Is(err, ErrPreconditionFailed) {
				t2.Fatalf("want ErrPreconditionFailed, got %v", err)
			}
		})
	}
}

func TestOpeningBeforeTheWindowRefused(t *testing.T) {
	ev := ready()
	ev.Now = opensAt.Add(-time.Hour)
	if err := Guard(StatusConfigured, StatusOpen, ev); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("want ErrPreconditionFailed, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// The bridge to the allocation engine
// ---------------------------------------------------------------------------

// TestTermsAndBallotParamsAgree is the drift guard.
//
// The two structs overlap on seven fields and answer to different authorities: Terms mirrors the offers
// table, Params is versioned by AlgoVersion because changing it invalidates anchored results. Drift
// between them would mean the offer accepting bids the draw rejects, or enforcing a cap the draw does
// not, and neither failure is loud.
func TestTermsAndBallotParamsAgree(t *testing.T) {
	want := ballot.V1Params()
	got := TermsFromBallotParams(want).BallotParams()

	if got != want {
		t.Fatalf("round trip through Terms changed the parameters:\n got %+v\nwant %+v", got, want)
	}

	// And the conversion must always demand full subscription, regardless of what it was handed.
	p := ballot.V1Params()
	p.RequireFullSubscription = false
	if !TermsFromBallotParams(p).BallotParams().RequireFullSubscription {
		t.Fatal("full subscription is structural for a v1 scheme and must not be overridable: the " +
			"asset value is units times price and the band floor leaves no room to shrink")
	}
}

func TestBallotParamsFromV1TermsValidate(t *testing.T) {
	if err := v1Terms().BallotParams().Validate(); err != nil {
		t.Fatalf("parameters derived from valid terms must satisfy the engine: %v", err)
	}
}

// TestFeasibilityVerdictNamesTheFailingTest is what makes a refusal actionable.
func TestFeasibilityVerdictNamesTheFailingTest(t *testing.T) {
	t.Run("engine reasons are preserved", func(t *testing.T) {
		feasible, reason := FeasibilityVerdict(ballot.Feasibility{
			Reasons: []string{"only 51 distinct bidders against a floor of 200"},
		})
		if feasible {
			t.Fatal("should not be feasible")
		}
		if !strings.Contains(reason, "51 distinct bidders") {
			t.Errorf("reason = %q", reason)
		}
	})

	t.Run("fallback names each failing test", func(t *testing.T) {
		_, reason := FeasibilityVerdict(ballot.Feasibility{
			SubscriptionMet: false, HolderFloorMet: false, FullSubscriptionMet: false,
		})
		for _, want := range []string{"subscription", "unitholder floor", "cannot shrink"} {
			if !strings.Contains(reason, want) {
				t.Errorf("reason %q does not mention %q", reason, want)
			}
		}
	})

	t.Run("a bare infeasible verdict is still explained", func(t *testing.T) {
		_, reason := FeasibilityVerdict(ballot.Feasibility{
			SubscriptionMet: true, HolderFloorMet: true, FullSubscriptionMet: true,
		})
		if reason == "" {
			t.Fatal("a verdict must never be empty")
		}
	})

	t.Run("a pass carries the counts", func(t *testing.T) {
		feasible, reason := FeasibilityVerdict(ballot.Feasibility{
			Feasible: true, EligibleBids: 880, TotalUnitsBid: 1760, TechnicalRejects: 20,
		})
		if !feasible {
			t.Fatal("should be feasible")
		}
		if !strings.Contains(reason, "880") || !strings.Contains(reason, "20") {
			t.Errorf("an approving operator should see the counts, got %q", reason)
		}
	})
}

func TestApplyFeasibility(t *testing.T) {
	ev := ready()
	ev.ApplyFeasibility(ballot.Feasibility{Reasons: []string{"undersubscribed by 40 units"}})

	if ev.Feasible {
		t.Error("Feasible should be false")
	}
	if !strings.Contains(ev.FeasibilityReason, "40 units") {
		t.Errorf("reason = %q", ev.FeasibilityReason)
	}
	if err := Guard(StatusFeasibilityChecked, StatusBidbookAnchored, ev); !errors.Is(err, ErrNotFeasible) {
		t.Errorf("the applied verdict should block anchoring, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// The scheme mapping
// ---------------------------------------------------------------------------

// TestSchemeStatusMappingMatchesTheContract guards against advancing the scheme to a wrong state.
//
// A transposed ordinal would succeed on-chain while asserting something untrue, so the mapping is
// checked against the contract's enum positions explicitly.
func TestSchemeStatusMappingMatchesTheContract(t *testing.T) {
	// Ordinals as declared in AcreSyncScheme.Status.
	if SchemeDraft != 0 || SchemeOfferOpen != 5 || SchemeOfferClosed != 6 ||
		SchemeAllocated != 7 || SchemeSettled != 8 || SchemeUndersubscribed != 11 ||
		SchemeRefunding != 12 || SchemeAborted != 13 {
		t.Fatal("scheme status ordinals no longer match the contract enum")
	}

	want := map[Status]SchemeStatus{
		StatusOpen:               SchemeOfferOpen,
		StatusClosed:             SchemeOfferClosed,
		StatusAllotmentFinalised: SchemeAllocated,
	}

	for _, s := range AllStatuses() {
		got, ok := s.SchemeStatusFor()
		expected, shouldMap := want[s]

		if ok != shouldMap {
			t.Errorf("%s: mapped = %v, want %v", s, ok, shouldMap)
			continue
		}
		if ok && got != expected {
			t.Errorf("%s maps to %d, want %d", s, got, expected)
		}
	}
}

// TestSettledHasNoSchemeMapping records the contract's refusal.
//
// advanceStatus returns false for Settled unconditionally: only finaliseSettlement may set it, and only
// once its invariants hold. Offering a mapping would invite a call that reverts.
func TestSettledHasNoSchemeMapping(t *testing.T) {
	if _, ok := StatusSettled.SchemeStatusFor(); ok {
		t.Fatal("SETTLED must not map to a scheme status: only finaliseSettlement can set it")
	}
}

// TestAbortPathIsThreeSteps covers the sequence the contract requires.
//
// Undersubscribed, then Refunding, then Aborted. The middle state is the point: refunding is where
// blocked funds are released, and reaching Aborted without it would claim an outcome that had not been
// carried out.
func TestAbortPathIsThreeSteps(t *testing.T) {
	path := AbortPath()

	want := []SchemeStatus{SchemeUndersubscribed, SchemeRefunding, SchemeAborted}
	if len(path) != len(want) {
		t.Fatalf("abort path has %d steps, want %d", len(path), len(want))
	}
	for i := range want {
		if path[i] != want[i] {
			t.Errorf("step %d is %d, want %d", i, path[i], want[i])
		}
	}
}

func TestAcceptsBidsOnlyWhenOpen(t *testing.T) {
	for _, s := range AllStatuses() {
		if got, want := s.AcceptsBids(), s == StatusOpen; got != want {
			t.Errorf("%s.AcceptsBids() = %v, want %v", s, got, want)
		}
	}
}

func TestCeremonyInProgress(t *testing.T) {
	for _, s := range AllStatuses() {
		want := s == StatusSeedCommitted || s == StatusSeedRevealed
		if got := s.CeremonyInProgress(); got != want {
			t.Errorf("%s.CeremonyInProgress() = %v, want %v", s, got, want)
		}
	}
}

func TestAnchorRefIsConfirmed(t *testing.T) {
	var nilRef *AnchorRef
	if nilRef.IsConfirmed() {
		t.Error("a nil anchor is not confirmed")
	}
	if (&AnchorRef{Status: "CONFIRMED", Confirmations: 4}).IsConfirmed() {
		t.Error("four confirmations is not enough")
	}
	if (&AnchorRef{Status: "CONFIRMING", Confirmations: 9}).IsConfirmed() {
		t.Error("CONFIRMING is not CONFIRMED")
	}
	if !(&AnchorRef{Status: "CONFIRMED", Confirmations: 5}).IsConfirmed() {
		t.Error("five confirmations on a CONFIRMED row is the gate")
	}
}

func TestCanAdvance(t *testing.T) {
	ev := ready()
	if !CanAdvance(StatusFeasibilityChecked, StatusBidbookAnchored, ev) {
		t.Error("a feasible offer should be able to anchor")
	}
	ev.Feasible = false
	if CanAdvance(StatusFeasibilityChecked, StatusBidbookAnchored, ev) {
		t.Error("an infeasible offer should not")
	}
}

func TestSameStatusIsIdempotent(t *testing.T) {
	for _, s := range AllStatuses() {
		if err := Guard(s, s, Evidence{}); err != nil {
			t.Errorf("restating %s should be a no-op, got %v", s, err)
		}
	}
}
