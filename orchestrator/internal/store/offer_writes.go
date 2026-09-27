package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/acresync/orchestrator/internal/asba"
	"github.com/acresync/orchestrator/internal/bidbook"
	"github.com/acresync/orchestrator/internal/merkle"
	"github.com/acresync/orchestrator/internal/money"
	"github.com/acresync/orchestrator/internal/offer"
)

// Writes for the primary market.
//
// None of these decides anything. Each is called after offer.Guard, ballotrun.Ceremony or settlement.Plan has
// already said yes, and records that decision. Where a write races another, it is a compare-and-set against the
// state the decision was made on, so a decision taken on stale evidence fails instead of landing.

// InsertOffer records a new offer in CONFIGURED.
func InsertOffer(ctx context.Context, w Writer, schemeID, offerType string, t offer.Terms, key []byte) (string, error) {
	var id string
	err := w.QueryRow(ctx, `
		INSERT INTO offers (
			scheme_id, offer_type, price_band_lower_paise, price_band_upper_paise,
			units_on_offer, min_bid_units, max_bid_units, min_subscription_units,
			min_distinct_holders, opens_at, closes_at, allotment_due_at, status, idempotency_key
		) VALUES ($1, $2::offer_type, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'CONFIGURED', $13)
		RETURNING id`,
		schemeID, offerType,
		int64(t.PriceBandLowerPaise), int64(t.PriceBandUpperPaise),
		t.UnitsOnOffer, t.MinBidUnits, t.MaxBidUnits, t.MinSubscriptionUnits, t.MinDistinctHolders,
		t.OpensAt.UTC(), t.ClosesAt.UTC(), t.AllotmentDueAt.UTC(), key).Scan(&id)
	return id, err
}

