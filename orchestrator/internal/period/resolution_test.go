package period

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var resolvedAt = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

// wallet(n) is defined in period_test.go and reused here rather than redeclared.

func pos(n int, units uint32) Position {
	return Position{
		InvestorID:    "inv-" + string(rune('a'+n%26)),
		WalletAddress: wallet(n),
		Units:         units,
	}
}

// divergedRun builds a run where the depository shows one more unit than our mirror for wallet 1.
func divergedRun(t *testing.T) *Reconciliation {
	t.Helper()

	dep := []Position{pos(1, 3), pos(2, 2), pos(3, 5)}
	mir := []Position{pos(1, 2), pos(2, 2), pos(3, 5)}

	rec, err := Reconcile(dep, mir)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != ReconDiverged {
		t.Fatalf("fixture should diverge, got %s", rec.Status)
	}
	return rec
}

func matchedRun(t *testing.T, units ...uint32) *Reconciliation {
	t.Helper()

	var dep, mir []Position
	for i, u := range units {
		dep = append(dep, pos(i+1, u))
		mir = append(mir, pos(i+1, u))
	}
	rec, err := Reconcile(dep, mir)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != ReconMatched {
		t.Fatalf("fixture should match, got %s", rec.Status)
	}
	return rec
}

func chainUpdated(w string) DiffOutcome {
	return DiffOutcome{
		WalletAddress: w,
		Resolution:    ResolutionChainUpdated,
		ChainTx:       "0xcorrection",
		ResolvedAt:    resolvedAt,
	}
}

// --- the central rule ----------------------------------------------------------------------------

