package settlement

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/acresync/orchestrator/internal/idempotency"
	"github.com/acresync/orchestrator/internal/merkle"
)

// ChainState is what the contract currently reports.
//
// Read back rather than assumed, because finaliseSettlement compares the register against the settlement
// counters and those are two independent pieces of state. Inferring one from the other is how a mismatch
// goes unnoticed until the call reverts.
type ChainState struct {
	Stage Stage

	// TotalUnitsIssued is the register total, manager included.
	TotalUnitsIssued uint32

	// DistinctHolderCount excludes the manager.
	DistinctHolderCount uint32

	IMWallet      string
	IMWalletUnits uint32
	IMExcluded    bool

	ExpectedUnits   uint32
	ExpectedHolders uint32
	CreditedUnits   uint32
	CreditedHolders uint32
}

// FinalisationReport is the outcome of evaluating all five checks.
type FinalisationReport struct {
	// Ready is true only when every check passes.
	Ready bool

	// Failures lists every unmet check, not just the first.
	//
	// The contract reverts on the earliest failure, so an operator fixing them one at a time discovers
	// the next only after another transaction. Reporting all of them turns that into one pass.
	Failures []string
}

func (r FinalisationReport) Err() error {
	if r.Ready {
		return nil
	}
	return fmt.Errorf("settlement: finaliseSettlement would revert: %v", r.Failures)
}

// CheckFinalisation evaluates the five checks finaliseSettlement performs.
//
// # Why this exists even though the contract checks the same things
//
// finaliseSettlement is permissionless and leaves the stage at InProgress when a check fails, so a failed
// call is recoverable and costs only gas. But it reverts on the first failure it finds, and its errors
// report figures rather than causes: a missing manager subscription surfaces as a total units mismatch,
// which points at the register rather than at the step nobody ran.
//
// Evaluating locally gives the operator every failure at once, in language that names the remedy.
func CheckFinalisation(p *Plan, cs ChainState) FinalisationReport {
	var fails []string

	if cs.Stage != StageInProgress {
		fails = append(fails, fmt.Sprintf(
			"the settlement stage is %s and finaliseSettlement requires IN_PROGRESS", cs.Stage))
	}

	// Check 1: totalUnitsIssued == totalUnits.
	if cs.TotalUnitsIssued != p.Params.TotalUnits {
		msg := fmt.Sprintf("the register holds %d units and the scheme is %d",
			cs.TotalUnitsIssued, p.Params.TotalUnits)

		// The single most likely cause, named rather than left to be deduced from a total.
		if cs.TotalUnitsIssued+p.Params.IMUnits == p.Params.TotalUnits {
			msg += fmt.Sprintf(". The shortfall is exactly the manager's %d units, so "+
				"recordImSubscription has not been called", p.Params.IMUnits)
		}
		fails = append(fails, msg)
	}

	// Check 2: distinctHolderCount >= minPublicHolders.
	if cs.DistinctHolderCount < p.Params.MinPublicHolders {
		msg := fmt.Sprintf("the register counts %d holders against a statutory floor of %d",
			cs.DistinctHolderCount, p.Params.MinPublicHolders)

		// A count below the number of entries credited means addresses collided. The contract cannot
		// detect this itself; see checkWallets.
		if cs.CreditedHolders > cs.DistinctHolderCount {
			msg += fmt.Sprintf(". %d entries were credited but only %d addresses are counted, so "+
				"%d allottees shared a wallet with another", cs.CreditedHolders,
				cs.DistinctHolderCount, cs.CreditedHolders-cs.DistinctHolderCount)
		}
		fails = append(fails, msg)
	}

	// Check 3: the manager's holding.
	switch {
	case cs.IMWallet == "" || isZeroAddress(cs.IMWallet):
		fails = append(fails, "the manager's wallet is unset, so recordImSubscription has not been called")
	case cs.IMWalletUnits != p.Params.IMUnits:
		fails = append(fails, fmt.Sprintf("the manager's wallet holds %d units and must hold %d",
			cs.IMWalletUnits, p.Params.IMUnits))
	case !cs.IMExcluded:
		fails = append(fails, "the manager's wallet is not excluded from the holder count, so it "+
			"would be counted toward the statutory floor it is not permitted to satisfy")
	}

	// Check 4: creditedUnits == expectedUnits.
	if cs.CreditedUnits != cs.ExpectedUnits {
		remaining := int64(cs.ExpectedUnits) - int64(cs.CreditedUnits)
		fails = append(fails, fmt.Sprintf(
			"%d of %d declared units are credited, leaving %d; more batches are outstanding",
			cs.CreditedUnits, cs.ExpectedUnits, remaining))
	}

	// Check 5: creditedHolders == expectedHolders.
	if cs.CreditedHolders != cs.ExpectedHolders {
		fails = append(fails, fmt.Sprintf(
			"%d of %d declared holders are credited; the next batch starts at cursor %d",
			cs.CreditedHolders, cs.ExpectedHolders, cs.CreditedHolders))
	}

	// The plan must be the one the chain was opened with, or the checks above compared the wrong numbers.
	if cs.ExpectedUnits != 0 && cs.ExpectedUnits != p.ExpectedUnits {
		fails = append(fails, fmt.Sprintf(
			"the chain was opened for %d units but this plan declares %d, so they are different "+
				"settlements", cs.ExpectedUnits, p.ExpectedUnits))
	}
	if cs.ExpectedHolders != 0 && cs.ExpectedHolders != p.ExpectedHolders {
		fails = append(fails, fmt.Sprintf(
			"the chain was opened for %d holders but this plan declares %d",
			cs.ExpectedHolders, p.ExpectedHolders))
	}

	return FinalisationReport{Ready: len(fails) == 0, Failures: fails}
}

