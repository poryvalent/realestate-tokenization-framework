// Package settlement turns a drawn ballot into the cap table on-chain.
//
// # The three transactions
//
// beginSettlement binds the settlement to one anchored ballot result and declares how many holders and
// units are coming. settleBatch credits them in chunks behind a strict cursor. finaliseSettlement checks
// five invariants and, only if all hold, moves the scheme to Settled.
//
// Nothing here is arithmetic on money. The draw already decided who gets what and the ASBA instruction
// already decided what is debited. What this package owns is the part that is easy to get wrong and
// impossible to undo: the counts.
//
// # The arithmetic that decides whether finalisation succeeds
//
// The scheme is 500 units, of which the investment manager holds 25 and the public holds 475. The
// manager never bids, so the ballot only ever allots the public 475. recordImSubscription credits the
// manager's units directly and deliberately does NOT pass through the settlement cursor, so:
//
//	expectedUnits    = the ballot's allotted units      (475, public only)
//	expectedHolders  = the number of settleBatch entries (the allottees, public only)
//	totalUnitsIssued = 25 (manager) + 475 (public)      = 500 = totalUnits
//
// Declaring expectedUnits as 500 would make every batch fail the running check. Omitting the manager's
// subscription instead leaves totalUnitsIssued at 475 and finaliseSettlement reverts on its first check,
// short by exactly 25, with an error that names a total rather than the missing step.
package settlement

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/acresync/orchestrator/internal/ballotrun"
	"github.com/acresync/orchestrator/internal/merkle"
)

// Stage mirrors the contract's SettlementStage enum, ordinals included.
type Stage uint8

const (
	StageNotStarted Stage = 0
	StageInProgress Stage = 1
	StageFinalised  Stage = 2
)

func (s Stage) String() string {
	switch s {
	case StageNotStarted:
		return "NOT_STARTED"
	case StageInProgress:
		return "IN_PROGRESS"
	case StageFinalised:
		return "FINALISED"
	}
	return fmt.Sprintf("UNKNOWN(%d)", uint8(s))
}

var (
	ErrNoAllottees        = errors.New("settlement: the draw produced no allottees")
	ErrZeroWallet         = errors.New("settlement: an allottee has no wallet address")
	ErrBadWallet          = errors.New("settlement: wallet address is not 0x-prefixed 40-hex lowercase")
	ErrDuplicateWallet    = errors.New("settlement: two allottees share a wallet address")
	ErrZeroUnits          = errors.New("settlement: an entry credits zero units")
	ErrBatchTooLarge      = errors.New("settlement: batch exceeds the contract's cap")
	ErrCursorMismatch     = errors.New("settlement: batch cursor does not match what is credited")
	ErrUnitsMismatch      = errors.New("settlement: credited units do not match the declared total")
	ErrHoldersMismatch    = errors.New("settlement: credited holders do not match the declared total")
	ErrHolderFloor        = errors.New("settlement: fewer countable holders than the statutory floor")
	ErrIMNotRecorded      = errors.New("settlement: the manager's subscription has not been recorded")
	ErrTotalUnitsMismatch = errors.New("settlement: issued units do not equal the scheme's total")
	ErrNotBoundToBallot   = errors.New("settlement: no anchored ballot result to bind to")
)

// walletRe is the address form the register and the allowlist both use.
var walletRe = regexp.MustCompile(`^0x[0-9a-f]{40}$`)

// SchemeParams are the immutable scheme constants the contract was deployed with.
//
// Held as data rather than constants so the same code serves a second scheme, and validated against the
// deploy script by test so they cannot silently drift from the live contract.
type SchemeParams struct {
	TotalUnits       uint32
	IMUnits          uint32
	MinPublicHolders uint32
	MaxBatchSize     uint32
}

// PublicUnits is what the ballot allots.
func (p SchemeParams) PublicUnits() uint32 { return p.TotalUnits - p.IMUnits }