// TestResolutionRequiresAMatchingRerun is the rule the whole design rests on.
//
// RESOLVED unblocks payouts. If an operator could reach it by asserting a fix, the DIVERGED state would
// be no barrier: a mistaken belief would release the money. So the register itself has to testify.
func TestResolutionRequiresAMatchingRerun(t *testing.T) {
	original := divergedRun(t)

	// Corrections believed to be done, but the re-run still disagrees.
	stillBad, err := Reconcile(
		[]Position{pos(1, 3), pos(2, 2), pos(3, 5)},
		[]Position{pos(1, 2), pos(2, 2), pos(3, 5)},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = Resolve(ResolveInput{
		Original:   original,
		Fresh:      stillBad,
		Outcomes:   []DiffOutcome{chainUpdated(wallet(1))},
		ApprovedBy: "ops@acresync",
		ResolvedAt: resolvedAt,
	})
	if !errors.Is(err, ErrStillDiverged) {
		t.Fatalf("a re-run that still diverges must not resolve anything, got %v", err)
	}
}

func TestResolutionSucceedsOnAMatchingRerun(t *testing.T) {
	original := divergedRun(t)
	fresh := matchedRun(t, 3, 2, 5) // the mirror was credited the missing unit

	res, err := Resolve(ResolveInput{
		Original:   original,
		Fresh:      fresh,
		Outcomes:   []DiffOutcome{chainUpdated(wallet(1))},
		ApprovedBy: "ops@acresync",
		ResolvedAt: resolvedAt,
	})
	if err != nil {
		t.Fatalf("a matching re-run with every divergence explained must resolve: %v", err)
	}

	if res.Status != ReconResolved {
		t.Fatalf("status %s, want RESOLVED", res.Status)
	}
	if len(res.Cleared) != 1 || res.Cleared[0].WalletAddress != wallet(1) {
		t.Fatal("the resolution must retain what was wrong, not just the corrected state")
	}
}

// --- accounting for every divergence -------------------------------------------------------------

func TestUnexplainedDivergenceIsRefused(t *testing.T) {
	// Two divergences, one explanation.
	dep := []Position{pos(1, 3), pos(2, 4)}
	mir := []Position{pos(1, 2), pos(2, 2)}
	original, err := Reconcile(dep, mir)
	if err != nil {
		t.Fatal(err)
	}
	if original.DivergenceCount() != 2 {
		t.Fatalf("fixture should have 2 divergences, got %d", original.DivergenceCount())
	}

	_, err = Resolve(ResolveInput{
		Original:   original,
		Fresh:      matchedRun(t, 3, 4),
		Outcomes:   []DiffOutcome{chainUpdated(wallet(1))},
		ApprovedBy: "ops@acresync",
		ResolvedAt: resolvedAt,
	})
	if !errors.Is(err, ErrDiffUnaccounted) {
		t.Fatalf("a matching re-run is necessary but not sufficient; every divergence needs a "+
			"recorded reason, got %v", err)
	}
}

func TestResolutionForAWalletThatNeverDivergedIsRefused(t *testing.T) {
	_, err := Resolve(ResolveInput{
		Original:   divergedRun(t),
		Fresh:      matchedRun(t, 3, 2, 5),
		Outcomes:   []DiffOutcome{chainUpdated(wallet(1)), chainUpdated(wallet(9))},
		ApprovedBy: "ops@acresync",
		ResolvedAt: resolvedAt,
	})
	if !errors.Is(err, ErrDiffNotInRun) {
		t.Fatalf("paperwork describing a different run must be refused, got %v", err)
	}
}

func TestPendingOutcomeIsRefused(t *testing.T) {
	_, err := Resolve(ResolveInput{
		Original: divergedRun(t),
		Fresh:    matchedRun(t, 3, 2, 5),
		Outcomes: []DiffOutcome{{
			WalletAddress: wallet(1),
			Resolution:    ResolutionPending,
			ResolvedAt:    resolvedAt,
		}},
		ApprovedBy: "ops@acresync",
		ResolvedAt: resolvedAt,
	})
	if !errors.Is(err, ErrDiffUnaccounted) {
		t.Fatalf("a pending outcome leaves the divergence open, got %v", err)
	}
}

func TestDuplicateOutcomeIsRefused(t *testing.T) {
	_, err := Resolve(ResolveInput{
		Original:   divergedRun(t),
		Fresh:      matchedRun(t, 3, 2, 5),
		Outcomes:   []DiffOutcome{chainUpdated(wallet(1)), chainUpdated(wallet(1))},
		ApprovedBy: "ops@acresync",
		ResolvedAt: resolvedAt,
	})
	if err == nil {
		t.Fatal("one divergence cannot have two resolutions")
	}
}

// --- evidence ------------------------------------------------------------------------------------

// TestChainUpdatedMustCiteATransaction is why the mirror being on-chain matters.
func TestChainUpdatedMustCiteATransaction(t *testing.T) {
	_, err := Resolve(ResolveInput{
		Original: divergedRun(t),
		Fresh:    matchedRun(t, 3, 2, 5),
		Outcomes: []DiffOutcome{{
			WalletAddress: wallet(1),
			Resolution:    ResolutionChainUpdated,
			ResolvedAt:    resolvedAt,
		}},
		ApprovedBy: "ops@acresync",
		ResolvedAt: resolvedAt,
	})
	if !errors.Is(err, ErrUnexplained) {
		t.Fatalf("want ErrUnexplained, got %v", err)
	}
	if !strings.Contains(err.Error(), "unverifiable") {
		t.Errorf("the error should say why a citation is required: %v", err)
	}
}

// TestATransactionOnANonCorrectingPathIsRefused catches paperwork that contradicts itself.
func TestATransactionOnANonCorrectingPathIsRefused(t *testing.T) {
	_, err := Resolve(ResolveInput{
		Original: divergedRun(t),
		Fresh:    matchedRun(t, 3, 2, 5),
		Outcomes: []DiffOutcome{{
			WalletAddress: wallet(1),
			Resolution:    ResolutionDepositoryQueried,
			ChainTx:       "0xsomething",
			ResolvedAt:    resolvedAt,
		}},
		ApprovedBy: "ops@acresync",
		ResolvedAt: resolvedAt,
	})
	if !errors.Is(err, ErrUnexplained) {
		t.Fatalf("a re-query changes nothing on our side, so citing a correction is incoherent: %v", err)
	}
}

// TestManualResolutionMustBeJustified keeps the least convenient path inconvenient.
func TestManualResolutionMustBeJustified(t *testing.T) {
	_, err := Resolve(ResolveInput{
		Original: divergedRun(t),
		Fresh:    matchedRun(t, 3, 2, 5),
		Outcomes: []DiffOutcome{{
			WalletAddress: wallet(1),
			Resolution:    ResolutionManual,
			ResolvedAt:    resolvedAt,
		}},
		ApprovedBy: "ops@acresync",
		ResolvedAt: resolvedAt,
	})
	if !errors.Is(err, ErrUnexplained) {
		t.Fatalf("a manual resolution with no narrative is the one that buries a mistake: %v", err)
	}

	// With a narrative it is accepted.
	var narrative [32]byte
	narrative[0] = 0x01

	if _, err := Resolve(ResolveInput{
		Original: divergedRun(t),
		Fresh:    matchedRun(t, 3, 2, 5),
		Outcomes: []DiffOutcome{{
			WalletAddress:   wallet(1),
			Resolution:      ResolutionManual,
			NarrativeSHA256: narrative,
			ResolvedAt:      resolvedAt,
		}},
		ApprovedBy: "trustee@acresync",
		ResolvedAt: resolvedAt,
	}); err != nil {
		t.Fatalf("a justified manual resolution must be accepted: %v", err)
	}
}

func TestAccountabilityIsRequired(t *testing.T) {
	_, err := Resolve(ResolveInput{
		Original:   divergedRun(t),
		Fresh:      matchedRun(t, 3, 2, 5),
		Outcomes:   []DiffOutcome{chainUpdated(wallet(1))},
		ResolvedAt: resolvedAt,
	})
	if !errors.Is(err, ErrUnexplained) {
		t.Fatalf("somebody has to be accountable, got %v", err)
	}
}

// --- the consequence operators miss --------------------------------------------------------------

// TestCorrectingTheRegisterRequiresARetakenSnapshot is the non-obvious outcome.
//
// Entitlements are computed from the frozen snapshot. Moving a unit count after that means every
// entitlement derived from it was computed against a register that no longer exists.
func TestCorrectingTheRegisterRequiresARetakenSnapshot(t *testing.T) {
	res, err := Resolve(ResolveInput{
		Original:   divergedRun(t),
		Fresh:      matchedRun(t, 3, 2, 5), // mirror went from 9 units to 10
		Outcomes:   []DiffOutcome{chainUpdated(wallet(1))},
		ApprovedBy: "ops@acresync",
		ResolvedAt: resolvedAt,
	})
	if err != nil {
		t.Fatal(err)
	}

	if !res.RequiresResnapshot {
		t.Fatal("our register moved, so the snapshot the entitlements came from is stale")
	}
	if res.UnitsBefore != 9 || res.UnitsAfter != 10 {
		t.Fatalf("units %d -> %d, want 9 -> 10", res.UnitsBefore, res.UnitsAfter)
	}
	if !strings.Contains(res.Summary(), "snapshot must be retaken") {
		t.Errorf("the summary should state the consequence: %s", res.Summary())
	}
}

// TestAStaleReadNeedsNoResnapshot is the other half of the same rule.
//
// If the divergence was only ever an artefact of when we read the depository, our register never moved
// and the snapshot is still a true picture of it.
func TestAStaleReadNeedsNoResnapshot(t *testing.T) {
	// The depository settled mid-read and now agrees with our unchanged mirror.
	original, err := Reconcile(
		[]Position{pos(1, 2), pos(2, 2)},
		[]Position{pos(1, 2), pos(2, 3)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if original.Status != ReconDiverged {
		t.Fatalf("fixture should diverge, got %s", original.Status)
	}

	// Our mirror is untouched at 5 units; the depository's fresh read now shows the same.
	fresh, err := Reconcile(
		[]Position{pos(1, 2), pos(2, 3)},
		[]Position{pos(1, 2), pos(2, 3)},
	)
	if err != nil {
		t.Fatal(err)
	}

	res, err := Resolve(ResolveInput{
		Original: original,
		Fresh:    fresh,
		Outcomes: []DiffOutcome{{
			WalletAddress: wallet(2),
			Resolution:    ResolutionDepositoryQueried,
			ResolvedAt:    resolvedAt,
		}},
		ApprovedBy: "ops@acresync",
		ResolvedAt: resolvedAt,
	})
	if err != nil {
		t.Fatal(err)
	}

	if res.RequiresResnapshot {
		t.Fatalf("our register never moved (%d -> %d units), so the snapshot is still true",
			res.UnitsBefore, res.UnitsAfter)
	}
}

// TestAConcurrentChangeStillForcesAResnapshot is the second, independent signal.
//
// If our register moved without any outcome saying it would, comparing the totals catches it. The two
// signals are kept separate precisely so one can catch what the other misses.
func TestAConcurrentChangeStillForcesAResnapshot(t *testing.T) {
	original, err := Reconcile(
		[]Position{pos(1, 2), pos(2, 2)},
		[]Position{pos(1, 2), pos(2, 3)},
	)
	if err != nil {
		t.Fatal(err)
	}

	// Resolved as a stale read, yet our mirror is now 4 units rather than the 5 it held.
	fresh, err := Reconcile(
		[]Position{pos(1, 2), pos(2, 2)},
		[]Position{pos(1, 2), pos(2, 2)},
	)
	if err != nil {
		t.Fatal(err)
	}

	res, err := Resolve(ResolveInput{
		Original: original,
		Fresh:    fresh,
		Outcomes: []DiffOutcome{{
			WalletAddress: wallet(2),
			Resolution:    ResolutionDepositoryQueried,
			ResolvedAt:    resolvedAt,
		}},
		ApprovedBy: "ops@acresync",
		ResolvedAt: resolvedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.RequiresResnapshot {
		t.Fatalf("the register moved from %d to %d units with no outcome admitting it; the totals "+
			"comparison must catch that", res.UnitsBefore, res.UnitsAfter)
	}
}

// --- guards against nonsense ---------------------------------------------------------------------

func TestResolvingAMatchedRunIsRefused(t *testing.T) {
	_, err := Resolve(ResolveInput{
		Original:   matchedRun(t, 2, 2),
		Fresh:      matchedRun(t, 2, 2),
		ApprovedBy: "ops@acresync",
		ResolvedAt: resolvedAt,
	})
	if !errors.Is(err, ErrNotDiverged) {
		t.Fatalf("want ErrNotDiverged, got %v", err)
	}
}

// TestAnEmptyRegisterIsNotProof closes the trivial way to satisfy the re-run rule.
func TestAnEmptyRegisterIsNotProof(t *testing.T) {
	empty, err := Reconcile(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Status != ReconMatched {
		t.Fatalf("an empty comparison matches by definition, got %s", empty.Status)
	}

	_, err = Resolve(ResolveInput{
		Original:   divergedRun(t),
		Fresh:      empty,
		Outcomes:   []DiffOutcome{chainUpdated(wallet(1))},
		ApprovedBy: "ops@acresync",
		ResolvedAt: resolvedAt,
	})
	if !errors.Is(err, ErrProofMismatched) {
		t.Fatalf("a re-run over nothing matches trivially and proves nothing, got %v", err)
	}
}

func TestResolutionNeedsBothRuns(t *testing.T) {
	if _, err := Resolve(ResolveInput{Original: divergedRun(t), ApprovedBy: "x", ResolvedAt: resolvedAt}); err == nil {
		t.Fatal("a resolution without a fresh run must be refused")
	}
	if _, err := Resolve(ResolveInput{Fresh: matchedRun(t, 2), ApprovedBy: "x", ResolvedAt: resolvedAt}); err == nil {
		t.Fatal("a resolution without the original run must be refused")
	}
}

// --- vocabulary ----------------------------------------------------------------------------------

func TestDiffResolutionVocabularyMatchesTheSchema(t *testing.T) {
	// The reconciliation_resolution Postgres enum, in declaration order.
	want := []DiffResolution{
		ResolutionPending, ResolutionChainUpdated, ResolutionDepositoryQueried, ResolutionManual,
	}
	got := AllDiffResolutions()

	if len(got) != len(want) {
		t.Fatalf("the enum has %d values, AllDiffResolutions returns %d", len(want), len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d is %q, want %q", i, got[i], want[i])
		}
	}
}

func TestOnlyPendingIsUnresolved(t *testing.T) {
	for _, r := range AllDiffResolutions() {
		want := r != ResolutionPending
		if r.IsResolved() != want {
			t.Errorf("%s: IsResolved = %v, want %v", r, r.IsResolved(), want)
		}
	}
	if DiffResolution("").IsResolved() {
		t.Error("an empty resolution is not a resolution")
	}
}

func TestOnlyChainUpdatedMovesOurRegister(t *testing.T) {
	for _, r := range AllDiffResolutions() {
		want := r == ResolutionChainUpdated
		if r.MovedOurRegister() != want {
			t.Errorf("%s: MovedOurRegister = %v, want %v", r, r.MovedOurRegister(), want)
		}
		if r.RequiresChainTx() != want {
			t.Errorf("%s: RequiresChainTx = %v, want %v", r, r.RequiresChainTx(), want)
		}
	}
}
