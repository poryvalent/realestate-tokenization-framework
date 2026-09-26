package settlement

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/acresync/orchestrator/internal/asba"
	"github.com/acresync/orchestrator/internal/ballot"
	"github.com/acresync/orchestrator/internal/ballotrun"
	"github.com/acresync/orchestrator/internal/bidbook"
	"github.com/acresync/orchestrator/internal/idempotency"
	"github.com/acresync/orchestrator/internal/merkle"
	"github.com/acresync/orchestrator/internal/money"
)

const (
	testOfferID  = "3f2a1c4e-5b6d-4e8f-9a0b-1c2d3e4f5a6b"
	testSchemeID = "8d7c6b5a-4e3f-4a2b-8c1d-0e9f8a7b6c5d"
	imWallet     = "0x00000000000000000000000000000000000000aa"
)

var (
	testPepper = []byte("settlement-test-pepper")
	frozenAt   = time.Date(2026, 4, 1, 9, 30, 0, 0, time.UTC)
	drawnAt    = time.Date(2026, 4, 3, 11, 0, 0, 0, time.UTC)
)

func hashFor(n int) merkle.Hash {
	var h merkle.Hash
	binary.BigEndian.PutUint32(h[:4], uint32(0xB0000000+n))
	h[31] = byte(n)
	return h
}

func bidRefFor(n int) string {
	var b [16]byte
	binary.BigEndian.PutUint32(b[:4], uint32(n*2654435761))
	binary.BigEndian.PutUint32(b[12:], uint32(n))
	return hex.EncodeToString(b[:])
}

func walletFor(n int) string { return fmt.Sprintf("0x%040x", n) }

// drawFixture produces a real drawn ballot: 500 single-unit bids for 475 units.
func drawFixture(t *testing.T) *ballotrun.Run {
	t.Helper()

	at := frozenAt.Add(-24 * time.Hour)
	es := make([]bidbook.Entry, 0, 500)
	for i := 1; i <= 500; i++ {
		total := money.MinUnitPricePaise
		es = append(es, bidbook.Entry{
			BidID:             fmt.Sprintf("bid-%04d", i),
			InvestorID:        fmt.Sprintf("inv-%04d", i),
			BidRef:            bidRefFor(i),
			InvestorAnchor:    hashFor(i),
			UnitsBid:          1,
			PricePerUnitPaise: money.MinUnitPricePaise,
			Block: &asba.Block{
				BidID:          fmt.Sprintf("bid-%04d", i),
				Status:         asba.StatusBlocked,
				RequestedPaise: total,
				BlockedPaise:   total,
				RequestedAt:    at,
				BlockedAt:      &at,
			},
		})
	}

	book, err := bidbook.Freeze(bidbook.FreezeInput{
		SchemeRef: hashFor(0x5c), SchemeID: testSchemeID, OfferID: testOfferID,
		FrozenAt: frozenAt, Entries: es,
	})
	if err != nil {
		t.Fatalf("freezing: %v", err)
	}

	secret, err := ballotrun.DeriveSecret(testPepper, testSchemeID, testOfferID)
	if err != nil {
		t.Fatal(err)
	}

	cer := &ballotrun.Ceremony{
		SchemeID: testSchemeID, OfferID: testOfferID,
		Stage: ballotrun.StageSeedRevealed, BidbookRoot: book.MerkleRoot,
		Commitment: ballot.Commitment(secret), CommitmentAnchored: true,
		TargetBlock: 1_000_000, Attempt: 1, Config: ballotrun.V1Config(),
	}

	run, err := ballotrun.Draw(ballotrun.RunInput{
		Book: book, Ceremony: cer, Secret: secret,
		TargetBlockHash: hashFor(0xB10C), Params: ballot.V1Params(), DrawnAt: drawnAt,
	})
	if err != nil {
		t.Fatalf("drawing: %v", err)
	}
	return run
}

// walletsFor maps every allottee to a distinct address.
func walletsFor(run *ballotrun.Run) map[string]string {
	out := make(map[string]string, len(run.Lines))
	for i, l := range run.Lines {
		out[l.InvestorID] = walletFor(i + 1000)
	}
	return out
}