func (p SchemeParams) Validate() error {
	if p.TotalUnits == 0 {
		return errors.New("settlement: total units must be positive")
	}
	if p.IMUnits == 0 {
		// The manager's holding is a regulatory minimum, not an option. Zero would mean the scheme has
		// no sponsor skin in the game and finaliseSettlement's third check could never be satisfied.
		return errors.New("settlement: the manager's holding must be positive")
	}
	if p.IMUnits >= p.TotalUnits {
		return fmt.Errorf("settlement: the manager holds %d of %d units, leaving nothing public",
			p.IMUnits, p.TotalUnits)
	}
	if p.MinPublicHolders == 0 {
		return errors.New("settlement: the holder floor must be positive")
	}
	if p.MinPublicHolders > p.PublicUnits() {
		return fmt.Errorf("settlement: a floor of %d holders cannot be met from %d public units, "+
			"because a unit cannot be split", p.MinPublicHolders, p.PublicUnits())
	}
	if p.MaxBatchSize == 0 {
		return errors.New("settlement: the batch cap must be positive")
	}
	return nil
}

// V1Params are the constants of the deployed scheme.
//
// TestSchemeParamsMatchTheDeployedContract parses these out of the deploy script, because a value here
// that disagrees with the immutable on-chain one produces transactions that revert.
func V1Params() SchemeParams {
	return SchemeParams{TotalUnits: 500, IMUnits: 25, MinPublicHolders: 200, MaxBatchSize: 100}
}

// Entry is one holder to credit.
type Entry struct {
	// LeafIndex ties the entry back to the published allotment file.
	LeafIndex uint32

	InvestorID string
	BidID      string

	// Wallet is the register address credited on-chain.
	Wallet string

	Units uint32
}

// Batch is one settleBatch call.
type Batch struct {
	// CursorFrom must equal the contract's creditedHolders exactly.
	CursorFrom uint32

	Entries []Entry

	// Units is the sum credited by this batch.
	Units uint32
}

// CursorTo is where the cursor lands if this batch succeeds.
func (b Batch) CursorTo() uint32 { return b.CursorFrom + uint32(len(b.Entries)) }

// Wallets returns the address array for the call.
func (b Batch) Wallets() []string {
	out := make([]string, 0, len(b.Entries))
	for _, e := range b.Entries {
		out = append(out, e.Wallet)
	}
	return out
}

// UnitsArray returns the units array for the call.
func (b Batch) UnitsArray() []uint32 {
	out := make([]uint32, 0, len(b.Entries))
	for _, e := range b.Entries {
		out = append(out, e.Units)
	}
	return out
}

// Plan is a complete settlement, ready to execute.
type Plan struct {
	SchemeID string
	OfferID  string

	Params SchemeParams

	// AllotmentFileHash is the digest anchored with the ballot result.
	AllotmentFileHash [32]byte

	// BallotResultRoot is the root beginSettlement reads from the ballot contract.
	BallotResultRoot merkle.Hash

	// Entries are the allottees in cursor order.
	Entries []Entry

	// Batches partition Entries under the contract's cap.
	Batches []Batch

	// ExpectedUnits and ExpectedHolders are the beginSettlement arguments.
	//
	// Public only. The manager's units are credited outside the cursor; see the package comment.
	ExpectedUnits   uint32
	ExpectedHolders uint32

	// IMWallet is the manager's register address, credited by recordImSubscription.
	IMWallet string
}

// BuildInput is everything needed to plan a settlement.
type BuildInput struct {
	Run *ballotrun.Run

	Params SchemeParams

	// Wallets maps investor identifier to register address.
	//
	// Passed in rather than looked up, because the mapping is the one part of settlement that comes from
	// KYC records rather than from the draw, and it is the part most likely to be incomplete.
	Wallets map[string]string

	// IMWallet is the manager's address.
	IMWallet string

	// AllotmentFileHash is the anchored document digest.
	AllotmentFileHash [32]byte

	// BallotResultRoot is the anchored result root.
	BallotResultRoot merkle.Hash
}

