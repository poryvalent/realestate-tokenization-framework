package httpapi

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/acresync/orchestrator/internal/asba"
	"github.com/acresync/orchestrator/internal/ballot"
	"github.com/acresync/orchestrator/internal/bidbook"
	"github.com/acresync/orchestrator/internal/idempotency"
	"github.com/acresync/orchestrator/internal/merkle"
	"github.com/acresync/orchestrator/internal/offer"
	"github.com/acresync/orchestrator/internal/store"
)

// Offer lifecycle writes: create, advance, and the readiness read that reports what an advance would need.
//
// # Who may do what
//
// The contract publishes one operator token with a role claim and leaves the per-operation mapping to the
// server. It is set here, and it mirrors the chain:
//
//   - The investment manager runs the offer. Creating it, opening and closing it, and every step of the
//     ceremony are MANAGER actions.
//   - An abort may come from the manager or the trustee. The trustee's job is protecting unitholders, and an
//     offer that has to be abandoned is exactly the case where the trustee must not depend on the manager.
//   - Compliance observes. It reads everything and writes nothing, except finalisation, which the contract
//     makes permissionless on purpose.

var (
	rolesRunOffer = []Role{RoleManager}
	rolesAbort    = []Role{RoleManager, RoleTrustee}
	rolesFinalise = []Role{RoleManager, RoleTrustee, RoleCompliance}
)

// advanceTargets are the statuses advanceOffer reaches directly. The rest have their own endpoints, because
// reaching them does more than change a status: freezing pins and anchors, the ceremony commits and reveals,
// the draw allocates, and settlement credits the register.
var advanceTargets = map[offer.Status]idempotency.Action{
	offer.StatusOpen:               idempotency.ActionOpenOffer,
	offer.StatusClosed:             idempotency.ActionCloseOffer,
	offer.StatusAllotmentFinalised: idempotency.ActionFinaliseAllotment,
	offer.StatusAborted:            idempotency.ActionAbortOffer,
}

// dedicatedEndpoint names where a status is reached when it is not reached through advanceOffer.
var dedicatedEndpoint = map[offer.Status]string{
	offer.StatusBookFrozen:         "POST /admin/offers/{offerId}/book/freeze",
	offer.StatusFeasibilityChecked: "POST /admin/offers/{offerId}/book/freeze",
	offer.StatusBidbookAnchored:    "POST /admin/offers/{offerId}/book/freeze",
	offer.StatusSeedCommitted:      "POST /admin/offers/{offerId}/ballot/commit",
	offer.StatusSeedRevealed:       "POST /admin/offers/{offerId}/ballot/reveal",
	offer.StatusBallotDrawn:        "POST /admin/offers/{offerId}/ballot/draw",
	offer.StatusSettled:            "POST /admin/offers/{offerId}/settlement/finalise",
}

// key derives a content key for an action. The key every table's UNIQUE idempotency_key holds.
func key(action idempotency.Action, schemeID, scope string, payload map[string]any) ([]byte, error) {
	k, err := idempotency.Derive(idempotency.Input{
		Action: action, SchemeID: schemeID, ScopeID: scope, Payload: payload,
	})
	if err != nil {
		return nil, err
	}
	return k[:], nil
}

// evidenceWithNow loads an offer's evidence and stamps it with the scheme's business time.
func (s *Server) evidenceWithNow(ctx context.Context, q store.Querier, offerID string) (*store.OfferEvidence, time.Time, error) {
	oe, err := store.LoadOfferEvidence(ctx, q, offerID)
	if err != nil {
		return nil, time.Time{}, resourceErr("offer", err)
	}
	now, err := s.nowFor(ctx, oe.Scheme.ID)
	if err != nil {
		return nil, time.Time{}, err
	}
	oe.Evidence.Now = now
	return oe, now, nil
}

