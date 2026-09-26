package ndcf

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/acresync/orchestrator/internal/ipfsguard"
	"github.com/acresync/orchestrator/internal/money"
)

func ev(s string) [32]byte { return sha256.Sum256([]byte(s)) }

func item(lt LineType, amount money.Paise) LineItem {
	return LineItem{LineType: lt, AmountPaise: amount, EvidenceSHA256: ev(string(lt) + amount.String())}
}

// realisticItems models one quarter for a ₹50 Cr scheme.
//
// Gross rent of ₹3.3 crore with ₹30 lakh of deductions, so NDCF is ₹3 crore. The gap between the two
// is the whole reason this package exists.
func realisticItems() []LineItem {
	return []LineItem{
		item(LineGrossRent, money.Paise(33_000_000_000)),
		item(LineCAMRecovery, money.Paise(1_000_000_000)),
		item(LinePropertyTax, money.Paise(2_000_000_000)),
		item(LineInsurance, money.Paise(300_000_000)),
		item(LineMaintenance, money.Paise(500_000_000)),
		item(LineTrusteeFee, money.Paise(200_000_000)),
		item(LineIMFee, money.Paise(800_000_000)),
		item(LineAuditFee, money.Paise(100_000_000)),
		item(LineStatutoryReserve, money.Paise(600_000_000)),
		item(LineWorkingCapitalReserve, money.Paise(500_000_000)),
	}
}

// ---------------------------------------------------------------------------
// The mistake this package prevents
// ---------------------------------------------------------------------------

// TestFloorAppliesToNetNotGrossRent is the reason the package exists.
//
// The floor applies to net distributable cash flow after deductions, not to rent collected. Computing
// 95% of gross rent errs high, which means paying out money the scheme needed for property tax and
// statutory reserves, and the shortfall surfaces a quarter later as a cash gap nobody budgeted for.
//
// The two figures are far apart on realistic numbers, so the test asserts the distance rather than
// just the value: a regression to a gross-rent basis cannot pass by coincidence.
func TestFloorAppliesToNetNotGrossRent(t *testing.T) {
	res, err := Compute(realisticItems())
	if err != nil {
		t.Fatal(err)
	}

	const grossRent = money.Paise(33_000_000_000)
	if res.ByType[LineGrossRent] != grossRent {
		t.Fatalf("fixture drift: gross rent is %d", res.ByType[LineGrossRent])
	}

	// Inflow ₹3.4 Cr, outflow ₹50 lakh, net ₹2.9 Cr.
	wantNDCF := money.Paise(34_000_000_000 - 5_000_000_000)
	if res.NDCFPaise != wantNDCF {
		t.Fatalf("NDCF = %d, want %d", res.NDCFPaise, wantNDCF)
	}

	floorOnNDCF := res.MinimumDistributablePaise
	floorOnGross, err := MinimumDistributable(grossRent, FloorBps)
	if err != nil {
		t.Fatal(err)
	}

	if floorOnGross <= floorOnNDCF {
		t.Fatal("the fixture must separate the two bases for this test to mean anything")
	}
	if floorOnGross > res.NDCFPaise {
		// Exactly the point: distributing 95% of gross rent would exceed everything the scheme has.
		t.Logf("95%% of gross rent is %d paise, which exceeds the entire NDCF of %d paise",
			floorOnGross, res.NDCFPaise)
	} else {
		t.Fatal("expected the gross-rent basis to overshoot NDCF on these numbers")
	}
}

// ---------------------------------------------------------------------------
// Direction cannot be misstated
// ---------------------------------------------------------------------------

// TestDirectionIsDerivedNotSupplied documents a structural choice.
//
// LineItem has no Direction field, so a caller cannot record a property tax payment as an inflow.
// The database enforces the same rule with a CHECK constraint; the Go type makes the invalid state
// unrepresentable rather than merely rejected.
func TestDirectionIsDerivedNotSupplied(t *testing.T) {
	inflows := map[LineType]bool{LineGrossRent: true, LineCAMRecovery: true, LineInterestIncome: true}

	for _, lt := range AllLineTypes() {
		dir, err := lt.Direction()
		if err != nil {
			t.Fatalf("%s: %v", lt, err)
		}
		want := Outflow
		if inflows[lt] {
			want = Inflow
		}
		if dir != want {
			t.Errorf("%s direction = %s, want %s", lt, dir, want)
		}
	}
}

