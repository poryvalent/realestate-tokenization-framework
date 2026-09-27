package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/acresync/orchestrator/internal/asba"
	"github.com/acresync/orchestrator/internal/ballotrun"
	"github.com/acresync/orchestrator/internal/idempotency"
	"github.com/acresync/orchestrator/internal/ipfsguard"
	"github.com/acresync/orchestrator/internal/merkle"
	"github.com/acresync/orchestrator/internal/offer"
	"github.com/acresync/orchestrator/internal/settlement"
	"github.com/acresync/orchestrator/internal/store"
)

// Settlement: turning the anchored draw into the cap table.
//
// # The plan is rebuilt, never stored
//
// Every settlement request replays the draw from the frozen book and the revealed seed, and requires the replay
// to reproduce both anchored values: the result root, and the digest of the pinned allotment file. Only then is
// the plan built from it. A stored plan would be a second copy of the allotment that could drift from the one
// the chain is bound to; a replay that must match the anchors cannot.
//
// # The chain's position is read from confirmed calls
//
// The settlement cursor, the manager's subscription and the finalisation are what confirmed outbox rows say
// the contract accepted. Nothing queued or still confirming counts. There is no contract read here: a
// ChainState assembled from confirmed calls is what the contract holds if each call did what its payload says,
// which the contract's own checks make true or revert.

// settlementView is the plan, the draw it came from, and the chain's position.
type settlementView struct {
	oe   *store.OfferEvidence
	now  time.Time
	draw *ballotrun.Run
	plan *settlement.Plan
	cs   settlement.ChainState

	imInvestorID string
}

// liveStatus reports whether an outbox row is still going to land, or already has.
func liveStatus(status string) bool {
	switch status {
	case "FAILED", "REORGED", "DEAD_LETTER", "CANCELLED":
		return false
	}
	return true
}

// liveCall returns the newest row for fn that is queued, in flight or confirmed.
func liveCall(oe *store.OfferEvidence, fn string, match func(store.Anchor) bool) *store.Anchor {
	for i := range oe.Anchors[fn] {
		a := &oe.Anchors[fn][i]
		if liveStatus(a.Status) && (match == nil || match(*a)) {
			return a
		}
	}
	return nil
}

// batchAt matches a settleBatch row by the cursor it was queued at.
func batchAt(cursor uint32) func(store.Anchor) bool {
	return func(a store.Anchor) bool {
		var p struct {
			CursorFrom uint32 `json:"cursorFrom"`
		}
		return json.Unmarshal(a.Payload, &p) == nil && p.CursorFrom == cursor
	}
}