func baseInput(t *testing.T) BuildInput {
	t.Helper()
	run := drawFixture(t)
	return BuildInput{
		Run:               run,
		Params:            V1Params(),
		Wallets:           walletsFor(run),
		IMWallet:          imWallet,
		AllotmentFileHash: hashFor(0xF11E),
		BallotResultRoot:  run.Result.ResultRoot,
	}
}

func buildPlan(t *testing.T) *Plan {
	t.Helper()
	p, err := BuildPlan(baseInput(t))
	if err != nil {
		t.Fatalf("building a valid plan: %v", err)
	}
	return p
}

// --- the arithmetic that decides finalisation ----------------------------------------------------

// TestPublicSideIsTheBallotAllocationNotTheSchemeTotal is the core sum of the whole task.
//
// The manager's units are credited outside the settlement cursor, so expectedUnits is 475 and not 500.
// Declaring 500 would make every batch fail the running check; omitting the manager's subscription
// leaves the register 25 short and finaliseSettlement reverts on its first check.
func TestPublicSideIsTheBallotAllocationNotTheSchemeTotal(t *testing.T) {
	p := buildPlan(t)

	if p.ExpectedUnits != 475 {
		t.Fatalf("expectedUnits must be the public allocation of 475, got %d", p.ExpectedUnits)
	}
	if p.ExpectedUnits == p.Params.TotalUnits {
		t.Fatal("expectedUnits must not be the scheme total; the manager is credited separately")
	}
	if p.ExpectedUnits+p.Params.IMUnits != p.Params.TotalUnits {
		t.Fatalf("%d public plus %d manager must equal the scheme's %d",
			p.ExpectedUnits, p.Params.IMUnits, p.Params.TotalUnits)
	}
	if p.ExpectedHolders != 475 {
		t.Fatalf("expectedHolders must be the 475 allottees, got %d", p.ExpectedHolders)
	}
}

// TestOnlyAllotteesAreCredited keeps zero-unit entries out of every batch.
//
// settleBatch reverts on a zero amount inside its loop, which fails the whole batch rather than skipping
// the entry, so a nil bid in a batch would cost the batch and not just the line.
func TestOnlyAllotteesAreCredited(t *testing.T) {
	run := drawFixture(t)
	p := buildPlan(t)

	if len(run.Lines) != 500 {
		t.Fatalf("the draw should cover all 500 bids, got %d", len(run.Lines))
	}
	if len(p.Entries) != 475 {
		t.Fatalf("only the 475 allottees are credited, got %d entries", len(p.Entries))
	}
	for _, e := range p.Entries {
		if e.Units == 0 {
			t.Fatalf("leaf %d credits zero units", e.LeafIndex)
		}
	}
}

// --- batching and the cursor ---------------------------------------------------------------------

func TestBatchesRespectTheContractCapAndCoverEveryEntry(t *testing.T) {
	p := buildPlan(t)

	// 475 entries at a cap of 100 is five batches: four of 100 and one of 75.
	if len(p.Batches) != 5 {
		t.Fatalf("want 5 batches, got %d", len(p.Batches))
	}

	var seen int
	for i, b := range p.Batches {
		if uint32(len(b.Entries)) > p.Params.MaxBatchSize {
			t.Fatalf("batch %d has %d entries, cap is %d", i, len(b.Entries), p.Params.MaxBatchSize)
		}
		seen += len(b.Entries)
	}
	if seen != 475 {
		t.Fatalf("the batches cover %d of 475 entries", seen)
	}
	if got := len(p.Batches[4].Entries); got != 75 {
		t.Fatalf("the final batch should hold the remaining 75, got %d", got)
	}
}

// TestCursorsAreContiguousAndStrict mirrors the contract's equality check.
//
// cursorFrom must equal creditedHolders exactly, so batches cannot overlap, skip or be reordered.
func TestCursorsAreContiguousAndStrict(t *testing.T) {
	p := buildPlan(t)

	var cursor uint32
	for i, b := range p.Batches {
		if b.CursorFrom != cursor {
			t.Fatalf("batch %d starts at cursor %d, want %d", i, b.CursorFrom, cursor)
		}
		cursor = b.CursorTo()
	}
	if cursor != p.ExpectedHolders {
		t.Fatalf("the cursor ends at %d, want %d", cursor, p.ExpectedHolders)
	}
}