// NextStep reports what to do next given the chain's current state.
func (p *Plan) NextStep(cs ChainState) (string, error) {
	switch cs.Stage {
	case StageNotStarted:
		return "beginSettlement", nil

	case StageInProgress:
		if cs.CreditedHolders < p.ExpectedHolders {
			return "settleBatch", nil
		}
		if report := CheckFinalisation(p, cs); !report.Ready {
			return "", report.Err()
		}
		return "finaliseSettlement", nil

	case StageFinalised:
		return "", errors.New("settlement: already finalised; the scheme is Settled and only " +
			"finaliseSettlement can set that status, so there is nothing left to do")
	}
	return "", fmt.Errorf("settlement: unknown stage %s", cs.Stage)
}

// --- call payloads -------------------------------------------------------------------------------

// BeginPayload renders the beginSettlement arguments.
func (p *Plan) BeginPayload() ([]byte, error) {
	return json.Marshal(map[string]any{
		"expectedHolders":   p.ExpectedHolders,
		"expectedUnits":     p.ExpectedUnits,
		"allotmentFileHash": p.AllotmentFileHash[:],
	})
}

// BatchPayload renders the settleBatch arguments.
func (p *Plan) BatchPayload(b Batch) ([]byte, error) {
	return json.Marshal(map[string]any{
		"holders":    b.Wallets(),
		"units":      b.UnitsArray(),
		"cursorFrom": b.CursorFrom,
	})
}

// IMPayload renders the recordImSubscription arguments.
func (p *Plan) IMPayload() ([]byte, error) {
	return json.Marshal(map[string]any{
		"wallet": p.IMWallet,
		"units":  p.Params.IMUnits,
	})
}

// --- idempotency ---------------------------------------------------------------------------------

// BeginIdempotencyInput describes the key for opening settlement.
//
// Carries the ballot result root, so a settlement opened against a different draw derives a different
// key. The contract binds itself to the root it reads at that moment and can never be rebound.
func (p *Plan) BeginIdempotencyInput() idempotency.Input {
	return idempotency.Input{
		Action:   idempotency.ActionBeginSettlement,
		SchemeID: p.SchemeID,
		ScopeID:  p.OfferID,
		Payload: map[string]any{
			"resultRoot":        p.BallotResultRoot.Hex(),
			"allotmentFileHash": merkle.Hash(p.AllotmentFileHash).Hex(),
			"expectedUnits":     p.ExpectedUnits,
			"expectedHolders":   p.ExpectedHolders,
		},
	}
}

// BatchIdempotencyInput describes the key for one batch.
//
// # Why the cursor is in the key and the contents are too
//
// A batch that ran out of gas did not advance the cursor, so it is retryable verbatim, and the retry must
// derive the same key or the idempotency guard would reject a legitimate attempt. Keying on the cursor
// alone would do that.
//
// The contents are included as well, because a corrected batch at the same cursor is a different
// instruction. Without them, fixing a wrong wallet and resubmitting at the same cursor would derive a key
// already recorded, and the correction would be swallowed as a duplicate of the thing it was fixing.
func (p *Plan) BatchIdempotencyInput(b Batch) idempotency.Input {
	entries := make([]any, 0, len(b.Entries))
	for _, e := range b.Entries {
		entries = append(entries, map[string]any{
			"wallet": e.Wallet,
			"units":  e.Units,
		})
	}

	return idempotency.Input{
		Action:   idempotency.ActionSettleBatch,
		SchemeID: p.SchemeID,
		ScopeID:  p.OfferID,
		Payload: map[string]any{
			"cursorFrom": b.CursorFrom,
			"entries":    entries,
		},
	}
}

// FinaliseIdempotencyInput describes the key for finalisation.
//
// # Why a failed finalisation must stay retryable
//
// finaliseSettlement leaves the stage at InProgress when a check fails, precisely so the operator can
// correct and call again. On a revert the freshness mark is rolled back with everything else, so the same
// key is available to the retry. That is the behaviour this key relies on: it is derived from the
// settlement's identity alone, with no attempt counter, because there is only ever one finalisation of
// one settlement.
func (p *Plan) FinaliseIdempotencyInput() idempotency.Input {
	return idempotency.Input{
		Action:   idempotency.ActionFinaliseSettlement,
		SchemeID: p.SchemeID,
		ScopeID:  p.OfferID,
		Payload: map[string]any{
			"resultRoot":      p.BallotResultRoot.Hex(),
			"expectedUnits":   p.ExpectedUnits,
			"expectedHolders": p.ExpectedHolders,
		},
	}
}

// IMIdempotencyInput describes the key for the manager's subscription.
//
// Scoped to the scheme rather than the offer. recordImSubscription reverts once imWallet is set, so it
// happens exactly once in the scheme's life and is not an offer-level act.
func (p *Plan) IMIdempotencyInput() idempotency.Input {
	return idempotency.Input{
		Action:   idempotency.ActionIMSubscription,
		SchemeID: p.SchemeID,
		Payload: map[string]any{
			"wallet": p.IMWallet,
			"units":  p.Params.IMUnits,
		},
	}
}
