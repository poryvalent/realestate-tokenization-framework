package entitlement

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/acresync/orchestrator/internal/distribution"
	"github.com/acresync/orchestrator/internal/merkle"
	"github.com/acresync/orchestrator/internal/money"
	"github.com/acresync/orchestrator/internal/payout"
	"github.com/acresync/orchestrator/internal/snapshot"
)

var (
	ErrNotExact          = errors.New("entitlement: amounts do not reconcile exactly")
	ErrMissingHolder     = errors.New("entitlement: snapshot line has no holder record")
	ErrUnmatchedAnchor   = errors.New("entitlement: allocator returned an anchor not on the register")
	ErrMissingBankDetail = errors.New("entitlement: holder has no fund account for payment")
	ErrBelowProviderMin  = errors.New("entitlement: net payable is below the provider minimum")
)

// HolderDetail is the per-investor data the register snapshot deliberately does not carry.
//
// Kept separate because the snapshot is published. A bank reference or tax class in the snapshot
// struct would be one careless field away from IPFS, and IPFS content cannot be withdrawn.
type HolderDetail struct {
	InvestorID string

	// Class drives the withholding rate.
	Class InvestorClass

	// BankAccountID is the internal reference recorded on the payout instruction.
	BankAccountID string

	// FundAccountID is the provider-side beneficiary handle, fa_xxx.
	//
	// The provider holds the account number and IFSC under its own registration flow, so this side
	// of the system handles an opaque reference and never the bank details themselves.
	FundAccountID string

	Deduction DeductionOptions
}

// Line is one computed entitlement, gross and net.
type Line struct {
	// Identity, from the snapshot.
	InvestorID     string
	WalletAddress  string
	InvestorAnchor merkle.Hash
	LeafIndex      uint32
	Units          uint32
	Excluded       bool

	SnapshotTotalUnits uint32

	// FloorPaise is floor(distributed × units / totalUnits).
	FloorPaise money.Paise

	// ResiduePaise is 0 or 1 paise from the largest-remainder pass.
	ResiduePaise money.Paise

	// GrossPaise is Floor + Residue, the figure anchored on-chain.
	GrossPaise money.Paise

	// RemainderNumerator is the discarded fraction as an exact integer, kept so the ordering that
	// produced this allocation can be re-derived and audited.
	RemainderNumerator *big.Int

	// TDS and the net payable. Never anchored, never pinned.
	TDS TaxDeduction

	Detail HolderDetail

	// Payable reports whether an instruction can be created.
	Payable bool

	// NotPayableReason explains why not, when Payable is false.
	NotPayableReason string
}

// Batch is the complete set of entitlements for one period.
type Batch struct {
	PeriodID uint32

	DistributedPaise money.Paise
	TotalUnits       uint32

	// Lines are ordered by ascending investor anchor, matching the allocator output, so a batch built
	// twice from the same inputs is identical.
	Lines []Line

	// Totals, each reconciled against the others before the batch is returned.
	GrossTotalPaise money.Paise
	TDSTotalPaise   money.Paise
	NetTotalPaise   money.Paise

	ResidueDistributedPaise money.Paise

	// PayableCount and UnpayableCount partition Lines.
	PayableCount   int
	UnpayableCount int
}

// BuildInput is everything needed to compute a batch.
type BuildInput struct {
	PeriodID uint32

	// Snapshot is the frozen register. Its EntitlementDenominator is the divisor.
	Snapshot *snapshot.Snapshot

	// DistributedPaise is the approved distribution.
	DistributedPaise money.Paise

	// Details is keyed by investor id and must cover every snapshot line.
	Details map[string]HolderDetail

	TDS *TDSTable

	// ProviderMinimumPaise is the smallest amount the payout provider accepts.
	ProviderMinimumPaise money.Paise
}

