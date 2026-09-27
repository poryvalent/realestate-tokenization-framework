package period

import (
	"errors"
	"fmt"
	"time"
)

// DiffResolution mirrors the reconciliation_resolution Postgres enum.
//
// Per divergence rather than per run, because two holders can disagree for entirely different reasons in
// the same reconciliation: one because a transfer settled at the depository that we never mirrored, the
// other because our read of the depository caught it mid-settlement. Recording one reason for the run
// would lose that, and the reason is what tells an operator whether the same fault is recurring.
type DiffResolution string

const (
	// ResolutionPending is an open divergence. Payouts stay blocked.
	ResolutionPending DiffResolution = "PENDING"

	// ResolutionChainUpdated means our mirror was corrected to match the legal register.
	//
	// The ordinary case, and the only one that moves units. The depository is authoritative, so when it
	// shows a holding we do not, the remedy is to credit our side and not to argue.
	ResolutionChainUpdated DiffResolution = "CHAIN_UPDATED"

	// ResolutionDepositoryQueried means a fresh read of the depository now agrees.
	//
	// The divergence was an artefact of when we looked, not a disagreement. Nothing is corrected because
	// nothing was wrong; what changed is that we are no longer reading a settlement in progress.
	ResolutionDepositoryQueried DiffResolution = "DEPOSITORY_QUERIED"

	// ResolutionManual is anything else, and it must carry a narrative.
	//
	// Deliberately the least convenient option. A resolution nobody has to explain is the one that
	// quietly buries a mistake, so this path costs the operator a written justification whose digest is
	// recorded.
	ResolutionManual DiffResolution = "MANUAL"
)

func AllDiffResolutions() []DiffResolution {
	return []DiffResolution{
		ResolutionPending, ResolutionChainUpdated, ResolutionDepositoryQueried, ResolutionManual,
	}
}

// IsResolved reports whether this divergence is closed.
func (r DiffResolution) IsResolved() bool { return r != ResolutionPending && r != "" }

// RequiresChainTx reports whether the resolution must cite an on-chain correction.
//
// Only CHAIN_UPDATED. Our mirror is on-chain, so claiming to have corrected it without a transaction to
// point at is claiming something unverifiable.
func (r DiffResolution) RequiresChainTx() bool { return r == ResolutionChainUpdated }

// RequiresNarrative reports whether the resolution must carry a written justification.
func (r DiffResolution) RequiresNarrative() bool { return r == ResolutionManual }

// MovedOurRegister reports whether taking this path changed our unit counts.
func (r DiffResolution) MovedOurRegister() bool { return r == ResolutionChainUpdated }

var (
	ErrNotDiverged     = errors.New("period: nothing to resolve; the run is not diverged")
	ErrStillDiverged   = errors.New("period: the re-run still diverges, so nothing is resolved")
	ErrUnexplained     = errors.New("period: a resolution is missing its evidence")
	ErrDiffUnaccounted = errors.New("period: a divergence has no recorded resolution")
	ErrDiffNotInRun    = errors.New("period: a resolution refers to a divergence not in this run")
	ErrProofMismatched = errors.New("period: the re-run does not describe the same register")
)

// DiffOutcome is how one divergence was closed.
type DiffOutcome struct {
	// WalletAddress identifies the divergence being closed.
	WalletAddress string

	Resolution DiffResolution

	// ChainTx is the correction transaction. Required for CHAIN_UPDATED.
	ChainTx string

	// NarrativeSHA256 is the digest of the written justification. Required for MANUAL.
	NarrativeSHA256 [32]byte

	ResolvedAt time.Time
}

// ResolveInput is everything needed to close a diverged reconciliation.
type ResolveInput struct {
	// Original is the run that diverged.
	Original *Reconciliation

	// Fresh is a new reconciliation over the current state, and it is the proof.
	//
	// See Resolve for why a re-run is required rather than an operator's assertion.
	Fresh *Reconciliation

	// Outcomes explains every divergence in Original, one per wallet.
	Outcomes []DiffOutcome

	// ApprovedBy is the operator accountable for the resolution.
	ApprovedBy string

	ResolvedAt time.Time
}