// BuildPlan turns a drawn ballot into an executable settlement.
//
// Every check the contract performs is performed here first, plus one it cannot: see checkWallets.
func BuildPlan(in BuildInput) (*Plan, error) {
	if in.Run == nil {
		return nil, errors.New("settlement: no draw to settle")
	}
	if err := in.Params.Validate(); err != nil {
		return nil, err
	}
	if err := in.Run.Validate(); err != nil {
		return nil, fmt.Errorf("settlement: the draw is not anchorable, so it cannot be settled: %w", err)
	}

	if in.BallotResultRoot.IsZero() {
		// beginSettlement reads ballot.resultRoot() and requires it non-zero. That read is what binds
		// the settlement permanently to one ballot outcome, so a zero here means the result was never
		// anchored and the settlement would have nothing to be bound to.
		return nil, ErrNotBoundToBallot
	}
	if in.AllotmentFileHash == ([32]byte{}) {
		return nil, errors.New("settlement: the allotment file hash is zero; beginSettlement rejects it")
	}
	if in.BallotResultRoot != in.Run.Result.ResultRoot {
		return nil, fmt.Errorf("%w: settling against root %s but the draw produced %s",
			ErrNotBoundToBallot, in.BallotResultRoot.Hex(), in.Run.Result.ResultRoot.Hex())
	}

	if err := checkIMWallet(in.IMWallet); err != nil {
		return nil, err
	}

	// Only allottees are credited. A nil bid has zero units and settleBatch reverts on a zero amount
	// inside its loop, which would fail the whole batch rather than skip the entry.
	allottees := in.Run.Allottees()
	if len(allottees) == 0 {
		return nil, ErrNoAllottees
	}

	entries := make([]Entry, 0, len(allottees))
	for _, l := range allottees {
		wallet, ok := in.Wallets[l.InvestorID]
		if !ok || wallet == "" {
			// An allottee with no wallet cannot be credited, and the contract's only response would be
			// to revert on a zero address partway through a batch.
			return nil, fmt.Errorf("%w: leaf %d (bid %s) has no register address",
				ErrZeroWallet, l.LeafIndex, l.BidID)
		}
		if l.UnitsAllotted == 0 {
			return nil, fmt.Errorf("%w: leaf %d", ErrZeroUnits, l.LeafIndex)
		}

		entries = append(entries, Entry{
			LeafIndex:  l.LeafIndex,
			InvestorID: l.InvestorID,
			BidID:      l.BidID,
			Wallet:     wallet,
			Units:      l.UnitsAllotted,
		})
	}

	// Cursor order is by leaf index.
	//
	// The cursor is strictly sequential, so the order has to be a function of the data rather than of
	// whatever order a query returned. Leaf index is the ordering the allotment file already publishes,
	// so a reader can map a cursor position in an event back to a published line.
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].LeafIndex < entries[j].LeafIndex })

	if err := checkWallets(entries, in.IMWallet); err != nil {
		return nil, err
	}

	plan := &Plan{
		SchemeID:          in.Run.SchemeID,
		OfferID:           in.Run.OfferID,
		Params:            in.Params,
		AllotmentFileHash: in.AllotmentFileHash,
		BallotResultRoot:  in.BallotResultRoot,
		Entries:           entries,
		IMWallet:          in.IMWallet,
	}

	var units uint32
	for _, e := range entries {
		units += e.Units
	}
	plan.ExpectedUnits = units
	plan.ExpectedHolders = uint32(len(entries))

	// The declared totals must be the ballot's, not a recount that happens to agree.
	if plan.ExpectedUnits != in.Run.Result.UnitsAllotted {
		return nil, fmt.Errorf("%w: the entries credit %d units, the draw allotted %d",
			ErrUnitsMismatch, plan.ExpectedUnits, in.Run.Result.UnitsAllotted)
	}
	if plan.ExpectedHolders != in.Run.Result.DistinctAllottees {
		return nil, fmt.Errorf("%w: %d entries against %d allottees",
			ErrHoldersMismatch, plan.ExpectedHolders, in.Run.Result.DistinctAllottees)
	}

	// The public side must be exactly the public allocation, because the manager's units make up the
	// difference and finaliseSettlement checks the sum against the scheme's fixed total.
	if plan.ExpectedUnits != in.Params.PublicUnits() {
		return nil, fmt.Errorf("%w: settling %d public units against a public allocation of %d. "+
			"The manager's %d units are credited separately by recordImSubscription, so the public "+
			"side must account for the remaining %d exactly",
			ErrUnitsMismatch, plan.ExpectedUnits, in.Params.PublicUnits(),
			in.Params.IMUnits, in.Params.PublicUnits())
	}

	if plan.ExpectedHolders < in.Params.MinPublicHolders {
		return nil, fmt.Errorf("%w: %d holders against a floor of %d",
			ErrHolderFloor, plan.ExpectedHolders, in.Params.MinPublicHolders)
	}

	plan.Batches = partition(entries, in.Params.MaxBatchSize)

	if err := plan.Validate(); err != nil {
		return nil, err
	}
	return plan, nil
}