// frozenBook rebuilds the bid book from the rows.
//
// frozenAt is part of the document, so a rebuild that should reproduce an anchored root must pass the time the
// original was frozen at, which the ballot run records.
func frozenBook(ctx context.Context, q store.Querier, oe *store.OfferEvidence, frozenAt time.Time) (*bidbook.Book, error) {
	entries, err := store.BookEntries(ctx, q, oe.Offer.ID)
	if err != nil {
		return nil, err
	}
	return bidbook.Freeze(bidbook.FreezeInput{
		SchemeRef: schemeRef(oe.Scheme),
		SchemeID:  oe.Scheme.ID,
		OfferID:   oe.Offer.ID,
		FrozenAt:  frozenAt,
		Entries:   entries,
	})
}

// schemeRef is the scheme's public identifier in published documents: a hash of the SEBI reference.
func schemeRef(sc store.Scheme) merkle.Hash {
	return merkle.Hash(sha256.Sum256([]byte(sc.SebiSchemeRef)))
}

// applyFeasibility fills the verdict for an offer whose book is fixed but whose run does not exist yet.
//
// The verdict is not stored; it is recomputed from the rows, deterministically. That is the only way a
// readiness answer about feasibility is an answer rather than a recollection.
func applyFeasibility(ctx context.Context, q store.Querier, oe *store.OfferEvidence) error {
	if oe.Run != nil {
		oe.Evidence.Feasible = true
		return nil
	}
	book, err := frozenBook(ctx, q, oe, oe.Evidence.Now)
	if err != nil {
		if errors.Is(err, bidbook.ErrEmptyBook) {
			oe.Evidence.Feasible, oe.Evidence.FeasibilityReason = false, "no bid survived validation"
			return nil
		}
		return err
	}
	f, err := ballot.CheckFeasibility(book.BallotBids(), oe.Offer.Terms.BallotParams())
	if err != nil {
		return err
	}
	oe.Evidence.ApplyFeasibility(f)
	return nil
}

// --- readiness ----------------------------------------------------------------------------------------

type wireReadiness struct {
	CurrentStatus string   `json:"currentStatus"`
	NextExpected  *string  `json:"nextExpected"`
	CanAdvance    bool     `json:"canAdvance"`
	Blockers      []string `json:"blockers"`
	Paused        bool     `json:"paused"`
}

