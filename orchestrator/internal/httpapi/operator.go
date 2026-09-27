package httpapi

import (
	"context"
	"net/http"

	"github.com/acresync/orchestrator/internal/store"
)

// The operator surface.
//
// # What is here and what is not
//
// The contract publishes fourteen operator operations: four reads and ten writes. This file serves the chain
// outbox, which is the one an operator looks at most and the one that can be served completely.
//
// The other three reads are deliberately absent rather than approximated, and the reason is worth stating
// because "partially implemented" is the wrong description:
//
//   - Offer readiness would report canAdvance and a list of blockers. Those come from offer.Guard, which takes
//     an offer.Evidence assembled from the offer, its bids, their funds blocks, the ballot run, the IPFS pins,
//     the allocation rows and the settlement cursor. An Evidence gathered from fewer sources than that produces
//     a confident canAdvance:true on evidence nobody checked, and an operator would act on it. Under-reporting
//     blockers on a commit-reveal ceremony is worse than having no endpoint, because the absence of an endpoint
//     is visible and a wrong answer is not.
//   - Settlement state needs the same plan reconstruction, plus settlement.CheckFinalisation, whose whole value
//     is reporting every unmet invariant rather than the first.
//   - Period reconciliation cannot be served at all as the contract describes it. See below.
//
// # A genuine mismatch between the contract and the schema
//
// GET /admin/periods/{periodId}/reconciliation is keyed by period, and its response requires periodId. But
// reconciliation_runs has no distribution_period_id: it is keyed by scheme_id and run_at. There is no recorded
// link from a period to the run that reconciled it.
//
// It could be guessed at by taking the scheme's latest run around the period's record date. That guess decides
// blocksPayout, which is the flag the database uses to refuse paying against a register known to be wrong.
// Attaching the wrong run to a period would either block a clean distribution or, far worse, present a diverged
// register as reconciled. So this is reported rather than implemented, and the fix belongs in the schema: either
// reconciliation_runs gains a period reference, or the contract's endpoint becomes scheme-scoped.

// OutboxReader is the read surface the outbox endpoint needs.
type OutboxReader interface {
	List(ctx context.Context, f store.OutboxFilter, p store.Pagination) ([]store.OutboxEntry, error)
}

// wireOutboxEntry is the contract's OutboxEntry.
//
// Nonce, txHash, blockNumber and lastError are all nullable, and each is a pointer for the same reason: zero and
// absent mean different things. A nonce of zero is the first transaction an account ever sends, and reporting an
// unsigned entry that way would be a specific false statement rather than a vague one.
type wireOutboxEntry struct {
	ID             string    `json:"id"`
	TargetContract string    `json:"targetContract"`
	FunctionName   string    `json:"functionName"`
	Status         string    `json:"status"`
	Nonce          *int64    `json:"nonce"`
	TxHash         *string   `json:"txHash"`
	BlockNumber    *int64    `json:"blockNumber"`
	Confirmations  int32     `json:"confirmations"`
	Attempts       int32     `json:"attempts"`
	LastError      *string   `json:"lastError"`
	EnvironmentTag string    `json:"environmentTag"`
	CreatedAt      timestamp `json:"createdAt"`
}

func outboxEntryToWire(e store.OutboxEntry) wireOutboxEntry {
	out := wireOutboxEntry{
		ID:             e.ID,
		TargetContract: e.TargetContract,
		FunctionName:   e.FunctionName,
		Status:         string(e.Status),
		Nonce:          e.Nonce,
		BlockNumber:    e.BlockNumber,
		Confirmations:  e.Confirmations,
		Attempts:       e.Attempts,
		EnvironmentTag: e.EnvironmentTag,
		CreatedAt:      e.CreatedAt,
	}
	if e.TxHash != "" {
		hash := e.TxHash
		out.TxHash = &hash
	}
	if e.LastError != "" {
		// The relayer's error text, passed through. It is operational detail for an operator who is already
		// authenticated and holds a role, and it is the thing that explains a stuck queue.
		msg := e.LastError
		out.LastError = &msg
	}
	return out
}

// handleListOutbox serves GET /v1/admin/outbox.
//
// Available to every operator role. This is the queue that carries every anchor to the chain, and a trustee
// asking why an approval has not landed needs the same view as the manager who queued it. There is nothing here
// a role boundary would protect: it is the operator's own infrastructure, and it contains no investor identity.
func (s *Server) handleListOutbox(w http.ResponseWriter, r *http.Request) {
	p, err := pagination(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	filter := store.OutboxFilter{
		SchemeID: r.URL.Query().Get("schemeId"),
		Status:   r.URL.Query().Get("status"),
	}

	// Both filters are validated rather than passed through. A malformed scheme id would otherwise reach the
	// uuid cast and surface as a 500, and an unknown status would silently return nothing, which reads as a
	// quiet queue rather than as a mistyped filter.
	if filter.SchemeID != "" && !looksLikeUUID(filter.SchemeID) {
		writeError(w, r, badRequest("schemeId is not a uuid", nil))
		return
	}
	if filter.Status != "" && !knownOutboxStatus(filter.Status) {
		writeError(w, r, badRequest("status is not one the outbox uses", nil))
		return
	}

	entries, err := s.deps.Outbox.List(r.Context(), filter, p)
	if err != nil {
		writeError(w, r, err)
		return
	}

	items := make([]wireOutboxEntry, 0, len(entries))
	for _, e := range entries {
		items = append(items, outboxEntryToWire(e))
	}

	// No ETag. The queue changes constantly while the relayer works, so a conditional request would nearly
	// always miss, and the header would cost a hash on every poll for no saving.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, newWireList(items, ""))
}

// knownOutboxStatus reports whether a filter value is a status the queue uses.
func knownOutboxStatus(value string) bool {
	for _, s := range store.AllOutboxStatuses() {
		if string(s) == value {
			return true
		}
	}
	return false
}
