package snapshot

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/acresync/orchestrator/internal/merkle"
)

// MinUnitholders is the statutory minimum number of countable unitholders.
const MinUnitholders = 200

var (
	ErrNoHoldings        = errors.New("snapshot: register is empty")
	ErrDuplicateAnchor   = errors.New("snapshot: two lines share an investor anchor")
	ErrDuplicateWallet   = errors.New("snapshot: two lines share a wallet address")
	ErrDuplicateInvestor = errors.New("snapshot: two lines share an investor id")
	ErrUnitsMismatch     = errors.New("snapshot: register total does not match the units issued")
	ErrTooFewHolders     = errors.New("snapshot: countable unitholders below the statutory minimum")
	ErrRecordDateBefore  = errors.New("snapshot: record date precedes the period it settles")
)

// Holding is one investor's position at the record date.
type Holding struct {
	// InvestorID is the internal UUID. Never published.
	InvestorID string

	// WalletAddress is the canonical lowercase form.
	WalletAddress string

	// InvestorAnchor is the HMAC of the internal investor id under a key held in KMS.
	//
	// Published in place of any identifying detail. It is not a hash of the PAN: the PAN space is
	// small enough to enumerate, so a plain digest of one is reversible by brute force in minutes.
	// An HMAC under a key that never leaves KMS is not.
	InvestorAnchor [32]byte

	Units uint32

	// ExcludedFromHolderCount marks a holder who does not count toward the statutory minimum.
	//
	// # The trap in this field
	//
	// Excluded from the *count*, not from the *distribution*. The investment manager's 25 units do
	// not help satisfy the 200-unitholder requirement, but the manager is still entitled to
	// distributions on those units, and the entitlement denominator is therefore every unit issued.
	//
	// Reading this as "excluded from distribution" would divide the distributable amount across 475
	// units instead of 500, overpaying every public holder by about 5% and leaving the manager
	// unpaid. The figure would satisfy the 95% floor and reconcile against itself, so nothing
	// downstream would catch it. See EntitlementDenominator.
	ExcludedFromHolderCount bool
}

// Line is a frozen snapshot row with its computed leaf.
type Line struct {
	Holding
	LeafIndex uint32
	Leaf      merkle.Hash
}

// BuildInput is everything needed to freeze a register.
type BuildInput struct {
	SchemeRef [32]byte
	PeriodID  uint32

	// RecordDate is the date on which entitlement is determined.
	RecordDate time.Time

	// PeriodEnd bounds the period being settled. The record date must not precede it.
	PeriodEnd time.Time

	// TakenAt is the business time the freeze happened, read from the scheme clock.
	TakenAt time.Time

	Holdings []Holding

	// ExpectedTotalUnits is the scheme's issued unit count, checked against the register sum.
	//
	// Supplied rather than inferred. The register is the thing under test here: if a holding was
	// dropped by a bad join or double-counted by a retry, summing the register and trusting the
	// result would anchor the mistake. Comparing against the independently known issued count turns a
	// silent discrepancy into a refusal.
	ExpectedTotalUnits uint32

	// RequireMinimumHolders enforces the 200-unitholder floor.
	//
	// Configurable only so tests can build small fixtures. Production paths set it true.
	RequireMinimumHolders bool
}

// Snapshot is a frozen register with its Merkle commitment.
type Snapshot struct {
	SchemeRef  [32]byte
	PeriodID   uint32
	RecordDate time.Time
	TakenAt    time.Time

	// TotalUnits is every unit on the register, including excluded holders.
	TotalUnits uint32

	// DistinctHolders counts only holders who are not excluded.
	DistinctHolders uint32

	MerkleRoot  merkle.Hash
	AlgoVersion int

	Lines []Line

	tree     *merkle.Tree
	byWallet map[string]uint32
}