func TestEntriesAreOrderedByLeafIndex(t *testing.T) {
	p := buildPlan(t)

	for i := 1; i < len(p.Entries); i++ {
		if p.Entries[i-1].LeafIndex >= p.Entries[i].LeafIndex {
			t.Fatalf("entries %d and %d are out of leaf order: %d then %d",
				i-1, i, p.Entries[i-1].LeafIndex, p.Entries[i].LeafIndex)
		}
	}
}

// TestBatchAtOnlyAcceptsTheCurrentCursor is the retry path.
//
// A gas-exhausted batch did not advance the cursor, so the same batch is served again. Any other cursor
// has no batch, because the contract would reject it.
func TestBatchAtOnlyAcceptsTheCurrentCursor(t *testing.T) {
	p := buildPlan(t)

	b, err := p.BatchAt(0)
	if err != nil {
		t.Fatal(err)
	}
	if b.CursorFrom != 0 || len(b.Entries) != 100 {
		t.Fatalf("cursor 0 should serve the first batch of 100, got %d entries at %d",
			len(b.Entries), b.CursorFrom)
	}

	// Retrying the same cursor serves the identical batch.
	again, err := p.BatchAt(0)
	if err != nil {
		t.Fatal(err)
	}
	if again.CursorFrom != b.CursorFrom || len(again.Entries) != len(b.Entries) {
		t.Fatal("a gas-exhausted batch must be retryable verbatim")
	}

	// A cursor in the middle of a batch has no batch: the contract would reject it.
	if _, err := p.BatchAt(50); !errors.Is(err, ErrCursorMismatch) {
		t.Fatalf("cursor 50 falls inside a batch and must not resolve, got %v", err)
	}

	// The terminal cursor points at finalisation rather than a batch.
	_, err = p.BatchAt(475)
	if err == nil {
		t.Fatal("cursor 475 is past the last batch")
	}
	if !strings.Contains(err.Error(), "finaliseSettlement") {
		t.Errorf("the error should point at the next step: %v", err)
	}
}

func TestValidateCatchesATamperedCursor(t *testing.T) {
	p := buildPlan(t)
	p.Batches[2].CursorFrom = 250 // should be 200

	if err := p.Validate(); !errors.Is(err, ErrCursorMismatch) {
		t.Fatalf("want ErrCursorMismatch, got %v", err)
	}
}

func TestValidateCatchesAnOversizedBatch(t *testing.T) {
	p := buildPlan(t)
	p.Params.MaxBatchSize = 50

	if err := p.Validate(); !errors.Is(err, ErrBatchTooLarge) {
		t.Fatalf("want ErrBatchTooLarge, got %v", err)
	}
}

// --- the wallet checks ---------------------------------------------------------------------------

// TestDuplicateWalletIsRefused is the check the contract cannot make.
//
// settleBatch credits additively, so two investors sharing an address both land on it. The units still
// sum, the cursor still advances twice, totalUnitsIssued still reaches 500, and distinctHolderCount ends
// one short while remaining above the floor. All five finalisation checks pass on a register that has
// lost a holder and commingled two investors' units in one address.
func TestDuplicateWalletIsRefused(t *testing.T) {
	in := baseInput(t)

	// Two allottees pointed at one address.
	var first, second string
	for _, l := range in.Run.Allottees() {
		if first == "" {
			first = l.InvestorID
			continue
		}
		second = l.InvestorID
		break
	}
	in.Wallets[second] = in.Wallets[first]

	_, err := BuildPlan(in)
	if !errors.Is(err, ErrDuplicateWallet) {
		t.Fatalf("want ErrDuplicateWallet, got %v", err)
	}
	if !strings.Contains(err.Error(), "additively") {
		t.Errorf("the error should explain why the contract cannot catch this: %v", err)
	}
}

// TestAllotteeSharingTheManagerWalletIsRefused closes the other half of the same hole.
//
// The manager's address is excluded from the holder count, so a public allottee credited to it would
// vanish from the statutory count while still being owed a distribution.
func TestAllotteeSharingTheManagerWalletIsRefused(t *testing.T) {
	in := baseInput(t)
	in.Wallets[in.Run.Allottees()[0].InvestorID] = imWallet

	_, err := BuildPlan(in)
	if !errors.Is(err, ErrDuplicateWallet) {
		t.Fatalf("want ErrDuplicateWallet, got %v", err)
	}
	if !strings.Contains(err.Error(), "excluded") {
		t.Errorf("the error should say why the manager's wallet is special: %v", err)
	}
}

