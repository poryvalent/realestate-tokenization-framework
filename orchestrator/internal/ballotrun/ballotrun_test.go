package ballotrun

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/acresync/orchestrator/internal/asba"
	"github.com/acresync/orchestrator/internal/ballot"
	"github.com/acresync/orchestrator/internal/bidbook"
	"github.com/acresync/orchestrator/internal/idempotency"
	"github.com/acresync/orchestrator/internal/ipfs"
	"github.com/acresync/orchestrator/internal/ipfsguard"
	"github.com/acresync/orchestrator/internal/merkle"
	"github.com/acresync/orchestrator/internal/money"
)

const (
	testOfferID  = "3f2a1c4e-5b6d-4e8f-9a0b-1c2d3e4f5a6b"
	testSchemeID = "8d7c6b5a-4e3f-4a2b-8c1d-0e9f8a7b6c5d"
)

var (
	testPepper = []byte("ballotrun-test-pepper-never-used-in-production")
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

// buildBook makes a frozen book of count bids, each for units at the floor price.
func buildBook(t *testing.T, count int, units uint32) *bidbook.Book {
	t.Helper()

	at := frozenAt.Add(-24 * time.Hour)
	es := make([]bidbook.Entry, 0, count)
	for i := 1; i <= count; i++ {
		total := money.Paise(int64(units)) * money.MinUnitPricePaise
		es = append(es, bidbook.Entry{
			BidID:             fmt.Sprintf("bid-%04d", i),
			InvestorID:        fmt.Sprintf("inv-%04d", i),
			BidRef:            bidRefFor(i),
			InvestorAnchor:    hashFor(i),
			UnitsBid:          units,
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
		SchemeRef: hashFor(0x5c),
		SchemeID:  testSchemeID,
		OfferID:   testOfferID,
		FrozenAt:  frozenAt,
		Entries:   es,
	})
	if err != nil {
		t.Fatalf("freezing the fixture book: %v", err)
	}
	return book
}

func revealedCeremony(t *testing.T, book *bidbook.Book) *Ceremony {
	t.Helper()

	secret, err := DeriveSecret(testPepper, testSchemeID, testOfferID)
	if err != nil {
		t.Fatal(err)
	}

	return &Ceremony{
		SchemeID:           testSchemeID,
		OfferID:            testOfferID,
		Stage:              StageSeedRevealed,
		BidbookRoot:        book.MerkleRoot,
		Commitment:         ballot.Commitment(secret),
		CommitmentAnchored: true,
		TargetBlock:        1_000_000,
		Attempt:            1,
		Config:             V1Config(),
	}
}

// drawFixture runs a full draw over a book of count single-unit bids.
func drawFixture(t *testing.T, count int) (*Run, *bidbook.Book) {
	t.Helper()

	book := buildBook(t, count, 1)
	cer := revealedCeremony(t, book)

	secret, err := DeriveSecret(testPepper, testSchemeID, testOfferID)
	if err != nil {
		t.Fatal(err)
	}

	run, err := Draw(RunInput{
		Book:            book,
		Ceremony:        cer,
		Secret:          secret,
		TargetBlockHash: hashFor(0xB10C),
		Params:          ballot.V1Params(),
		DrawnAt:         drawnAt,
	})
	if err != nil {
		t.Fatalf("drawing: %v", err)
	}
	return run, book
}

// --- secret derivation ---------------------------------------------------------------------------

func TestDeriveSecretIsDeterministic(t *testing.T) {
	a, err := DeriveSecret(testPepper, testSchemeID, testOfferID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := DeriveSecret(testPepper, testSchemeID, testOfferID)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("the secret must be recomputable, or a crash between the commit and the reveal " +
			"loses it permanently")
	}
	if a.IsZero() {
		t.Fatal("the derived secret must not be zero")
	}
}

// TestDeriveSecretDoesNotDependOnAttempt is the constraint recommitSeed imposes.
//
// recommitSeed takes no commitment argument and seed_commitment is immutable once set, so an abandoned
// window must reroll the target block and nothing else. A secret that varied by attempt would no longer
// match the anchored commitment and the reveal would revert.
func TestDeriveSecretDoesNotDependOnAttempt(t *testing.T) {
	// The signature has no attempt parameter, which is the point. This test asserts the derivation
	// inputs are exactly the two identifiers, so adding an attempt later fails to compile here.
	secret, err := DeriveSecret(testPepper, testSchemeID, testOfferID)
	if err != nil {
		t.Fatal(err)
	}

	cer := &Ceremony{Commitment: ballot.Commitment(secret)}

	// After two recommits the same secret must still verify.
	cer.Attempt = 3
	if err := cer.VerifySecret(secret); err != nil {
		t.Fatalf("the secret must still match the original commitment on a later attempt: %v", err)
	}
}

func TestDeriveSecretVariesByOfferAndScheme(t *testing.T) {
	base, err := DeriveSecret(testPepper, testSchemeID, testOfferID)
	if err != nil {
		t.Fatal(err)
	}

	otherOffer, err := DeriveSecret(testPepper, testSchemeID, "11111111-2222-4333-8444-555555555555")
	if err != nil {
		t.Fatal(err)
	}
	otherScheme, err := DeriveSecret(testPepper, "11111111-2222-4333-8444-555555555555", testOfferID)
	if err != nil {
		t.Fatal(err)
	}

	if base == otherOffer || base == otherScheme || otherOffer == otherScheme {
		t.Fatal("two offers must not share a seed secret")
	}
}

func TestDeriveSecretVariesByPepper(t *testing.T) {
	a, err := DeriveSecret(testPepper, testSchemeID, testOfferID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := DeriveSecret([]byte("a-different-pepper"), testSchemeID, testOfferID)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("the pepper must determine the secret")
	}
}

func TestDeriveSecretRequiresAPepperAndBothIdentifiers(t *testing.T) {
	if _, err := DeriveSecret(nil, testSchemeID, testOfferID); !errors.Is(err, ErrNoPepper) {
		t.Fatalf("want ErrNoPepper, got %v", err)
	}
	if _, err := DeriveSecret(testPepper, "", testOfferID); err == nil {
		t.Fatal("a missing scheme identifier must be refused")
	}
	if _, err := DeriveSecret(testPepper, testSchemeID, ""); err == nil {
		t.Fatal("a missing offer identifier must be refused")
	}
}

// TestRotatingThePepperIsCaughtWithAUsefulMessage is the cost of deriving rather than storing.
//
// Rotate the KMS key between the commit and the reveal and the derivation yields a different secret for
// the same offer. The commitment is immutable, so the ceremony cannot recover through the normal path.
// The contract would revert with CommitmentMismatch; this must say what actually happened.
func TestRotatingThePepperIsCaughtWithAUsefulMessage(t *testing.T) {
	committed, err := DeriveSecret(testPepper, testSchemeID, testOfferID)
	if err != nil {
		t.Fatal(err)
	}
	cer := &Ceremony{Commitment: ballot.Commitment(committed)}

	rotated, err := DeriveSecret([]byte("rotated-pepper"), testSchemeID, testOfferID)
	if err != nil {
		t.Fatal(err)
	}

	err = cer.VerifySecret(rotated)
	if !errors.Is(err, ErrCommitmentMismatch) {
		t.Fatalf("want ErrCommitmentMismatch, got %v", err)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("pepper")) {
		t.Errorf("the error should name the likely cause: %v", err)
	}
}

// --- configuration -------------------------------------------------------------------------------

func TestV1ConfigIsValid(t *testing.T) {
	if err := V1Config().Validate(); err != nil {
		t.Fatalf("the shipped configuration must be acceptable: %v", err)
	}
}

// TestRevealWindowCannotExceedTheBlockhashHorizon mirrors the contract constructor.
//
// A window wider than 250 blocks is shorter than it claims: past the 256-block horizon blockhash
// returns zero and no reveal can succeed, so the last stretch of the advertised window is unusable.
func TestRevealWindowCannotExceedTheBlockhashHorizon(t *testing.T) {
	c := V1Config()
	c.RevealWindowBlocks = 251

	err := c.Validate()
	if err == nil {
		t.Fatal("a reveal window above 250 blocks must be refused")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("250")) {
		t.Errorf("the error should state the limit: %v", err)
	}

	c.RevealWindowBlocks = 250
	if err := c.Validate(); err != nil {
		t.Fatalf("250 is the boundary and must be accepted: %v", err)
	}
}

func TestZeroConfigurationValuesAreRefused(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"zero delay":    func(c *Config) { c.DelayBlocks = 0 },
		"zero window":   func(c *Config) { c.RevealWindowBlocks = 0 },
		"zero attempts": func(c *Config) { c.MaxAttempts = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			c := V1Config()
			mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatalf("%s must be refused", name)
			}
		})
	}
}