// EntitlementDenominator returns the divisor for pro-rata entitlement.
//
// Every issued unit, including units held by excluded holders. A method rather than a bare field so
// there is one named answer to the question and no temptation to compute a denominator at the call
// site from whatever fields look relevant.
func (s *Snapshot) EntitlementDenominator() uint32 { return s.TotalUnits }

// MeetsHolderMinimum reports whether the countable holder count satisfies the statutory floor.
func (s *Snapshot) MeetsHolderMinimum() bool { return s.DistinctHolders >= MinUnitholders }

// Build freezes a register into an ordered, committed snapshot.
func Build(in BuildInput) (*Snapshot, error) {
	if len(in.Holdings) == 0 {
		return nil, ErrNoHoldings
	}
	if in.RecordDate.Before(in.PeriodEnd) {
		return nil, fmt.Errorf("%w: record date %s is before period end %s",
			ErrRecordDateBefore, isoDate(in.RecordDate), isoDate(in.PeriodEnd))
	}

	ordered, err := orderHoldings(in.Holdings)
	if err != nil {
		return nil, err
	}

	snap := &Snapshot{
		SchemeRef:   in.SchemeRef,
		PeriodID:    in.PeriodID,
		RecordDate:  in.RecordDate,
		TakenAt:     in.TakenAt,
		AlgoVersion: AlgoVersion,
		Lines:       make([]Line, 0, len(ordered)),
		byWallet:    make(map[string]uint32, len(ordered)),
	}

	leafHashes := make([]merkle.Hash, 0, len(ordered))

	for i, h := range ordered {
		if h.Units == 0 {
			// The database requires units > 0 on a snapshot line. A zero-unit holder is not a
			// holder at the record date, and including one would inflate the holder count toward
			// the statutory minimum with positions that are entitled to nothing.
			return nil, fmt.Errorf("%w: investor %s", ErrZeroUnits, h.InvestorID)
		}
		if h.InvestorAnchor == ([32]byte{}) {
			return nil, fmt.Errorf("%w: investor %s", ErrZeroAnchor, h.InvestorID)
		}

		addr, err := ParseAddress(h.WalletAddress)
		if err != nil {
			return nil, fmt.Errorf("investor %s: %w", h.InvestorID, err)
		}

		idx := uint32(i)
		leaf := EntitlementLeaf(idx, addr, h.InvestorAnchor, h.Units, h.ExcludedFromHolderCount)

		snap.Lines = append(snap.Lines, Line{Holding: h, LeafIndex: idx, Leaf: leaf})
		leafHashes = append(leafHashes, leaf)
		snap.byWallet[h.WalletAddress] = idx

		total := snap.TotalUnits + h.Units
		if total < snap.TotalUnits {
			return nil, fmt.Errorf("snapshot: unit total overflowed uint32 at investor %s", h.InvestorID)
		}
		snap.TotalUnits = total

		if !h.ExcludedFromHolderCount {
			snap.DistinctHolders++
		}
	}

	if in.ExpectedTotalUnits != 0 && snap.TotalUnits != in.ExpectedTotalUnits {
		return nil, fmt.Errorf("%w: register sums to %d, scheme has issued %d",
			ErrUnitsMismatch, snap.TotalUnits, in.ExpectedTotalUnits)
	}
	if in.RequireMinimumHolders && !snap.MeetsHolderMinimum() {
		return nil, fmt.Errorf("%w: %d countable holders, minimum is %d",
			ErrTooFewHolders, snap.DistinctHolders, MinUnitholders)
	}

	tree, err := merkle.New(leafHashes)
	if err != nil {
		return nil, fmt.Errorf("snapshot: building the tree: %w", err)
	}
	snap.tree = tree
	snap.MerkleRoot = tree.Root()

	return snap, nil
}