// loadSettlement rebuilds the settlement for an offer whose allotment is final.
func (s *Server) loadSettlement(ctx context.Context, q store.Querier, offerID string) (*settlementView, error) {
	oe, now, err := s.evidenceWithNow(ctx, q, offerID)
	if err != nil {
		return nil, err
	}
	if err := requireDeployed(oe.Scheme); err != nil {
		return nil, err
	}
	if st := oe.Offer.Status; st != offer.StatusAllotmentFinalised && st != offer.StatusSettled {
		return nil, conflict(CodePreconditionFailed, fmt.Sprintf(
			"settlement opens once the allotment is finalised; this offer is %s", st))
	}
	run := oe.Run
	if run == nil || run.ResultRoot.IsZero() || run.ExecutedAt == nil {
		return nil, errors.New("httpapi: an allotment-finalised offer has no drawn ballot run")
	}
	if len(s.deps.SeedPepper) == 0 {
		return nil, errors.New("httpapi: no seed pepper is configured")
	}

	// Replay the draw exactly as it ran.
	book, err := frozenBook(ctx, q, oe, run.SnapshotAt, true)
	if err != nil {
		return nil, err
	}
	if book.MerkleRoot != run.BidbookRoot {
		return nil, fmt.Errorf("the rebuilt book root %s does not match the anchored root %s",
			book.MerkleRoot.Hex(), run.BidbookRoot.Hex())
	}
	secret, err := ballotrun.DeriveSecret(s.deps.SeedPepper, oe.Scheme.ID, oe.Offer.ID)
	if err != nil {
		return nil, err
	}
	cer := ceremonyFor(oe)
	// The run has moved past SEED_REVEALED, and Draw accepts only the stage it runs in. This is the same draw
	// replayed for verification, and it is checked against both anchors below.
	cer.Stage = ballotrun.StageSeedRevealed
	draw, err := ballotrun.Draw(ballotrun.RunInput{
		Book: book, Ceremony: cer, Secret: secret, TargetBlockHash: run.TargetBlockHash,
		Params: oe.Offer.Terms.BallotParams(), DrawnAt: *run.ExecutedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("replaying the draw: %w", err)
	}
	if draw.Result.ResultRoot != run.ResultRoot {
		return nil, fmt.Errorf("the replayed draw has result root %s, the anchored root is %s",
			draw.Result.ResultRoot.Hex(), run.ResultRoot.Hex())
	}
	raw, err := draw.Document()
	if err != nil {
		return nil, err
	}
	canonical, err := ipfsguard.ValidateAndCanonicalize(ipfsguard.DocAllotmentFile, raw)
	if err != nil {
		return nil, err
	}
	if digest := sha256.Sum256(canonical); merkle.Hash(digest) != run.ResultCID {
		return nil, fmt.Errorf("the replayed allotment file hashes to %x, the anchored digest is %s",
			digest, run.ResultCID.Hex())
	}

	imID, imWallet, err := store.InvestmentManager(ctx, q, oe.Scheme.ID)
	if err != nil {
		return nil, err
	}
	if imID == "" {
		return nil, conflict(CodePreconditionFailed, "the scheme has no investment manager recorded, so there is "+
			"no wallet to credit the manager's units to; finaliseSettlement cannot pass without them")
	}
	if imWallet == "" {
		return nil, conflict(CodePreconditionFailed, "the investment manager has no active wallet to credit")
	}

	allottees := draw.Allottees()
	ids := make([]string, 0, len(allottees))
	for _, l := range allottees {
		ids = append(ids, l.InvestorID)
	}
	wallets, err := store.ActiveWallets(ctx, q, ids)
	if err != nil {
		return nil, err
	}

	plan, err := settlement.BuildPlan(settlement.BuildInput{
		Run: draw,
		Params: settlement.SchemeParams{
			TotalUnits:       uint32(oe.Scheme.TotalUnits),
			IMUnits:          uint32(oe.Scheme.IMUnits),
			MinPublicHolders: uint32(oe.Scheme.MinPublicHolders),
			MaxBatchSize:     settlement.V1Params().MaxBatchSize,
		},
		Wallets:           wallets,
		IMWallet:          imWallet,
		AllotmentFileHash: run.ResultCID,
		BallotResultRoot:  run.ResultRoot,
	})
	if err != nil {
		// Missing or colliding wallets, mostly: facts about the data that an operator has to fix, with the
		// domain's explanation of which leaf and why.
		return nil, conflict(CodePreconditionFailed, err.Error())
	}

	cs, err := chainState(oe, plan)
	if err != nil {
		return nil, err
	}
	return &settlementView{oe: oe, now: now, draw: draw, plan: plan, cs: cs, imInvestorID: imID}, nil
}

// chainState is the contract's settlement position as evidenced by confirmed calls.
func chainState(oe *store.OfferEvidence, plan *settlement.Plan) (settlement.ChainState, error) {
	ev := oe.Evidence.Settlement
	cs := settlement.ChainState{
		Stage:           settlement.StageNotStarted,
		ExpectedUnits:   ev.ExpectedUnits,
		ExpectedHolders: ev.ExpectedHolders,
		CreditedUnits:   ev.CreditedUnits,
		CreditedHolders: ev.CreditedHolders,
		// Every wallet in the plan is distinct and none is the manager's, which BuildPlan checks, so each
		// credited entry is one more countable holder.
		DistinctHolderCount: ev.CreditedHolders,
		TotalUnitsIssued:    ev.CreditedUnits,
	}
	if ev.Begun {
		cs.Stage = settlement.StageInProgress
	}
	if oe.Latest(store.FnFinaliseSettlement).IsConfirmed() {
		cs.Stage = settlement.StageFinalised
	}
	if im := oe.Confirmed(store.FnRecordIMSubscription); len(im) > 0 {
		var p struct {
			Wallet string `json:"wallet"`
			Units  uint32 `json:"units"`
		}
		if err := json.Unmarshal(im[0].Payload, &p); err != nil {
			return cs, fmt.Errorf("reading the confirmed recordImSubscription payload: %w", err)
		}
		// The contract sets the wallet, excludes it from the count and credits it in one call.
		cs.IMWallet, cs.IMWalletUnits, cs.IMExcluded = p.Wallet, p.Units, true
		cs.TotalUnitsIssued += p.Units
	}
	return cs, nil
}

// --- wire -----------------------------------------------------------------------------------------------

type wireSettlementBatch struct {
	CursorFrom  uint32 `json:"cursorFrom"`
	CursorTo    uint32 `json:"cursorTo"`
	HolderCount int    `json:"holderCount"`
	Units       uint32 `json:"units"`
	Submitted   bool   `json:"submitted"`
}

type wireFinalisation struct {
	Ready    bool     `json:"ready"`
	Failures []string `json:"failures"`
}

type wireSettlement struct {
	OfferID           string                `json:"offerId"`
	Stage             string                `json:"stage"`
	ExpectedUnits     uint32                `json:"expectedUnits"`
	ExpectedHolders   uint32                `json:"expectedHolders"`
	CreditedUnits     uint32                `json:"creditedUnits"`
	CreditedHolders   uint32                `json:"creditedHolders"`
	AllotmentFileHash string                `json:"allotmentFileHash"`
	BallotResultRoot  string                `json:"ballotResultRoot"`
	IMUnitsRecorded   bool                  `json:"imUnitsRecorded"`
	Batches           []wireSettlementBatch `json:"batches"`
	NextStep          *string               `json:"nextStep"`
	Finalisation      wireFinalisation      `json:"finalisation"`
}

// toWire renders the view.
//
// expectedUnits and expectedHolders are the plan's until beginSettlement confirms, and the chain's after: the
// plan is what will be declared, and once declared the chain's figures are the ones that bind.
func (v *settlementView) toWire() wireSettlement {
	p, cs := v.plan, v.cs
	out := wireSettlement{
		OfferID:           v.oe.Offer.ID,
		Stage:             cs.Stage.String(),
		ExpectedUnits:     p.ExpectedUnits,
		ExpectedHolders:   p.ExpectedHolders,
		CreditedUnits:     cs.CreditedUnits,
		CreditedHolders:   cs.CreditedHolders,
		AllotmentFileHash: merkle.Hash(p.AllotmentFileHash).Hex(),
		BallotResultRoot:  p.BallotResultRoot.Hex(),
		IMUnitsRecorded:   cs.IMWallet != "",
		Batches:           make([]wireSettlementBatch, 0, len(p.Batches)),
	}
	if cs.Stage != settlement.StageNotStarted {
		out.ExpectedUnits, out.ExpectedHolders = cs.ExpectedUnits, cs.ExpectedHolders
	}
	for _, b := range p.Batches {
		out.Batches = append(out.Batches, wireSettlementBatch{
			CursorFrom: b.CursorFrom, CursorTo: b.CursorTo(), HolderCount: len(b.Entries), Units: b.Units,
			Submitted: liveCall(v.oe, store.FnSettleBatch, batchAt(b.CursorFrom)) != nil,
		})
	}

	report := settlement.CheckFinalisation(p, cs)
	out.Finalisation = wireFinalisation{Ready: report.Ready, Failures: append([]string{}, report.Failures...)}

	if next := v.nextStep(report); next != "" {
		out.NextStep = &next
	}
	return out
}

// nextStep is the call an operator can make now. Empty when the next call is already queued and the only thing
// to do is wait for it, when finalisation is blocked, or when settlement is over.
func (v *settlementView) nextStep(report settlement.FinalisationReport) string {
	oe, cs := v.oe, v.cs
	switch {
	case cs.Stage == settlement.StageFinalised, liveCall(oe, store.FnFinaliseSettlement, nil) != nil:
		return ""
	case cs.Stage == settlement.StageNotStarted:
		if liveCall(oe, store.FnBeginSettlement, nil) != nil {
			return ""
		}
		return store.FnBeginSettlement
	case cs.CreditedHolders < cs.ExpectedHolders:
		if liveCall(oe, store.FnSettleBatch, batchAt(cs.CreditedHolders)) != nil {
			return ""
		}
		return store.FnSettleBatch
	case report.Ready:
		return store.FnFinaliseSettlement
	}
	return ""
}

// --- handlers -------------------------------------------------------------------------------------------

// handleGetSettlement serves GET /v1/admin/offers/{offerId}/settlement.
func (s *Server) handleGetSettlement(w http.ResponseWriter, r *http.Request) {
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

	v, err := s.loadSettlement(r.Context(), tx, offerID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSONWithETag(w, r, http.StatusOK, v.toWire())
}

// settlementWrite loads the view for a mutation and refuses the preconditions they share.
func (s *Server) settlementWrite(c *writeCtx) (*settlementView, error) {
	offerID, err := pathUUID(c.r, "offerId")
	if err != nil {
		return nil, err
	}
	v, err := s.loadSettlement(c.ctx(), c.tx, offerID)
	if err != nil {
		return nil, err
	}
	if v.oe.Offer.Status == offer.StatusSettled {
		return nil, conflict(CodePreconditionFailed, "the offer is settled; finaliseSettlement has been queued")
	}
	// Every settlement call on the contract is notPaused.
	if v.oe.Evidence.Paused {
		return nil, fmt.Errorf("%w: settlement calls revert while the scheme is paused", offer.ErrPaused)
	}
	return v, nil
}

// respond re-reads the settlement after a write, inside the same transaction, so the response shows the call
// just queued.
func (s *Server) settlementResponse(c *writeCtx, offerID string) (int, any, error) {
	v, err := s.loadSettlement(c.ctx(), c.tx, offerID)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusAccepted, v.toWire(), nil
}

// handleBeginSettlement serves POST /v1/admin/offers/{offerId}/settlement/begin.
//
// Three things, in one transaction: the bank is told to debit each allottee's allotted amount and release the
// rest, the manager's subscription is credited and queued if it is not already, and beginSettlement is queued.
//
// The money moves here rather than after the chain confirms, because the depository credits an allottee at
// allotment and the chain is a public copy of that register, not its precondition. Settle is idempotent at the
// bank, so a transaction that fails after the bank acted is retried with the same instruction.
func (s *Server) handleBeginSettlement(c *writeCtx) (int, any, error) {
	v, err := s.settlementWrite(c)
	if err != nil {
		return 0, nil, err
	}
	oe, plan := v.oe, v.plan
	if v.cs.Stage != settlement.StageNotStarted || liveCall(oe, store.FnBeginSettlement, nil) != nil {
		return 0, nil, conflict(CodePreconditionFailed, "settlement has already been opened for this offer")
	}

	for _, l := range v.draw.Lines {
		k, err := key(idempotency.ActionSettleASBABlock, oe.Scheme.ID, oe.Offer.ID,
			map[string]any{"bidRef": l.BidRef, "debit": int64(l.AmountPayable)})
		if err != nil {
			return 0, nil, err
		}
		settled, err := s.deps.ASBA.Settle(c.ctx(), asba.SettleRequest{
			IdempotencyKey: fmt.Sprintf("%x", k), BankRef: l.Block.BankRef, DebitPaise: l.AmountPayable,
		})
		if err != nil {
			return 0, nil, fmt.Errorf("settling the funds block for bid %s: %w", l.BidRef, err)
		}
		if err := settled.Reconcile(); err != nil {
			return 0, nil, fmt.Errorf("the bank's settlement of bid %s does not reconcile: %w", l.BidRef, err)
		}
		if err := store.RecordASBASettlement(c.ctx(), c.tx, l.BidID, settled); err != nil {
			return 0, nil, err
		}
	}

	// The manager's units, once per scheme.
	var imOutbox string
	if liveCall(oe, store.FnRecordIMSubscription, nil) == nil {
		ledgerKey, err := key(idempotency.ActionIMSubscription, oe.Scheme.ID, v.imInvestorID,
			map[string]any{"ledger": v.imInvestorID, "units": plan.Params.IMUnits})
		if err != nil {
			return 0, nil, err
		}
		if err := store.CreditHolding(c.ctx(), c.tx, store.Credit{
			SchemeID: oe.Scheme.ID, InvestorID: v.imInvestorID, Wallet: plan.IMWallet, Units: plan.Params.IMUnits,
			EntryType: "IM_SUBSCRIPTION", Excluded: true, At: v.now, Key: ledgerKey,
		}); err != nil {
			return 0, nil, err
		}
		payload, err := plan.IMPayload()
		if err != nil {
			return 0, nil, err
		}
		k, err := idempotency.Derive(plan.IMIdempotencyInput())
		if err != nil {
			return 0, nil, err
		}
		if imOutbox, err = enqueue(c, oe.Scheme, oe.Scheme.SchemeAddress, store.FnRecordIMSubscription, payload, k,
			store.RelatedOffer, oe.Offer.ID); err != nil {
			return 0, nil, err
		}
	}

	payload, err := plan.BeginPayload()
	if err != nil {
		return 0, nil, err
	}
	k, err := idempotency.Derive(plan.BeginIdempotencyInput())
	if err != nil {
		return 0, nil, err
	}
	outboxID, err := enqueue(c, oe.Scheme, oe.Scheme.SchemeAddress, store.FnBeginSettlement, payload, k,
		store.RelatedOffer, oe.Offer.ID)
	if err != nil {
		return 0, nil, err
	}

	if err := recordAdminAction(c, oe.Scheme.EnvironmentTag, v.now, adminAction{
		Action: string(idempotency.ActionBeginSettlement), SchemeID: oe.Scheme.ID,
		TargetType: store.RelatedOffer, TargetID: oe.Offer.ID, Key: k[:], OutboxID: outboxID,
		Params: map[string]any{
			"expectedUnits": plan.ExpectedUnits, "expectedHolders": plan.ExpectedHolders,
			"batches": len(plan.Batches), "imSubscriptionOutbox": imOutbox,
		},
	}); err != nil {
		return 0, nil, err
	}
	return s.settlementResponse(c, oe.Offer.ID)
}

// handleSubmitSettlementBatch serves POST /v1/admin/offers/{offerId}/settlement/batches.
//
// The caller states the cursor it believes is next, and it must equal the confirmed credited-holder count
// exactly. Stating it rather than inferring it means a client acting on a stale view is refused instead of
// silently submitting a different batch from the one it meant.
func (s *Server) handleSubmitSettlementBatch(c *writeCtx) (int, any, error) {
	var body struct {
		CursorFrom *int64 `json:"cursorFrom"`
	}
	if err := c.decode(&body); err != nil {
		return 0, nil, err
	}
	if body.CursorFrom == nil {
		return 0, nil, validation("cursorFrom is required")
	}
	if *body.CursorFrom < 0 || *body.CursorFrom > int64(^uint32(0)>>1) {
		return 0, nil, validation("cursorFrom must be a non-negative int32")
	}
	cursor := uint32(*body.CursorFrom)

	v, err := s.settlementWrite(c)
	if err != nil {
		return 0, nil, err
	}
	oe, plan, cs := v.oe, v.plan, v.cs

	if cs.Stage == settlement.StageNotStarted {
		if liveCall(oe, store.FnBeginSettlement, nil) != nil {
			return 0, nil, conflict(CodeAnchorNotConfirmed, fmt.Sprintf(
				"beginSettlement needs %d confirmations before a batch can be credited against it",
				offer.MinAnchorConfirmations))
		}
		return 0, nil, conflict(CodePreconditionFailed, "settlement has not been opened; call beginSettlement first")
	}
	if cursor != cs.CreditedHolders {
		return 0, nil, conflict(CodePreconditionFailed, fmt.Sprintf(
			"cursorFrom is %d but the chain has credited %d holders; the next batch must start exactly there. "+
				"A batch still confirming has not moved the cursor", cursor, cs.CreditedHolders))
	}
	batch, err := plan.BatchAt(cursor)
	if err != nil {
		return 0, nil, conflict(CodePreconditionFailed, err.Error())
	}
	if prev := liveCall(oe, store.FnSettleBatch, batchAt(cursor)); prev != nil {
		return 0, nil, conflict(CodePreconditionFailed, fmt.Sprintf(
			"the batch at cursor %d is already %s; wait for it to confirm", cursor, prev.Status))
	}
	for _, a := range oe.Anchors[store.FnSettleBatch] {
		if batchAt(cursor)(a) {
			// Resubmitting would derive the same key. A failed call is retried from its own outbox row, which
			// is the verbatim retry the contract's cursor is designed to accept.
			return 0, nil, conflict(CodePreconditionFailed, fmt.Sprintf(
				"the batch at cursor %d is %s; it is retried from its outbox row, not resubmitted", cursor, a.Status))
		}
	}

	bidIDs := make([]string, 0, len(batch.Entries))
	for _, e := range batch.Entries {
		ledgerKey, err := key(idempotency.ActionSettleBatch, oe.Scheme.ID, oe.Offer.ID,
			map[string]any{"ledger": e.InvestorID, "units": e.Units})
		if err != nil {
			return 0, nil, err
		}
		if err := store.CreditHolding(c.ctx(), c.tx, store.Credit{
			SchemeID: oe.Scheme.ID, InvestorID: e.InvestorID, Wallet: e.Wallet, Units: e.Units,
			EntryType: "ALLOTMENT", At: v.now, Key: ledgerKey,
		}); err != nil {
			return 0, nil, err
		}
		bidIDs = append(bidIDs, e.BidID)
	}
	if err := store.MarkUnitsCredited(c.ctx(), c.tx, bidIDs); err != nil {
		return 0, nil, err
	}

	payload, err := plan.BatchPayload(batch)
	if err != nil {
		return 0, nil, err
	}
	k, err := idempotency.Derive(plan.BatchIdempotencyInput(batch))
	if err != nil {
		return 0, nil, err
	}
	outboxID, err := enqueue(c, oe.Scheme, oe.Scheme.SchemeAddress, store.FnSettleBatch, payload, k,
		store.RelatedOffer, oe.Offer.ID)
	if err != nil {
		return 0, nil, err
	}
	if err := recordAdminAction(c, oe.Scheme.EnvironmentTag, v.now, adminAction{
		Action: string(idempotency.ActionSettleBatch), SchemeID: oe.Scheme.ID,
		TargetType: store.RelatedOffer, TargetID: oe.Offer.ID, Key: k[:], OutboxID: outboxID,
		Params: map[string]any{"cursorFrom": batch.CursorFrom, "cursorTo": batch.CursorTo(), "units": batch.Units},
	}); err != nil {
		return 0, nil, err
	}
	return s.settlementResponse(c, oe.Offer.ID)
}

// handleFinaliseSettlement serves POST /v1/admin/offers/{offerId}/settlement/finalise.
//
// Refused unless all five of the contract's checks pass against the confirmed state, with every failure named.
// The offer moves to SETTLED when the call is queued, as every other step moves when its call is queued; the
// chain's agreement is read from the outbox row, and the settlement view reports FINALISED only once it
// confirms.
func (s *Server) handleFinaliseSettlement(c *writeCtx) (int, any, error) {
	v, err := s.settlementWrite(c)
	if err != nil {
		return 0, nil, err
	}
	oe, plan := v.oe, v.plan

	if liveCall(oe, store.FnFinaliseSettlement, nil) != nil {
		return 0, nil, conflict(CodePreconditionFailed, "finaliseSettlement is already queued")
	}
	if report := settlement.CheckFinalisation(plan, v.cs); !report.Ready {
		return 0, nil, conflict(CodeSettlementIncomplete,
			"finaliseSettlement would revert: "+strings.Join(report.Failures, "; "))
	}
	if err := offer.Guard(oe.Offer.Status, offer.StatusSettled, oe.Evidence); err != nil {
		return 0, nil, err
	}
	if err := store.AdvanceOffer(c.ctx(), c.tx, oe.Offer.ID, offer.StatusAllotmentFinalised, offer.StatusSettled); err != nil {
		return 0, nil, err
	}

	k, err := idempotency.Derive(plan.FinaliseIdempotencyInput())
	if err != nil {
		return 0, nil, err
	}
	// finaliseSettlement(bytes32 idempotencyKey): the key is appended by the transaction builder, so there are
	// no other arguments to carry.
	outboxID, err := enqueue(c, oe.Scheme, oe.Scheme.SchemeAddress, store.FnFinaliseSettlement, []byte(`{}`), k,
		store.RelatedOffer, oe.Offer.ID)
	if err != nil {
		return 0, nil, err
	}
	if err := recordAdminAction(c, oe.Scheme.EnvironmentTag, v.now, adminAction{
		Action: string(idempotency.ActionFinaliseSettlement), SchemeID: oe.Scheme.ID,
		TargetType: store.RelatedOffer, TargetID: oe.Offer.ID, Key: k[:], OutboxID: outboxID,
		Params: map[string]any{"creditedUnits": v.cs.CreditedUnits, "creditedHolders": v.cs.CreditedHolders},
	}); err != nil {
		return 0, nil, err
	}
	return s.settlementResponse(c, oe.Offer.ID)
}