// AdvanceOffer moves an offer from one status to another, only if it is still in the first.
func AdvanceOffer(ctx context.Context, w Writer, offerID string, from, to offer.Status) error {
	tag, err := w.Exec(ctx, `UPDATE offers SET status = $3 WHERE id = $1 AND status = $2`,
		offerID, string(from), string(to))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

// AdmitFundedBids moves every funded bid into the book when the offer closes.
//
// A bid whose block is still outstanding stays where it is, which is exactly what makes freezing refuse: a
// book whose membership can still change must not have a root computed over it.
func AdmitFundedBids(ctx context.Context, w Writer, offerID string) (int64, error) {
	tag, err := w.Exec(ctx, `
		UPDATE bids b SET status = 'IN_BOOK', updated_at = now()
		  FROM asba_blocks a
		 WHERE a.bid_id = b.id AND b.offer_id = $1
		   AND b.status = 'FUNDS_BLOCKED' AND a.block_status = 'BLOCKED'`, offerID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// BookEntries reads the in-book bids with their funds blocks, as the bid book is built from them.
//
// Read back from the rows rather than reused from anything held in memory, so the root is computed over what
// was actually persisted, including anything a column type quietly changed on the way in.
func BookEntries(ctx context.Context, q Querier, offerID string) ([]bidbook.Entry, error) {
	rows, err := q.Query(ctx, `
		SELECT b.id, b.investor_id, b.bid_reference, b.investor_anchor_hash,
		       b.units_bid, b.price_per_unit_paise,
		       a.requested_amount_paise, coalesce(a.blocked_amount_paise, 0), a.block_status::text,
		       a.requested_at, a.blocked_at, coalesce(a.bank_ref, '')
		  FROM bids b
		  JOIN asba_blocks a ON a.bid_id = b.id
		 WHERE b.offer_id = $1 AND b.status::text = ANY($2)
		 ORDER BY b.bid_reference`, offerID, inBookStatuses)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []bidbook.Entry
	for rows.Next() {
		var (
			bidID, investorID, ref, status, bankRef string
			anchorBytes                             []byte
			units                                   int32
			price, requested, blocked               int64
			requestedAt                             time.Time
			blockedAt                               *time.Time
		)
		if err := rows.Scan(&bidID, &investorID, &ref, &anchorBytes, &units, &price,
			&requested, &blocked, &status, &requestedAt, &blockedAt, &bankRef); err != nil {
			return nil, err
		}

		var anchor merkle.Hash
		copy(anchor[:], anchorBytes)

		out = append(out, bidbook.Entry{
			BidID:             bidID,
			InvestorID:        investorID,
			BidRef:            ref,
			InvestorAnchor:    anchor,
			UnitsBid:          uint32(units),
			PricePerUnitPaise: money.Paise(price),
			Block: &asba.Block{
				BidID:          bidID,
				BankRef:        bankRef,
				Status:         asba.BlockStatus(status),
				RequestedPaise: money.Paise(requested),
				BlockedPaise:   money.Paise(blocked),
				RequestedAt:    requestedAt.UTC(),
				BlockedAt:      utcPtr(blockedAt),
			},
		})
	}
	return out, rows.Err()
}

// InvestorAnchor returns an investor's anchor for a scheme.
//
// The anchor is an HMAC of the investor's identity under a KMS pepper, written at onboarding. It is what goes
// into the public bid book instead of anything that identifies a person.
func InvestorAnchor(ctx context.Context, q Querier, schemeID, investorID string) (merkle.Hash, error) {
	var raw []byte
	err := q.QueryRow(ctx, `
		SELECT anchor_hash FROM investor_anchors
		 WHERE scheme_id = $1 AND investor_id = $2 AND shredded_at IS NULL`,
		schemeID, investorID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return merkle.Hash{}, ErrNotFound
	}
	if err != nil {
		return merkle.Hash{}, err
	}
	var h merkle.Hash
	copy(h[:], raw)
	return h, nil
}

// AccountOwnership reports whether a demat and a bank account both belong to an investor.
//
// Checked in the same query, because the answer the caller needs is "may this investor bid with these
// accounts", and two separate lookups invite answering half of that question.
func AccountOwnership(ctx context.Context, q Querier, investorID, dematID, bankID string) (dematOK, bankOK, asbaEnabled bool, err error) {
	err = q.QueryRow(ctx, `
		SELECT
			EXISTS (SELECT 1 FROM demat_accounts WHERE id = $2 AND investor_id = $1),
			EXISTS (SELECT 1 FROM bank_accounts  WHERE id = $3 AND investor_id = $1),
			coalesce((SELECT asba_enabled FROM bank_accounts WHERE id = $3 AND investor_id = $1), false)`,
		investorID, dematID, bankID).Scan(&dematOK, &bankOK, &asbaEnabled)
	return dematOK, bankOK, asbaEnabled, err
}

// NewBid is a bid about to be recorded.
type NewBid struct {
	OfferID        string
	InvestorID     string
	InvestorAnchor merkle.Hash
	DematID        string
	BankID         string
	Units          uint32
	Price          money.Paise
	BidRef         string
	Key            []byte
}

// InsertBid records a bid in SUBMITTED.
func InsertBid(ctx context.Context, w Writer, b NewBid) (string, time.Time, error) {
	var (
		id string
		at time.Time
	)
	total := int64(b.Units) * int64(b.Price)
	err := w.QueryRow(ctx, `
		INSERT INTO bids (
			offer_id, investor_id, investor_anchor_hash, demat_account_id, bank_account_id,
			units_bid, price_per_unit_paise, total_amount_paise, bid_reference, status, idempotency_key
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'SUBMITTED', $10)
		RETURNING id, submitted_at`,
		b.OfferID, b.InvestorID, b.InvestorAnchor[:], b.DematID, b.BankID,
		b.Units, int64(b.Price), total, b.BidRef, b.Key).Scan(&id, &at)
	return id, at.UTC(), err
}

// RecordBlock writes the bank's answer to a funds block and moves the bid accordingly.
//
// A refused block rejects the bid with ASBA_BLOCK_FAILED rather than leaving it pending. A pending bid blocks
// the freeze for everybody, so a bid that can never be funded has to leave the book's way.
func RecordBlock(ctx context.Context, w Writer, bidID string, blk *asba.Block, key []byte) error {
	var blocked any
	if blk.Status.HoldsFunds() {
		blocked = int64(blk.BlockedPaise)
	}
	var failure any
	if blk.FailureCode != "" {
		failure = blk.FailureCode
	}

	if _, err := w.Exec(ctx, `
		INSERT INTO asba_blocks (
			bid_id, provider, bank_ref, requested_amount_paise, blocked_amount_paise,
			block_status, requested_at, blocked_at, failure_code, idempotency_key
		) VALUES ($1, 'RAZORPAYX_SANDBOX', $2, $3, $4, $5::asba_block_status, $6, $7, $8, $9)`,
		bidID, blk.BankRef, int64(blk.RequestedPaise), blocked, string(blk.Status),
		blk.RequestedAt, blk.BlockedAt, failure, key); err != nil {
		return err
	}

	switch {
	case blk.Status.HoldsFunds():
		_, err := w.Exec(ctx, `UPDATE bids SET status = 'FUNDS_BLOCKED', updated_at = now() WHERE id = $1`, bidID)
		return err
	case blk.Status == asba.StatusFailed:
		_, err := w.Exec(ctx, `
			UPDATE bids SET status = 'REJECTED_TECHNICAL', rejection_reason = 'ASBA_BLOCK_FAILED', updated_at = now()
			 WHERE id = $1`, bidID)
		return err
	default:
		_, err := w.Exec(ctx, `UPDATE bids SET status = 'BLOCK_REQUESTED', updated_at = now() WHERE id = $1`, bidID)
		return err
	}
}
