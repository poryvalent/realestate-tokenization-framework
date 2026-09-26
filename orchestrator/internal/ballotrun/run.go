package ballotrun

import (
	"errors"
	"fmt"
	"time"

	"github.com/acresync/orchestrator/internal/asba"
	"github.com/acresync/orchestrator/internal/ballot"
	"github.com/acresync/orchestrator/internal/bidbook"
	"github.com/acresync/orchestrator/internal/merkle"
	"github.com/acresync/orchestrator/internal/money"
)

var (
	ErrAllotteeFloor    = errors.New("ballotrun: fewer allottees than the statutory floor")
	ErrOverAllotted     = errors.New("ballotrun: more units allotted than offered")
	ErrCountsImpossible = errors.New("ballotrun: more allottees than units allotted")
	ErrMoneyMismatch    = errors.New("ballotrun: an allocation does not reconcile against its bid")
)

// Line is one bid's outcome, joined back to the bid it belongs to.
//
// ballot.Allocation works in leaf indices because that is what the published document and the contract
// verify against. The database works in bid identifiers. This carries both so the join happens once,
// against the book that produced the leaf indices, rather than at every call site.
type Line struct {
	ballot.Allocation

	BidID      string
	InvestorID string
	BidRef     string

	// Block is the ASBA reservation, needed to compute the settle instruction.
	Block *asba.Block
}

// SettleDebitPaise is what must be debited for this bid.
func (l Line) SettleDebitPaise() money.Paise { return l.AmountPayable }

// SettleReleasePaise is what must be handed back.
func (l Line) SettleReleasePaise() money.Paise { return l.RefundAmount }

// Run is a completed draw.
type Run struct {
	SchemeRef merkle.Hash
	SchemeID  string
	OfferID   string

	DrawnAt time.Time

	// Result is the engine's output, untouched.
	Result *ballot.Result

	// Lines are the allocations joined to their bids, ordered by leaf index.
	Lines []Line

	// UnitsOnOffer and MinDistinctHolders are the offer terms the contract re-checks at anchor time.
	UnitsOnOffer       uint32
	MinDistinctHolders uint32
}

// RunInput is everything needed to draw.
type RunInput struct {
	Book *bidbook.Book

	// Ceremony supplies the anchored commitment, the anchored book root and the target block.
	Ceremony *Ceremony

	Secret          merkle.Hash
	TargetBlockHash merkle.Hash

	Params ballot.Params

	DrawnAt time.Time
}

