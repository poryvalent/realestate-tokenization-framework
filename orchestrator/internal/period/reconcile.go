package period

import (
	"fmt"
	"sort"
)

// ReconciliationStatus mirrors the reconciliation_status Postgres enum.
type ReconciliationStatus string

const (
	ReconMatched  ReconciliationStatus = "MATCHED"
	ReconDiverged ReconciliationStatus = "DIVERGED"
	ReconResolved ReconciliationStatus = "RESOLVED"
)

// AllReconciliationStatuses lists every reconciliation outcome.
func AllReconciliationStatuses() []ReconciliationStatus {
	return []ReconciliationStatus{ReconMatched, ReconDiverged, ReconResolved}
}

// Position is one holder's unit count according to one source.
type Position struct {
	InvestorID    string
	WalletAddress string
	Units         uint32
}

// Diff is one holder whose two sources disagree.
type Diff struct {
	InvestorID    string
	WalletAddress string

	DepositoryUnits uint32
	ChainUnits      uint32

	// DiffUnits is depository minus chain, signed. The database constrains it to exactly that
	// difference and to be non-zero, so a Diff that is not a difference cannot be stored.
	DiffUnits int64
}

// FavoursDepository reports whether the depository shows more units than our mirror.
//
// Worth distinguishing because the two directions mean different things. More units at the
// depository means a transfer settled that we never saw, so our register is stale and a
// distribution would underpay the new holder. Fewer means we credited something the depository did
// not, which is worse: we would pay someone who does not legally hold the units.
func (d Diff) FavoursDepository() bool { return d.DiffUnits > 0 }

// Reconciliation is the outcome of comparing our mirror against the depository.
type Reconciliation struct {
	Status ReconciliationStatus

	DepositoryTotalUnits  uint32
	ChainTotalUnits       uint32
	DepositoryHolderCount int
	ChainHolderCount      int

	Diffs []Diff

	// BlocksPayout is true exactly when Status is DIVERGED.
	//
	// The database expresses the same rule as a CHECK constraint, because a diverged run that does
	// not block payouts is precisely the failure the whole design exists to prevent. Carried as a
	// field rather than computed at each call site so the stored row and the in-memory value cannot
	// drift.
	BlocksPayout bool
}

// DivergenceCount returns the number of disagreeing holders.
func (r *Reconciliation) DivergenceCount() int { return len(r.Diffs) }