// --- the reveal window ---------------------------------------------------------------------------

// TestRevealWindowBoundaries pins the exact inequalities the contract uses.
//
// The valid range is targetBlock < current <= targetBlock + window. The lower bound is strict because
// the hash of a block is not determined until the block after it, so revealing AT the target would read
// a value that does not exist yet.
func TestRevealWindowBoundaries(t *testing.T) {
	cer := &Ceremony{
		Stage:              StageSeedCommitted,
		CommitmentAnchored: true,
		Commitment:         hashFor(1),
		BidbookRoot:        hashFor(2),
		TargetBlock:        1000,
		Attempt:            1,
		Config:             Config{DelayBlocks: 20, RevealWindowBlocks: 200, MaxAttempts: 3},
	}

	if got := cer.Deadline(); got != 1200 {
		t.Fatalf("deadline should be 1200, got %d", got)
	}

	cases := []struct {
		block uint64
		want  error
		why   string
	}{
		{999, ErrTargetNotReached, "before the target block"},
		{1000, ErrTargetNotReached, "AT the target block: its hash is not determined yet"},
		{1001, nil, "one block after the target: the earliest valid reveal"},
		{1200, nil, "the last block of the window"},
		{1201, ErrWindowExpired, "one block past the deadline"},
	}

	for _, c := range cases {
		err := cer.CanReveal(c.block)
		if c.want == nil {
			if err != nil {
				t.Errorf("block %d (%s): want a reveal to be allowed, got %v", c.block, c.why, err)
			}
			continue
		}
		if !errors.Is(err, c.want) {
			t.Errorf("block %d (%s): want %v, got %v", c.block, c.why, c.want, err)
		}
	}
}

func TestRevealRequiresTheCommitmentToBeAnchored(t *testing.T) {
	cer := &Ceremony{
		Stage:              StageSeedCommitted,
		CommitmentAnchored: false,
		Commitment:         hashFor(1),
		TargetBlock:        1000,
		Config:             V1Config(),
	}

	err := cer.CanReveal(1001)
	if err == nil {
		t.Fatal("revealing before the commitment is on-chain must be refused; otherwise an operator " +
			"who disliked the draw could claim a different secret was always intended")
	}
}