// Draw runs the allocation and joins the outcome back to the bids.
//
// # What is checked before the draw
//
// The book handed in must be the book that was anchored. Drawing against a different book would produce
// allocations for leaf indices that are not the ones the public document describes, and because the
// anchored root feeds the seed, it would also mean the seed was derived from a root that does not
// describe the population being allocated. Neither failure is visible in the output: the result would be
// internally consistent and wrong.
func Draw(in RunInput) (*Run, error) {
	if in.Book == nil {
		return nil, errors.New("ballotrun: no book to draw against")
	}
	if in.Ceremony == nil {
		return nil, errors.New("ballotrun: no ceremony state")
	}
	if in.Ceremony.Stage != StageSeedRevealed && in.Ceremony.Stage != StageEscalated {
		return nil, fmt.Errorf("%w: the draw requires SEED_REVEALED or ESCALATED, this ceremony is %s",
			ErrWrongStage, in.Ceremony.Stage)
	}

	if in.Ceremony.BidbookRoot.IsZero() {
		return nil, ErrBookNotAnchored
	}
	if in.Book.MerkleRoot != in.Ceremony.BidbookRoot {
		return nil, fmt.Errorf("%w: the book is %s, the anchored root is %s",
			ErrRootMismatch, in.Book.MerkleRoot.Hex(), in.Ceremony.BidbookRoot.Hex())
	}

	seed, err := in.Ceremony.FinalSeed(in.Secret, in.TargetBlockHash)
	if err != nil {
		return nil, err
	}

	result, err := ballot.Run(in.Book.BallotBids(), in.Params, seed)
	if err != nil {
		return nil, fmt.Errorf("ballotrun: running the allocation: %w", err)
	}

	// The engine binds its result to the root it was given. Re-checked because this is the value the
	// allotment file publishes and the settlement is later bound to.
	if result.BidbookRoot != in.Ceremony.BidbookRoot {
		return nil, fmt.Errorf("%w: the result claims root %s, the anchored root is %s",
			ErrRootMismatch, result.BidbookRoot.Hex(), in.Ceremony.BidbookRoot.Hex())
	}

	run := &Run{
		SchemeRef:          in.Book.SchemeRef,
		SchemeID:           in.Book.SchemeID,
		OfferID:            in.Book.OfferID,
		DrawnAt:            in.DrawnAt,
		Result:             result,
		UnitsOnOffer:       in.Params.UnitsOnOffer,
		MinDistinctHolders: in.Params.MinDistinctHolders,
		Lines:              make([]Line, 0, len(result.Allocations)),
	}

	// Every bid must appear, including the ones that won nothing.
	//
	// A losing bidder needs a published outcome to check their own rank against. Recording only winners
	// would make the draw unfalsifiable for precisely the people with the strongest reason to check it.
	if len(result.Allocations) != len(in.Book.Lines) {
		return nil, fmt.Errorf("ballotrun: %d allocations for %d bids; every bid needs an outcome, "+
			"including the nil ones", len(result.Allocations), len(in.Book.Lines))
	}

	for _, a := range result.Allocations {
		bookLine, ok := lineAt(in.Book, a.LeafIndex)
		if !ok {
			return nil, fmt.Errorf("ballotrun: allocation for leaf %d, which is not in the book",
				a.LeafIndex)
		}

		// The join is on leaf index, so the anchors must agree or the allocation has been matched to the
		// wrong investor. Silent here would mean crediting units to whoever happens to sit at that
		// position in a differently ordered book.
		if a.InvestorAnchor != bookLine.InvestorAnchor {
			return nil, fmt.Errorf("ballotrun: leaf %d is investor %s in the book and %s in the "+
				"allocation", a.LeafIndex, bookLine.InvestorAnchor.Hex(), a.InvestorAnchor.Hex())
		}

		if err := reconcileLine(a, bookLine); err != nil {
			return nil, err
		}

		run.Lines = append(run.Lines, Line{
			Allocation: a,
			BidID:      bookLine.BidID,
			InvestorID: bookLine.InvestorID,
			BidRef:     bookLine.BidRef,
			Block:      bookLine.Block,
		})
	}

	return run, nil
}

// reconcileLine checks one allocation's money against the bid it came from.
//
// # The identity
//
//	amountPayable + refundAmount == unitsBid * price
//
// with no tolerance, and it is the same identity the ASBA block must satisfy. Every paise reserved is
// either taken for units or handed back. A shortfall leaves an investor's money frozen with nothing
// claiming it; an excess instructs the bank to release more than it ever held.
//
// Checked per line rather than only in aggregate, because a total that balances can hide two investors
// wrong in opposite directions.
func reconcileLine(a ballot.Allocation, bookLine bidbook.Line) error {
	if a.UnitsAllotted > bookLine.UnitsBid {
		return fmt.Errorf("%w: leaf %d was allotted %d units against a bid for %d",
			ErrMoneyMismatch, a.LeafIndex, a.UnitsAllotted, bookLine.UnitsBid)
	}

	total, err := bookLine.TotalPaise()
	if err != nil {
		return err
	}

	sum, err := a.AmountPayable.Add(a.RefundAmount)
	if err != nil {
		return fmt.Errorf("leaf %d: %w", a.LeafIndex, err)
	}
	if sum != total {
		return fmt.Errorf("%w: leaf %d has %d payable plus %d refundable, which is %d against a "+
			"blocked amount of %d", ErrMoneyMismatch, a.LeafIndex,
			a.AmountPayable, a.RefundAmount, sum, total)
	}

	// The payable amount must be exactly the allotted units at the bid price. A bid partially filled at
	// a price other than the one bid is not a smaller version of that bid.
	wantPayable := money.Paise(int64(a.UnitsAllotted)) * bookLine.PricePerUnitPaise
	if a.AmountPayable != wantPayable {
		return fmt.Errorf("%w: leaf %d owes %d for %d units at %d, but the allocation says %d",
			ErrMoneyMismatch, a.LeafIndex, wantPayable, a.UnitsAllotted,
			bookLine.PricePerUnitPaise, a.AmountPayable)
	}

	if bookLine.Block != nil && bookLine.Block.BlockedPaise != total {
		return fmt.Errorf("%w: leaf %d reconciles to %d but %d is blocked",
			ErrMoneyMismatch, a.LeafIndex, total, bookLine.Block.BlockedPaise)
	}
	return nil
}