// Resolution is a closed divergence.
type Resolution struct {
	Status ReconciliationStatus

	ResolvedAt time.Time
	ApprovedBy string

	// Outcomes is the per-divergence record, in the order the original reported them.
	Outcomes []DiffOutcome

	// Cleared is the divergences that were outstanding, kept alongside their resolutions.
	//
	// Retained rather than discarded because the corrected state alone does not show what was wrong.
	// Being able to show the fault, the remedy and the proof is worth more than a clean register.
	Cleared []Diff

	UnitsBefore uint32
	UnitsAfter  uint32

	HoldersBefore int
	HoldersAfter  int

	// RequiresResnapshot is true when our register moved.
	//
	// The consequence operators miss. Entitlements are computed from the frozen snapshot, so if
	// resolving the divergence changed a unit count, every entitlement derived from that snapshot is now
	// computed against a register that no longer exists. The period has to go back and freeze again;
	// the status graph already routes CHAIN_STALE to SNAPSHOT_TAKEN for exactly this.
	RequiresResnapshot bool
}

// Resolve closes a diverged reconciliation, given proof that it is actually closed.
//
// # Resolution is demonstrated, not declared
//
// The obvious design lets an operator mark a divergence resolved once they believe they have fixed it.
// That cannot be allowed to work, because RESOLVED unblocks payouts: the whole purpose of the DIVERGED
// state is to stop money moving over a register known to be wrong, and a status an operator can simply
// assert is no barrier at all. A mistaken belief would unblock the payment.
//
// So a resolution requires a second reconciliation, run after the corrections, that MATCHES. The register
// itself testifies. If three of five divergences were fixed, the re-run still diverges and this still
// refuses, which is the correct answer and not an inconvenience.
//
// # Every divergence has to be accounted for
//
// A matching re-run is necessary and not sufficient. It proves the register agrees now; it does not
// explain why it disagreed, and that explanation is what shows whether the same fault is recurring. So
// each original divergence needs an outcome, and an outcome for a wallet that never diverged is rejected
// too, since it means the paperwork describes a different run.
//
// # There is no tolerance
//
// Deliberately absent, and worth stating because monetary reconciliation usually has one. Units are whole
// and indivisible; a unit count either agrees or it does not. A tolerance here would not absorb rounding
// because there is no rounding, it would silently accept a real holder holding a real number of units we
// have wrong.
func Resolve(in ResolveInput) (*Resolution, error) {
	if in.Original == nil || in.Fresh == nil {
		return nil, errors.New("period: resolution needs both the original run and a fresh one")
	}
	if in.Original.Status != ReconDiverged {
		return nil, fmt.Errorf("%w: it is %s", ErrNotDiverged, in.Original.Status)
	}
	if in.ApprovedBy == "" {
		return nil, fmt.Errorf("%w: no operator is accountable for the resolution", ErrUnexplained)
	}
	if in.ResolvedAt.IsZero() {
		return nil, fmt.Errorf("%w: a resolution needs a timestamp", ErrUnexplained)
	}

	// Both runs must be internally consistent before either is trusted.
	if err := in.Original.Validate(); err != nil {
		return nil, fmt.Errorf("period: the original run is not valid: %w", err)
	}
	if err := in.Fresh.Validate(); err != nil {
		return nil, fmt.Errorf("period: the fresh run is not valid: %w", err)
	}

	// The proof.
	if in.Fresh.Status != ReconMatched {
		return nil, fmt.Errorf("%w: %s", ErrStillDiverged, in.Fresh.Summary())
	}

	// A matching re-run over an empty register would satisfy the check above while proving nothing, so
	// the re-run has to describe a register that still exists.
	if in.Fresh.ChainHolderCount == 0 && in.Original.ChainHolderCount > 0 {
		return nil, fmt.Errorf("%w: the fresh run sees no holders at all where the original saw %d",
			ErrProofMismatched, in.Original.ChainHolderCount)
	}

	byWallet := make(map[string]Diff, len(in.Original.Diffs))
	for _, d := range in.Original.Diffs {
		byWallet[d.WalletAddress] = d
	}

	seen := make(map[string]bool, len(in.Outcomes))
	for _, o := range in.Outcomes {
		if _, ok := byWallet[o.WalletAddress]; !ok {
			return nil, fmt.Errorf("%w: %s did not diverge in this run", ErrDiffNotInRun, o.WalletAddress)
		}
		if seen[o.WalletAddress] {
			return nil, fmt.Errorf("period: %s has two resolutions", o.WalletAddress)
		}
		seen[o.WalletAddress] = true

		if err := validateOutcome(o); err != nil {
			return nil, err
		}
	}

	for _, d := range in.Original.Diffs {
		if !seen[d.WalletAddress] {
			return nil, fmt.Errorf("%w: %s diverged by %d units and nothing says why",
				ErrDiffUnaccounted, d.WalletAddress, d.DiffUnits)
		}
	}

	// Ordered to follow the original report so two renderings of one resolution read identically.
	ordered := make([]DiffOutcome, 0, len(in.Original.Diffs))
	outcomeByWallet := make(map[string]DiffOutcome, len(in.Outcomes))
	for _, o := range in.Outcomes {
		outcomeByWallet[o.WalletAddress] = o
	}
	for _, d := range in.Original.Diffs {
		ordered = append(ordered, outcomeByWallet[d.WalletAddress])
	}

	res := &Resolution{
		Status:        ReconResolved,
		ResolvedAt:    in.ResolvedAt,
		ApprovedBy:    in.ApprovedBy,
		Outcomes:      ordered,
		Cleared:       append([]Diff(nil), in.Original.Diffs...),
		UnitsBefore:   in.Original.ChainTotalUnits,
		UnitsAfter:    in.Fresh.ChainTotalUnits,
		HoldersBefore: in.Original.ChainHolderCount,
		HoldersAfter:  in.Fresh.ChainHolderCount,
	}

	// Two independent signals that our register moved, kept separately because they can disagree.
	//
	// A CHAIN_UPDATED outcome says we intended to move it. The totals comparison says it actually moved,
	// which also catches a concurrent change nobody recorded an outcome for.
	for _, o := range ordered {
		if o.Resolution.MovedOurRegister() {
			res.RequiresResnapshot = true
		}
	}
	if res.UnitsAfter != res.UnitsBefore || res.HoldersAfter != res.HoldersBefore {
		res.RequiresResnapshot = true
	}

	return res, nil
}