// Build computes every holder's entitlement and withholding.
func Build(in BuildInput) (*Batch, error) {
	if in.Snapshot == nil {
		return nil, errors.New("entitlement: no snapshot")
	}
	if in.TDS == nil {
		return nil, errors.New("entitlement: no withholding table")
	}
	if len(in.Snapshot.Lines) == 0 {
		return nil, errors.New("entitlement: snapshot has no lines")
	}

	// The denominator is every issued unit, taken from the named method rather than recomputed.
	//
	// Excluded holders are excluded from the statutory holder count and not from the distribution. A
	// denominator of the public float would overpay every public holder and pay the manager nothing,
	// while still satisfying the 95% floor and reconciling against itself.
	totalUnits := in.Snapshot.EntitlementDenominator()

	holders := make([]distribution.Holder, 0, len(in.Snapshot.Lines))
	byAnchor := make(map[merkle.Hash]snapshot.Line, len(in.Snapshot.Lines))

	for _, l := range in.Snapshot.Lines {
		anchor := merkle.Hash(l.InvestorAnchor)
		holders = append(holders, distribution.Holder{
			InvestorAnchor: anchor,
			Units:          l.Units,
			Excluded:       l.ExcludedFromHolderCount,
		})
		byAnchor[anchor] = l
	}

	alloc, err := distribution.Allocate(in.DistributedPaise, totalUnits, holders)
	if err != nil {
		return nil, fmt.Errorf("entitlement: allocation: %w", err)
	}

	batch := &Batch{
		PeriodID:                in.PeriodID,
		DistributedPaise:        in.DistributedPaise,
		TotalUnits:              totalUnits,
		Lines:                   make([]Line, 0, len(alloc.Entitlements)),
		ResidueDistributedPaise: alloc.ResidueDistributed,
	}

	for _, e := range alloc.Entitlements {
		src, ok := byAnchor[e.InvestorAnchor]
		if !ok {
			// Cannot happen unless the allocator invented an anchor, which would mean the join
			// between economics and identity had silently shifted. Checked because the consequence is
			// paying the wrong person.
			return nil, fmt.Errorf("%w: %s", ErrUnmatchedAnchor, e.InvestorAnchor.Hex())
		}
		if e.Units != src.Units {
			return nil, fmt.Errorf("%w: anchor %s has %d units in the allocation and %d on the register",
				ErrNotExact, e.InvestorAnchor.Hex(), e.Units, src.Units)
		}

		detail, ok := in.Details[src.InvestorID]
		if !ok {
			return nil, fmt.Errorf("%w: investor %s", ErrMissingHolder, src.InvestorID)
		}
		if !detail.Class.Valid() {
			return nil, fmt.Errorf("entitlement: investor %s has invalid class %q",
				src.InvestorID, detail.Class)
		}

		rule, err := in.TDS.For(detail.Class)
		if err != nil {
			return nil, fmt.Errorf("investor %s: %w", src.InvestorID, err)
		}
		tds, err := ComputeTDS(e.Gross, rule, detail.Deduction)
		if err != nil {
			return nil, fmt.Errorf("investor %s: %w", src.InvestorID, err)
		}

		line := Line{
			InvestorID:         src.InvestorID,
			WalletAddress:      src.WalletAddress,
			InvestorAnchor:     e.InvestorAnchor,
			LeafIndex:          src.LeafIndex,
			Units:              e.Units,
			Excluded:           src.ExcludedFromHolderCount,
			SnapshotTotalUnits: totalUnits,
			FloorPaise:         e.Floor,
			ResiduePaise:       e.ResidueAwarded,
			GrossPaise:         e.Gross,
			RemainderNumerator: e.RemainderNumerator,
			TDS:                tds,
			Detail:             detail,
		}

		line.Payable, line.NotPayableReason = payability(line, in.ProviderMinimumPaise)
		if line.Payable {
			batch.PayableCount++
		} else {
			batch.UnpayableCount++
		}

		if batch.GrossTotalPaise, err = batch.GrossTotalPaise.Add(e.Gross); err != nil {
			return nil, err
		}
		if batch.TDSTotalPaise, err = batch.TDSTotalPaise.Add(tds.AmountPaise); err != nil {
			return nil, err
		}
		if batch.NetTotalPaise, err = batch.NetTotalPaise.Add(tds.NetPayablePaise); err != nil {
			return nil, err
		}

		batch.Lines = append(batch.Lines, line)
	}

	if err := batch.Reconcile(); err != nil {
		return nil, err
	}
	return batch, nil
}

// payability decides whether a line can become a payout instruction.
//
// Two separate floors sit here and they are not the same number.
//
// The database requires amount_paise > 0 on a payout instruction, so a holder whose entire gross is
// withheld cannot have one at all. That is correct rather than an obstacle: there is no payment to
// make, only a withholding to remit, and inventing a zero-value instruction would make the payout
// count disagree with the number of transfers.
//
// The provider separately refuses anything below its own minimum, which is ₹1. An amount between one
// paise and that minimum is storable and unpayable, which is the awkward case: without this check the
// batch would be built, the run would start, and the provider would reject that one holder partway
// through. On a ₹50 Cr scheme at roughly ₹5.7 lakh per unit it should never arise, which is exactly
// why it needs to be handled here rather than discovered later.
func payability(l Line, providerMin money.Paise) (bool, string) {
	if l.Detail.FundAccountID == "" {
		return false, fmt.Sprintf("no fund account registered for investor %s", l.InvestorID)
	}
	if l.TDS.NetPayablePaise == 0 {
		return false, fmt.Sprintf(
			"net payable is zero: gross %d paise fully withheld at %d bps, so there is a "+
				"withholding to remit and no transfer to make", l.GrossPaise, l.TDS.RateBps)
	}
	if providerMin > 0 && l.TDS.NetPayablePaise < providerMin {
		return false, fmt.Sprintf(
			"net payable %d paise is below the provider minimum of %d paise",
			l.TDS.NetPayablePaise, providerMin)
	}
	return true, ""
}