func lineAt(book *bidbook.Book, leafIndex uint32) (bidbook.Line, bool) {
	if leafIndex >= uint32(len(book.Lines)) {
		return bidbook.Line{}, false
	}
	l := book.Lines[leafIndex]
	if l.LeafIndex != leafIndex {
		return bidbook.Line{}, false
	}
	return l, true
}

// Allottees returns the lines that won at least one unit.
func (r *Run) Allottees() []Line {
	out := make([]Line, 0, r.Result.DistinctAllottees)
	for _, l := range r.Lines {
		if l.UnitsAllotted > 0 {
			out = append(out, l)
		}
	}
	return out
}

// Rejected returns the lines that won nothing.
func (r *Run) Rejected() []Line {
	out := make([]Line, 0)
	for _, l := range r.Lines {
		if l.UnitsAllotted == 0 {
			out = append(out, l)
		}
	}
	return out
}

// TotalPayablePaise is the sum to be debited across all bids.
func (r *Run) TotalPayablePaise() (money.Paise, error) {
	var total money.Paise
	var err error
	for _, l := range r.Lines {
		if total, err = total.Add(l.AmountPayable); err != nil {
			return 0, err
		}
	}
	return total, nil
}

// TotalRefundPaise is the sum to be released across all bids.
func (r *Run) TotalRefundPaise() (money.Paise, error) {
	var total money.Paise
	var err error
	for _, l := range r.Lines {
		if total, err = total.Add(l.RefundAmount); err != nil {
			return 0, err
		}
	}
	return total, nil
}

// Validate checks the run against everything anchorBallotResult will check.
//
// Run before a transaction is built. A revert costs gas and names a Solidity selector; this names the
// figure that is wrong and, for the holder floor, what it means.
func (r *Run) Validate() error {
	res := r.Result

	if res.ResultRoot.IsZero() {
		return errors.New("ballotrun: the result root is zero")
	}
	if res.UnitsAllotted == 0 {
		// anchorBallotResult rejects a zero allotment. An offer that allotted nothing is not a draw to
		// be anchored, it is an offer that failed, and it belongs on the abort path where the blocked
		// funds are released.
		return errors.New("ballotrun: nothing was allotted, so there is no result to anchor; an " +
			"offer that allots nothing goes down the abort path instead")
	}
	if res.DistinctAllottees == 0 {
		return errors.New("ballotrun: no distinct allottees")
	}

	if res.UnitsAllotted > r.UnitsOnOffer {
		return fmt.Errorf("%w: %d allotted against %d on offer",
			ErrOverAllotted, res.UnitsAllotted, r.UnitsOnOffer)
	}

	// The statutory floor. Checked at the anchor as well as at settlement because an allotment that
	// cannot reach the required number of unitholders produces a scheme that cannot list, and catching
	// it here is three transactions cheaper than catching it at finalisation.
	if res.DistinctAllottees < r.MinDistinctHolders {
		return fmt.Errorf("%w: %d allottees against a floor of %d; this allotment produces a scheme "+
			"that cannot be listed", ErrAllotteeFloor, res.DistinctAllottees, r.MinDistinctHolders)
	}

	// Every holder needs at least one whole unit, so allottees can never exceed units. Units are
	// indivisible: decimals() is zero.
	if res.DistinctAllottees > res.UnitsAllotted {
		return fmt.Errorf("%w: %d allottees holding %d units, and a unit cannot be split",
			ErrCountsImpossible, res.DistinctAllottees, res.UnitsAllotted)
	}

	// The counts must match the lines, not merely be plausible.
	var units uint32
	var allottees uint32
	for _, l := range r.Lines {
		units += l.UnitsAllotted
		if l.UnitsAllotted > 0 {
			allottees++
		}
	}
	if units != res.UnitsAllotted {
		return fmt.Errorf("ballotrun: the result claims %d units allotted, the lines sum to %d",
			res.UnitsAllotted, units)
	}
	if allottees != res.DistinctAllottees {
		return fmt.Errorf("ballotrun: the result claims %d allottees, the lines contain %d",
			res.DistinctAllottees, allottees)
	}

	return nil
}