// orderHoldings sorts the register into a total order and rejects ambiguity.
//
// # Why the order is imposed rather than preserved
//
// Leaf index is part of the leaf, and the leaf set determines the root. So the root is only
// reproducible if the ordering is a function of the data. Whatever order the database returned would
// not be: a query plan change, a new index, or a reconciliation that rewrote rows would reorder the
// register and produce a different root for an identical set of holdings.
//
// The anchor is the sort key because it is present on every line, unique per investor, and fixed
// width, so the comparison is total without tiebreaks.
func orderHoldings(in []Holding) ([]Holding, error) {
	out := make([]Holding, len(in))
	copy(out, in)

	sort.SliceStable(out, func(i, j int) bool {
		return bytes.Compare(out[i].InvestorAnchor[:], out[j].InvestorAnchor[:]) < 0
	})

	seenAnchor := make(map[[32]byte]string, len(out))
	seenWallet := make(map[string]string, len(out))
	seenInvestor := make(map[string]bool, len(out))

	for _, h := range out {
		if prev, dup := seenAnchor[h.InvestorAnchor]; dup {
			// Two investors cannot share an anchor. If they did, the sort key would not be total and
			// the register order, and therefore the root, would depend on input order again.
			return nil, fmt.Errorf("%w: investors %s and %s", ErrDuplicateAnchor, prev, h.InvestorID)
		}
		seenAnchor[h.InvestorAnchor] = h.InvestorID

		if prev, dup := seenWallet[h.WalletAddress]; dup {
			// Rejected because locating your own line by wallet address is how a holder verifies
			// their entitlement without access to our systems. Two lines on one wallet makes that
			// lookup ambiguous, and the holder cannot tell which line is theirs.
			return nil, fmt.Errorf("%w: %s held by investors %s and %s",
				ErrDuplicateWallet, h.WalletAddress, prev, h.InvestorID)
		}
		seenWallet[h.WalletAddress] = h.InvestorID

		if seenInvestor[h.InvestorID] {
			// The database enforces one line per investor per snapshot. Two rows would double-count
			// the position and overpay.
			return nil, fmt.Errorf("%w: %s", ErrDuplicateInvestor, h.InvestorID)
		}
		seenInvestor[h.InvestorID] = true
	}

	return out, nil
}

// Proof returns the inclusion proof for a leaf index.
func (s *Snapshot) Proof(leafIndex uint32) ([]merkle.Hash, error) {
	return s.tree.Proof(int(leafIndex))
}

// ProofForWallet returns the leaf and proof for a wallet address.
//
// The lookup a holder actually performs: they know their address, not their leaf index.
func (s *Snapshot) ProofForWallet(wallet string) (Line, []merkle.Hash, error) {
	idx, ok := s.byWallet[wallet]
	if !ok {
		return Line{}, nil, fmt.Errorf("snapshot: %s is not on the register at the record date", wallet)
	}
	proof, err := s.Proof(idx)
	if err != nil {
		return Line{}, nil, err
	}
	return s.Lines[idx], proof, nil
}

// VerifyLine checks a line against the root using only published values.
//
// Recomputes the leaf from the line's fields rather than reusing the stored digest. Comparing a
// stored leaf against a root built from those same stored leaves proves nothing; recomputing proves
// the encoding rule and the tree agree, which is what an external verifier will be doing.
func (s *Snapshot) VerifyLine(line Line, proof []merkle.Hash) bool {
	addr, err := ParseAddress(line.WalletAddress)
	if err != nil {
		return false
	}
	leaf := EntitlementLeaf(line.LeafIndex, addr, line.InvestorAnchor, line.Units, line.ExcludedFromHolderCount)
	return merkle.Verify(s.MerkleRoot, leaf, proof)
}

// UnitsFor returns a holder's units at the record date.
func (s *Snapshot) UnitsFor(wallet string) (uint32, bool) {
	idx, ok := s.byWallet[wallet]
	if !ok {
		return 0, false
	}
	return s.Lines[idx].Units, true
}

func isoDate(t time.Time) string { return t.UTC().Format("2006-01-02") }