func TestRevealRequiresTheRightStage(t *testing.T) {
	for _, st := range []Stage{StagePending, StageBidbookAnchored, StageSeedRevealed, StageDrawn} {
		cer := &Ceremony{
			Stage: st, CommitmentAnchored: true, Commitment: hashFor(1),
			TargetBlock: 1000, Config: V1Config(),
		}
		if err := cer.CanReveal(1001); !errors.Is(err, ErrWrongStage) {
			t.Errorf("stage %s: want ErrWrongStage, got %v", st, err)
		}
	}
}

// TestRecommitOnlyAfterTheWindowLapses is the anti-skip rule.
//
// Recommitting while a reveal is still possible would let an operator who can already compute the draw
// abandon it and roll again. That is the single thing the ceremony exists to prevent.
func TestRecommitOnlyAfterTheWindowLapses(t *testing.T) {
	cer := &Ceremony{
		Stage: StageSeedCommitted, CommitmentAnchored: true, Commitment: hashFor(1),
		TargetBlock: 1000, Attempt: 1,
		Config: Config{DelayBlocks: 20, RevealWindowBlocks: 200, MaxAttempts: 3},
	}

	for _, block := range []uint64{1000, 1001, 1200} {
		if err := cer.CanRecommit(block); !errors.Is(err, ErrWindowStillOpen) {
			t.Errorf("block %d: a recommit must be refused while a reveal is possible, got %v", block, err)
		}
	}
	if err := cer.CanRecommit(1201); err != nil {
		t.Errorf("block 1201: the window has lapsed so a recommit is allowed, got %v", err)
	}
}

func TestRecommitStopsAtTheAttemptCap(t *testing.T) {
	cer := &Ceremony{
		Stage: StageSeedCommitted, CommitmentAnchored: true, Commitment: hashFor(1),
		TargetBlock: 1000, Config: Config{DelayBlocks: 20, RevealWindowBlocks: 200, MaxAttempts: 3},
	}

	for _, attempt := range []uint32{1, 2} {
		cer.Attempt = attempt
		if err := cer.CanRecommit(1201); err != nil {
			t.Errorf("attempt %d of 3 must still allow a recommit, got %v", attempt, err)
		}
	}

	cer.Attempt = 3
	if err := cer.CanRecommit(1201); !errors.Is(err, ErrAttemptsExhausted) {
		t.Fatalf("attempt 3 of 3 exhausts the cap, got %v", err)
	}
}

// TestEscalationIsTheMirrorOfRecommit checks the two paths cannot both be open.
//
// Below the cap a recommit is available and escalation is not; at the cap the operator loses the ability
// to keep rolling and a trustee has to put their name to what happens next. Exactly one is available at
// any point once the window has lapsed.
func TestEscalationIsTheMirrorOfRecommit(t *testing.T) {
	cer := &Ceremony{
		Stage: StageSeedCommitted, CommitmentAnchored: true, Commitment: hashFor(1),
		TargetBlock: 1000, Config: Config{DelayBlocks: 20, RevealWindowBlocks: 200, MaxAttempts: 3},
	}

	for attempt := uint32(1); attempt <= 3; attempt++ {
		cer.Attempt = attempt

		recommit := cer.CanRecommit(1201)
		escalate := cer.CanEscalate(1201)

		if (recommit == nil) == (escalate == nil) {
			t.Fatalf("attempt %d: exactly one of recommit and escalate must be available, got "+
				"recommit=%v escalate=%v", attempt, recommit, escalate)
		}
	}

	// Escalation also needs the window to have lapsed.
	cer.Attempt = 3
	if err := cer.CanEscalate(1200); !errors.Is(err, ErrWindowStillOpen) {
		t.Fatalf("escalating inside the window must be refused, got %v", err)
	}
}

func TestCommitRequiresTheAnchoredBook(t *testing.T) {
	cer := &Ceremony{Stage: StageBidbookAnchored, Commitment: hashFor(1)}
	if err := cer.CanCommit(); !errors.Is(err, ErrBookNotAnchored) {
		t.Fatalf("committing a seed before the book is anchored must be refused, got %v", err)
	}

	cer.BidbookRoot = hashFor(2)
	if err := cer.CanCommit(); err != nil {
		t.Fatalf("with an anchored book the commit is allowed: %v", err)
	}
}

// TestPlaintextCannotBePersistedBeforeTheAnchor mirrors ballot_runs_enforce_reveal_order.
func TestPlaintextCannotBePersistedBeforeTheAnchor(t *testing.T) {
	cer := &Ceremony{Commitment: hashFor(1), TargetBlock: 1000}

	if err := cer.CanPersistPlaintext(); err == nil {
		t.Fatal("the plaintext must not be storable before the commitment is anchored")
	}

	cer.CommitmentAnchored = true
	if err := cer.CanPersistPlaintext(); err != nil {
		t.Fatalf("once anchored the plaintext may be stored: %v", err)
	}

	cer.Commitment = merkle.Hash{}
	if err := cer.CanPersistPlaintext(); err == nil {
		t.Fatal("the plaintext must not be storable without a commitment")
	}
}

// --- the seed ------------------------------------------------------------------------------------