// Reconcile asserts the arithmetic, with no tolerance.
//
// Called before a batch is returned and available to callers afterwards. Exactness is the property
// the whole distribution rests on: the gross total is anchored on-chain, and if it disagrees with the
// approved figure by a single paise then either the chain is wrong or the bank transfers are, and no
// later process can tell which.
func (b *Batch) Reconcile() error {
	if b.GrossTotalPaise != b.DistributedPaise {
		return fmt.Errorf("%w: entitlements total %d paise, the approved distribution is %d",
			ErrNotExact, b.GrossTotalPaise, b.DistributedPaise)
	}

	sum, err := b.TDSTotalPaise.Add(b.NetTotalPaise)
	if err != nil {
		return err
	}
	if sum != b.GrossTotalPaise {
		return fmt.Errorf("%w: withholding %d plus net %d is %d, gross is %d",
			ErrNotExact, b.TDSTotalPaise, b.NetTotalPaise, sum, b.GrossTotalPaise)
	}

	var units uint32
	for _, l := range b.Lines {
		perLine, err := l.FloorPaise.Add(l.ResiduePaise)
		if err != nil {
			return err
		}
		if perLine != l.GrossPaise {
			return fmt.Errorf("%w: investor %s floor %d plus residue %d is %d, gross is %d",
				ErrNotExact, l.InvestorID, l.FloorPaise, l.ResiduePaise, perLine, l.GrossPaise)
		}

		lineSum, err := l.TDS.AmountPaise.Add(l.TDS.NetPayablePaise)
		if err != nil {
			return err
		}
		if lineSum != l.GrossPaise {
			return fmt.Errorf("%w: investor %s withholding %d plus net %d is %d, gross is %d",
				ErrNotExact, l.InvestorID, l.TDS.AmountPaise, l.TDS.NetPayablePaise, lineSum, l.GrossPaise)
		}

		// The residue is at most one paise per holder; the database constrains it to 0 or 1.
		if l.ResiduePaise < 0 || l.ResiduePaise > 1 {
			return fmt.Errorf("%w: investor %s was awarded %d residue paise, the bound is 0 or 1",
				ErrNotExact, l.InvestorID, l.ResiduePaise)
		}

		units += l.Units
	}

	if units != b.TotalUnits {
		return fmt.Errorf("%w: lines hold %d units, the snapshot total is %d",
			ErrNotExact, units, b.TotalUnits)
	}
	if b.PayableCount+b.UnpayableCount != len(b.Lines) {
		return fmt.Errorf("%w: %d payable plus %d unpayable does not account for %d lines",
			ErrNotExact, b.PayableCount, b.UnpayableCount, len(b.Lines))
	}
	return nil
}

// ChainBatch is the holder and unit arrays for one anchorEntitlementsBatch call.
type ChainBatch struct {
	Holders []string
	Units   []uint32
}

// ChainBatches splits the batch into calldata-sized groups.
//
// # Why batching is needed at all
//
// anchorEntitlementsBatch takes parallel arrays and a 202-line register does not fit in one
// transaction's gas budget. Splitting is therefore a hard requirement, and the split has to be
// deterministic: the contract accumulates entitledUnitsAccrued across calls and only permits the
// finalisation once the accumulated total equals the snapshot total, so a batch resent after a
// timeout must be the same batch or the accumulation double-counts.
//
// Order follows Lines, which follows ascending investor anchor, so batch n contains the same holders
// on every rebuild.
//
// Only gross is sent. The contract stores units against a holder and the per-unit amount is derivable
// from the period struct, so writing the amount would spend gas to store a number that adds no
// information. Net and tax never go near the chain.
func (b *Batch) ChainBatches(size int) ([]ChainBatch, error) {
	if size <= 0 {
		return nil, fmt.Errorf("entitlement: batch size must be positive, got %d", size)
	}

	var out []ChainBatch
	for start := 0; start < len(b.Lines); start += size {
		end := start + size
		if end > len(b.Lines) {
			end = len(b.Lines)
		}

		cb := ChainBatch{
			Holders: make([]string, 0, end-start),
			Units:   make([]uint32, 0, end-start),
		}
		for _, l := range b.Lines[start:end] {
			cb.Holders = append(cb.Holders, l.WalletAddress)
			cb.Units = append(cb.Units, l.Units)
		}
		out = append(out, cb)
	}
	return out, nil
}

