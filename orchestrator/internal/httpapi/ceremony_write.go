package httpapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/acresync/orchestrator/internal/ballot"
	"github.com/acresync/orchestrator/internal/ballotrun"
	"github.com/acresync/orchestrator/internal/idempotency"
	"github.com/acresync/orchestrator/internal/ipfs"
	"github.com/acresync/orchestrator/internal/ipfsguard"
	"github.com/acresync/orchestrator/internal/merkle"
	"github.com/acresync/orchestrator/internal/offer"
	"github.com/acresync/orchestrator/internal/outbox"
	"github.com/acresync/orchestrator/internal/store"
)

// The bid book freeze and the commit-reveal ceremony.
//
// # The order is the security property
//
// Freeze and anchor the book, then commit to a secret, then wait for the commitment to confirm, then reveal the
// secret against a block hash nobody could know at commit time, then draw. Each endpoint refuses unless the
// previous step's chain call has confirmed to depth, read from the outbox row that carries it. A step that
// proceeded on a queued-but-unconfirmed anchor would bind the ceremony to something a reorg could still remove.
//
// # Nothing here is a free choice
//
// The secret is derived, not supplied. The target block is chosen by the contract, not by the caller. The
// block hash is read from the chain. The draw is a pure function of the anchored book and the final seed. An
// operator pressing these buttons decides only when, never what.

// enqueue queues a chain call in the request's transaction.
func enqueue(c *writeCtx, sc store.Scheme, target, fn string, payload []byte, k idempotency.Key, entityType, entityID string) (string, error) {
	e, err := outbox.EnqueueWith(c.ctx(), c.tx, outbox.NewEntry{
		SchemeID:          sc.ID,
		TargetContract:    target,
		FunctionName:      fn,
		Payload:           payload,
		IdempotencyKey:    k,
		RelatedEntityType: entityType,
		RelatedEntityID:   entityID,
		EnvironmentTag:    sc.EnvironmentTag,
	})
	if errors.Is(err, outbox.ErrDuplicateKey) {
		return "", conflict(CodePreconditionFailed, fmt.Sprintf("%s for this offer is already queued", fn))
	}
	if err != nil {
		return "", err
	}
	return e.ID, nil
}

// wirePublished is the contract's PublishedDocument.
type wirePublished struct {
	DocType             string  `json:"docType"`
	Digest              string  `json:"digest"`
	CID                 *string `json:"cid"`
	ByteSize            *int    `json:"byteSize"`
	AnchoredTx          *string `json:"anchoredTx"`
	AnchorConfirmations *int    `json:"anchorConfirmations"`
}

func publishedToWire(docType string, pin *ipfs.Pin) *wirePublished {
	cid := pin.ProviderCID
	if cid == "" {
		cid = pin.DerivedCID
	}
	size := pin.ByteSize
	return &wirePublished{DocType: docType, Digest: hexBytes(pin.Digest[:]), CID: &cid, ByteSize: &size}
}

// pinDocument pins a document and records the pin against the ballot run, in the request's transaction.
func (s *Server) pinDocument(c *writeCtx, sc store.Scheme, docType ipfsguard.DocType, dbDocType string, raw []byte, runID string) (*ipfs.Pin, error) {
	pin, err := s.deps.Publisher.Publish(c.ctx(), docType, raw)
	if err != nil {
		return nil, fmt.Errorf("pinning the %s: %w", dbDocType, err)
	}
	k, err := key(idempotency.ActionAnchorDocument, sc.ID, runID, map[string]any{"digest": merkle.Hash(pin.Digest).Hex()})
	if err != nil {
		return nil, err
	}
	if err := store.InsertPin(c.ctx(), c.tx, sc.ID, dbDocType, s.deps.Publisher.ProviderName(), pin,
		store.RelatedBallotRun, runID, k); err != nil {
		return nil, err
	}
	return pin, nil
}

// --- freezeBook -----------------------------------------------------------------------------------------

type wireFeasibility struct {
	Feasible            bool     `json:"feasible"`
	SubscriptionMet     bool     `json:"subscriptionMet"`
	HolderFloorMet      bool     `json:"holderFloorMet"`
	FullSubscriptionMet bool     `json:"fullSubscriptionMet"`
	Reasons             []string `json:"reasons"`
}