func TestMissingWalletIsRefused(t *testing.T) {
	in := baseInput(t)
	delete(in.Wallets, in.Run.Allottees()[3].InvestorID)

	if _, err := BuildPlan(in); !errors.Is(err, ErrZeroWallet) {
		t.Fatalf("want ErrZeroWallet, got %v", err)
	}
}

func TestZeroAddressIsRefused(t *testing.T) {
	in := baseInput(t)
	in.Wallets[in.Run.Allottees()[3].InvestorID] = "0x0000000000000000000000000000000000000000"

	if _, err := BuildPlan(in); !errors.Is(err, ErrZeroWallet) {
		t.Fatalf("settleBatch reverts on a zero address inside its loop: want ErrZeroWallet, got %v", err)
	}
}

func TestMalformedWalletIsRefused(t *testing.T) {
	for _, w := range []string{
		"0xABCDEF0123456789012345678901234567890123", // uppercase
		"0x123", // too short
		"1234567890123456789012345678901234567890", // no prefix
	} {
		in := baseInput(t)
		in.Wallets[in.Run.Allottees()[3].InvestorID] = w

		if _, err := BuildPlan(in); !errors.Is(err, ErrBadWallet) {
			t.Errorf("wallet %q: want ErrBadWallet, got %v", w, err)
		}
	}
}

func TestManagerWalletIsRequired(t *testing.T) {
	in := baseInput(t)
	in.IMWallet = ""

	_, err := BuildPlan(in)
	if !errors.Is(err, ErrIMNotRecorded) {
		t.Fatalf("want ErrIMNotRecorded, got %v", err)
	}
}

// --- binding to the ballot -----------------------------------------------------------------------

// TestSettlementMustBindToTheAnchoredResult mirrors what beginSettlement reads.
//
// beginSettlement reads ballot.resultRoot() and requires it non-zero. That read is what permanently
// binds the settlement to one ballot outcome.
func TestSettlementMustBindToTheAnchoredResult(t *testing.T) {
	in := baseInput(t)
	in.BallotResultRoot = merkle.Hash{}

	if _, err := BuildPlan(in); !errors.Is(err, ErrNotBoundToBallot) {
		t.Fatalf("want ErrNotBoundToBallot, got %v", err)
	}
}

func TestSettlingAgainstADifferentDrawIsRefused(t *testing.T) {
	in := baseInput(t)
	in.BallotResultRoot = hashFor(0xBAD)

	_, err := BuildPlan(in)
	if !errors.Is(err, ErrNotBoundToBallot) {
		t.Fatalf("want ErrNotBoundToBallot, got %v", err)
	}
}

func TestZeroAllotmentFileHashIsRefused(t *testing.T) {
	in := baseInput(t)
	in.AllotmentFileHash = [32]byte{}

	if _, err := BuildPlan(in); err == nil {
		t.Fatal("beginSettlement rejects a zero allotment file hash")
	}
}

// --- scheme parameters ---------------------------------------------------------------------------

func TestSchemeParamsValidation(t *testing.T) {
	for name, mutate := range map[string]func(*SchemeParams){
		"zero total":         func(p *SchemeParams) { p.TotalUnits = 0 },
		"zero manager units": func(p *SchemeParams) { p.IMUnits = 0 },
		"manager holds all":  func(p *SchemeParams) { p.IMUnits = p.TotalUnits },
		"zero floor":         func(p *SchemeParams) { p.MinPublicHolders = 0 },
		"zero batch cap":     func(p *SchemeParams) { p.MaxBatchSize = 0 },
		"floor above units":  func(p *SchemeParams) { p.MinPublicHolders = p.TotalUnits },
	} {
		t.Run(name, func(t *testing.T) {
			p := V1Params()
			mutate(&p)
			if err := p.Validate(); err == nil {
				t.Fatalf("%s must be refused", name)
			}
		})
	}
}