func validateOutcome(o DiffOutcome) error {
	switch o.Resolution {
	case ResolutionPending, "":
		return fmt.Errorf("%w: %s is still pending, so the divergence is open",
			ErrDiffUnaccounted, o.WalletAddress)
	case ResolutionChainUpdated, ResolutionDepositoryQueried, ResolutionManual:
		// known
	default:
		return fmt.Errorf("period: unknown resolution %q for %s", o.Resolution, o.WalletAddress)
	}

	if o.ResolvedAt.IsZero() {
		return fmt.Errorf("%w: %s has no resolution timestamp", ErrUnexplained, o.WalletAddress)
	}

	if o.Resolution.RequiresChainTx() && o.ChainTx == "" {
		return fmt.Errorf("%w: %s claims the mirror was corrected but cites no transaction; our "+
			"register is on-chain, so an uncited correction is unverifiable",
			ErrUnexplained, o.WalletAddress)
	}
	if !o.Resolution.RequiresChainTx() && o.ChainTx != "" {
		// A transaction on a path that does not change units means the paperwork disagrees with itself.
		return fmt.Errorf("%w: %s resolved as %s but cites a correction transaction",
			ErrUnexplained, o.WalletAddress, o.Resolution)
	}

	if o.Resolution.RequiresNarrative() && o.NarrativeSHA256 == ([32]byte{}) {
		return fmt.Errorf("%w: %s was resolved manually and must carry a written justification",
			ErrUnexplained, o.WalletAddress)
	}
	return nil
}

// Summary renders the resolution for an operator or a log.
func (r *Resolution) Summary() string {
	counts := map[DiffResolution]int{}
	for _, o := range r.Outcomes {
		counts[o.Resolution]++
	}

	s := fmt.Sprintf("resolved %d divergence(s) by %s", len(r.Outcomes), r.ApprovedBy)
	for _, p := range []DiffResolution{ResolutionChainUpdated, ResolutionDepositoryQueried, ResolutionManual} {
		if counts[p] > 0 {
			s += fmt.Sprintf("; %s x%d", p, counts[p])
		}
	}
	if r.RequiresResnapshot {
		s += fmt.Sprintf("; register moved from %d to %d units, so the snapshot must be retaken",
			r.UnitsBefore, r.UnitsAfter)
	}
	return s
}