type wireFreezeResult struct {
	OfferID         string           `json:"offerId"`
	MerkleRoot      string           `json:"merkleRoot"`
	LeafCount       uint32           `json:"leafCount"`
	TotalUnitsBid   uint32           `json:"totalUnitsBid"`
	DistinctBidders uint32           `json:"distinctBidders"`
	FrozenAt        timestamp        `json:"frozenAt"`
	Feasibility     *wireFeasibility `json:"feasibility,omitempty"`
	Document        *wirePublished   `json:"document,omitempty"`
}

// handleFreezeBook serves POST /v1/admin/offers/{offerId}/book/freeze.
//
// Freezes, tests feasibility, and only if the offer can list, pins the book and queues its anchor. An
// infeasible offer stops at FEASIBILITY_CHECKED with the reasons, because anchoring is the first irreversible
// public act and an offer that cannot list should be abandoned quietly rather than anchored and then found
// unlistable with the evidence already permanent.
func (s *Server) handleFreezeBook(c *writeCtx) (int, any, error) {
	offerID, err := pathUUID(c.r, "offerId")
	if err != nil {
		return 0, nil, err
	}
	oe, now, err := s.evidenceWithNow(c.ctx(), c.tx, offerID)
	if err != nil {
		return 0, nil, err
	}
	if err := requireDeployed(oe.Scheme); err != nil {
		return 0, nil, err
	}
	ev := &oe.Evidence

	if err := offer.Guard(oe.Offer.Status, offer.StatusBookFrozen, *ev); err != nil {
		return 0, nil, err
	}
	book, err := frozenBook(c.ctx(), c.tx, oe, now, false)
	if err != nil {
		return 0, nil, err
	}
	if err := store.AdvanceOffer(c.ctx(), c.tx, offerID, offer.StatusClosed, offer.StatusBookFrozen); err != nil {
		return 0, nil, err
	}

	f, err := ballot.CheckFeasibility(book.BallotBids(), oe.Offer.Terms.BallotParams())
	if err != nil {
		return 0, nil, err
	}
	ev.ApplyFeasibility(f)
	if err := offer.Guard(offer.StatusBookFrozen, offer.StatusFeasibilityChecked, *ev); err != nil {
		return 0, nil, err
	}
	if err := store.AdvanceOffer(c.ctx(), c.tx, offerID, offer.StatusBookFrozen, offer.StatusFeasibilityChecked); err != nil {
		return 0, nil, err
	}

	freezeKey, err := idempotency.Derive(book.FreezeIdempotencyInput())
	if err != nil {
		return 0, nil, err
	}
	out := wireFreezeResult{
		OfferID: offerID, MerkleRoot: book.MerkleRoot.Hex(), LeafCount: book.LeafCount,
		TotalUnitsBid: book.TotalUnitsBid, DistinctBidders: book.DistinctBidders, FrozenAt: book.FrozenAt,
		Feasibility: &wireFeasibility{
			Feasible: f.Feasible, SubscriptionMet: f.SubscriptionMet, HolderFloorMet: f.HolderFloorMet,
			FullSubscriptionMet: f.FullSubscriptionMet, Reasons: append([]string{}, f.Reasons...),
		},
	}

	if !f.Feasible {
		if err := recordAdminAction(c, oe.Scheme.EnvironmentTag, now, adminAction{
			Action: string(idempotency.ActionFreezeBook), SchemeID: oe.Scheme.ID,
			TargetType: store.RelatedOffer, TargetID: offerID, Key: freezeKey[:],
			Params: map[string]any{"root": book.MerkleRoot.Hex(), "feasible": false},
		}); err != nil {
			return 0, nil, err
		}
		return http.StatusOK, out, nil
	}

	raw, err := book.Document()
	if err != nil {
		return 0, nil, err
	}

	// The run row is needed first so the pin can point at it.
	runKey, err := key(idempotency.ActionAnchorBidbook, oe.Scheme.ID, offerID, map[string]any{"root": book.MerkleRoot.Hex()})
	if err != nil {
		return 0, nil, err
	}
	pin, err := s.deps.Publisher.Publish(c.ctx(), ipfsguard.DocBidbook, raw)
	if err != nil {
		return 0, nil, fmt.Errorf("pinning the bid book: %w", err)
	}
	req, err := book.AnchorRequest(pin)
	if err != nil {
		return 0, nil, err
	}
	num, den := book.OversubscriptionRatio(oe.Offer.Terms.UnitsOnOffer)
	runID, err := store.InsertBallotRun(c.ctx(), c.tx, store.NewBallotRun{
		OfferID: offerID, FrozenAt: book.FrozenAt, Anchor: req,
		OversubNum: num, OversubDen: den, Algo: book.AlgoVersion, Key: runKey,
	})
	if err != nil {
		return 0, nil, err
	}
	pinKey, err := key(idempotency.ActionAnchorDocument, oe.Scheme.ID, runID, map[string]any{"digest": merkle.Hash(pin.Digest).Hex()})
	if err != nil {
		return 0, nil, err
	}
	if err := store.InsertPin(c.ctx(), c.tx, oe.Scheme.ID, "BIDBOOK", s.deps.Publisher.ProviderName(), pin,
		store.RelatedBallotRun, runID, pinKey); err != nil {
		return 0, nil, err
	}

	ev.Ceremony.BidbookRoot = book.MerkleRoot
	ev.BidbookPinned = true
	if err := offer.Guard(offer.StatusFeasibilityChecked, offer.StatusBidbookAnchored, *ev); err != nil {
		return 0, nil, err
	}
	if err := store.AdvanceOffer(c.ctx(), c.tx, offerID, offer.StatusFeasibilityChecked, offer.StatusBidbookAnchored); err != nil {
		return 0, nil, err
	}

	payload, err := req.OutboxPayload()
	if err != nil {
		return 0, nil, err
	}
	anchorKey, err := book.IdempotencyKey(req)
	if err != nil {
		return 0, nil, err
	}
	outboxID, err := enqueue(c, oe.Scheme, oe.Scheme.BallotAddress, store.FnAnchorBidbook, payload, anchorKey,
		store.RelatedBallotRun, runID)
	if err != nil {
		return 0, nil, err
	}

	if err := recordAdminAction(c, oe.Scheme.EnvironmentTag, now, adminAction{
		Action: string(idempotency.ActionFreezeBook), SchemeID: oe.Scheme.ID,
		TargetType: store.RelatedBallotRun, TargetID: runID, Key: freezeKey[:], OutboxID: outboxID,
		Params: map[string]any{"root": book.MerkleRoot.Hex(), "feasible": true, "leafCount": book.LeafCount},
	}); err != nil {
		return 0, nil, err
	}

	out.Document = publishedToWire("BIDBOOK", pin)
	return http.StatusOK, out, nil
}