// PayoutRequestInput carries the settings shared by every request in a run.
type PayoutRequestInput struct {
	SchemeID string

	// AnchorOutboxID is the confirmed anchor that authorises these payments, recorded on each
	// instruction as gated_on_anchor_tx. Not optional: fiat cannot leave escrow without naming the
	// on-chain attestation it rests on.
	AnchorOutboxID string

	// AccountNumber is the source account.
	AccountNumber string

	Mode    payout.Mode
	Purpose payout.Purpose

	// Narration appears on the holder's bank statement.
	Narration string

	// Attempt is the reissue generation, persisted per entitlement.
	//
	// Incremented only when a previous attempt terminally failed without money moving. A retry after
	// a dropped connection must reuse the same value, or the holder is paid twice.
	Attempts map[string]uint32
}

// PayoutRequests builds one request per payable line.
//
// Unpayable lines are skipped and reported, not silently dropped. A holder with no fund account still
// has an entitlement recorded and anchored; what they lack is a way to receive it, which is an
// operational problem to chase rather than a reason to lose the record.
func (b *Batch) PayoutRequests(in PayoutRequestInput) ([]payout.Request, []Line, error) {
	if in.AnchorOutboxID == "" {
		return nil, nil, errors.New("entitlement: payouts require the confirmed anchor that authorises them")
	}

	narration := payout.SanitiseNarration(in.Narration)

	var reqs []payout.Request
	var skipped []Line

	for _, l := range b.Lines {
		if !l.Payable {
			skipped = append(skipped, l)
			continue
		}

		attempt := in.Attempts[l.InvestorID]

		key, err := payout.DeriveIdempotencyKey(
			in.SchemeID, l.InvestorID, b.PeriodID, attempt,
			l.TDS.NetPayablePaise, l.Detail.FundAccountID,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("investor %s: %w", l.InvestorID, err)
		}

		req := payout.Request{
			IdempotencyKey: key,
			AccountNumber:  in.AccountNumber,
			FundAccountID:  l.Detail.FundAccountID,

			// The net amount, after withholding. The gross is what the chain attests; the net is what
			// the bank moves, and the difference is remitted to the tax authority separately.
			AmountPaise: l.TDS.NetPayablePaise,

			Mode:    in.Mode,
			Purpose: in.Purpose,

			// Never queue on low balance. A queued payout stalls a distribution without alerting
			// anyone and is auto-failed after three months.
			QueueIfLowBalance: false,

			// The reference is our own identifier, echoed back by the provider for reconciliation.
			ReferenceID: fmt.Sprintf("AS-P%d-L%d", b.PeriodID, l.LeafIndex),

			Narration: narration,

			// Notes carry only internal references. A holder's name or PAN here would leak through the
			// provider's dashboard and API responses, which is the same exposure the IPFS allowlist
			// exists to prevent, reached through a different door.
			Notes: map[string]string{
				"periodId":  fmt.Sprintf("%d", b.PeriodID),
				"leafIndex": fmt.Sprintf("%d", l.LeafIndex),
				"anchorTx":  in.AnchorOutboxID,
			},
		}

		if err := req.Validate(); err != nil {
			return nil, nil, fmt.Errorf("investor %s: %w", l.InvestorID, err)
		}
		reqs = append(reqs, req)
	}

	return reqs, skipped, nil
}

// NetTotalForPayableLines returns the sum that will actually leave the account.
//
// Distinct from NetTotalPaise, which includes lines that cannot be paid. Keeping them apart matters
// when checking the balance before a run: funding for the full net total would over-provision, and
// funding for nothing would fail partway.
func (b *Batch) NetTotalForPayableLines() (money.Paise, error) {
	var total money.Paise
	var err error
	for _, l := range b.Lines {
		if !l.Payable {
			continue
		}
		if total, err = total.Add(l.TDS.NetPayablePaise); err != nil {
			return 0, err
		}
	}
	return total, nil
}

// PerUnitRate returns the headline rate a holder will check by hand.
func (b *Batch) PerUnitRate() (money.Paise, money.Paise, error) {
	return distribution.PerUnitRate(b.DistributedPaise, b.TotalUnits)
}