// checkIMWallet validates the manager's address.
func checkIMWallet(wallet string) error {
	if wallet == "" {
		return fmt.Errorf("%w: the manager's wallet is required, because finaliseSettlement checks "+
			"imWallet is set, holds exactly the manager's units, and is excluded from the holder "+
			"count", ErrIMNotRecorded)
	}
	if !walletRe.MatchString(wallet) {
		return fmt.Errorf("%w: manager wallet %q", ErrBadWallet, wallet)
	}
	if isZeroAddress(wallet) {
		return fmt.Errorf("%w: the manager's wallet is the zero address", ErrZeroWallet)
	}
	return nil
}

// checkWallets rejects malformed, zero and duplicated addresses.
//
// # The duplicate check is the one the contract cannot make
//
// settleBatch credits additively: _setBalance(holder, unitsOf[holder] + amount). If two investors share
// a wallet, both entries land on the same address. The array entry count still advances the cursor twice,
// the units still sum correctly, and totalUnitsIssued still reaches the scheme's total. But the holder
// array gains one member rather than two, so distinctHolderCount ends one short.
//
// Every one of finaliseSettlement's five checks still passes: the issued total is right, the holder count
// is merely one lower and still above the floor, the manager's holding is untouched, and credited units
// and holders both equal what was declared. The settlement completes and the register is wrong, with two
// investors' units commingled in one address and neither individually creditable or payable.
//
// This is demonstrated rather than asserted. contracts/test/SchemeWalletCollision.t.sol settles 300
// holders with one collision and shows the scheme reaching Settled with 299 holders on the register, one
// address holding both allotments and the other holding nothing.
//
// The same file establishes the boundary: at exactly the holder floor the collision IS caught, because
// 200 minus one fails the second check. So the contract catches this only when the allotment happens to
// sit on the floor, which is the one case an operator cannot arrange to rely on.
//
// None of that is a contract defect. The contract is handed an address and cannot know two entries were
// meant to be two people. Which is why the check belongs here, where the investor identities still exist.
func checkWallets(entries []Entry, imWallet string) error {
	seen := make(map[string]Entry, len(entries))

	for _, e := range entries {
		if !walletRe.MatchString(e.Wallet) {
			return fmt.Errorf("%w: leaf %d has %q", ErrBadWallet, e.LeafIndex, e.Wallet)
		}
		if isZeroAddress(e.Wallet) {
			return fmt.Errorf("%w: leaf %d", ErrZeroWallet, e.LeafIndex)
		}

		if prev, dup := seen[e.Wallet]; dup {
			return fmt.Errorf("%w: leaf %d (investor %s) and leaf %d (investor %s) both credit %s. "+
				"The contract credits additively, so both would land on one address: the units and "+
				"the cursor would still reconcile, distinctHolderCount would end one short, and all "+
				"five finalisation checks would still pass on a register that had lost a holder",
				ErrDuplicateWallet, prev.LeafIndex, prev.InvestorID, e.LeafIndex, e.InvestorID, e.Wallet)
		}
		seen[e.Wallet] = e

		if strings.EqualFold(e.Wallet, imWallet) {
			// The manager's address is excluded from the holder count. A public allottee sharing it
			// would have their units credited to an excluded address and vanish from the statutory
			// count while still being owed a distribution.
			return fmt.Errorf("%w: leaf %d credits the manager's wallet %s, which is excluded from "+
				"the holder count", ErrDuplicateWallet, e.LeafIndex, imWallet)
		}
	}
	return nil
}

func isZeroAddress(w string) bool {
	return w == "0x0000000000000000000000000000000000000000"
}

