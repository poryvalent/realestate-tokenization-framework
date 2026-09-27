package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/acresync/orchestrator/internal/merkle"
	"github.com/acresync/orchestrator/internal/offer"
)

// Everything an offer transition is judged against, gathered from where it actually lives.
//
// offer.Guard is pure and takes an offer.Evidence. That is only as good as the evidence handed to it, and an
// Evidence assembled from fewer sources than the guard reads produces a confident "yes" on facts nobody
// checked. So this file reads every source the guard consults: the offer, its bids and their funds blocks, the
// ballot run, the IPFS pins, the allocation rows, the chain outbox and the operator's pause record.
//
// # Anchors are read from the outbox, never assumed
//
// A guard that requires an anchor to be confirmed is answered by the chain_outbox row that carries it. Status
// and confirmation depth come from that row and nowhere else. Nothing in this file treats "queued" as "done".
//
// # Linking rows to what they anchor
//
// Outbox entries written by the API carry related_entity_type and related_entity_id. Ceremony calls point at
// the ballot run, settlement calls at the offer. That link is the only way to find the anchor for a given
// artefact, which is why every enqueue in the write path sets it.

// Related entity types written on outbox entries and pins.
const (
	RelatedBallotRun = "ballot_run"
	RelatedOffer     = "offer"
)

// Outbox function names, as the contracts spell them.
const (
	FnAnchorBidbook        = "anchorBidbook"
	FnCommitSeed           = "commitSeed"
	FnRevealSeed           = "revealSeed"
	FnRecommitSeed         = "recommitSeed"
	FnAnchorBallotResult   = "anchorBallotResult"
	FnBeginSettlement      = "beginSettlement"
	FnSettleBatch          = "settleBatch"
	FnRecordIMSubscription = "recordImSubscription"
	FnFinaliseSettlement   = "finaliseSettlement"
)

// inBookStatuses are the bid statuses a bid can reach only by having been admitted to the book.
var inBookStatuses = []string{
	"IN_BOOK", "ALLOTTED_FULL", "ALLOTTED_PARTIAL", "REJECTED_BALLOT",
	"DEBIT_INSTRUCTED", "FUNDS_DEBITED", "UNITS_CREDITED", "ANCHORED",
}

// pendingStatuses are bids whose validation or funds block has not resolved.
var pendingStatuses = []string{"SUBMITTED", "BLOCK_REQUESTED", "VALIDATED", "FUNDS_BLOCKED"}

// Anchor is one outbox row, with the block it landed in.
type Anchor struct {
	offer.AnchorRef
	FunctionName string
	BlockNumber  *int64
	Payload      []byte
}

// BallotRun is the ballot_runs row.
type BallotRun struct {
	ID              string
	Status          string
	BidbookRoot     merkle.Hash
	BidbookCID      merkle.Hash
	SnapshotAt      time.Time
	LeafCount       int32
	TotalUnitsBid   int64
	DistinctBidders int32
	OversubNum      int64
	OversubDen      int64
	Commitment      merkle.Hash
	TargetBlock     *int64
	Attempt         int16
	SeedPlaintext   merkle.Hash
	TargetBlockHash merkle.Hash
	FinalSeed       merkle.Hash
	CommitmentTx    string
	ResultRoot      merkle.Hash
	ResultCID       merkle.Hash
	AlgoVersion     int32
}

// OfferEvidence is an offer with everything its guards need.
type OfferEvidence struct {
	Offer  Offer
	Scheme Scheme

	// Evidence is ready for offer.Guard except Now and AbortReason, which the caller supplies: one is the
	// scheme's business time and the other comes from the request.
	Evidence offer.Evidence

	// Run is nil until the bid book is anchored.
	Run *BallotRun

	// Anchors are the offer's outbox rows, newest first, keyed by function name. The first entry for a
	// name is the latest attempt.
	Anchors map[string][]Anchor
}

// Latest returns the newest outbox row for a function, or nil.
func (e *OfferEvidence) Latest(fn string) *Anchor {
	if list := e.Anchors[fn]; len(list) > 0 {
		return &list[0]
	}
	return nil
}

// Confirmed returns every confirmed outbox row for a function, oldest first.
func (e *OfferEvidence) Confirmed(fn string) []Anchor {
	var out []Anchor
	list := e.Anchors[fn]
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].IsConfirmed() {
			out = append(out, list[i])
		}
	}
	return out
}