// TestSchemeParamsMatchTheDeployedContract reads the constants out of the deploy script.
//
// These are immutable on the deployed instance, so a value here that disagrees produces transactions that
// revert. The same technique as the ballot ceremony's configuration test, and for the same reason: a
// comment claiming the two agree stops being true silently.
func TestSchemeParamsMatchTheDeployedContract(t *testing.T) {
	path := filepath.Join("..", "..", "..", "contracts", "script", "Deploy.s.sol")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the deploy script: %v", err)
	}
	src := string(raw)

	p := V1Params()
	want := map[string]uint32{
		"TOTAL_UNITS":        p.TotalUnits,
		"IM_UNITS":           p.IMUnits,
		"MIN_PUBLIC_HOLDERS": p.MinPublicHolders,
		"MAX_BATCH_SIZE":     p.MaxBatchSize,
	}

	for name, expected := range want {
		got, ok := solidityConstant(src, name)
		if !ok {
			t.Fatalf("%s is not declared in Deploy.s.sol", name)
		}
		if got != expected {
			t.Errorf("%s is %d on-chain but %d in V1Params", name, got, expected)
		}
	}

	// The public side follows, and it is the number every batch is measured against.
	if p.PublicUnits() != 475 {
		t.Fatalf("public units should be 475, got %d", p.PublicUnits())
	}
}