func TestFinalSeedMixesAllThreeInputs(t *testing.T) {
	book := buildBook(t, 10, 1)
	cer := revealedCeremony(t, book)
	secret, _ := DeriveSecret(testPepper, testSchemeID, testOfferID)

	base, err := cer.FinalSeed(secret, hashFor(0xAA))
	if err != nil {
		t.Fatal(err)
	}

	// A different blockhash must change the seed.
	other, err := cer.FinalSeed(secret, hashFor(0xBB))
	if err != nil {
		t.Fatal(err)
	}
	if base == other {
		t.Fatal("the target blockhash must affect the seed; it is the only input the operator " +
			"cannot choose")
	}

	// A different book root must change the seed.
	cer2 := revealedCeremony(t, book)
	cer2.BidbookRoot = hashFor(0xCC)
	shifted, err := cer2.FinalSeed(secret, hashFor(0xAA))
	if err != nil {
		t.Fatal(err)
	}
	if shifted == base {
		t.Fatal("the book root must affect the seed, so an edited book produces a visibly different draw")
	}
}

func TestFinalSeedRefusesAZeroBlockhash(t *testing.T) {
	book := buildBook(t, 10, 1)
	cer := revealedCeremony(t, book)
	secret, _ := DeriveSecret(testPepper, testSchemeID, testOfferID)

	if _, err := cer.FinalSeed(secret, merkle.Hash{}); !errors.Is(err, ErrBlockhashLost) {
		t.Fatalf("a zero blockhash folded into the seed would produce a draw derived from nothing, "+
			"got %v", err)
	}
}

func TestFinalSeedRefusesAWrongSecret(t *testing.T) {
	book := buildBook(t, 10, 1)
	cer := revealedCeremony(t, book)

	if _, err := cer.FinalSeed(hashFor(999), hashFor(1)); !errors.Is(err, ErrCommitmentMismatch) {
		t.Fatalf("want ErrCommitmentMismatch, got %v", err)
	}
}

// --- the draw ------------------------------------------------------------------------------------

func TestDrawOverAnOversubscribedBook(t *testing.T) {
	run, book := drawFixture(t, 500)

	if err := run.Validate(); err != nil {
		t.Fatalf("a 500-bid draw for 475 units must be anchorable: %v", err)
	}

	if run.Result.UnitsAllotted != 475 {
		t.Fatalf("want all 475 units allotted, got %d", run.Result.UnitsAllotted)
	}

	// Every bid gets a line, winners and losers alike.
	if len(run.Lines) != len(book.Lines) {
		t.Fatalf("%d lines for %d bids", len(run.Lines), len(book.Lines))
	}
	if len(run.Allottees())+len(run.Rejected()) != len(run.Lines) {
		t.Fatal("every line is either an allottee or rejected")
	}
	if len(run.Allottees()) != int(run.Result.DistinctAllottees) {
		t.Fatalf("%d allottees against a reported %d",
			len(run.Allottees()), run.Result.DistinctAllottees)
	}

	// 500 single-unit bids for 475 units: 475 win one unit, 25 win nothing.
	if len(run.Rejected()) != 25 {
		t.Fatalf("want 25 rejected, got %d", len(run.Rejected()))
	}
}

// TestMoneyReconcilesPerLineAndInAggregate is the identity the settle instruction depends on.
func TestMoneyReconcilesPerLineAndInAggregate(t *testing.T) {
	run, book := drawFixture(t, 500)

	for _, l := range run.Lines {
		sum, err := l.AmountPayable.Add(l.RefundAmount)
		if err != nil {
			t.Fatal(err)
		}
		want := money.Paise(int64(l.Block.BlockedPaise))
		if sum != want {
			t.Fatalf("leaf %d: %d payable plus %d refundable is %d against %d blocked",
				l.LeafIndex, l.AmountPayable, l.RefundAmount, sum, want)
		}
	}

	payable, err := run.TotalPayablePaise()
	if err != nil {
		t.Fatal(err)
	}
	refund, err := run.TotalRefundPaise()
	if err != nil {
		t.Fatal(err)
	}

	total, err := payable.Add(refund)
	if err != nil {
		t.Fatal(err)
	}
	if total != book.TotalAmountPaise {
		t.Fatalf("the draw accounts for %d paise, the book blocked %d", total, book.TotalAmountPaise)
	}

	// 475 units at the floor price must be debited.
	wantPayable := money.Paise(475) * money.MinUnitPricePaise
	if payable != wantPayable {
		t.Fatalf("want %d payable for 475 units, got %d", wantPayable, payable)
	}
}

// TestDrawRefusesABookThatIsNotTheAnchoredOne is the binding between the seed and the population.
//
// The anchored root feeds the seed. Drawing against a different book means the seed was derived from a
// root that does not describe the population being allocated, and the output would be internally
// consistent and wrong.
func TestDrawRefusesABookThatIsNotTheAnchoredOne(t *testing.T) {
	book := buildBook(t, 50, 1)
	cer := revealedCeremony(t, book)

	other := buildBook(t, 40, 1)
	if other.MerkleRoot == book.MerkleRoot {
		t.Fatal("the fixture is wrong: two different books must have different roots")
	}

	secret, _ := DeriveSecret(testPepper, testSchemeID, testOfferID)
	_, err := Draw(RunInput{
		Book: other, Ceremony: cer, Secret: secret,
		TargetBlockHash: hashFor(1), Params: ballot.V1Params(), DrawnAt: drawnAt,
	})
	if !errors.Is(err, ErrRootMismatch) {
		t.Fatalf("want ErrRootMismatch, got %v", err)
	}
}