// --- the ceremony ---------------------------------------------------------------------------------------

// ceremonyFor rebuilds the ceremony state from the ballot run and its confirmed chain calls.
func ceremonyFor(oe *store.OfferEvidence) *ballotrun.Ceremony {
	run := oe.Run
	c := &ballotrun.Ceremony{
		SchemeID:    oe.Scheme.ID,
		OfferID:     oe.Offer.ID,
		Stage:       ballotrun.Stage(run.Status),
		BidbookRoot: run.BidbookRoot,
		Commitment:  run.Commitment,
		Attempt:     uint32(run.Attempt),
		Config:      ballotrun.V1Config(),
	}
	c.CommitmentAnchored = oe.Latest(store.FnCommitSeed).IsConfirmed()
	c.TargetBlock = targetBlockFor(oe)
	return c
}

// targetBlockFor is the block whose hash feeds the seed.
//
// The contract sets it to the block the commit (or the latest recommit) was mined in, plus the delay. That
// block is recorded on the confirmed outbox row, so the target is derived from the chain's own record rather
// than supplied by anybody. Until the call confirms, there is no target, and zero says so.
func targetBlockFor(oe *store.OfferEvidence) uint64 {
	if oe.Run.TargetBlock != nil {
		return uint64(*oe.Run.TargetBlock)
	}
	a := oe.Latest(store.FnRecommitSeed)
	if a == nil {
		a = oe.Latest(store.FnCommitSeed)
	}
	if a == nil || !a.IsConfirmed() || a.BlockNumber == nil {
		return 0
	}
	return uint64(*a.BlockNumber) + uint64(ballotrun.V1Config().DelayBlocks)
}