// LoadOfferEvidence gathers everything offer.Guard reads for one offer.
func LoadOfferEvidence(ctx context.Context, q Querier, offerID string) (*OfferEvidence, error) {
	o, err := NewOffers(q).ByID(ctx, offerID)
	if err != nil {
		return nil, err
	}
	sc, err := NewSchemes(q).ByID(ctx, o.SchemeID)
	if err != nil {
		return nil, err
	}

	out := &OfferEvidence{Offer: o, Scheme: sc, Anchors: map[string][]Anchor{}}
	ev := &out.Evidence
	ev.Terms = o.Terms

	if ev.Paused, err = schemePaused(ctx, q, sc.ID); err != nil {
		return nil, err
	}
	if ev.Book, err = bookState(ctx, q, offerID); err != nil {
		return nil, err
	}

	run, err := ballotRunFor(ctx, q, offerID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if run != nil {
		out.Run = run
		ev.Ceremony.BidbookRoot = run.BidbookRoot
		ev.Ceremony.SeedCommitment = run.Commitment
		ev.Ceremony.Attempt = int(run.Attempt)
		ev.Ceremony.TargetBlockHash = run.TargetBlockHash
		ev.Ceremony.SecretRevealed = !run.SeedPlaintext.IsZero()
		ev.Ceremony.FinalSeed = run.FinalSeed
		ev.Ceremony.ResultRoot = run.ResultRoot
		ev.Ceremony.Escalated = run.Status == "ESCALATED"
		if run.TargetBlock != nil {
			ev.Ceremony.TargetBlock = uint64(*run.TargetBlock)
		}

		if err := loadAnchors(ctx, q, RelatedBallotRun, run.ID, out.Anchors); err != nil {
			return nil, err
		}
		ev.Ceremony.BidbookAnchor = anchorRef(out.Latest(FnAnchorBidbook))
		ev.Ceremony.CommitmentAnchor = anchorRef(out.Latest(FnCommitSeed))
		ev.Ceremony.ResultAnchor = anchorRef(out.Latest(FnAnchorBallotResult))

		if ev.BidbookPinned, err = pinned(ctx, q, run.ID, "BIDBOOK"); err != nil {
			return nil, err
		}
		if ev.AllotmentFilePinned, err = pinned(ctx, q, run.ID, "ALLOTMENT_FILE"); err != nil {
			return nil, err
		}
		if err := q.QueryRow(ctx,
			`SELECT count(*) FROM allocations WHERE ballot_run_id = $1`, run.ID).
			Scan(&ev.AllocationsRecorded); err != nil {
			return nil, err
		}
	}

	if err := loadAnchors(ctx, q, RelatedOffer, offerID, out.Anchors); err != nil {
		return nil, err
	}
	if err := settlementEvidence(out); err != nil {
		return nil, err
	}
	return out, nil
}

func anchorRef(a *Anchor) *offer.AnchorRef {
	if a == nil {
		return nil
	}
	ref := a.AnchorRef
	return &ref
}

// schemePaused reads the operator's own pause record.
//
// The contract is the authority on whether the scheme is paused, and a paused contract reverts regardless of
// what this says. The admin_actions trail is the operator's record of having paused it, which is what lets the
// guard refuse before a transaction is queued that would only revert.
func schemePaused(ctx context.Context, q Querier, schemeID string) (bool, error) {
	var action string
	err := q.QueryRow(ctx, `
		SELECT action::text FROM admin_actions
		 WHERE scheme_id = $1 AND action::text IN ('PAUSE', 'UNPAUSE')
		 ORDER BY created_at DESC, id DESC
		 LIMIT 1`, schemeID).Scan(&action)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return action == "PAUSE", nil
}

func bookState(ctx context.Context, q Querier, offerID string) (offer.BookState, error) {
	var (
		b                         offer.BookState
		unitsBid, distinctBidders int64
	)
	err := q.QueryRow(ctx, `
		SELECT
			count(*),
			count(*) FILTER (WHERE b.status::text = ANY($2)),
			count(*) FILTER (WHERE b.status::text = ANY($2) AND a.block_status::text IN ('BLOCKED', 'DEBITED')),
			coalesce(sum(b.units_bid) FILTER (WHERE b.status::text = ANY($2)), 0),
			count(DISTINCT b.investor_id) FILTER (WHERE b.status::text = ANY($2)),
			count(*) FILTER (WHERE b.status::text = ANY($3))
		FROM bids b
		LEFT JOIN asba_blocks a ON a.bid_id = b.id
		WHERE b.offer_id = $1`,
		offerID, inBookStatuses, pendingStatuses).Scan(
		&b.BidCount, &b.InBookCount, &b.FundsBlockedCount, &unitsBid, &distinctBidders, &b.PendingValidation)
	if err != nil {
		return offer.BookState{}, err
	}
	b.UnitsBid = uint32(unitsBid)
	b.DistinctBidders = uint32(distinctBidders)
	return b, nil
}

func ballotRunFor(ctx context.Context, q Querier, offerID string) (*BallotRun, error) {
	var (
		r                                                     BallotRun
		root, cid, commit, plain, hash, seed, resRoot, resCID []byte
		commitTx                                              *string
	)
	err := q.QueryRow(ctx, `
		SELECT id, status::text, bidbook_merkle_root, bidbook_cid_digest, bidbook_snapshot_at,
		       bid_leaf_count, total_units_bid, distinct_bidders, oversubscription_num, oversubscription_den,
		       seed_commitment, target_block, attempt, seed_plaintext, target_block_hash, final_seed,
		       commitment_anchored_tx, result_merkle_root, result_cid_digest, algo_version
		  FROM ballot_runs WHERE offer_id = $1`, offerID).Scan(
		&r.ID, &r.Status, &root, &cid, &r.SnapshotAt,
		&r.LeafCount, &r.TotalUnitsBid, &r.DistinctBidders, &r.OversubNum, &r.OversubDen,
		&commit, &r.TargetBlock, &r.Attempt, &plain, &hash, &seed,
		&commitTx, &resRoot, &resCID, &r.AlgoVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	copy(r.BidbookRoot[:], root)
	copy(r.BidbookCID[:], cid)
	copy(r.Commitment[:], commit)
	copy(r.SeedPlaintext[:], plain)
	copy(r.TargetBlockHash[:], hash)
	copy(r.FinalSeed[:], seed)
	copy(r.ResultRoot[:], resRoot)
	copy(r.ResultCID[:], resCID)
	if commitTx != nil {
		r.CommitmentTx = *commitTx
	}
	r.SnapshotAt = r.SnapshotAt.UTC()
	return &r, nil
}

// loadAnchors appends an entity's outbox rows, newest first, grouped by function.
func loadAnchors(ctx context.Context, q Querier, entityType, entityID string, into map[string][]Anchor) error {
	rows, err := q.Query(ctx, `
		SELECT id, function_name, status::text, confirmations, coalesce(tx_hash, ''), block_number,
		       payload_json::text
		  FROM chain_outbox
		 WHERE related_entity_type = $1 AND related_entity_id = $2
		 ORDER BY created_at DESC, id DESC`, entityType, entityID)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			a       Anchor
			payload string
			confs   int32
		)
		if err := rows.Scan(&a.OutboxID, &a.FunctionName, &a.Status, &confs, &a.TxHash,
			&a.BlockNumber, &payload); err != nil {
			return err
		}
		a.Confirmations = int(confs)
		a.Payload = []byte(payload)
		into[a.FunctionName] = append(into[a.FunctionName], a)
	}
	return rows.Err()
}