// handleGetReadiness serves GET /v1/admin/offers/{offerId}/readiness.
//
// This was withheld until the evidence loader existed, because a readiness answer built on partial evidence
// says canAdvance:true about facts nobody checked. It now evaluates offer.Guard against the same evidence an
// advance would be judged against, so the answer and the outcome of pressing the button cannot disagree.
func (s *Server) handleGetReadiness(w http.ResponseWriter, r *http.Request) {
	offerID, err := pathUUID(r, "offerId")
	if err != nil {
		writeError(w, r, err)
		return
	}

	tx, err := s.deps.DB.Begin(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	oe, _, err := s.evidenceWithNow(r.Context(), tx, offerID)
	if err != nil {
		writeError(w, r, err)
		return
	}

	out := wireReadiness{
		CurrentStatus: string(oe.Offer.Status),
		Blockers:      []string{},
		Paused:        oe.Evidence.Paused,
	}

	next, ok := offer.NextExpected(oe.Offer.Status)
	if !ok {
		out.Blockers = append(out.Blockers, fmt.Sprintf("%s is terminal; nothing follows it", oe.Offer.Status))
		writeJSONWithETag(w, r, http.StatusOK, out)
		return
	}
	nextText := string(next)
	out.NextExpected = &nextText

	if oe.Offer.Status.BookIsFixed() {
		if err := applyFeasibility(r.Context(), tx, oe); err != nil {
			writeError(w, r, err)
			return
		}
	}

	if err := offer.Guard(oe.Offer.Status, next, oe.Evidence); err != nil {
		out.Blockers = append(out.Blockers, err.Error())
	} else {
		out.CanAdvance = true
	}
	writeJSONWithETag(w, r, http.StatusOK, out)
}

// --- createOffer --------------------------------------------------------------------------------------

type createOfferRequest struct {
	SchemeID  string         `json:"schemeId"`
	OfferType string         `json:"offerType"`
	Terms     wireOfferTerms `json:"terms"`
}

func (t wireOfferTerms) domain() offer.Terms {
	return offer.Terms{
		UnitsOnOffer:         t.UnitsOnOffer,
		MinBidUnits:          t.MinBidUnits,
		MaxBidUnits:          t.MaxBidUnits,
		MinSubscriptionUnits: t.MinSubscriptionUnits,
		MinDistinctHolders:   t.MinDistinctHolders,
		PriceBandLowerPaise:  t.PriceBandLowerPaise,
		PriceBandUpperPaise:  t.PriceBandUpperPaise,
		OpensAt:              t.OpensAt.UTC(),
		ClosesAt:             t.ClosesAt.UTC(),
		AllotmentDueAt:       t.AllotmentDueAt.UTC(),
	}
}

// handleCreateOffer serves POST /v1/admin/offers.
func (s *Server) handleCreateOffer(c *writeCtx) (int, any, error) {
	var req createOfferRequest
	if err := c.decode(&req); err != nil {
		return 0, nil, err
	}
	if !looksLikeUUID(req.SchemeID) {
		return 0, nil, validation("schemeId is not a uuid")
	}
	if req.OfferType != "INITIAL" && req.OfferType != "FOLLOW_ON" {
		return 0, nil, validation("offerType must be INITIAL or FOLLOW_ON")
	}

	sc, err := store.NewSchemes(c.tx).ByID(c.ctx(), req.SchemeID)
	if err != nil {
		return 0, nil, resourceErr("scheme", err)
	}

	terms := req.Terms.domain()
	if err := terms.Validate(); err != nil {
		// The contract answers bad terms with 422: the request is well formed and describes an offer the
		// rules do not permit.
		return 0, nil, validation(err.Error())
	}

	// An initial offer must place exactly the scheme's public units. Anything else and settlement cannot
	// issue the scheme's total: the register would end short by the difference and finaliseSettlement
	// would refuse, after the draw was already anchored.
	if req.OfferType == "INITIAL" && int64(terms.UnitsOnOffer) != int64(sc.PublicUnits) {
		return 0, nil, validation(fmt.Sprintf(
			"an initial offer must place the scheme's %d public units, not %d; the manager's %d units are "+
				"recorded separately and the register must total %d",
			sc.PublicUnits, terms.UnitsOnOffer, sc.IMUnits, sc.TotalUnits))
	}
	if int64(terms.MinDistinctHolders) < int64(sc.MinPublicHolders) {
		return 0, nil, validation(fmt.Sprintf(
			"the holder floor of %d is below the scheme's statutory %d", terms.MinDistinctHolders, sc.MinPublicHolders))
	}

	now, err := s.nowFor(c.ctx(), sc.ID)
	if err != nil {
		return 0, nil, err
	}

	k, err := key(idempotency.ActionCreateOffer, sc.ID, "", map[string]any{
		"offerType": req.OfferType,
		"units":     terms.UnitsOnOffer,
		"band":      []int64{int64(terms.PriceBandLowerPaise), int64(terms.PriceBandUpperPaise)},
		"opensAt":   terms.OpensAt.Format(time.RFC3339),
		"closesAt":  terms.ClosesAt.Format(time.RFC3339),
	})
	if err != nil {
		return 0, nil, err
	}

	id, err := store.InsertOffer(c.ctx(), c.tx, sc.ID, req.OfferType, terms, k)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, nil, conflict(CodePreconditionFailed,
				"this scheme already has an initial offer, or this exact offer was already created")
		}
		return 0, nil, err
	}

	if err := recordAdminAction(c, sc.EnvironmentTag, now, adminAction{
		Action: string(idempotency.ActionCreateOffer), SchemeID: sc.ID,
		TargetType: store.RelatedOffer, TargetID: id, Key: k,
		Params: map[string]any{"offerType": req.OfferType, "unitsOnOffer": terms.UnitsOnOffer},
	}); err != nil {
		return 0, nil, err
	}

	created, err := store.NewOffers(c.tx).ByID(c.ctx(), id)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, offerToWire(created, nil), nil
}

