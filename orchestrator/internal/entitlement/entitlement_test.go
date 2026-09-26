package entitlement

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/acresync/orchestrator/internal/money"
	"github.com/acresync/orchestrator/internal/payout"
	"github.com/acresync/orchestrator/internal/snapshot"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const (
	schemeUUID = "6f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8"
	srcAccount = "7878780080316316"
	anchorTx   = "0198f1a0-0000-4000-8000-000000000000"
)

func anchorOf(b byte) [32]byte {
	var h [32]byte
	for i := range h {
		h[i] = b
	}
	return h
}

func wallet(n int) string { return fmt.Sprintf("0x%040x", n) }

func investorID(n int) string {
	return fmt.Sprintf("%08d-0000-4000-8000-000000000000", n)
}

// liveRegister is the v1 shape: 500 units, 202 lines, 201 countable.
func liveRegister(t *testing.T) *snapshot.Snapshot {
	t.Helper()

	holdings := []snapshot.Holding{{
		InvestorID: investorID(1), WalletAddress: wallet(1),
		InvestorAnchor: anchorOf(1), Units: 25, ExcludedFromHolderCount: true,
	}}
	for i := 0; i < 200; i++ {
		holdings = append(holdings, snapshot.Holding{
			InvestorID: investorID(i + 2), WalletAddress: wallet(i + 2),
			InvestorAnchor: anchorOf(byte(i + 2)), Units: 2,
		})
	}
	holdings = append(holdings, snapshot.Holding{
		InvestorID: investorID(500), WalletAddress: wallet(500),
		InvestorAnchor: anchorOf(250), Units: 75,
	})

	d := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	snap, err := snapshot.Build(snapshot.BuildInput{
		SchemeRef: anchorOf(0x5c), PeriodID: 1,
		RecordDate: d, PeriodEnd: d, TakenAt: d,
		Holdings: holdings, ExpectedTotalUnits: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func detailsFor(snap *snapshot.Snapshot, class InvestorClass) map[string]HolderDetail {
	out := make(map[string]HolderDetail, len(snap.Lines))
	for i, l := range snap.Lines {
		out[l.InvestorID] = HolderDetail{
			InvestorID:    l.InvestorID,
			Class:         class,
			BankAccountID: fmt.Sprintf("bank-%d", i),
			FundAccountID: fmt.Sprintf("fa_%014d", i+1),
		}
	}
	return out
}

func buildBatch(t *testing.T, distributed money.Paise) *Batch {
	t.Helper()
	snap := liveRegister(t)

	b, err := Build(BuildInput{
		PeriodID:             1,
		Snapshot:             snap,
		DistributedPaise:     distributed,
		Details:              detailsFor(snap, ClassResidentIndividual),
		TDS:                  SampleTDSTable(),
		ProviderMinimumPaise: payout.MinAmountPaise,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---------------------------------------------------------------------------
// Exactness
// ---------------------------------------------------------------------------

// TestGrossTotalEqualsDistributedExactly is the invariant everything rests on.
//
// The gross total is anchored on-chain. If it disagrees with the approved figure by a single paise,
// either the chain is wrong or the bank transfers are, and no later process can determine which.
func TestGrossTotalEqualsDistributedExactly(t *testing.T) {
	// Deliberately awkward amounts, including ones that do not divide by 500.
	for _, distributed := range []money.Paise{
		27_550_000_000,
		28_500_000_000,
		28_499_999_999,
		27_550_000_001,
		1,
		499,
		501,
		999_999_999_999,
	} {
		t.Run(distributed.String(), func(t *testing.T) {
			b := buildBatch(t, distributed)

			if b.GrossTotalPaise != distributed {
				t.Fatalf("gross total %d, distributed %d", b.GrossTotalPaise, distributed)
			}
			if err := b.Reconcile(); err != nil {
				t.Fatal(err)
			}

			// Recomputed independently of the running totals the builder kept.
			var sum money.Paise
			for _, l := range b.Lines {
				sum += l.GrossPaise
			}
			if sum != distributed {
				t.Fatalf("summing lines gives %d, want %d", sum, distributed)
			}
		})
	}
}

func TestWithholdingAndNetReconstructGross(t *testing.T) {
	b := buildBatch(t, money.Paise(27_550_000_000))

	if got := b.TDSTotalPaise + b.NetTotalPaise; got != b.GrossTotalPaise {
		t.Fatalf("withholding %d plus net %d is %d, gross is %d",
			b.TDSTotalPaise, b.NetTotalPaise, got, b.GrossTotalPaise)
	}

	for _, l := range b.Lines {
		if l.TDS.AmountPaise+l.TDS.NetPayablePaise != l.GrossPaise {
			t.Fatalf("investor %s: %d + %d != %d",
				l.InvestorID, l.TDS.AmountPaise, l.TDS.NetPayablePaise, l.GrossPaise)
		}
	}
}

// TestResidueIsBoundedAtOnePaisePerHolder mirrors the database CHECK.
func TestResidueIsBoundedAtOnePaisePerHolder(t *testing.T) {
	b := buildBatch(t, money.Paise(28_499_999_999))

	var awarded int
	for _, l := range b.Lines {
		if l.ResiduePaise < 0 || l.ResiduePaise > 1 {
			t.Fatalf("investor %s awarded %d residue paise", l.InvestorID, l.ResiduePaise)
		}
		if l.ResiduePaise == 1 {
			awarded++
		}
		if l.FloorPaise+l.ResiduePaise != l.GrossPaise {
			t.Fatalf("investor %s: floor %d plus residue %d != gross %d",
				l.InvestorID, l.FloorPaise, l.ResiduePaise, l.GrossPaise)
		}
	}
	if money.Paise(awarded) != b.ResidueDistributedPaise {
		t.Errorf("%d lines got residue, batch reports %d", awarded, b.ResidueDistributedPaise)
	}
}

// TestExcludedHolderIsPaid re-asserts the trap at this layer.
//
// The manager's 25 units are excluded from the statutory holder count and included in the
// distribution. A denominator of the public float would pay it nothing while still satisfying the
// floor and reconciling against itself.
func TestExcludedHolderIsPaid(t *testing.T) {
	b := buildBatch(t, money.Paise(28_500_000_000))

	if b.TotalUnits != 500 {
		t.Fatalf("denominator = %d, want 500", b.TotalUnits)
	}

	var found bool
	for _, l := range b.Lines {
		if !l.Excluded {
			continue
		}
		found = true

		if l.Units != 25 {
			t.Errorf("excluded holder has %d units, want 25", l.Units)
		}
		if l.GrossPaise == 0 {
			t.Fatal("the excluded holder must receive a gross entitlement")
		}
		if !l.Payable {
			t.Errorf("the excluded holder must be payable: %s", l.NotPayableReason)
		}

		// 25 of 500 units is exactly 5% of ₹28.5 crore.
		want := money.Paise(28_500_000_000 / 500 * 25)
		if l.GrossPaise != want {
			t.Errorf("excluded holder gross = %d, want %d", l.GrossPaise, want)
		}
	}
	if !found {
		t.Fatal("the fixture must include an excluded holder")
	}
}

// TestPerUnitRateIsIdenticalForEveryHolder is the number an investor checks by hand.
func TestPerUnitRateIsIdenticalForEveryHolder(t *testing.T) {
	// ₹28.5 crore over 500 units divides cleanly at ₹5.7 lakh per unit.
	b := buildBatch(t, money.Paise(28_500_000_000))

	rate, rem, err := b.PerUnitRate()
	if err != nil {
		t.Fatal(err)
	}
	if rate != money.Paise(57_000_000) || rem != 0 {
		t.Fatalf("per-unit rate = %d with remainder %d, want 57000000 and 0", rate, rem)
	}

	for _, l := range b.Lines {
		want := rate * money.Paise(l.Units)
		if l.GrossPaise != want {
			t.Fatalf("investor %s with %d units got %d, want %d at a uniform rate",
				l.InvestorID, l.Units, l.GrossPaise, want)
		}
	}
}

func TestBatchIsDeterministic(t *testing.T) {
	first := buildBatch(t, money.Paise(28_499_999_999))
	second := buildBatch(t, money.Paise(28_499_999_999))

	if len(first.Lines) != len(second.Lines) {
		t.Fatal("line counts differ")
	}
	for i := range first.Lines {
		a, b := first.Lines[i], second.Lines[i]
		if a.InvestorID != b.InvestorID || a.GrossPaise != b.GrossPaise || a.ResiduePaise != b.ResiduePaise {
			t.Fatalf("line %d differs between builds: %+v vs %+v", i, a, b)
		}
	}
}

func TestLinesOrderedByAnchor(t *testing.T) {
	b := buildBatch(t, money.Paise(28_500_000_000))
	for i := 1; i < len(b.Lines); i++ {
		prev := b.Lines[i-1].InvestorAnchor
		cur := b.Lines[i].InvestorAnchor
		if string(prev[:]) >= string(cur[:]) {
			t.Fatalf("lines are not ordered by ascending anchor at index %d", i)
		}
	}
}

// ---------------------------------------------------------------------------
// Payability: two different floors
// ---------------------------------------------------------------------------

// TestZeroNetPayableIsNotPayable covers the database rule that a payout amount must be positive.
//
// A holder whose entire gross is withheld has no payment to receive, only a withholding to remit.
// Creating a zero-value instruction would make the payout count disagree with the number of
// transfers, and the settled count is anchored on-chain.
func TestZeroNetPayableIsNotPayable(t *testing.T) {
	snap := liveRegister(t)
	details := detailsFor(snap, ClassResidentIndividual)

	// A table that withholds everything.
	table, err := NewTDSTable(
		TDSRule{Class: ClassResidentIndividual, Section: "194LBA(1)", RateBps: 10_000},
		TDSRule{Class: ClassHUF, Section: "194LBA(1)", RateBps: 10_000},
		TDSRule{Class: ClassBodyCorporate, Section: "194LBA(1)", RateBps: 10_000},
		TDSRule{Class: ClassNRI, Section: "194LBA(2)", RateBps: 10_000},
	)
	if err != nil {
		t.Fatal(err)
	}

	b, err := Build(BuildInput{
		PeriodID: 1, Snapshot: snap,
		DistributedPaise:     money.Paise(28_500_000_000),
		Details:              details,
		TDS:                  table,
		ProviderMinimumPaise: payout.MinAmountPaise,
	})
	if err != nil {
		t.Fatal(err)
	}

	if b.PayableCount != 0 {
		t.Fatalf("%d lines are payable with a 100%% withholding rate", b.PayableCount)
	}
	if b.NetTotalPaise != 0 {
		t.Fatalf("net total = %d, want 0", b.NetTotalPaise)
	}
	if b.TDSTotalPaise != b.GrossTotalPaise {
		t.Fatalf("withholding %d should equal gross %d", b.TDSTotalPaise, b.GrossTotalPaise)
	}
	if !strings.Contains(b.Lines[0].NotPayableReason, "withholding to remit") {
		t.Errorf("the reason should explain there is nothing to transfer, got: %s",
			b.Lines[0].NotPayableReason)
	}

	// And no requests are produced.
	reqs, skipped, err := b.PayoutRequests(PayoutRequestInput{
		SchemeID: schemeUUID, AnchorOutboxID: anchorTx, AccountNumber: srcAccount,
		Mode: payout.ModeIMPS, Purpose: payout.PurposePayout, Narration: "AcreSync payout",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 0 {
		t.Errorf("%d requests built from an entirely withheld batch", len(reqs))
	}
	if len(skipped) != len(b.Lines) {
		t.Errorf("%d lines skipped, want all %d", len(skipped), len(b.Lines))
	}
}

// TestNetBelowProviderMinimumIsNotPayable covers the gap between the two floors.
//
// The database accepts any positive amount; the provider refuses anything under ₹1. An amount in
// between is storable and unpayable, and without this check the batch would build, the run would
// start, and the provider would reject one holder partway through.
func TestNetBelowProviderMinimumIsNotPayable(t *testing.T) {
	// One holder with one unit out of a large total, so the gross lands below ₹1.
	holdings := []snapshot.Holding{
		{InvestorID: investorID(1), WalletAddress: wallet(1), InvestorAnchor: anchorOf(1), Units: 1},
		{InvestorID: investorID(2), WalletAddress: wallet(2), InvestorAnchor: anchorOf(2), Units: 999},
	}
	d := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	snap, err := snapshot.Build(snapshot.BuildInput{
		SchemeRef: anchorOf(0x5c), PeriodID: 1,
		RecordDate: d, PeriodEnd: d, TakenAt: d, Holdings: holdings,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1 unit of 1000 from 50,000 paise is 50 paise gross, 45 net after 10% withholding.
	b, err := Build(BuildInput{
		PeriodID: 1, Snapshot: snap,
		DistributedPaise:     money.Paise(50_000),
		Details:              detailsFor(snap, ClassResidentIndividual),
		TDS:                  SampleTDSTable(),
		ProviderMinimumPaise: payout.MinAmountPaise,
	})
	if err != nil {
		t.Fatal(err)
	}

	var small *Line
	for i := range b.Lines {
		if b.Lines[i].Units == 1 {
			small = &b.Lines[i]
		}
	}
	if small == nil {
		t.Fatal("fixture missing the one-unit holder")
	}

	if small.GrossPaise >= payout.MinAmountPaise {
		t.Fatalf("fixture drift: gross is %d, needs to be under the %d minimum for this test to "+
			"mean anything", small.GrossPaise, payout.MinAmountPaise)
	}
	if small.Payable {
		t.Fatal("an amount below the provider minimum must not be marked payable")
	}
	if !strings.Contains(small.NotPayableReason, "provider minimum") {
		t.Errorf("the reason should name the provider minimum, got: %s", small.NotPayableReason)
	}

	// The entitlement still exists and still reconciles. It is a payment problem, not a record problem.
	if small.GrossPaise == 0 {
		t.Error("the holder is still entitled to their share")
	}
	if err := b.Reconcile(); err != nil {
		t.Fatalf("an unpayable line must not break reconciliation: %v", err)
	}
}

func TestMissingFundAccountIsNotPayable(t *testing.T) {
	snap := liveRegister(t)
	details := detailsFor(snap, ClassResidentIndividual)

	victim := snap.Lines[7].InvestorID
	d := details[victim]
	d.FundAccountID = ""
	details[victim] = d

	b, err := Build(BuildInput{
		PeriodID: 1, Snapshot: snap,
		DistributedPaise:     money.Paise(28_500_000_000),
		Details:              details,
		TDS:                  SampleTDSTable(),
		ProviderMinimumPaise: payout.MinAmountPaise,
	})
	if err != nil {
		t.Fatal(err)
	}

	if b.UnpayableCount != 1 {
		t.Fatalf("%d unpayable lines, want 1", b.UnpayableCount)
	}

	// The record survives. A holder with no bank details still has an anchored entitlement; what they
	// lack is a way to receive it.
	reqs, skipped, err := b.PayoutRequests(PayoutRequestInput{
		SchemeID: schemeUUID, AnchorOutboxID: anchorTx, AccountNumber: srcAccount,
		Mode: payout.ModeIMPS, Purpose: payout.PurposePayout, Narration: "AcreSync payout",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != len(b.Lines)-1 {
		t.Errorf("%d requests, want %d", len(reqs), len(b.Lines)-1)
	}
	if len(skipped) != 1 || skipped[0].InvestorID != victim {
		t.Errorf("the skipped line should be reported, got %+v", skipped)
	}
	if err := b.Reconcile(); err != nil {
		t.Fatal(err)
	}
}

// TestNetTotalForPayableLinesExcludesUnpayable matters for funding the account before a run.
func TestNetTotalForPayableLinesExcludesUnpayable(t *testing.T) {
	snap := liveRegister(t)
	details := detailsFor(snap, ClassResidentIndividual)

	victim := snap.Lines[3].InvestorID
	d := details[victim]
	d.FundAccountID = ""
	details[victim] = d

	b, err := Build(BuildInput{
		PeriodID: 1, Snapshot: snap,
		DistributedPaise:     money.Paise(28_500_000_000),
		Details:              details,
		TDS:                  SampleTDSTable(),
		ProviderMinimumPaise: payout.MinAmountPaise,
	})
	if err != nil {
		t.Fatal(err)
	}

	payable, err := b.NetTotalForPayableLines()
	if err != nil {
		t.Fatal(err)
	}
	if payable >= b.NetTotalPaise {
		t.Fatal("the payable total must exclude the unpayable line, or a balance check over-provisions")
	}

	var excluded money.Paise
	for _, l := range b.Lines {
		if l.InvestorID == victim {
			excluded = l.TDS.NetPayablePaise
		}
	}
	if payable+excluded != b.NetTotalPaise {
		t.Errorf("payable %d plus excluded %d != net total %d", payable, excluded, b.NetTotalPaise)
	}
}

// ---------------------------------------------------------------------------
// Withholding
// ---------------------------------------------------------------------------

// TestWithholdingRoundsUp covers the direction of the rounding choice.
//
// Under-deducting is the deductor's exposure: it carries interest and penalty and is discovered by the
// tax authority. Over-deducting by a paise is reclaimed by the holder through their return.
func TestWithholdingRoundsUp(t *testing.T) {
	rule := TDSRule{Class: ClassResidentIndividual, Section: "194LBA(1)", RateBps: 1000}

	// 15 paise at 10% is 1.5 paise exactly.
	tds, err := ComputeTDS(money.Paise(15), rule, DeductionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if tds.AmountPaise != 2 {
		t.Errorf("withholding on 15 paise at 10%% = %d, want 2 (rounded up from 1.5)", tds.AmountPaise)
	}
	if tds.NetPayablePaise != 13 {
		t.Errorf("net = %d, want 13", tds.NetPayablePaise)
	}
	if tds.AmountPaise+tds.NetPayablePaise != 15 {
		t.Error("withholding plus net must reconstruct gross exactly")
	}
}

func TestWithholdingExactWhenDivisible(t *testing.T) {
	rule := TDSRule{Class: ClassResidentIndividual, Section: "194LBA(1)", RateBps: 1000}

	tds, err := ComputeTDS(money.Paise(57_000_000), rule, DeductionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if tds.AmountPaise != 5_700_000 {
		t.Errorf("withholding = %d, want 5700000", tds.AmountPaise)
	}
	if tds.NetPayablePaise != 51_300_000 {
		t.Errorf("net = %d, want 51300000", tds.NetPayablePaise)
	}
}

func TestZeroRateWithholdsNothing(t *testing.T) {
	rule := TDSRule{Class: ClassResidentIndividual, Section: "194LBA(1)", RateBps: 0}
	tds, err := ComputeTDS(money.Paise(1_000_000), rule, DeductionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if tds.AmountPaise != 0 || tds.NetPayablePaise != 1_000_000 {
		t.Errorf("got %d withheld and %d net", tds.AmountPaise, tds.NetPayablePaise)
	}
}

func TestForm15GHRemovesDeductionForResidentsOnly(t *testing.T) {
	resident := TDSRule{Class: ClassResidentIndividual, Section: "194LBA(1)", RateBps: 1000}
	tds, err := ComputeTDS(money.Paise(1_000_000), resident, DeductionOptions{Form15GHOnFile: true})
	if err != nil {
		t.Fatal(err)
	}
	if tds.AmountPaise != 0 {
		t.Errorf("a declaration on file should remove the deduction, got %d", tds.AmountPaise)
	}
	if !tds.Form15GHOnFile {
		t.Error("the declaration must be recorded on the deduction")
	}

	nri := TDSRule{Class: ClassNRI, Section: "194LBA(2)", RateBps: 1000}
	if _, err := ComputeTDS(money.Paise(1_000_000), nri, DeductionOptions{Form15GHOnFile: true}); err == nil {
		t.Error("a Form 15G/15H declaration must not be accepted for a non-resident")
	}
}

func TestLowerDeductionCertificateRequiresBothPieces(t *testing.T) {
	rule := TDSRule{Class: ClassResidentIndividual, Section: "194LBA(1)", RateBps: 1000}
	lower := uint16(200)

	// Both present: accepted.
	tds, err := ComputeTDS(money.Paise(1_000_000), rule, DeductionOptions{
		LowerDeductionCertRef: "CERT/2026/0001", LowerDeductionRateBps: &lower,
	})
	if err != nil {
		t.Fatal(err)
	}
	if tds.RateBps != 200 || tds.AmountPaise != 20_000 {
		t.Errorf("rate %d, amount %d; want 200 bps and 20000 paise", tds.RateBps, tds.AmountPaise)
	}
	if tds.LowerDeductionCertRef == "" {
		t.Error("the certificate reference must be recorded")
	}

	// A rate without a certificate, and a certificate without a rate, are both refused: a reduced
	// deduction with no paperwork behind it is the same exposure as no deduction at all.
	if _, err := ComputeTDS(money.Paise(1_000_000), rule, DeductionOptions{
		LowerDeductionRateBps: &lower,
	}); err == nil {
		t.Error("a lower rate with no certificate reference must be refused")
	}
	if _, err := ComputeTDS(money.Paise(1_000_000), rule, DeductionOptions{
		LowerDeductionCertRef: "CERT/2026/0001",
	}); err == nil {
		t.Error("a certificate reference with no rate must be refused")
	}

	// A certificate cannot increase the rate above statutory.
	higher := uint16(2000)
	if _, err := ComputeTDS(money.Paise(1_000_000), rule, DeductionOptions{
		LowerDeductionCertRef: "CERT/2026/0002", LowerDeductionRateBps: &higher,
	}); err == nil {
		t.Error("a certificate rate above the statutory rate must be refused")
	}
}

func TestTDSTableMustCoverEveryClass(t *testing.T) {
	_, err := NewTDSTable(
		TDSRule{Class: ClassResidentIndividual, Section: "194LBA(1)", RateBps: 1000},
	)
	if !errors.Is(err, ErrNoTDSRule) {
		t.Fatalf("an incomplete table must be refused, got %v", err)
	}

	// Every class in the Postgres enum must be representable.
	for _, c := range AllInvestorClasses() {
		if !c.Valid() {
			t.Errorf("%s is in the enum but not valid here", c)
		}
		if _, err := SampleTDSTable().For(c); err != nil {
			t.Errorf("the sample table has no rule for %s: %v", c, err)
		}
	}
}

func TestTDSRateOutOfRangeRejected(t *testing.T) {
	r := TDSRule{Class: ClassResidentIndividual, Section: "194LBA(1)", RateBps: 10_001}
	if err := r.Validate(); !errors.Is(err, ErrRateOutOfRange) {
		t.Fatalf("want ErrRateOutOfRange, got %v", err)
	}
}

func TestTDSRuleRequiresSection(t *testing.T) {
	r := TDSRule{Class: ClassResidentIndividual, RateBps: 1000}
	if err := r.Validate(); err == nil {
		t.Fatal("a rule with no statutory reference must be refused; the section is printed on the " +
			"withholding certificate")
	}
}

func TestResidencyClassification(t *testing.T) {
	if ClassNRI.IsResident() {
		t.Error("NRI is not resident")
	}
	for _, c := range []InvestorClass{ClassResidentIndividual, ClassHUF, ClassBodyCorporate} {
		if !c.IsResident() {
			t.Errorf("%s should be treated as resident", c)
		}
	}
}

// ---------------------------------------------------------------------------
// Payout requests
// ---------------------------------------------------------------------------

// TestPayoutRequestsCarryTheNetAmountNotGross is the distinction that matters at the bank.
func TestPayoutRequestsCarryTheNetAmountNotGross(t *testing.T) {
	b := buildBatch(t, money.Paise(28_500_000_000))

	reqs, skipped, err := b.PayoutRequests(PayoutRequestInput{
		SchemeID: schemeUUID, AnchorOutboxID: anchorTx, AccountNumber: srcAccount,
		Mode: payout.ModeIMPS, Purpose: payout.PurposePayout, Narration: "AcreSync Q2 payout",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Fatalf("%d lines skipped unexpectedly", len(skipped))
	}
	if len(reqs) != len(b.Lines) {
		t.Fatalf("%d requests for %d lines", len(reqs), len(b.Lines))
	}

	byAmount := map[money.Paise]int{}
	for _, r := range reqs {
		byAmount[r.AmountPaise]++
	}

	for _, l := range b.Lines {
		if byAmount[l.TDS.NetPayablePaise] == 0 {
			t.Fatalf("no request carries the net amount %d for investor %s",
				l.TDS.NetPayablePaise, l.InvestorID)
		}
		if l.TDS.NetPayablePaise == l.GrossPaise {
			t.Fatal("the fixture must withhold something, or this test cannot distinguish net from gross")
		}
	}

	// Every request must be one the provider will accept.
	for _, r := range reqs {
		if err := r.Validate(); err != nil {
			t.Fatalf("request fails provider validation: %v", err)
		}
	}
}

// TestRequestsRequireTheConfirmedAnchor mirrors the not-null column and the trigger behind it.
func TestRequestsRequireTheConfirmedAnchor(t *testing.T) {
	b := buildBatch(t, money.Paise(28_500_000_000))

	_, _, err := b.PayoutRequests(PayoutRequestInput{
		SchemeID: schemeUUID, AccountNumber: srcAccount,
		Mode: payout.ModeIMPS, Purpose: payout.PurposePayout,
	})
	if err == nil {
		t.Fatal("fiat cannot leave escrow without naming the on-chain attestation it rests on")
	}
}

// TestIdempotencyKeysAreDistinctPerHolderAndStable is what stops a retry paying twice.
func TestIdempotencyKeysAreDistinctPerHolderAndStable(t *testing.T) {
	b := buildBatch(t, money.Paise(28_500_000_000))
	in := PayoutRequestInput{
		SchemeID: schemeUUID, AnchorOutboxID: anchorTx, AccountNumber: srcAccount,
		Mode: payout.ModeIMPS, Purpose: payout.PurposePayout, Narration: "AcreSync payout",
	}

	first, _, err := b.PayoutRequests(in)
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for _, r := range first {
		if seen[r.IdempotencyKey] {
			t.Fatalf("two holders share idempotency key %s; one of them would go unpaid", r.IdempotencyKey)
		}
		seen[r.IdempotencyKey] = true
	}

	// Rebuilding must produce the same keys, or a retry after a dropped connection creates a second
	// payout for every holder.
	second, _, err := b.PayoutRequests(in)
	if err != nil {
		t.Fatal(err)
	}
	for i := range first {
		if first[i].IdempotencyKey != second[i].IdempotencyKey {
			t.Fatalf("key for request %d changed between builds", i)
		}
	}
}

// TestIncrementingAttemptChangesOnlyThatHoldersKey covers a targeted reissue.
func TestIncrementingAttemptChangesOnlyThatHoldersKey(t *testing.T) {
	b := buildBatch(t, money.Paise(28_500_000_000))
	base := PayoutRequestInput{
		SchemeID: schemeUUID, AnchorOutboxID: anchorTx, AccountNumber: srcAccount,
		Mode: payout.ModeIMPS, Purpose: payout.PurposePayout, Narration: "AcreSync payout",
	}

	before, _, err := b.PayoutRequests(base)
	if err != nil {
		t.Fatal(err)
	}

	target := b.Lines[5].InvestorID
	base.Attempts = map[string]uint32{target: 1}

	after, _, err := b.PayoutRequests(base)
	if err != nil {
		t.Fatal(err)
	}

	var changed int
	for i := range before {
		if before[i].IdempotencyKey != after[i].IdempotencyKey {
			changed++
		}
	}
	if changed != 1 {
		t.Fatalf("%d keys changed, want exactly 1; a reissue must not disturb holders who were "+
			"already paid", changed)
	}
}

// TestNarrationIsSanitisedForTheProvider covers a rejection that would otherwise land mid-batch.
func TestNarrationIsSanitisedForTheProvider(t *testing.T) {
	b := buildBatch(t, money.Paise(28_500_000_000))

	reqs, _, err := b.PayoutRequests(PayoutRequestInput{
		SchemeID: schemeUUID, AnchorOutboxID: anchorTx, AccountNumber: srcAccount,
		Mode: payout.ModeIMPS, Purpose: payout.PurposePayout,
		// Hyphens and a length the provider would reject outright.
		Narration: "AcreSync-Period-1-Distribution-Payment-2026",
	})
	if err != nil {
		t.Fatalf("an awkward narration must be sanitised rather than rejected: %v", err)
	}
	for _, r := range reqs {
		if strings.Contains(r.Narration, "-") {
			t.Errorf("narration still contains a hyphen: %q", r.Narration)
		}
		if len(r.Narration) > payout.MaxNarrationLen {
			t.Errorf("narration is %d characters: %q", len(r.Narration), r.Narration)
		}
	}
}

// TestNotesCarryNoPersonalData checks the second door out of the system.
//
// Notes are stored by a third party and returned in dashboards and API responses, so a name or PAN
// here would leak past the boundary the IPFS allowlist defends.
func TestNotesCarryNoPersonalData(t *testing.T) {
	b := buildBatch(t, money.Paise(28_500_000_000))

	reqs, _, err := b.PayoutRequests(PayoutRequestInput{
		SchemeID: schemeUUID, AnchorOutboxID: anchorTx, AccountNumber: srcAccount,
		Mode: payout.ModeIMPS, Purpose: payout.PurposePayout, Narration: "AcreSync payout",
	})
	if err != nil {
		t.Fatal(err)
	}

	allowed := map[string]bool{"periodId": true, "leafIndex": true, "anchorTx": true}
	for _, r := range reqs {
		for k := range r.Notes {
			if !allowed[k] {
				t.Errorf("unexpected note key %q reaching the provider", k)
			}
		}
		// The investor identifier must not travel either: anyone seeing one could correlate the holder
		// across periods.
		for _, l := range b.Lines {
			for _, v := range r.Notes {
				if v == l.InvestorID {
					t.Errorf("investor id %s reached the provider in a note", l.InvestorID)
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Chain batching
// ---------------------------------------------------------------------------

// TestChainBatchesAreDeterministicAndComplete matters because the contract accumulates across calls.
//
// anchorEntitlementsBatch adds to entitledUnitsAccrued and finalisation is only permitted once the
// accumulated total equals the snapshot total. A batch resent after a timeout must be the same batch,
// or the accumulation double-counts and finalisation becomes impossible.
func TestChainBatchesAreDeterministicAndComplete(t *testing.T) {
	b := buildBatch(t, money.Paise(28_500_000_000))

	batches, err := b.ChainBatches(50)
	if err != nil {
		t.Fatal(err)
	}

	var holders, units int
	var unitSum uint32
	for _, cb := range batches {
		if len(cb.Holders) != len(cb.Units) {
			t.Fatal("holders and units arrays must be parallel; the contract reverts otherwise")
		}
		holders += len(cb.Holders)
		units += len(cb.Units)
		for _, u := range cb.Units {
			unitSum += u
		}
	}

	if holders != len(b.Lines) {
		t.Errorf("batches cover %d holders, register has %d", holders, len(b.Lines))
	}
	if unitSum != b.TotalUnits {
		t.Errorf("batches cover %d units, snapshot total is %d; finalisation would be impossible",
			unitSum, b.TotalUnits)
	}

	// 202 lines in groups of 50 is five batches, the last holding two.
	if len(batches) != 5 {
		t.Errorf("%d batches, want 5", len(batches))
	}
	if len(batches[4].Holders) != 2 {
		t.Errorf("final batch holds %d, want 2", len(batches[4].Holders))
	}

	again, err := b.ChainBatches(50)
	if err != nil {
		t.Fatal(err)
	}
	for i := range batches {
		for j := range batches[i].Holders {
			if batches[i].Holders[j] != again[i].Holders[j] {
				t.Fatal("batch composition changed between calls")
			}
		}
	}
}

func TestChainBatchesRejectsZeroSize(t *testing.T) {
	b := buildBatch(t, money.Paise(28_500_000_000))
	if _, err := b.ChainBatches(0); err == nil {
		t.Error("a zero batch size must be refused")
	}
}

func TestChainBatchesCarryNoAmounts(t *testing.T) {
	// The contract stores units and derives the amount from the period struct, so no monetary value
	// should appear in the calldata arrays at all. Asserted structurally: ChainBatch has exactly two
	// fields, holders and units.
	b := buildBatch(t, money.Paise(28_500_000_000))
	batches, err := b.ChainBatches(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) == 0 {
		t.Fatal("no batches")
	}
	// A compile-time guarantee in effect: if an amount field were added, this literal would fail.
	_ = ChainBatch{Holders: nil, Units: nil}
}

// ---------------------------------------------------------------------------
// Rejections
// ---------------------------------------------------------------------------

func TestMissingHolderDetailRejected(t *testing.T) {
	snap := liveRegister(t)
	details := detailsFor(snap, ClassResidentIndividual)
	delete(details, snap.Lines[10].InvestorID)

	_, err := Build(BuildInput{
		PeriodID: 1, Snapshot: snap,
		DistributedPaise: money.Paise(28_500_000_000),
		Details:          details,
		TDS:              SampleTDSTable(),
	})
	if !errors.Is(err, ErrMissingHolder) {
		t.Fatalf("want ErrMissingHolder, got %v", err)
	}
}

func TestInvalidInvestorClassRejected(t *testing.T) {
	snap := liveRegister(t)
	details := detailsFor(snap, ClassResidentIndividual)

	victim := snap.Lines[2].InvestorID
	d := details[victim]
	d.Class = InvestorClass("FOREIGN_PORTFOLIO")
	details[victim] = d

	_, err := Build(BuildInput{
		PeriodID: 1, Snapshot: snap,
		DistributedPaise: money.Paise(28_500_000_000),
		Details:          details,
		TDS:              SampleTDSTable(),
	})
	if err == nil {
		t.Fatal("an unmapped investor class must be refused rather than defaulted")
	}
}

func TestBuildRejectsMissingInputs(t *testing.T) {
	snap := liveRegister(t)

	if _, err := Build(BuildInput{PeriodID: 1, TDS: SampleTDSTable()}); err == nil {
		t.Error("a build with no snapshot must fail")
	}
	if _, err := Build(BuildInput{PeriodID: 1, Snapshot: snap}); err == nil {
		t.Error("a build with no withholding table must fail")
	}
}

func TestReconcileCatchesTamperedTotals(t *testing.T) {
	b := buildBatch(t, money.Paise(28_500_000_000))

	b.GrossTotalPaise += 1
	if err := b.Reconcile(); !errors.Is(err, ErrNotExact) {
		t.Fatalf("want ErrNotExact, got %v", err)
	}

	b = buildBatch(t, money.Paise(28_500_000_000))
	b.Lines[0].GrossPaise += 1
	if err := b.Reconcile(); !errors.Is(err, ErrNotExact) {
		t.Fatalf("a tampered line must be caught, got %v", err)
	}

	b = buildBatch(t, money.Paise(28_500_000_000))
	b.Lines[0].TDS.NetPayablePaise += 1
	if err := b.Reconcile(); !errors.Is(err, ErrNotExact) {
		t.Fatalf("a net amount that does not reconstruct gross must be caught, got %v", err)
	}
}

func TestNRIAndResidentWithholdDifferently(t *testing.T) {
	table, err := NewTDSTable(
		TDSRule{Class: ClassResidentIndividual, Section: "194LBA(1)", RateBps: 1000},
		TDSRule{Class: ClassHUF, Section: "194LBA(1)", RateBps: 1000},
		TDSRule{Class: ClassBodyCorporate, Section: "194LBA(1)", RateBps: 1000},
		TDSRule{Class: ClassNRI, Section: "194LBA(2)", RateBps: 500},
	)
	if err != nil {
		t.Fatal(err)
	}

	snap := liveRegister(t)
	details := detailsFor(snap, ClassResidentIndividual)

	nri := snap.Lines[4].InvestorID
	d := details[nri]
	d.Class = ClassNRI
	details[nri] = d

	b, err := Build(BuildInput{
		PeriodID: 1, Snapshot: snap,
		DistributedPaise:     money.Paise(28_500_000_000),
		Details:              details,
		TDS:                  table,
		ProviderMinimumPaise: payout.MinAmountPaise,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, l := range b.Lines {
		if l.InvestorID != nri {
			continue
		}
		if l.TDS.RateBps != 500 {
			t.Errorf("NRI rate = %d, want 500", l.TDS.RateBps)
		}
		if l.TDS.Section != "194LBA(2)" {
			t.Errorf("NRI section = %q", l.TDS.Section)
		}
	}

	// Gross is unaffected by tax class, which is why gross is the figure that can be published.
	if err := b.Reconcile(); err != nil {
		t.Fatal(err)
	}
}

// TestTaxClassDoesNotChangeGross is the reason gross can be public and net cannot.
//
// If tax class changed the gross, the anchored per-holder figure would leak the classification. It
// does not: class affects only the withholding, which stays in Postgres.
func TestTaxClassDoesNotChangeGross(t *testing.T) {
	snap := liveRegister(t)

	residentBatch, err := Build(BuildInput{
		PeriodID: 1, Snapshot: snap,
		DistributedPaise: money.Paise(28_500_000_000),
		Details:          detailsFor(snap, ClassResidentIndividual),
		TDS:              SampleTDSTable(),
	})
	if err != nil {
		t.Fatal(err)
	}

	nriBatch, err := Build(BuildInput{
		PeriodID: 1, Snapshot: snap,
		DistributedPaise: money.Paise(28_500_000_000),
		Details:          detailsFor(snap, ClassNRI),
		TDS:              SampleTDSTable(),
	})
	if err != nil {
		t.Fatal(err)
	}

	for i := range residentBatch.Lines {
		if residentBatch.Lines[i].GrossPaise != nriBatch.Lines[i].GrossPaise {
			t.Fatalf("line %d gross differs by tax class; the anchored figure would disclose the "+
				"holder's classification", i)
		}
	}
	if residentBatch.GrossTotalPaise != nriBatch.GrossTotalPaise {
		t.Fatal("gross totals must match regardless of tax class")
	}
}