type wireCeremony struct {
	OfferID             string  `json:"offerId"`
	Stage               string  `json:"stage"`
	BidbookRoot         *string `json:"bidbookRoot"`
	BidbookCIDDigest    *string `json:"bidbookCidDigest"`
	SeedCommitment      *string `json:"seedCommitment"`
	TargetBlock         *uint64 `json:"targetBlock"`
	RevealWindowBlocks  uint32  `json:"revealWindowBlocks"`
	RevealDeadlineBlock *uint64 `json:"revealDeadlineBlock"`
	SeedPlaintext       *string `json:"seedPlaintext"`
	TargetBlockHash     *string `json:"targetBlockHash"`
	FinalSeed           *string `json:"finalSeed"`
	ResultRoot          *string `json:"resultRoot"`
	Attempt             uint32  `json:"attempt"`
	MaxAttempts         uint32  `json:"maxAttempts"`
	Escalated           bool    `json:"escalated"`
}

func optHash(h merkle.Hash) *string {
	if h.IsZero() {
		return nil
	}
	s := h.Hex()
	return &s
}

// ceremonyToWire renders the ceremony. The plaintext secret is shown only once it has been revealed, at which
// point it is on-chain and public anyway.
func ceremonyToWire(offerID string, run *store.BallotRun, cer *ballotrun.Ceremony) wireCeremony {
	out := wireCeremony{
		OfferID:            offerID,
		Stage:              string(cer.Stage),
		BidbookRoot:        optHash(run.BidbookRoot),
		BidbookCIDDigest:   optHash(run.BidbookCID),
		SeedCommitment:     optHash(cer.Commitment),
		RevealWindowBlocks: cer.Config.RevealWindowBlocks,
		SeedPlaintext:      optHash(run.SeedPlaintext),
		TargetBlockHash:    optHash(run.TargetBlockHash),
		FinalSeed:          optHash(run.FinalSeed),
		ResultRoot:         optHash(run.ResultRoot),
		Attempt:            cer.Attempt,
		MaxAttempts:        cer.Config.MaxAttempts,
		Escalated:          cer.Stage == ballotrun.StageEscalated,
	}
	if cer.TargetBlock != 0 {
		target, deadline := cer.TargetBlock, cer.Deadline()
		out.TargetBlock, out.RevealDeadlineBlock = &target, &deadline
	}
	return out
}

// ceremonyContext loads what every ceremony step needs and refuses the common preconditions.
func (s *Server) ceremonyContext(c *writeCtx) (*store.OfferEvidence, timestampT, *ballotrun.Ceremony, merkle.Hash, error) {
	offerID, err := pathUUID(c.r, "offerId")
	if err != nil {
		return nil, timestampT{}, nil, merkle.Hash{}, err
	}
	oe, now, err := s.evidenceWithNow(c.ctx(), c.tx, offerID)
	if err != nil {
		return nil, timestampT{}, nil, merkle.Hash{}, err
	}
	if err := requireDeployed(oe.Scheme); err != nil {
		return nil, timestampT{}, nil, merkle.Hash{}, err
	}
	if oe.Run == nil {
		return nil, timestampT{}, nil, merkle.Hash{}, conflict(CodeCeremonyOrder,
			"the bid book has not been frozen and anchored, so there is no ceremony yet")
	}
	if len(s.deps.SeedPepper) == 0 {
		return nil, timestampT{}, nil, merkle.Hash{}, errors.New("httpapi: no seed pepper is configured")
	}
	secret, err := ballotrun.DeriveSecret(s.deps.SeedPepper, oe.Scheme.ID, oe.Offer.ID)
	if err != nil {
		return nil, timestampT{}, nil, merkle.Hash{}, err
	}
	return oe, timestampT{now}, ceremonyFor(oe), secret, nil
}