func TestDirectionMappingIsTotal(t *testing.T) {
	// Every type the Postgres enum knows must be mapped, or a valid row fails to round-trip.
	pgEnum := []string{
		"GROSS_RENT", "CAM_RECOVERY", "INTEREST_INCOME",
		"PROPERTY_TAX", "INSURANCE", "MAINTENANCE",
		"TRUSTEE_FEE", "IM_FEE", "VALUER_FEE", "AUDIT_FEE",
		"STATUTORY_RESERVE", "WORKING_CAPITAL_RESERVE",
		"DEBT_SERVICE", "OTHER",
	}
	for _, name := range pgEnum {
		if !LineType(name).Valid() {
			t.Errorf("%s exists in the database enum but is unmapped here", name)
		}
	}
	if len(AllLineTypes()) != len(pgEnum) {
		t.Errorf("AllLineTypes has %d entries, the database enum has %d",
			len(AllLineTypes()), len(pgEnum))
	}
}

func TestUnknownLineTypeRejected(t *testing.T) {
	_, err := Compute([]LineItem{{
		LineType: LineType("RENTAL_INCOME"), AmountPaise: 100, EvidenceSHA256: ev("x"),
	}})
	if !errors.Is(err, ErrUnknownLineType) {
		t.Fatalf("want ErrUnknownLineType, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// The ceiling-division trap
// ---------------------------------------------------------------------------

// TestMinimumDistributableRoundsUp covers the defect that looks correct in review.
//
// The floor holds when distributed × 10000 ≥ ndcf × 9500. The smallest such value is the quotient
// rounded up. Truncating gives a figure short by one paise whenever the division is inexact, and the
// floor then fails by exactly that paise, presenting as a compliance breach rather than a rounding
// bug.
//
// Checked exhaustively over a range, and then against the boundary values where the product is
// inexact.
func TestMinimumDistributableRoundsUp(t *testing.T) {
	for ndcf := money.Paise(1); ndcf <= 5000; ndcf++ {
		minimum, err := MinimumDistributable(ndcf, FloorBps)
		if err != nil {
			t.Fatalf("ndcf %d: %v", ndcf, err)
		}

		ok, err := money.MeetsFloorBps(minimum, ndcf, FloorBps)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("ndcf %d: minimum %d does not meet the floor", ndcf, minimum)
		}

		// And it must be the *smallest* such value, or the scheme retains less than it could.
		if minimum > 1 {
			below, err := money.MeetsFloorBps(minimum-1, ndcf, FloorBps)
			if err != nil {
				t.Fatal(err)
			}
			if below {
				t.Fatalf("ndcf %d: %d also meets the floor, so %d is not minimal",
					ndcf, minimum-1, minimum)
			}
		}
	}
}

// TestMinimumDistributableAtInexactBoundaries names the concrete case.
func TestMinimumDistributableAtInexactBoundaries(t *testing.T) {
	cases := []money.Paise{
		30_000_000_001, // ₹3,00,00,000.01, where ndcf × 9500 / 10000 is not an integer
		30_000_000_003,
		1,
		7,
		9999,
		10_001,
		5_000_000_000_0, // ₹50 Cr in paise
	}

	for _, ndcf := range cases {
		minimum, err := MinimumDistributable(ndcf, FloorBps)
		if err != nil {
			t.Fatalf("ndcf %d: %v", ndcf, err)
		}

		// The truncating computation, which is the bug being guarded against.
		truncated, _, err := ndcf.MulDivFloor(int64(FloorBps), 10_000)
		if err != nil {
			t.Fatal(err)
		}

		ok, err := money.MeetsFloorBps(truncated, ndcf, FloorBps)
		if err != nil {
			t.Fatal(err)
		}
		if !ok && minimum != truncated+1 {
			t.Errorf("ndcf %d: truncation gives %d which fails the floor, so the minimum should be "+
				"%d, got %d", ndcf, truncated, truncated+1, minimum)
		}
		if ok && minimum != truncated {
			t.Errorf("ndcf %d: truncation gives %d which meets the floor, so it is already minimal, "+
				"got %d", ndcf, truncated, minimum)
		}
	}
}

// TestNoOverflowAtLargeNDCF confirms the intermediate product cannot wrap.
//
// An int64 multiplication by 10000 overflows above roughly 9.2×10^14 paise. Overflow does not error,
// it wraps, and a wrapped product passes a floor it should fail.
func TestNoOverflowAtLargeNDCF(t *testing.T) {
	huge := money.Paise(9_000_000_000_000_000) // far beyond any real scheme
	minimum, err := MinimumDistributable(huge, FloorBps)
	if err != nil {
		t.Fatalf("large NDCF must not overflow: %v", err)
	}
	ok, err := money.MeetsFloorBps(minimum, huge, FloorBps)
	if err != nil || !ok {
		t.Fatalf("floor check failed at scale: ok=%v err=%v", ok, err)
	}
}

// ---------------------------------------------------------------------------
// Compute
// ---------------------------------------------------------------------------

func TestComputeTotals(t *testing.T) {
	res, err := Compute(realisticItems())
	if err != nil {
		t.Fatal(err)
	}

	if res.TotalInflowPaise != money.Paise(34_000_000_000) {
		t.Errorf("inflow = %d", res.TotalInflowPaise)
	}
	if res.TotalOutflowPaise != money.Paise(5_000_000_000) {
		t.Errorf("outflow = %d", res.TotalOutflowPaise)
	}
	if res.ReservePaise != money.Paise(1_100_000_000) {
		t.Errorf("reserves = %d, want the statutory and working capital lines only", res.ReservePaise)
	}
	if res.FeePaise != money.Paise(1_100_000_000) {
		t.Errorf("fees = %d, want trustee, IM and audit", res.FeePaise)
	}
	if res.LineCount != 10 {
		t.Errorf("line count = %d", res.LineCount)
	}

	// Inflow minus outflow, exactly.
	sum, err := res.TotalInflowPaise.Sub(res.TotalOutflowPaise)
	if err != nil {
		t.Fatal(err)
	}
	if res.NDCFPaise != sum {
		t.Errorf("NDCF %d is not inflow less outflow (%d)", res.NDCFPaise, sum)
	}
}

func TestSameTypeAccumulates(t *testing.T) {
	res, err := Compute([]LineItem{
		{LineType: LineGrossRent, AmountPaise: 100, EvidenceSHA256: ev("a")},
		{LineType: LineGrossRent, AmountPaise: 250, EvidenceSHA256: ev("b")},
		{LineType: LinePropertyTax, AmountPaise: 50, EvidenceSHA256: ev("c")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ByType[LineGrossRent] != 350 {
		t.Errorf("two rent lines should total 350, got %d", res.ByType[LineGrossRent])
	}
	if res.NDCFPaise != 300 {
		t.Errorf("NDCF = %d, want 300", res.NDCFPaise)
	}
}

// TestOutflowsExceedingInflowsRefused covers the period that cannot proceed.
//
// 95% of zero or of a negative number is not a distribution instruction, and the database requires a
// positive NDCF. Returning a negative figure and letting a later layer notice would risk anchoring a
// distribution of nothing and calling the quarter settled.
func TestOutflowsExceedingInflowsRefused(t *testing.T) {
	cases := map[string][]LineItem{
		"outflow exceeds inflow": {
			item(LineGrossRent, 1_000),
			item(LinePropertyTax, 1_500),
		},
		"exactly break even": {
			item(LineGrossRent, 1_000),
			item(LinePropertyTax, 1_000),
		},
	}

	for name, items := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Compute(items); !errors.Is(err, ErrNotDistributable) {
				t.Fatalf("want ErrNotDistributable, got %v", err)
			}
		})
	}
}

func TestNonPositiveAmountRejected(t *testing.T) {
	for _, amount := range []money.Paise{0, -1, -100_000} {
		_, err := Compute([]LineItem{
			item(LineGrossRent, 1_000),
			{LineType: LinePropertyTax, AmountPaise: amount, EvidenceSHA256: ev("x")},
		})
		if !errors.Is(err, ErrNonPositiveAmount) {
			t.Errorf("amount %d: want ErrNonPositiveAmount, got %v", amount, err)
		}
	}
}

// TestEvidenceRequiredOnEveryLine guards the audit chain.
//
// An unevidenced deduction is an assertion, and deductions determine the number the regulatory floor
// is computed against. One missing digest moves that number on somebody's word.
func TestEvidenceRequiredOnEveryLine(t *testing.T) {
	_, err := Compute([]LineItem{
		item(LineGrossRent, 1_000),
		{LineType: LinePropertyTax, AmountPaise: 100}, // zero digest
	})
	if !errors.Is(err, ErrMissingEvidence) {
		t.Fatalf("want ErrMissingEvidence, got %v", err)
	}
}

func TestEmptyPeriodRejected(t *testing.T) {
	if _, err := Compute(nil); !errors.Is(err, ErrNoLineItems) {
		t.Fatalf("want ErrNoLineItems, got %v", err)
	}
}

func TestOverflowDetectedInTotals(t *testing.T) {
	_, err := Compute([]LineItem{
		{LineType: LineGrossRent, AmountPaise: money.Paise(1) << 62, EvidenceSHA256: ev("a")},
		{LineType: LineGrossRent, AmountPaise: money.Paise(1) << 62, EvidenceSHA256: ev("b")},
		{LineType: LineGrossRent, AmountPaise: money.Paise(1) << 62, EvidenceSHA256: ev("c")},
	})
	if err == nil {
		t.Fatal("a wrapped sum produces a plausible smaller number and must be an error")
	}
}

// ---------------------------------------------------------------------------
// Reserves
// ---------------------------------------------------------------------------

// TestReserveRatioIsSurfaced covers the lever for suppressing NDCF.
//
// Reserves are the only deduction class with no external counterparty, so they are the natural way to
// distribute 95% of a deliberately small number while appearing compliant. Nothing here can tell
// prudent reserving from that, so the ratio is computed and reported rather than judged.
func TestReserveRatioIsSurfaced(t *testing.T) {
	res, err := Compute(realisticItems())
	if err != nil {
		t.Fatal(err)
	}

	// ₹11,000,000 of reserves against ₹340,000,000 of inflow is about 324 bps.
	if res.ReserveRatioBps == 0 {
		t.Fatal("the reserve ratio must be computed")
	}
	if res.ReserveConcern() {
		t.Errorf("%d bps of reserves should not trip the concern threshold of %d",
			res.ReserveRatioBps, ReserveConcernBps)
	}

	// Now an aggressive reserve.
	heavy := []LineItem{
		item(LineGrossRent, money.Paise(10_000_000_000)),
		item(LineStatutoryReserve, money.Paise(4_000_000_000)),
	}
	res2, err := Compute(heavy)
	if err != nil {
		t.Fatal(err)
	}
	if !res2.ReserveConcern() {
		t.Errorf("reserving %d bps of inflow must be flagged", res2.ReserveRatioBps)
	}

	// The floor is still satisfied on the suppressed figure, which is exactly why the ratio has to be
	// visible: compliance with the floor is not evidence that the basis was honest.
	plan, err := PlanAtFloor(res2)
	if err != nil {
		t.Fatal(err)
	}
	if plan.DistributionBps < FloorBps {
		t.Error("the plan should still satisfy the floor on the reduced NDCF")
	}
}

// ---------------------------------------------------------------------------
// Plans
// ---------------------------------------------------------------------------

func TestPlanAtFloorMeetsRequirement(t *testing.T) {
	res, err := Compute(realisticItems())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanAtFloor(res)
	if err != nil {
		t.Fatal(err)
	}

	if plan.DistributedPaise != res.MinimumDistributablePaise {
		t.Errorf("distributed = %d, want the minimum %d",
			plan.DistributedPaise, res.MinimumDistributablePaise)
	}
	if plan.DistributionBps < FloorBps {
		t.Errorf("bps = %d, must be at least %d", plan.DistributionBps, FloorBps)
	}
	if plan.DistributionBps > 10_000 {
		t.Errorf("bps = %d exceeds 100%%, which the database CHECK rejects", plan.DistributionBps)
	}

	retained, err := res.NDCFPaise.Sub(plan.DistributedPaise)
	if err != nil {
		t.Fatal(err)
	}
	if plan.RetainedPaise != retained {
		t.Errorf("retained = %d, want %d", plan.RetainedPaise, retained)
	}
}

func TestPlanBelowFloorRejected(t *testing.T) {
	res, err := Compute(realisticItems())
	if err != nil {
		t.Fatal(err)
	}

	_, err = PlanDistribution(res, res.MinimumDistributablePaise-1)
	if !errors.Is(err, ErrBelowFloor) {
		t.Fatalf("one paise below the minimum must be rejected, got %v", err)
	}
	if !strings.Contains(err.Error(), res.MinimumDistributablePaise.String()) {
		t.Error("the error should state the minimum so an operator can correct it")
	}
}

func TestPlanExceedingNDCFRejected(t *testing.T) {
	res, err := Compute(realisticItems())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PlanDistribution(res, res.NDCFPaise+1); !errors.Is(err, ErrExceedsNDCF) {
		t.Fatalf("want ErrExceedsNDCF, got %v", err)
	}
}

func TestFullDistributionIsPermitted(t *testing.T) {
	res, err := Compute(realisticItems())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanDistribution(res, res.NDCFPaise)
	if err != nil {
		t.Fatalf("distributing all of NDCF must be allowed: %v", err)
	}
	if plan.DistributionBps != 10_000 {
		t.Errorf("bps = %d, want exactly 10000", plan.DistributionBps)
	}
	if plan.RetainedPaise != 0 {
		t.Errorf("retained = %d, want 0", plan.RetainedPaise)
	}
}

// ---------------------------------------------------------------------------
// The statement document
// ---------------------------------------------------------------------------

func statementInput(t *testing.T) StatementInput {
	t.Helper()
	res, err := Compute(realisticItems())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanAtFloor(res)
	if err != nil {
		t.Fatal(err)
	}
	return StatementInput{
		SchemeRef:   ev("scheme"),
		PeriodID:    1,
		PeriodStart: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		PeriodEnd:   time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC),
		Plan:        plan,
		Items:       realisticItems(),
	}
}

// TestStatementPassesTheFieldAllowlist is the check that the document is publishable.
//
// Building JSON that the guard then rejects would fail at pin time, midway through a period, so the
// shape is verified here against the same validator the publisher uses.
func TestStatementPassesTheFieldAllowlist(t *testing.T) {
	raw, err := BuildStatement(statementInput(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ipfsguard.ValidateAndCanonicalize(ipfsguard.DocNDCFStatement, raw); err != nil {
		t.Fatalf("the statement must satisfy the allowlist: %v", err)
	}
}

// TestStatementOmitsDescription checks the field that could carry personal data.
//
// Description is the one line-item field that might hold a tenant name. IPFS content cannot be
// withdrawn, so omission here is the first defence and the allowlist is the second. This test covers
// the first; the allowlist rejecting unknown keys is covered in its own package.
func TestStatementOmitsDescription(t *testing.T) {
	in := statementInput(t)
	for i := range in.Items {
		in.Items[i].Description = "Tenant Ramesh Kumar, invoice 4471"
	}

	raw, err := BuildStatement(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "Ramesh") {
		t.Fatal("a description reached the document; IPFS content cannot be withdrawn once published")
	}
	if strings.Contains(string(raw), "description") {
		t.Error("the description key must not appear at all")
	}
}

// TestStatementIsOrderIndependent is what makes the anchored digest meaningful.
//
// The statement digest goes on-chain, so the bytes have to be a function of content alone. Rebuilt
// from the database after a reconciliation, or assembled by a second service, the rows would arrive in
// a different order, and an unstable layout would give the same period two anchors.
func TestStatementIsOrderIndependent(t *testing.T) {
	in := statementInput(t)

	forward, err := BuildStatement(in)
	if err != nil {
		t.Fatal(err)
	}

	reversed := statementInput(t)
	items := reversed.Items
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
	backward, err := BuildStatement(reversed)
	if err != nil {
		t.Fatal(err)
	}

	if string(forward) != string(backward) {
		t.Fatal("reversing the input order changed the document, so the same period would produce " +
			"two different anchors")
	}
}

// TestStatementDoesNotMutateCallerInput covers the side effect that sorting would otherwise cause.
func TestStatementDoesNotMutateCallerInput(t *testing.T) {
	in := statementInput(t)
	before := make([]LineType, len(in.Items))
	for i, it := range in.Items {
		before[i] = it.LineType
	}

	if _, err := BuildStatement(in); err != nil {
		t.Fatal(err)
	}
	for i, it := range in.Items {
		if it.LineType != before[i] {
			t.Fatalf("rendering reordered the caller's slice at index %d", i)
		}
	}
}

func TestStatementCarriesDirectionRedundantly(t *testing.T) {
	raw, err := BuildStatement(statementInput(t))
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		LineItems []struct {
			LineType  string `json:"lineType"`
			Direction string `json:"direction"`
		} `json:"lineItems"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.LineItems) != 10 {
		t.Fatalf("%d line items", len(doc.LineItems))
	}

	// Emitting direction alongside type means a verifier can check the arithmetic without knowing our
	// mapping, and makes the mapping itself auditable.
	for _, li := range doc.LineItems {
		want, err := LineType(li.LineType).Direction()
		if err != nil {
			t.Fatal(err)
		}
		if li.Direction != string(want) {
			t.Errorf("%s carries direction %s, want %s", li.LineType, li.Direction, want)
		}
	}
}

func TestStatementFiguresMatchThePlan(t *testing.T) {
	in := statementInput(t)
	raw, err := BuildStatement(in)
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		NDCFPaise        money.Paise `json:"ndcfPaise"`
		DistributedPaise money.Paise `json:"distributedPaise"`
		DistributionBps  uint16      `json:"distributionBps"`
		Currency         string      `json:"currency"`
		PeriodStart      string      `json:"periodStart"`
		PeriodEnd        string      `json:"periodEnd"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	if doc.NDCFPaise != in.Plan.NDCFPaise {
		t.Errorf("ndcfPaise = %d, want %d", doc.NDCFPaise, in.Plan.NDCFPaise)
	}
	if doc.DistributedPaise != in.Plan.DistributedPaise {
		t.Errorf("distributedPaise = %d, want %d", doc.DistributedPaise, in.Plan.DistributedPaise)
	}
	if doc.DistributionBps != in.Plan.DistributionBps {
		t.Errorf("distributionBps = %d, want %d", doc.DistributionBps, in.Plan.DistributionBps)
	}
	if doc.Currency != "INR" {
		t.Errorf("currency = %q", doc.Currency)
	}
	if doc.PeriodStart != "2026-07-01" || doc.PeriodEnd != "2026-09-30" {
		t.Errorf("dates = %s to %s", doc.PeriodStart, doc.PeriodEnd)
	}
}

func TestStatementRejectsInvertedDates(t *testing.T) {
	in := statementInput(t)
	in.PeriodStart, in.PeriodEnd = in.PeriodEnd, in.PeriodStart

	if _, err := BuildStatement(in); err == nil {
		t.Fatal("a period ending before it starts must be refused")
	}
}

func TestStatementHexIsLowercase(t *testing.T) {
	raw, err := BuildStatement(statementInput(t))
	if err != nil {
		t.Fatal(err)
	}
	// The allowlist rejects uppercase hex so that one value has exactly one spelling. Two spellings
	// would give one document two encodings and therefore two anchors.
	for _, frag := range []string{"0xA", "0xB", "0xC", "0xD", "0xE", "0xF"} {
		if strings.Contains(string(raw), frag) {
			t.Errorf("uppercase hex %q found in the document", frag)
		}
	}
}

func TestSortedTypesIsStable(t *testing.T) {
	res, err := Compute(realisticItems())
	if err != nil {
		t.Fatal(err)
	}
	first := res.SortedTypes()
	for i := 0; i < 20; i++ {
		got := res.SortedTypes()
		for j := range got {
			if got[j] != first[j] {
				t.Fatal("SortedTypes must not depend on map iteration order")
			}
		}
	}
}

func TestOptionalReferencesOmittedWhenAbsent(t *testing.T) {
	in := statementInput(t)
	raw, err := BuildStatement(in)
	if err != nil {
		t.Fatal(err)
	}

	// No SPV or property refs were set, so the keys must be absent rather than empty. The allowlist
	// types them as UUIDs, and an empty string would fail validation.
	if strings.Contains(string(raw), `"spvRef":""`) || strings.Contains(string(raw), `"propertyRef":""`) {
		t.Error("absent references must be omitted, not emitted empty")
	}

	in.Items[0].SPVRef = "6f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8"
	raw, err = BuildStatement(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "6f1a2b3c") {
		t.Error("a supplied SPV reference must appear")
	}
	if _, err := ipfsguard.ValidateAndCanonicalize(ipfsguard.DocNDCFStatement, raw); err != nil {
		t.Fatalf("a statement carrying an SPV reference must still validate: %v", err)
	}
}