func TestDrawRequiresARevealedOrEscalatedCeremony(t *testing.T) {
	// Fully subscribed, because the escalated case below actually runs the draw and ballot.Run refuses
	// an infeasible book before it allocates anything.
	book := buildBook(t, 500, 1)
	secret, _ := DeriveSecret(testPepper, testSchemeID, testOfferID)

	for _, st := range []Stage{StagePending, StageBidbookAnchored, StageSeedCommitted, StageAbandoned} {
		cer := revealedCeremony(t, book)
		cer.Stage = st

		_, err := Draw(RunInput{
			Book: book, Ceremony: cer, Secret: secret,
			TargetBlockHash: hashFor(1), Params: ballot.V1Params(), DrawnAt: drawnAt,
		})
		if !errors.Is(err, ErrWrongStage) {
			t.Errorf("stage %s: want ErrWrongStage, got %v", st, err)
		}
	}

	// An escalated ceremony may still be drawn: the contract accepts a result anchored from Escalated.
	cer := revealedCeremony(t, book)
	cer.Stage = StageEscalated
	if _, err := Draw(RunInput{
		Book: book, Ceremony: cer, Secret: secret,
		TargetBlockHash: hashFor(1), Params: ballot.V1Params(), DrawnAt: drawnAt,
	}); err != nil {
		t.Fatalf("a trustee-escalated ceremony must still be drawable: %v", err)
	}
}

func TestSameSeedGivesTheSameDraw(t *testing.T) {
	a, _ := drawFixture(t, 500)
	b, _ := drawFixture(t, 500)

	if a.Result.ResultRoot != b.Result.ResultRoot {
		t.Fatal("the draw must be reproducible from the same inputs, or nobody can verify it")
	}
	for i := range a.Lines {
		if a.Lines[i].UnitsAllotted != b.Lines[i].UnitsAllotted ||
			a.Lines[i].BallotRank != b.Lines[i].BallotRank {
			t.Fatalf("line %d differs between two runs of identical inputs", i)
		}
	}
}