// timestampT wraps business time for the ceremony context's return list.
type timestampT struct{ at timestamp }

// handleCommitSeed serves POST /v1/admin/offers/{offerId}/ballot/commit.
func (s *Server) handleCommitSeed(c *writeCtx) (int, any, error) {
	oe, now, cer, secret, err := s.ceremonyContext(c)
	if err != nil {
		return 0, nil, err
	}

	cer.Commitment = ballot.Commitment(secret)
	if err := cer.CanCommit(); err != nil {
		return 0, nil, ceremonyErr(err)
	}

	ev := &oe.Evidence
	ev.Ceremony.SeedCommitment = cer.Commitment
	ev.Ceremony.Attempt = 1
	if err := offer.Guard(oe.Offer.Status, offer.StatusSeedCommitted, *ev); err != nil {
		return 0, nil, err
	}

	if err := store.CommitSeed(c.ctx(), c.tx, oe.Run.ID, cer.Commitment, 1); err != nil {
		return 0, nil, err
	}
	if err := store.AdvanceOffer(c.ctx(), c.tx, oe.Offer.ID, offer.StatusBidbookAnchored, offer.StatusSeedCommitted); err != nil {
		return 0, nil, err
	}

	payload, err := cer.CommitPayload()
	if err != nil {
		return 0, nil, err
	}
	k, err := idempotency.Derive(cer.CommitIdempotencyInput())
	if err != nil {
		return 0, nil, err
	}
	outboxID, err := enqueue(c, oe.Scheme, oe.Scheme.BallotAddress, store.FnCommitSeed, payload, k, store.RelatedBallotRun, oe.Run.ID)
	if err != nil {
		return 0, nil, err
	}
	if err := recordAdminAction(c, oe.Scheme.EnvironmentTag, now.at, adminAction{
		Action: string(idempotency.ActionCommitSeed), SchemeID: oe.Scheme.ID,
		TargetType: store.RelatedBallotRun, TargetID: oe.Run.ID, Key: k[:], OutboxID: outboxID,
		Params: map[string]any{"commitment": cer.Commitment.Hex()},
	}); err != nil {
		return 0, nil, err
	}

	cer.Stage, cer.Attempt, cer.TargetBlock = ballotrun.StageSeedCommitted, 1, 0
	oe.Run.Commitment = cer.Commitment
	return http.StatusAccepted, ceremonyToWire(oe.Offer.ID, oe.Run, cer), nil
}