func solidityConstant(src, name string) (uint32, bool) {
	re := regexp.MustCompile(`constant\s+` + regexp.QuoteMeta(name) + `\s*=\s*([0-9_]+)\s*;`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.ReplaceAll(m[1], "_", ""), 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(v), true
}

func TestStageOrdinalsMatchTheContract(t *testing.T) {
	// The contract enum is NotStarted, InProgress, Finalised in that order.
	if StageNotStarted != 0 || StageInProgress != 1 || StageFinalised != 2 {
		t.Fatal("the settlement stage ordinals must match the Solidity enum order")
	}
}

// --- finalisation prechecks ----------------------------------------------------------------------

// readyState is the chain state of a correctly completed settlement.
func readyState(p *Plan) ChainState {
	return ChainState{
		Stage:               StageInProgress,
		TotalUnitsIssued:    p.Params.TotalUnits,
		DistinctHolderCount: p.ExpectedHolders,
		IMWallet:            p.IMWallet,
		IMWalletUnits:       p.Params.IMUnits,
		IMExcluded:          true,
		ExpectedUnits:       p.ExpectedUnits,
		ExpectedHolders:     p.ExpectedHolders,
		CreditedUnits:       p.ExpectedUnits,
		CreditedHolders:     p.ExpectedHolders,
	}
}

func TestFinalisationReadyWhenEverythingHolds(t *testing.T) {
	p := buildPlan(t)

	report := CheckFinalisation(p, readyState(p))
	if !report.Ready {
		t.Fatalf("a complete settlement must be finalisable: %v", report.Failures)
	}
	if err := report.Err(); err != nil {
		t.Fatalf("Err must be nil when ready: %v", err)
	}
}

// TestMissingManagerSubscriptionIsDiagnosedByName is the failure most likely to happen.
//
// The contract reports a total units mismatch, which points at the register rather than at the step
// nobody ran. The shortfall is exactly the manager's holding, so the cause is identifiable.
func TestMissingManagerSubscriptionIsDiagnosedByName(t *testing.T) {
	p := buildPlan(t)
	cs := readyState(p)

	// The manager was never recorded: 475 public units issued, no manager wallet.
	cs.TotalUnitsIssued = p.ExpectedUnits
	cs.IMWallet = ""
	cs.IMWalletUnits = 0
	cs.IMExcluded = false

	report := CheckFinalisation(p, cs)
	if report.Ready {
		t.Fatal("finalisation must not be ready without the manager's units")
	}

	joined := strings.Join(report.Failures, " | ")
	if !strings.Contains(joined, "recordImSubscription") {
		t.Fatalf("the report should name the missing call: %v", report.Failures)
	}
	if !strings.Contains(joined, "25") {
		t.Errorf("the report should state the shortfall: %v", report.Failures)
	}
}

// TestReportListsEveryFailureNotJustTheFirst is why this runs locally.
//
// The contract reverts on the earliest failure, so an operator fixing them one at a time needs a fresh
// transaction to discover the next.
func TestReportListsEveryFailureNotJustTheFirst(t *testing.T) {
	p := buildPlan(t)

	cs := ChainState{
		Stage:               StageInProgress,
		TotalUnitsIssued:    100,
		DistinctHolderCount: 50,
		IMWallet:            "",
		ExpectedUnits:       p.ExpectedUnits,
		ExpectedHolders:     p.ExpectedHolders,
		CreditedUnits:       100,
		CreditedHolders:     100,
	}

	report := CheckFinalisation(p, cs)
	if report.Ready {
		t.Fatal("this state is nowhere near finalisable")
	}
	// Total units, holder floor, manager wallet, credited units, credited holders.
	if len(report.Failures) < 5 {
		t.Fatalf("want all five checks reported, got %d: %v", len(report.Failures), report.Failures)
	}
}

// TestCollidedWalletsAreDiagnosedFromTheCounts is the on-chain symptom of the duplicate hole.
//
// If a duplicate somehow reached the chain, credited entries exceed counted addresses. The report says so
// rather than reporting only that the floor was missed.
func TestCollidedWalletsAreDiagnosedFromTheCounts(t *testing.T) {
	p := buildPlan(t)
	cs := readyState(p)

	cs.DistinctHolderCount = 199 // below the floor, and below the 475 entries credited

	report := CheckFinalisation(p, cs)
	if report.Ready {
		t.Fatal("a holder count below the floor must block finalisation")
	}

	joined := strings.Join(report.Failures, " | ")
	if !strings.Contains(joined, "shared a wallet") {
		t.Fatalf("the report should identify wallet collisions as the cause: %v", report.Failures)
	}
}

func TestPartialSettlementIsNotFinalisable(t *testing.T) {
	p := buildPlan(t)
	cs := readyState(p)

	// Three of five batches applied.
	cs.CreditedHolders = 300
	cs.CreditedUnits = 300
	cs.DistinctHolderCount = 300
	cs.TotalUnitsIssued = 300 + p.Params.IMUnits

	report := CheckFinalisation(p, cs)
	if report.Ready {
		t.Fatal("a partial settlement must not be finalisable")
	}

	step, err := p.NextStep(cs)
	if err != nil {
		t.Fatal(err)
	}
	if step != "settleBatch" {
		t.Fatalf("the next step should be another batch, got %q", step)
	}

	b, err := p.BatchAt(cs.CreditedHolders)
	if err != nil {
		t.Fatal(err)
	}
	if b.CursorFrom != 300 {
		t.Fatalf("the next batch should start at 300, got %d", b.CursorFrom)
	}
}

func TestManagerNotExcludedIsCaught(t *testing.T) {
	p := buildPlan(t)
	cs := readyState(p)
	cs.IMExcluded = false

	report := CheckFinalisation(p, cs)
	if report.Ready {
		t.Fatal("an unexcluded manager wallet must block finalisation")
	}
	if !strings.Contains(strings.Join(report.Failures, " "), "excluded") {
		t.Errorf("the report should name the exclusion flag: %v", report.Failures)
	}
}

func TestManagerHoldingWrongAmountIsCaught(t *testing.T) {
	p := buildPlan(t)
	cs := readyState(p)
	cs.IMWalletUnits = 24

	report := CheckFinalisation(p, cs)
	if report.Ready {
		t.Fatal("the manager must hold exactly the scheme's manager allocation")
	}
}

// TestAPlanFromADifferentSettlementIsRejected guards against checking the wrong numbers.
func TestAPlanFromADifferentSettlementIsRejected(t *testing.T) {
	p := buildPlan(t)
	cs := readyState(p)
	cs.ExpectedUnits = 400
	cs.CreditedUnits = 400

	report := CheckFinalisation(p, cs)
	if report.Ready {
		t.Fatal("a plan that does not match what the chain was opened for must be rejected")
	}
	if !strings.Contains(strings.Join(report.Failures, " "), "different") {
		t.Errorf("the report should say the two are different settlements: %v", report.Failures)
	}
}

// --- next step -----------------------------------------------------------------------------------

func TestNextStepWalksTheWholeSequence(t *testing.T) {
	p := buildPlan(t)

	if step, err := p.NextStep(ChainState{Stage: StageNotStarted}); err != nil || step != "beginSettlement" {
		t.Fatalf("a fresh settlement begins: got %q, %v", step, err)
	}

	cs := readyState(p)
	if step, err := p.NextStep(cs); err != nil || step != "finaliseSettlement" {
		t.Fatalf("a complete settlement finalises: got %q, %v", step, err)
	}

	cs.Stage = StageFinalised
	if _, err := p.NextStep(cs); err == nil {
		t.Fatal("a finalised settlement has nothing left to do")
	}
}

// --- payloads ------------------------------------------------------------------------------------

func TestPayloadsCarryTheCallArguments(t *testing.T) {
	p := buildPlan(t)

	if _, err := p.BeginPayload(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.BatchPayload(p.Batches[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := p.IMPayload(); err != nil {
		t.Fatal(err)
	}

	b := p.Batches[0]
	if len(b.Wallets()) != len(b.UnitsArray()) {
		t.Fatal("settleBatch reverts with ArrayLengthMismatch on unequal arrays")
	}
	if len(b.Wallets()) == 0 {
		t.Fatal("settleBatch reverts with EmptyBatch")
	}
}

// --- idempotency ---------------------------------------------------------------------------------

func TestEveryBatchDerivesADistinctKey(t *testing.T) {
	p := buildPlan(t)

	seen := make(map[idempotency.Key]uint32, len(p.Batches))
	for _, b := range p.Batches {
		k, err := idempotency.Derive(p.BatchIdempotencyInput(b))
		if err != nil {
			t.Fatal(err)
		}
		if prev, dup := seen[k]; dup {
			t.Fatalf("batches at cursor %d and %d share a key", prev, b.CursorFrom)
		}
		seen[k] = b.CursorFrom
	}
}

// TestRetryingABatchVerbatimReusesItsKey is what makes a gas-exhausted batch recoverable.
func TestRetryingABatchVerbatimReusesItsKey(t *testing.T) {
	p := buildPlan(t)
	b := p.Batches[1]

	first, err := idempotency.Derive(p.BatchIdempotencyInput(b))
	if err != nil {
		t.Fatal(err)
	}
	second, err := idempotency.Derive(p.BatchIdempotencyInput(b))
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("a verbatim retry must reuse the key, or the guard would reject a legitimate attempt")
	}
}

// TestCorrectingABatchChangesItsKey keeps a fix from being swallowed as a duplicate.
func TestCorrectingABatchChangesItsKey(t *testing.T) {
	p := buildPlan(t)
	b := p.Batches[1]

	before, err := idempotency.Derive(p.BatchIdempotencyInput(b))
	if err != nil {
		t.Fatal(err)
	}

	// A wrong wallet corrected at the same cursor.
	corrected := b
	corrected.Entries = append([]Entry(nil), b.Entries...)
	corrected.Entries[0].Wallet = walletFor(999999)

	after, err := idempotency.Derive(p.BatchIdempotencyInput(corrected))
	if err != nil {
		t.Fatal(err)
	}

	if before == after {
		t.Fatal("a corrected batch at the same cursor must derive a new key, or the correction " +
			"would be swallowed as a duplicate of the thing it was fixing")
	}
}

func TestSettlementKeysAreAllDistinct(t *testing.T) {
	p := buildPlan(t)

	inputs := map[string]idempotency.Input{
		"begin":    p.BeginIdempotencyInput(),
		"finalise": p.FinaliseIdempotencyInput(),
		"im":       p.IMIdempotencyInput(),
		"batch0":   p.BatchIdempotencyInput(p.Batches[0]),
	}

	seen := make(map[idempotency.Key]string, len(inputs))
	for name, in := range inputs {
		k, err := idempotency.Derive(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if prev, dup := seen[k]; dup {
			t.Fatalf("%s and %s derive the same key", prev, name)
		}
		seen[k] = name
	}
}

// TestManagerSubscriptionKeyIsSchemeScoped reflects that it happens once per scheme.
//
// recordImSubscription reverts once imWallet is set, so it is not an offer-level act and a key scoped to
// an offer would imply a second one could exist for a follow-on offer.
func TestManagerSubscriptionKeyIsSchemeScoped(t *testing.T) {
	p := buildPlan(t)

	in := p.IMIdempotencyInput()
	if in.ScopeID != "" {
		t.Fatalf("the manager's subscription is scheme-scoped, got scope %q", in.ScopeID)
	}
	if in.SchemeID != testSchemeID {
		t.Fatalf("scheme %q, want %q", in.SchemeID, testSchemeID)
	}
}