// Reconcile compares the depository register against our mirror.
//
// # Why the depository wins
//
// The depository is the legal register. Our register is a mirror maintained for speed and for the
// audit trail, and when the two disagree the mirror is wrong by definition, whatever our ledger
// history says. So this function does not attempt to decide which side is correct: it reports the
// disagreement and blocks distribution until a human resolves it.
//
// Automatically reconciling toward either side would be worse than stopping. Copying the depository
// silently discards the mirror's history, which is the only thing that makes the divergence
// diagnosable. Copying the mirror asserts our records over the legal ones.
func Reconcile(depository, mirror []Position) (*Reconciliation, error) {
	depByWallet := make(map[string]Position, len(depository))
	for _, p := range depository {
		if prev, dup := depByWallet[p.WalletAddress]; dup {
			return nil, fmt.Errorf("period: depository lists %s twice (investors %s and %s)",
				p.WalletAddress, prev.InvestorID, p.InvestorID)
		}
		depByWallet[p.WalletAddress] = p
	}

	mirrorByWallet := make(map[string]Position, len(mirror))
	for _, p := range mirror {
		if prev, dup := mirrorByWallet[p.WalletAddress]; dup {
			return nil, fmt.Errorf("period: mirror lists %s twice (investors %s and %s)",
				p.WalletAddress, prev.InvestorID, p.InvestorID)
		}
		mirrorByWallet[p.WalletAddress] = p
	}

	rec := &Reconciliation{}

	// Every wallet in either source is examined. Iterating only the mirror would miss a holder the
	// depository knows about and we do not, which is the more dangerous direction: that holder is
	// legally entitled and would receive nothing.
	wallets := make([]string, 0, len(depByWallet)+len(mirrorByWallet))
	seen := make(map[string]bool, len(depByWallet)+len(mirrorByWallet))
	for w := range depByWallet {
		if !seen[w] {
			seen[w] = true
			wallets = append(wallets, w)
		}
	}
	for w := range mirrorByWallet {
		if !seen[w] {
			seen[w] = true
			wallets = append(wallets, w)
		}
	}

	// Sorted so a rerun reports the same diffs in the same order. Map order would make a
	// reconciliation report differ between runs over identical data, and an operator comparing two
	// reports would be chasing noise.
	sort.Strings(wallets)

	for _, w := range wallets {
		dep, inDep := depByWallet[w]
		mir, inMirror := mirrorByWallet[w]

		if inDep {
			rec.DepositoryTotalUnits += dep.Units
			if dep.Units > 0 {
				rec.DepositoryHolderCount++
			}
		}
		if inMirror {
			rec.ChainTotalUnits += mir.Units
			if mir.Units > 0 {
				rec.ChainHolderCount++
			}
		}

		if dep.Units == mir.Units {
			continue
		}

		investor := dep.InvestorID
		if investor == "" {
			investor = mir.InvestorID
		}

		rec.Diffs = append(rec.Diffs, Diff{
			InvestorID:      investor,
			WalletAddress:   w,
			DepositoryUnits: dep.Units,
			ChainUnits:      mir.Units,
			DiffUnits:       int64(dep.Units) - int64(mir.Units),
		})
	}

	if len(rec.Diffs) == 0 {
		rec.Status = ReconMatched
		rec.BlocksPayout = false
	} else {
		rec.Status = ReconDiverged
		rec.BlocksPayout = true
	}

	return rec, nil
}

// Validate checks the invariant the database enforces as recon_divergence_blocks.
//
// Asserted in Go as well because the pairing of status, count and blocking flag is the load-bearing
// part of this record. A DIVERGED run that does not block payouts would let a distribution proceed
// over a register we know to be wrong, and the constraint is cheap enough to check twice.
func (r *Reconciliation) Validate() error {
	switch r.Status {
	case ReconMatched:
		if len(r.Diffs) != 0 {
			return fmt.Errorf("period: MATCHED with %d divergences", len(r.Diffs))
		}
		if r.BlocksPayout {
			return fmt.Errorf("period: MATCHED must not block payouts")
		}
	case ReconDiverged:
		if len(r.Diffs) == 0 {
			return fmt.Errorf("period: DIVERGED with no divergences")
		}
		if !r.BlocksPayout {
			return fmt.Errorf("period: a DIVERGED run must block payouts; this is the exact " +
				"failure the design exists to prevent")
		}
	case ReconResolved:
		// Resolution is a human act recorded with a timestamp; nothing to check on the counts here.
	default:
		return fmt.Errorf("period: unknown reconciliation status %q", r.Status)
	}

	for _, d := range r.Diffs {
		if d.DiffUnits == 0 {
			return fmt.Errorf("period: %s recorded as a divergence with no difference", d.WalletAddress)
		}
		if want := int64(d.DepositoryUnits) - int64(d.ChainUnits); d.DiffUnits != want {
			return fmt.Errorf("period: %s diff is %d, should be %d", d.WalletAddress, d.DiffUnits, want)
		}
	}
	return nil
}

// Summary renders a one-line description for an operator or a log.
func (r *Reconciliation) Summary() string {
	if r.Status == ReconMatched {
		return fmt.Sprintf("matched: %d holders, %d units", r.ChainHolderCount, r.ChainTotalUnits)
	}
	return fmt.Sprintf("%s: %d holder(s) disagree; depository %d units across %d holders, "+
		"mirror %d units across %d holders",
		r.Status, len(r.Diffs),
		r.DepositoryTotalUnits, r.DepositoryHolderCount,
		r.ChainTotalUnits, r.ChainHolderCount)
}