// partition splits entries into batches no larger than the cap.
func partition(entries []Entry, cap_ uint32) []Batch {
	batches := make([]Batch, 0, (len(entries)+int(cap_)-1)/int(cap_))

	for start := 0; start < len(entries); start += int(cap_) {
		end := start + int(cap_)
		if end > len(entries) {
			end = len(entries)
		}

		chunk := entries[start:end]
		var units uint32
		for _, e := range chunk {
			units += e.Units
		}

		batches = append(batches, Batch{
			// The cursor is the count of entries already credited, which for a contiguous partition is
			// the start offset. Stated as such rather than accumulated separately, so the cursor cannot
			// drift from the slice boundaries it describes.
			CursorFrom: uint32(start),
			Entries:    chunk,
			Units:      units,
		})
	}
	return batches
}

// Validate checks the plan holds together.
func (p *Plan) Validate() error {
	if len(p.Entries) == 0 {
		return ErrNoAllottees
	}
	if len(p.Batches) == 0 {
		return errors.New("settlement: no batches")
	}

	var cursor uint32
	var units uint32
	var seen int

	for i, b := range p.Batches {
		if len(b.Entries) == 0 {
			return fmt.Errorf("settlement: batch %d is empty; settleBatch reverts with EmptyBatch", i)
		}
		if uint32(len(b.Entries)) > p.Params.MaxBatchSize {
			return fmt.Errorf("%w: batch %d has %d entries against a cap of %d",
				ErrBatchTooLarge, i, len(b.Entries), p.Params.MaxBatchSize)
		}

		// Strict cursor equality. Batches cannot overlap, skip or be reordered.
		if b.CursorFrom != cursor {
			return fmt.Errorf("%w: batch %d starts at %d, but %d entries are credited before it",
				ErrCursorMismatch, i, b.CursorFrom, cursor)
		}

		var batchUnits uint32
		for _, e := range b.Entries {
			if e.Units == 0 {
				return fmt.Errorf("%w: batch %d, leaf %d", ErrZeroUnits, i, e.LeafIndex)
			}
			batchUnits += e.Units
		}
		if batchUnits != b.Units {
			return fmt.Errorf("settlement: batch %d reports %d units, its entries sum to %d",
				i, b.Units, batchUnits)
		}

		// The running total the contract checks on every batch.
		if units+batchUnits > p.ExpectedUnits {
			return fmt.Errorf("%w: batch %d would bring credited units to %d against a declared %d",
				ErrUnitsMismatch, i, units+batchUnits, p.ExpectedUnits)
		}

		cursor = b.CursorTo()
		units += batchUnits
		seen += len(b.Entries)
	}

	if seen != len(p.Entries) {
		return fmt.Errorf("settlement: the batches cover %d of %d entries", seen, len(p.Entries))
	}
	if cursor != p.ExpectedHolders {
		return fmt.Errorf("%w: the batches credit %d holders against a declared %d",
			ErrHoldersMismatch, cursor, p.ExpectedHolders)
	}
	if units != p.ExpectedUnits {
		return fmt.Errorf("%w: the batches credit %d units against a declared %d",
			ErrUnitsMismatch, units, p.ExpectedUnits)
	}

	// The whole point of the manager's separate subscription.
	if total := units + p.Params.IMUnits; total != p.Params.TotalUnits {
		return fmt.Errorf("%w: %d public units plus the manager's %d is %d, and the scheme is %d",
			ErrTotalUnitsMismatch, units, p.Params.IMUnits, total, p.Params.TotalUnits)
	}

	return nil
}

// BatchAt returns the batch whose cursor matches, which is the only one the contract will accept next.
func (p *Plan) BatchAt(cursor uint32) (Batch, error) {
	for _, b := range p.Batches {
		if b.CursorFrom == cursor {
			return b, nil
		}
	}
	if cursor == p.ExpectedHolders {
		return Batch{}, fmt.Errorf("settlement: every holder is credited at cursor %d; the next step "+
			"is finaliseSettlement", cursor)
	}
	return Batch{}, fmt.Errorf("%w: no batch begins at %d", ErrCursorMismatch, cursor)
}