// handleRevealSeed serves POST /v1/admin/offers/{offerId}/ballot/reveal.
func (s *Server) handleRevealSeed(c *writeCtx) (int, any, error) {
	oe, now, cer, secret, err := s.ceremonyContext(c)
	if err != nil {
		return 0, nil, err
	}
	if s.deps.Chain == nil {
		return 0, nil, errors.New("httpapi: no chain reader is configured")
	}
	if !cer.CommitmentAnchored {
		return 0, nil, conflict(CodeAnchorNotConfirmed, fmt.Sprintf(
			"the commitment needs %d confirmations before the secret may be revealed; revealing earlier would let "+
				"the operator claim a different secret was always intended", offer.MinAnchorConfirmations))
	}
	if cer.TargetBlock == 0 {
		// Committed and confirmed, but a recommit is still in flight: the contract names the new target only
		// once that call is mined.
		return 0, nil, conflict(CodeAnchorNotConfirmed, fmt.Sprintf(
			"the latest recommit needs %d confirmations before there is a target block to reveal against",
			offer.MinAnchorConfirmations))
	}

	head, err := s.deps.Chain.Head(c.ctx())
	if err != nil {
		return 0, nil, fmt.Errorf("reading the chain head: %w", err)
	}
	if err := cer.CanReveal(head); err != nil {
		return 0, nil, ceremonyErr(err)
	}
	targetHash, err := s.deps.Chain.BlockHash(c.ctx(), cer.TargetBlock)
	if err != nil {
		return 0, nil, fmt.Errorf("reading the target block hash: %w", err)
	}
	finalSeed, err := cer.FinalSeed(secret, targetHash)
	if err != nil {
		return 0, nil, ceremonyErr(err)
	}
	payload, err := cer.RevealPayload(secret)
	if err != nil {
		return 0, nil, ceremonyErr(err)
	}

	ev := &oe.Evidence
	ev.Ceremony.TargetBlock = cer.TargetBlock
	ev.Ceremony.TargetBlockHash = targetHash
	if err := offer.Guard(oe.Offer.Status, offer.StatusSeedRevealed, *ev); err != nil {
		return 0, nil, err
	}

	commit := oe.Latest(store.FnCommitSeed)
	if err := store.Reveal(c.ctx(), c.tx, oe.Run.ID, commit.TxHash, cer.TargetBlock, secret, targetHash, finalSeed); err != nil {
		return 0, nil, err
	}
	if err := store.AdvanceOffer(c.ctx(), c.tx, oe.Offer.ID, offer.StatusSeedCommitted, offer.StatusSeedRevealed); err != nil {
		return 0, nil, err
	}

	k, err := idempotency.Derive(cer.RevealIdempotencyInput())
	if err != nil {
		return 0, nil, err
	}
	outboxID, err := enqueue(c, oe.Scheme, oe.Scheme.BallotAddress, store.FnRevealSeed, payload, k, store.RelatedBallotRun, oe.Run.ID)
	if err != nil {
		return 0, nil, err
	}
	if err := recordAdminAction(c, oe.Scheme.EnvironmentTag, now.at, adminAction{
		Action: string(idempotency.ActionRevealSeed), SchemeID: oe.Scheme.ID,
		TargetType: store.RelatedBallotRun, TargetID: oe.Run.ID, Key: k[:], OutboxID: outboxID,
		Params: map[string]any{"targetBlock": cer.TargetBlock, "finalSeed": finalSeed.Hex()},
	}); err != nil {
		return 0, nil, err
	}

	cer.Stage = ballotrun.StageSeedRevealed
	oe.Run.SeedPlaintext, oe.Run.TargetBlockHash, oe.Run.FinalSeed = secret, targetHash, finalSeed
	return http.StatusAccepted, ceremonyToWire(oe.Offer.ID, oe.Run, cer), nil
}

// handleRecommitSeed serves POST /v1/admin/offers/{offerId}/ballot/recommit.
//
// Only once the reveal window has lapsed. Recommitting while a reveal is still possible would let an operator
// skip a draw they could already compute, which is the one thing the ceremony exists to prevent.
func (s *Server) handleRecommitSeed(c *writeCtx) (int, any, error) {
	oe, now, cer, _, err := s.ceremonyContext(c)
	if err != nil {
		return 0, nil, err
	}
	if s.deps.Chain == nil {
		return 0, nil, errors.New("httpapi: no chain reader is configured")
	}
	if cer.TargetBlock == 0 {
		return 0, nil, conflict(CodeAnchorNotConfirmed,
			"the current commit window has not confirmed, so it has no target block and cannot have lapsed")
	}
	head, err := s.deps.Chain.Head(c.ctx())
	if err != nil {
		return 0, nil, fmt.Errorf("reading the chain head: %w", err)
	}
	if err := cer.CanRecommit(head); err != nil {
		return 0, nil, ceremonyErr(err)
	}

	k, err := idempotency.Derive(cer.RecommitIdempotencyInput())
	if err != nil {
		return 0, nil, err
	}
	if err := store.Recommit(c.ctx(), c.tx, oe.Run.ID, cer.Attempt, cer.Attempt+1); err != nil {
		return 0, nil, err
	}
	payload, err := cer.RecommitPayload()
	if err != nil {
		return 0, nil, err
	}
	outboxID, err := enqueue(c, oe.Scheme, oe.Scheme.BallotAddress, store.FnRecommitSeed, payload, k, store.RelatedBallotRun, oe.Run.ID)
	if err != nil {
		return 0, nil, err
	}
	if err := recordAdminAction(c, oe.Scheme.EnvironmentTag, now.at, adminAction{
		Action: string(idempotency.ActionRecommitSeed), SchemeID: oe.Scheme.ID,
		TargetType: store.RelatedBallotRun, TargetID: oe.Run.ID, Key: k[:], OutboxID: outboxID,
		Params: map[string]any{"attempt": cer.Attempt + 1, "lapsedTarget": cer.TargetBlock},
	}); err != nil {
		return 0, nil, err
	}

	cer.Attempt++
	cer.TargetBlock = 0
	return http.StatusAccepted, ceremonyToWire(oe.Offer.ID, oe.Run, cer), nil
}