// validation is a 422 carrying the reason.
func validation(msg string) error {
	return &statusError{status: http.StatusUnprocessableEntity, code: CodeValidationFailed, msg: msg}
}

// --- advanceOffer -------------------------------------------------------------------------------------

type advanceRequest struct {
	To     string `json:"to"`
	Reason string `json:"reason"`
}

// handleAdvanceOffer serves POST /v1/admin/offers/{offerId}/transitions.
//
// The guard decides and the UPDATE records the decision, as a compare-and-set against the status the decision
// was made on. Two operators advancing at once do not both succeed.
func (s *Server) handleAdvanceOffer(c *writeCtx) (int, any, error) {
	offerID, err := pathUUID(c.r, "offerId")
	if err != nil {
		return 0, nil, err
	}
	var req advanceRequest
	if err := c.decode(&req); err != nil {
		return 0, nil, err
	}

	to := offer.Status(req.To)
	if !to.Valid() {
		return 0, nil, validation(fmt.Sprintf("%q is not an offer status", req.To))
	}
	action, direct := advanceTargets[to]
	if !direct {
		if ep, ok := dedicatedEndpoint[to]; ok {
			return 0, nil, validation(fmt.Sprintf("%s is reached through %s, because reaching it does more "+
				"than change a status", to, ep))
		}
		return 0, nil, validation(fmt.Sprintf("%s cannot be requested", to))
	}

	// The abort boundary is wider than the rest. Checked here rather than in the route table because it
	// depends on the body.
	allowed := rolesRunOffer
	if to == offer.StatusAborted {
		allowed = rolesAbort
	}
	if !roleIn(c.principal.Role, allowed) {
		return 0, nil, forbidden(fmt.Sprintf("advancing to %s requires the %s role; the token carries %s",
			to, roleList(allowed), c.principal.Role))
	}

	oe, now, err := s.evidenceWithNow(c.ctx(), c.tx, offerID)
	if err != nil {
		return 0, nil, err
	}
	from := oe.Offer.Status
	oe.Evidence.AbortReason = req.Reason

	// Closing admits the funded bids to the book first, so the guard judges the book it will freeze.
	if to == offer.StatusClosed {
		if err := offer.Guard(from, to, oe.Evidence); err != nil {
			return 0, nil, err
		}
		if _, err := store.AdmitFundedBids(c.ctx(), c.tx, offerID); err != nil {
			return 0, nil, err
		}
	} else if err := offer.Guard(from, to, oe.Evidence); err != nil {
		return 0, nil, err
	}

	if to == offer.StatusAborted {
		if err := s.releaseForAbort(c, oe); err != nil {
			return 0, nil, err
		}
	}

	if err := store.AdvanceOffer(c.ctx(), c.tx, offerID, from, to); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return 0, nil, conflict(CodePreconditionFailed,
				"the offer moved while this request was being decided; reload and try again")
		}
		return 0, nil, err
	}

	payload := map[string]any{"from": string(from), "to": string(to)}
	if req.Reason != "" {
		payload["reason"] = req.Reason
	}
	k, err := key(action, oe.Scheme.ID, offerID, payload)
	if err != nil {
		return 0, nil, err
	}
	if err := recordAdminAction(c, oe.Scheme.EnvironmentTag, now, adminAction{
		Action: string(action), SchemeID: oe.Scheme.ID,
		TargetType: store.RelatedOffer, TargetID: offerID, Key: k, Params: payload,
	}); err != nil {
		return 0, nil, err
	}

	updated, err := store.NewOffers(c.tx).ByID(c.ctx(), offerID)
	if err != nil {
		return 0, nil, err
	}
	sub, err := store.NewOffers(c.tx).SubscriptionFor(c.ctx(), offerID)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, offerToWire(updated, &sub), nil
}

