package payout

import (
	"fmt"
	"time"
)

// DBStatus mirrors the payout_status Postgres enum.
//
// Six values against the provider's nine. The two vocabularies are deliberately not the same: ours
// records what the scheme needs to know, while the provider's distinguishes states that exist for its
// own operational reasons. Mapping between them is therefore lossy in one direction, and the mapping
// below is written out rather than inferred so the loss is a decision instead of an accident.
type DBStatus string

const (
	DBQueued     DBStatus = "QUEUED"
	DBSubmitted  DBStatus = "SUBMITTED"
	DBProcessing DBStatus = "PROCESSING"
	DBSettled    DBStatus = "SETTLED"
	DBFailed     DBStatus = "FAILED"
	DBReversed   DBStatus = "REVERSED"
)

// AllDBStatuses lists every value in the Postgres enum.
func AllDBStatuses() []DBStatus {
	return []DBStatus{DBQueued, DBSubmitted, DBProcessing, DBSettled, DBFailed, DBReversed}
}

func (s DBStatus) Valid() bool {
	switch s {
	case DBQueued, DBSubmitted, DBProcessing, DBSettled, DBFailed, DBReversed:
		return true
	default:
		return false
	}
}

// PersistedPayout is the row shape a payout instruction update has to satisfy.
//
// Returned as one value because the database enforces the fields together, not separately:
// payouts_settled_has_utr requires a UTR and a settlement timestamp whenever the status is SETTLED,
// and payouts_failed_has_code requires a failure code whenever it is FAILED. Returning a bare status
// and leaving the caller to remember the companions is how a constraint violation appears in the
// middle of a distribution run.
type PersistedPayout struct {
	Status      DBStatus
	UTR         string
	SettledAt   *time.Time
	FailureCode string
	ProviderRef string
}

// ToDBStatus maps a provider state to the stored enum.
//
// # The mapping and why each edge is where it is
//
//	pending    -> QUEUED       awaiting approval; nothing has been attempted
//	queued     -> QUEUED       awaiting balance
//	scheduled  -> QUEUED       awaiting its scheduled time
//	processing -> PROCESSING   the partner or beneficiary bank holds it
//	processed  -> SETTLED      the beneficiary was credited
//	reversed   -> REVERSED     a credit was clawed back
//	cancelled  -> FAILED       money never moved
//	rejected   -> FAILED       money never moved
//	failed     -> FAILED       money never moved
//
// Three provider states collapse onto QUEUED and three onto FAILED. That is acceptable because the
// distinctions they carry are operational rather than financial: for the scheme's purposes, a payout
// that is waiting is waiting, and one that failed without moving money needs reissuing regardless of
// which party refused it. The provider's own reason is preserved in failure_code and provider_ref, so
// the detail is not lost, only removed from the state machine that drives decisions.
//
// SUBMITTED has no provider counterpart. It is ours: the instruction was handed over and no response
// has been seen yet, which is the window a crash between the request and the response leaves behind.
// Nothing maps onto it from a provider reply, and that asymmetry is intentional.
func ToDBStatus(p *Payout, now time.Time) (PersistedPayout, error) {
	if p == nil {
		return PersistedPayout{}, fmt.Errorf("payout: nil payout")
	}
	if !p.Status.Valid() {
		return PersistedPayout{}, fmt.Errorf("%w: %q", ErrUnknownStatus, p.Status)
	}

	out := PersistedPayout{ProviderRef: p.ProviderID}

	switch p.Status {
	case StatusPending, StatusQueued, StatusScheduled:
		out.Status = DBQueued

	case StatusProcessing:
		out.Status = DBProcessing

	case StatusProcessed:
		out.Status = DBSettled

		// The UTR is required by payouts_settled_has_utr, and it is the only identifier our records
		// share with the holder's own bank statement. A SETTLED row without one cannot resolve an "I
		// never received it" dispute, so its absence is an error rather than a nullable field.
		if p.UTR == "" {
			return PersistedPayout{}, fmt.Errorf(
				"payout: %s reports processed with no UTR; a settled row must carry the bank "+
					"reference the holder can match against their statement", p.ProviderID)
		}
		out.UTR = p.UTR

		settled := now
		if !p.CreatedAt.IsZero() {
			settled = now
		}
		out.SettledAt = &settled

	case StatusReversed:
		out.Status = DBReversed
		// A reversal keeps the UTR when one exists. The credit did happen and was undone, and the
		// original reference is what ties the two bank entries together.
		out.UTR = p.UTR
		out.FailureCode = failureCode(p, "reversed")

	case StatusCancelled, StatusRejected, StatusFailed:
		out.Status = DBFailed
		out.FailureCode = failureCode(p, string(p.Status))

	default:
		return PersistedPayout{}, fmt.Errorf("%w: no stored status for %q", ErrUnknownStatus, p.Status)
	}

	return out, nil
}

// failureCode builds the value for the failure_code column.
//
// Never empty for a status that requires it. payouts_failed_has_code rejects a FAILED row without
// one, and a row that cannot be written is worse than a coarse code: the payout's real state would be
// stuck at whatever it was before.
func failureCode(p *Payout, fallback string) string {
	switch {
	case p.StatusDetails.Reason != "" && p.StatusDetails.Source != "":
		return fmt.Sprintf("%s/%s", p.StatusDetails.Source, p.StatusDetails.Reason)
	case p.StatusDetails.Reason != "":
		return p.StatusDetails.Reason
	case p.StatusDetails.Source != "":
		return string(p.StatusDetails.Source)
	default:
		return fallback
	}
}

// SettledCountAndAmount totals what confirmPayouts should be told.
//
// Only credit-confirmed payouts count. A payout in flight has not settled, and one that failed moved
// no money, so including either would anchor a settled figure that the bank statements do not
// support. The count is what the contract records and the amount is what has to reconcile against the
// distributed total less anything unpaid.
func SettledCountAndAmount(payouts []*Payout) (count int, total int64, err error) {
	for _, p := range payouts {
		if p == nil {
			return 0, 0, fmt.Errorf("payout: nil entry in the settlement tally")
		}
		if !p.Status.Valid() {
			return 0, 0, fmt.Errorf("%w: %q", ErrUnknownStatus, p.Status)
		}
		if !p.Status.IsCreditConfirmed() {
			continue
		}
		count++
		next := total + int64(p.AmountPaise)
		if next < total {
			return 0, 0, fmt.Errorf("payout: settled total overflowed int64")
		}
		total = next
	}
	return count, total, nil
}