// ceremonyErr maps the ceremony's own refusals onto the contract's codes.
//
// The ballotrun sentinels are not in the classification table because that package's errors are mostly about
// ordering, and the contract has a code for exactly that. Mapped here so each keeps its explanation.
func ceremonyErr(err error) error {
	switch {
	case errors.Is(err, ballotrun.ErrWrongStage), errors.Is(err, ballotrun.ErrBookNotAnchored):
		return &statusError{status: http.StatusConflict, code: CodeCeremonyOrder, msg: err.Error()}
	case errors.Is(err, ballotrun.ErrTargetNotReached), errors.Is(err, ballotrun.ErrWindowExpired),
		errors.Is(err, ballotrun.ErrWindowStillOpen), errors.Is(err, ballotrun.ErrAttemptsExhausted),
		errors.Is(err, ballotrun.ErrBlockhashLost):
		return &statusError{status: http.StatusConflict, code: CodePreconditionFailed, msg: err.Error()}
	case errors.Is(err, ballotrun.ErrCommitmentMismatch), errors.Is(err, ballotrun.ErrRootMismatch):
		// Not the caller's fault and not fixable by retrying: the pepper changed or the book moved. The
		// message names the likely cause and is logged in full.
		return &statusError{status: http.StatusConflict, code: CodePreconditionFailed, msg: err.Error()}
	}
	return err
}

// --- drawBallot -----------------------------------------------------------------------------------------

type wireOutcomeCounts struct {
	Full         int `json:"full"`
	Partial      int `json:"partial"`
	NilBallot    int `json:"nilBallot"`
	NilTechnical int `json:"nilTechnical"`
}

type wireDrawResult struct {
	OfferID           string            `json:"offerId"`
	FinalSeed         string            `json:"finalSeed"`
	BidbookRoot       string            `json:"bidbookRoot"`
	ResultRoot        string            `json:"resultRoot"`
	UnitsAllotted     uint32            `json:"unitsAllotted"`
	DistinctAllottees uint32            `json:"distinctAllottees"`
	AlgoVersion       uint32            `json:"algoVersion"`
	OutcomeCounts     wireOutcomeCounts `json:"outcomeCounts"`
	Document          *wirePublished    `json:"document,omitempty"`
}