func TestADifferentBlockhashGivesADifferentDraw(t *testing.T) {
	book := buildBook(t, 500, 1)
	cer := revealedCeremony(t, book)
	secret, _ := DeriveSecret(testPepper, testSchemeID, testOfferID)

	first, err := Draw(RunInput{
		Book: book, Ceremony: cer, Secret: secret,
		TargetBlockHash: hashFor(0x1111), Params: ballot.V1Params(), DrawnAt: drawnAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Draw(RunInput{
		Book: book, Ceremony: cer, Secret: secret,
		TargetBlockHash: hashFor(0x2222), Params: ballot.V1Params(), DrawnAt: drawnAt,
	})
	if err != nil {
		t.Fatal(err)
	}

	if first.Result.ResultRoot == second.Result.ResultRoot {
		t.Fatal("the blockhash must change the draw; it is the entropy the operator cannot control")
	}
}

// --- validation against the contract -------------------------------------------------------------

func TestValidateEnforcesTheHolderFloor(t *testing.T) {
	run, _ := drawFixture(t, 500)
	run.Result.DistinctAllottees = 199

	err := run.Validate()
	if !errors.Is(err, ErrAllotteeFloor) {
		t.Fatalf("want ErrAllotteeFloor, got %v", err)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("listed")) {
		t.Errorf("the error should say what the floor protects: %v", err)
	}
}

func TestValidateRefusesOverAllotment(t *testing.T) {
	run, _ := drawFixture(t, 500)
	run.Result.UnitsAllotted = run.UnitsOnOffer + 1

	if err := run.Validate(); !errors.Is(err, ErrOverAllotted) {
		t.Fatalf("want ErrOverAllotted, got %v", err)
	}
}

// TestValidateRefusesMoreAllotteesThanUnits follows from units being indivisible.
func TestValidateRefusesMoreAllotteesThanUnits(t *testing.T) {
	run, _ := drawFixture(t, 500)
	run.Result.UnitsAllotted = 300
	run.Result.DistinctAllottees = 301

	if err := run.Validate(); !errors.Is(err, ErrCountsImpossible) {
		t.Fatalf("want ErrCountsImpossible, got %v", err)
	}
}

func TestValidateRefusesAnEmptyAllotment(t *testing.T) {
	run, _ := drawFixture(t, 500)
	run.Result.UnitsAllotted = 0

	err := run.Validate()
	if err == nil {
		t.Fatal("anchorBallotResult rejects a zero allotment")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("abort")) {
		t.Errorf("the error should point at the abort path: %v", err)
	}
}

func TestValidateChecksTheCountsAgainstTheLines(t *testing.T) {
	run, _ := drawFixture(t, 500)
	run.Result.UnitsAllotted = 474 // one fewer than the lines actually carry

	if err := run.Validate(); err == nil {
		t.Fatal("a reported total that disagrees with the lines must be refused")
	}
}

// --- the allotment file --------------------------------------------------------------------------

func pinRun(t *testing.T, run *Run) *ipfs.Pin {
	t.Helper()

	mock, err := ipfs.NewMockProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pub := ipfs.NewPublisher(mock)

	raw, err := run.Document()
	if err != nil {
		t.Fatal(err)
	}
	pin, err := pub.Publish(context.Background(), ipfsguard.DocAllotmentFile, raw)
	if err != nil {
		t.Fatalf("pinning the allotment file: %v", err)
	}
	return pin
}

func TestAllotmentFileSatisfiesTheAllowlist(t *testing.T) {
	run, _ := drawFixture(t, 500)

	raw, err := run.Document()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ipfsguard.ValidateAndCanonicalize(ipfsguard.DocAllotmentFile, raw); err != nil {
		t.Fatalf("the allotment file must satisfy the allowlist: %v", err)
	}
}

func TestAllotmentFileCarriesEveryBidAndNoIdentifiers(t *testing.T) {
	run, book := drawFixture(t, 500)

	raw, err := run.Document()
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		OfferID     string `json:"offerId"`
		FinalSeed   string `json:"finalSeed"`
		BidbookRoot string `json:"bidbookRoot"`
		ResultRoot  string `json:"resultRoot"`
		OnOffer     uint32 `json:"unitsOnOffer"`
		Allotted    uint32 `json:"unitsAllotted"`
		Allottees   uint32 `json:"distinctAllottees"`
		Allotments  []struct {
			LeafIndex uint32 `json:"leafIndex"`
			Anchor    string `json:"investorAnchor"`
			Units     uint32 `json:"unitsAllotted"`
			Outcome   string `json:"outcome"`
			Rank      uint32 `json:"ballotRank"`
		} `json:"allotments"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	if len(doc.Allotments) != 500 {
		t.Fatalf("want all 500 bids published, including the 25 that won nothing, got %d",
			len(doc.Allotments))
	}
	if doc.FinalSeed != run.Result.FinalSeed.Hex() {
		t.Fatal("the final seed must be published so the draw can be rerun")
	}
	if doc.BidbookRoot != book.MerkleRoot.Hex() {
		t.Fatal("the bid book root must be published so the two documents can be joined")
	}
	if doc.ResultRoot != run.Result.ResultRoot.Hex() {
		t.Fatal("the result root must match the anchored one")
	}

	// No internal identifiers, and no bid references either.
	for _, l := range run.Lines {
		if bytes.Contains(raw, []byte(l.InvestorID)) {
			t.Fatalf("the allotment file contains investor identifier %s", l.InvestorID)
		}
		if bytes.Contains(raw, []byte(l.BidID)) {
			t.Fatalf("the allotment file contains internal bid identifier %s", l.BidID)
		}
		if bytes.Contains(raw, []byte(l.BidRef)) {
			t.Fatalf("the allotment file contains bid reference %s: the bid book already binds "+
				"reference to leaf index, so repeating it here lets anyone holding one investor's "+
				"receipt read their allotment", l.BidRef)
		}
	}
}

// TestPublishedRanksAreRecomputable is how a losing bidder checks the draw.
//
// They know their leaf index from the bid book and their anchor is published in both documents. The seed
// is published here. So they can recompute their own rank key and confirm their position was not
// invented, without access to anything of ours.
func TestPublishedRanksAreRecomputable(t *testing.T) {
	run, _ := drawFixture(t, 500)

	raw, err := run.Document()
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		FinalSeed  string `json:"finalSeed"`
		Allotments []struct {
			LeafIndex uint32 `json:"leafIndex"`
			Anchor    string `json:"investorAnchor"`
			Rank      uint32 `json:"ballotRank"`
		} `json:"allotments"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	seed, err := merkle.ParseHash(doc.FinalSeed)
	if err != nil {
		t.Fatal(err)
	}

	// Rebuild the ranking from the published fields alone and check it agrees with the published ranks.
	type entry struct {
		key  merkle.Hash
		rank uint32
	}
	rebuilt := make([]entry, 0, len(doc.Allotments))
	ranks := make(map[uint32]bool, len(doc.Allotments))
	for _, a := range doc.Allotments {
		anchor, err := merkle.ParseHash(a.Anchor)
		if err != nil {
			t.Fatal(err)
		}
		rebuilt = append(rebuilt, entry{key: ballot.RankKey(seed, anchor, a.LeafIndex), rank: a.Rank})
		ranks[a.Rank] = true
	}

	// Without this the pairwise check below would pass trivially on a document where every rank was
	// zero. The ranks must be a dense permutation of 0..n-1.
	if len(ranks) != len(doc.Allotments) {
		t.Fatalf("%d distinct ranks across %d bids: ranks must be unique",
			len(ranks), len(doc.Allotments))
	}
	for i := range uint32(len(doc.Allotments)) {
		if !ranks[i] {
			t.Fatalf("rank %d is missing, so the ranks are not a dense ordering", i)
		}
	}

	// A lower rank must mean a smaller key, for every pair.
	for i := range rebuilt {
		for j := range rebuilt {
			if rebuilt[i].rank < rebuilt[j].rank &&
				bytes.Compare(rebuilt[i].key[:], rebuilt[j].key[:]) > 0 {
				t.Fatalf("rank %d sorts before rank %d but its recomputed key is larger: the "+
					"published ranking is not the one the seed produces",
					rebuilt[i].rank, rebuilt[j].rank)
			}
		}
	}
}

func TestDocumentRefusesAnInvalidDraw(t *testing.T) {
	run, _ := drawFixture(t, 500)
	run.Result.DistinctAllottees = 10

	if _, err := run.Document(); err == nil {
		t.Fatal("a document describing an unanchorable draw would be pinned, immutable and useless")
	}
}

func TestDocumentRefusesANonCanonicalOfferID(t *testing.T) {
	run, _ := drawFixture(t, 500)
	run.OfferID = "NOT-A-UUID"

	if _, err := run.Document(); err == nil {
		t.Fatal("a non-canonical offerId must be refused before the allowlist sees it")
	}
}

// --- the result anchor ---------------------------------------------------------------------------

func TestResultAnchorRequestCarriesWhatTheContractChecks(t *testing.T) {
	run, _ := drawFixture(t, 500)
	pin := pinRun(t, run)

	req, err := run.AnchorRequest(pin)
	if err != nil {
		t.Fatalf("building the result anchor request: %v", err)
	}

	if req.Root != run.Result.ResultRoot {
		t.Fatal("the anchored root must be the result root")
	}
	if req.CIDDigest != sha256.Sum256(pin.CanonicalBytes) {
		t.Fatal("the anchored digest must be sha256 of the canonical document bytes")
	}
	if req.UnitsAllotted > req.UnitsOnOffer {
		t.Fatal("anchorBallotResult reverts with UnitsExceedOffer")
	}
	if req.DistinctAllottees < req.MinDistinctHolders {
		t.Fatal("anchorBallotResult reverts with AllotteeFloorNotMet")
	}
	if req.DistinctAllottees > req.UnitsAllotted {
		t.Fatal("anchorBallotResult reverts with InvalidCounts")
	}
	if req.AlgoVersion != ballot.AlgoVersion {
		t.Fatalf("algo version %d, engine %d", req.AlgoVersion, ballot.AlgoVersion)
	}
}

func TestAStaleAllotmentPinIsRefused(t *testing.T) {
	first, _ := drawFixture(t, 500)
	stalePin := pinRun(t, first)

	// A redraw against a different blockhash.
	book := buildBook(t, 500, 1)
	cer := revealedCeremony(t, book)
	secret, _ := DeriveSecret(testPepper, testSchemeID, testOfferID)
	second, err := Draw(RunInput{
		Book: book, Ceremony: cer, Secret: secret,
		TargetBlockHash: hashFor(0xDEAD), Params: ballot.V1Params(), DrawnAt: drawnAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Result.ResultRoot == first.Result.ResultRoot {
		t.Fatal("the fixture is wrong: a different blockhash must change the result")
	}

	if _, err := second.AnchorRequest(stalePin); !errors.Is(err, ErrPinDoesNotBind) {
		t.Fatalf("anchoring a redraw with the earlier pin must be refused, got %v", err)
	}
	if _, err := first.AnchorRequest(stalePin); err != nil {
		t.Fatalf("the pin must still bind to the draw it was taken of: %v", err)
	}
}

func TestResultAnchorRefusesAPinOfAnotherDocumentType(t *testing.T) {
	run, _ := drawFixture(t, 500)
	pin := pinRun(t, run)
	pin.DocType = ipfsguard.DocBidbook

	if _, err := run.AnchorRequest(pin); !errors.Is(err, ErrWrongDocType) {
		t.Fatalf("want ErrWrongDocType, got %v", err)
	}
}

func TestResultAnchorRefusesANilPin(t *testing.T) {
	run, _ := drawFixture(t, 500)
	if _, err := run.AnchorRequest(nil); !errors.Is(err, ErrNoPin) {
		t.Fatalf("want ErrNoPin, got %v", err)
	}
}

func TestResultOutboxPayloadCarriesTheCallArguments(t *testing.T) {
	run, _ := drawFixture(t, 500)
	pin := pinRun(t, run)

	req, err := run.AnchorRequest(pin)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := req.OutboxPayload()
	if err != nil {
		t.Fatal(err)
	}

	var got struct {
		Root      []byte `json:"root"`
		CIDDigest []byte `json:"cidDigest"`
		Allotted  uint32 `json:"allotted"`
		Allottees uint32 `json:"allottees"`
		OnOffer   uint32 `json:"unitsOnOffer"`
		Floor     uint32 `json:"minDistinctHolders"`
		Algo      uint32 `json:"algo"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(got.Root, run.Result.ResultRoot[:]) {
		t.Fatal("the payload must carry the result root")
	}
	if got.Allotted != 475 || got.OnOffer != 475 {
		t.Fatalf("payload says %d of %d units", got.Allotted, got.OnOffer)
	}
	if got.Floor != 200 {
		t.Fatalf("the statutory floor must be passed to the contract, got %d", got.Floor)
	}
}

// --- idempotency ---------------------------------------------------------------------------------

// TestCommitKeyIsStableAcrossAttempts is why the commit key omits the attempt.
//
// There is exactly one commitSeed per offer for all time, because the commitment is immutable and a
// recommit reuses it. A key that varied would let a retry after a dropped connection commit twice and
// burn an attempt from a cap of three.
func TestCommitKeyIsStableAcrossAttempts(t *testing.T) {
	cer := &Ceremony{
		SchemeID: testSchemeID, OfferID: testOfferID,
		Commitment: hashFor(1), BidbookRoot: hashFor(2), Attempt: 1, Config: V1Config(),
	}

	first, err := idempotency.Derive(cer.CommitIdempotencyInput())
	if err != nil {
		t.Fatal(err)
	}

	cer.Attempt = 3
	cer.TargetBlock = 999999
	second, err := idempotency.Derive(cer.CommitIdempotencyInput())
	if err != nil {
		t.Fatal(err)
	}

	if first != second {
		t.Fatal("the commit key must not depend on the attempt or the target block")
	}
}

// TestRecommitKeyVariesByAttempt is the opposite requirement.
//
// Each recommit is a distinct act that must get through. A shared key would make the second recommit a
// duplicate of the first and strand the ceremony at a lapsed window with attempts nominally left.
func TestRecommitKeyVariesByAttempt(t *testing.T) {
	cer := &Ceremony{
		SchemeID: testSchemeID, OfferID: testOfferID,
		Commitment: hashFor(1), TargetBlock: 1000, Attempt: 1, Config: V1Config(),
	}

	first, err := idempotency.Derive(cer.RecommitIdempotencyInput())
	if err != nil {
		t.Fatal(err)
	}

	cer.Attempt = 2
	cer.TargetBlock = 1300
	second, err := idempotency.Derive(cer.RecommitIdempotencyInput())
	if err != nil {
		t.Fatal(err)
	}

	if first == second {
		t.Fatal("two recommits must derive different keys")
	}
}

// TestRevealKeyVariesByTargetBlock keeps a reveal possible after a lapsed window.
func TestRevealKeyVariesByTargetBlock(t *testing.T) {
	cer := &Ceremony{
		SchemeID: testSchemeID, OfferID: testOfferID,
		Commitment: hashFor(1), TargetBlock: 1000, Attempt: 1, Config: V1Config(),
	}

	first, err := idempotency.Derive(cer.RevealIdempotencyInput())
	if err != nil {
		t.Fatal(err)
	}

	cer.TargetBlock = 1300
	second, err := idempotency.Derive(cer.RevealIdempotencyInput())
	if err != nil {
		t.Fatal(err)
	}

	if first == second {
		t.Fatal("a reveal against a new target block is a different act; a shared key would make a " +
			"ceremony that lapsed once unable to ever reveal")
	}
}

func TestCeremonyKeysAreAllDistinct(t *testing.T) {
	run, _ := drawFixture(t, 500)
	pin := pinRun(t, run)
	req, err := run.AnchorRequest(pin)
	if err != nil {
		t.Fatal(err)
	}

	cer := &Ceremony{
		SchemeID: testSchemeID, OfferID: testOfferID,
		Commitment: hashFor(1), BidbookRoot: hashFor(2), TargetBlock: 1000, Attempt: 1,
		Config: V1Config(),
	}

	inputs := map[string]idempotency.Input{
		"commit":   cer.CommitIdempotencyInput(),
		"recommit": cer.RecommitIdempotencyInput(),
		"reveal":   cer.RevealIdempotencyInput(),
		"run":      run.RunIdempotencyInput(),
		"anchor":   req.AnchorIdempotencyInput(testSchemeID, testOfferID),
	}

	seen := make(map[idempotency.Key]string, len(inputs))
	for name, in := range inputs {
		k, err := idempotency.Derive(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if prev, dup := seen[k]; dup {
			t.Fatalf("%s and %s derive the same key, so one would be swallowed as a duplicate of "+
				"the other", prev, name)
		}
		seen[k] = name
	}
}

// --- stages --------------------------------------------------------------------------------------

func TestStageTerminality(t *testing.T) {
	for _, s := range AllStages() {
		want := s == StageResultAnchored || s == StageAbandoned
		if s.IsTerminal() != want {
			t.Errorf("%s: IsTerminal = %v, want %v", s, s.IsTerminal(), want)
		}
	}
	if len(AllStages()) != 8 {
		t.Fatalf("ballot_status has 8 values, AllStages has %d", len(AllStages()))
	}
}

// --- agreement with the deployed contract --------------------------------------------------------

// TestConfigMatchesTheDeployedContract reads the timing out of the deploy script.
//
// The Ballot contract takes its delay, window and attempt cap in the constructor and stores them in
// immutable state. They cannot be changed on the deployed instance at 0x63615652d8ff9190454229c2add1f173
// d28b32d5, so a Config here that disagrees is not a configuration difference, it is the Go layer
// reasoning about a chain that behaves differently.
//
// The specific failure that motivated this: V1Config was written with a 20-block delay while the
// deployed contract uses 10. Nothing broke, because the contract chooses the target block itself and the
// orchestrator reads it back rather than predicting it. That is exactly why it needed a test. A comment
// claiming the values mirror the contract had already stopped being true.
func TestConfigMatchesTheDeployedContract(t *testing.T) {
	path := filepath.Join("..", "..", "..", "contracts", "script", "Deploy.s.sol")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the deploy script: %v", err)
	}
	src := string(raw)

	want := map[string]uint32{
		"SEED_DELAY_BLOCKS":         V1Config().DelayBlocks,
		"SEED_REVEAL_WINDOW_BLOCKS": V1Config().RevealWindowBlocks,
		"MAX_SEED_ATTEMPTS":         V1Config().MaxAttempts,
	}

	for name, expected := range want {
		got, ok := solidityConstant(src, name)
		if !ok {
			t.Fatalf("%s is not declared in Deploy.s.sol; if it was renamed, this test needs to "+
				"follow it rather than be deleted", name)
		}
		if got != expected {
			t.Errorf("%s is %d in the deployed contract but %d in V1Config", name, got, expected)
		}
	}
}

// solidityConstant pulls an integer constant out of Solidity source.
func solidityConstant(src, name string) (uint32, bool) {
	re := regexp.MustCompile(`constant\s+` + regexp.QuoteMeta(name) + `\s*=\s*(\d+)\s*;`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseUint(m[1], 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(v), true
}

// TestContractRefusesTheSameWindowCeilingWeDo keeps the two ceiling checks aligned.
//
// The contract's constructor reverts above 250 and Config.Validate refuses above 250. If one moved
// without the other, either a configuration Go accepted would fail to deploy, or one it rejected would
// be live and unreachable through the orchestrator.
func TestContractRefusesTheSameWindowCeilingWeDo(t *testing.T) {
	path := filepath.Join("..", "..", "..", "contracts", "src", "AcreSyncBallot.sol")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the ballot contract: %v", err)
	}

	if !bytes.Contains(raw, []byte("revealWindowBlocks > 250")) {
		t.Fatal("the contract's 250-block window ceiling has moved or been reworded; Config.Validate " +
			"mirrors it and must be updated together")
	}
}