// releaseForAbort returns every investor's blocked money when an offer is abandoned.
//
// An abort under ASBA releases money rather than moving it, which is why the guard allows one even while
// paused. It is refused once any block has been debited: at that point settlement has taken money, and
// abandoning the offer would need refunds, which this system does not perform. Saying so is better than
// marking the offer aborted with investors' money gone.
func (s *Server) releaseForAbort(c *writeCtx, oe *store.OfferEvidence) error {
	var debited int
	if err := c.tx.QueryRow(c.ctx(), `
		SELECT count(*) FROM asba_blocks a JOIN bids b ON b.id = a.bid_id
		 WHERE b.offer_id = $1 AND a.block_status = 'DEBITED'`, oe.Offer.ID).Scan(&debited); err != nil {
		return err
	}
	if debited > 0 {
		return conflict(CodePreconditionFailed, fmt.Sprintf(
			"%d funds block(s) have already been debited for settlement; aborting now would require "+
				"refunds, which this system does not perform", debited))
	}

	rows, err := c.tx.Query(c.ctx(), `
		SELECT b.id, b.bid_reference, a.bank_ref
		  FROM bids b JOIN asba_blocks a ON a.bid_id = b.id
		 WHERE b.offer_id = $1 AND a.block_status = 'BLOCKED'`, oe.Offer.ID)
	if err != nil {
		return err
	}
	type held struct{ bidID, ref, bankRef string }
	var blocks []held
	for rows.Next() {
		var h held
		var bankRef *string
		if err := rows.Scan(&h.bidID, &h.ref, &bankRef); err != nil {
			rows.Close()
			return err
		}
		if bankRef != nil {
			h.bankRef = *bankRef
		}
		blocks = append(blocks, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if len(blocks) > 0 && s.deps.ASBA == nil {
		return conflict(CodePreconditionFailed, "funds are blocked and no ASBA provider is configured to release them")
	}

	for _, h := range blocks {
		k, err := key(idempotency.ActionReleaseASBABlock, oe.Scheme.ID, oe.Offer.ID, map[string]any{"bidRef": h.ref})
		if err != nil {
			return err
		}
		released, err := s.deps.ASBA.Release(c.ctx(), fmt.Sprintf("%x", k), h.bankRef)
		if err != nil {
			return fmt.Errorf("releasing the block for bid %s: %w", h.ref, err)
		}
		if released.Status != asba.StatusUnblocked {
			return fmt.Errorf("the bank reports the block for bid %s as %s after release", h.ref, released.Status)
		}
		if _, err := c.tx.Exec(c.ctx(), `
			UPDATE asba_blocks SET block_status = 'UNBLOCKED', unblocked_at = $2 WHERE bid_id = $1`,
			h.bidID, released.UnblockedAt); err != nil {
			return err
		}
		if _, err := c.tx.Exec(c.ctx(),
			`UPDATE bids SET status = 'FUNDS_UNBLOCKED', updated_at = now() WHERE id = $1`, h.bidID); err != nil {
			return err
		}
	}

	// Bids that never held money are rejected with the reason, so nobody's bid simply vanishes.
	if _, err := c.tx.Exec(c.ctx(), `
		UPDATE bids SET status = 'REJECTED_TECHNICAL', rejection_reason = 'OFFER_ABORTED', updated_at = now()
		 WHERE offer_id = $1 AND status IN ('SUBMITTED', 'BLOCK_REQUESTED', 'VALIDATED')`, oe.Offer.ID); err != nil {
		return err
	}

	if oe.Run != nil {
		if _, err := c.tx.Exec(c.ctx(), `
			UPDATE ballot_runs SET status = 'ABANDONED'
			 WHERE id = $1 AND status NOT IN ('RESULT_ANCHORED', 'ABANDONED')`, oe.Run.ID); err != nil {
			return err
		}
	}
	return nil
}