// handleDrawBallot serves POST /v1/admin/offers/{offerId}/ballot/draw.
//
// Waits for revealSeed to confirm. The contract recomputes the final seed from the block hash it reads itself,
// so drawing before the reveal is on-chain would publish a result the chain has not yet agreed the seed for.
//
// The book is rebuilt from the rows and must reproduce the anchored root before anything is drawn. A book that
// no longer matches its anchor is refused, because a draw over it would allocate units against bids the chain
// never saw.
func (s *Server) handleDrawBallot(c *writeCtx) (int, any, error) {
	oe, now, cer, secret, err := s.ceremonyContext(c)
	if err != nil {
		return 0, nil, err
	}
	if !oe.Latest(store.FnRevealSeed).IsConfirmed() {
		return 0, nil, conflict(CodeAnchorNotConfirmed, fmt.Sprintf(
			"revealSeed needs %d confirmations before the draw; until then the chain has not agreed the seed",
			offer.MinAnchorConfirmations))
	}

	book, err := frozenBook(c.ctx(), c.tx, oe, oe.Run.SnapshotAt, true)
	if err != nil {
		return 0, nil, err
	}
	if book.MerkleRoot != oe.Run.BidbookRoot {
		return 0, nil, fmt.Errorf("the rebuilt book root %s does not match the anchored root %s",
			book.MerkleRoot.Hex(), oe.Run.BidbookRoot.Hex())
	}

	draw, err := ballotrun.Draw(ballotrun.RunInput{
		Book: book, Ceremony: cer, Secret: secret, TargetBlockHash: oe.Run.TargetBlockHash,
		Params: oe.Offer.Terms.BallotParams(), DrawnAt: now.at,
	})
	if err != nil {
		return 0, nil, ceremonyErr(err)
	}
	if err := draw.Validate(); err != nil {
		return 0, nil, err
	}
	if draw.Result.FinalSeed != oe.Run.FinalSeed {
		return 0, nil, fmt.Errorf("the draw was seeded with %s but the recorded final seed is %s",
			draw.Result.FinalSeed.Hex(), oe.Run.FinalSeed.Hex())
	}

	raw, err := draw.Document()
	if err != nil {
		return 0, nil, err
	}
	pin, err := s.pinDocument(c, oe.Scheme, ipfsguard.DocAllotmentFile, "ALLOTMENT_FILE", raw, oe.Run.ID)
	if err != nil {
		return 0, nil, err
	}
	req, err := draw.AnchorRequest(pin)
	if err != nil {
		return 0, nil, err
	}

	if err := store.RecordDraw(c.ctx(), c.tx, oe.Run.ID, draw, req.Root, merkle.Hash(req.CIDDigest), now.at); err != nil {
		return 0, nil, err
	}

	ev := &oe.Evidence
	ev.Ceremony.SecretRevealed = true
	ev.Ceremony.FinalSeed = draw.Result.FinalSeed
	ev.Ceremony.ResultRoot = draw.Result.ResultRoot
	ev.AllocationsRecorded = len(draw.Lines)
	if err := offer.Guard(oe.Offer.Status, offer.StatusBallotDrawn, *ev); err != nil {
		return 0, nil, err
	}
	if err := store.AdvanceOffer(c.ctx(), c.tx, oe.Offer.ID, offer.StatusSeedRevealed, offer.StatusBallotDrawn); err != nil {
		return 0, nil, err
	}

	payload, err := req.OutboxPayload()
	if err != nil {
		return 0, nil, err
	}
	anchorKey, err := idempotency.Derive(req.AnchorIdempotencyInput(oe.Scheme.ID, oe.Offer.ID))
	if err != nil {
		return 0, nil, err
	}
	outboxID, err := enqueue(c, oe.Scheme, oe.Scheme.BallotAddress, store.FnAnchorBallotResult, payload, anchorKey,
		store.RelatedBallotRun, oe.Run.ID)
	if err != nil {
		return 0, nil, err
	}
	runKey, err := idempotency.Derive(draw.RunIdempotencyInput())
	if err != nil {
		return 0, nil, err
	}
	if err := recordAdminAction(c, oe.Scheme.EnvironmentTag, now.at, adminAction{
		Action: string(idempotency.ActionRunBallot), SchemeID: oe.Scheme.ID,
		TargetType: store.RelatedBallotRun, TargetID: oe.Run.ID, Key: runKey[:], OutboxID: outboxID,
		Params: map[string]any{"resultRoot": draw.Result.ResultRoot.Hex(), "unitsAllotted": draw.Result.UnitsAllotted},
	}); err != nil {
		return 0, nil, err
	}

	var counts wireOutcomeCounts
	for _, l := range draw.Lines {
		switch l.Outcome {
		case ballot.OutcomeFull:
			counts.Full++
		case ballot.OutcomePartial:
			counts.Partial++
		case ballot.OutcomeNilBallot:
			counts.NilBallot++
		default:
			counts.NilTechnical++
		}
	}
	return http.StatusOK, wireDrawResult{
		OfferID: oe.Offer.ID, FinalSeed: draw.Result.FinalSeed.Hex(), BidbookRoot: draw.Result.BidbookRoot.Hex(),
		ResultRoot: draw.Result.ResultRoot.Hex(), UnitsAllotted: draw.Result.UnitsAllotted,
		DistinctAllottees: draw.Result.DistinctAllottees, AlgoVersion: draw.Result.AlgoVersion,
		OutcomeCounts: counts, Document: publishedToWire("ALLOTMENT_FILE", pin),
	}, nil
}