func pinned(ctx context.Context, q Querier, runID, docType string) (bool, error) {
	var n int
	err := q.QueryRow(ctx, `
		SELECT count(*) FROM ipfs_pins
		 WHERE related_entity_type = $1 AND related_entity_id = $2 AND doc_type::text = $3`,
		RelatedBallotRun, runID, docType).Scan(&n)
	return n > 0, err
}

// settlementEvidence derives the settlement cursor from confirmed chain calls.
//
// Only confirmed entries count. A batch that is queued, or broadcast and still confirming, has not credited
// anybody as far as the chain is concerned, and counting it would let finalisation be queued against a cursor
// the contract has not reached.
func settlementEvidence(e *OfferEvidence) error {
	s := &e.Evidence.Settlement

	if begin := e.Confirmed(FnBeginSettlement); len(begin) > 0 {
		var p struct {
			ExpectedHolders   uint32 `json:"expectedHolders"`
			ExpectedUnits     uint32 `json:"expectedUnits"`
			AllotmentFileHash []byte `json:"allotmentFileHash"`
		}
		if err := json.Unmarshal(begin[0].Payload, &p); err != nil {
			return fmt.Errorf("reading the confirmed beginSettlement payload: %w", err)
		}
		s.Begun = true
		s.ExpectedHolders = p.ExpectedHolders
		s.ExpectedUnits = p.ExpectedUnits
		copy(s.AllotmentFileHash[:], p.AllotmentFileHash)
	}

	for _, b := range e.Confirmed(FnSettleBatch) {
		var p struct {
			Holders []string `json:"holders"`
			Units   []uint32 `json:"units"`
		}
		if err := json.Unmarshal(b.Payload, &p); err != nil {
			return fmt.Errorf("reading a confirmed settleBatch payload: %w", err)
		}
		s.CreditedHolders += uint32(len(p.Holders))
		for _, u := range p.Units {
			s.CreditedUnits += u
		}
	}

	s.IMUnitsRecorded = len(e.Confirmed(FnRecordIMSubscription)) > 0
	return nil
}
