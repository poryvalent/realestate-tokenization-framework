package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/acresync/orchestrator/internal/money"
	"github.com/acresync/orchestrator/internal/offer"
)

// Offer is a row of the offers table, carrying the domain's own types.
//
// Terms is offer.Terms rather than a copy of those columns: the contract documents OfferTerms as mirroring
// offer.Terms, and going through the domain type means the two cannot drift apart without the compiler
// noticing. Status is offer.Status, validated on read.
type Offer struct {
	ID        string
	SchemeID  string
	OfferType string
	Status    offer.Status
	Terms     offer.Terms
	CreatedAt time.Time
}

// Subscription is the aggregate state of bidding on an offer.
//
// Separate from Offer because it is a query over the bids and asba_blocks tables rather than columns, and
// because the contract marks it as present only once bidding has begun. Loading it with every offer read
// would make the cheapest endpoint in the API do three joins.
type Subscription struct {
	BidCount          int64
	DistinctBidders   int64
	UnitsBid          int64
	FundsBlockedCount int64
}

// Oversubscribed reports whether demand exceeded the units on offer.
//
// Expressed as a comparison rather than a ratio, because a ratio would need rounding and the only question
// anybody asks of it is which side of one it falls on.
func (s Subscription) Oversubscribed(unitsOnOffer int64) bool {
	return s.UnitsBid > unitsOnOffer
}

// Offers reads the offers table.
type Offers struct {
	q Querier
}

// NewOffers binds a store to a pool or transaction.
func NewOffers(q Querier) Offers { return Offers{q: q} }

const offerColumns = `
	id, scheme_id, offer_type::text, status::text,
	units_on_offer, min_bid_units, max_bid_units,
	min_subscription_units, min_distinct_holders,
	price_band_lower_paise, price_band_upper_paise,
	opens_at, closes_at, allotment_due_at,
	created_at`

func scanOffer(row pgx.Row) (Offer, error) {
	var (
		o                            Offer
		statusText                   string
		unitsOnOffer, minBid, maxBid int32
		minSubscription, minHolders  int32
		bandLower, bandUpper         int64
	)

	err := row.Scan(
		&o.ID, &o.SchemeID, &o.OfferType, &statusText,
		&unitsOnOffer, &minBid, &maxBid,
		&minSubscription, &minHolders,
		&bandLower, &bandUpper,
		&o.Terms.OpensAt, &o.Terms.ClosesAt, &o.Terms.AllotmentDueAt,
		&o.CreatedAt,
	)
	if err != nil {
		return Offer{}, err
	}

	// Validated rather than cast. An offer served with a status this build does not define would be read by
	// every caller as whatever their default branch does, which for a lifecycle status means showing an
	// offer as open when it is not.
	o.Status = offer.Status(statusText)
	if !o.Status.Valid() {
		return Offer{}, unknownEnum("offers.status", statusText)
	}

	o.Terms.UnitsOnOffer = uint32(unitsOnOffer)
	o.Terms.MinBidUnits = uint32(minBid)
	o.Terms.MaxBidUnits = uint32(maxBid)
	o.Terms.MinSubscriptionUnits = uint32(minSubscription)
	o.Terms.MinDistinctHolders = uint32(minHolders)
	o.Terms.PriceBandLowerPaise = money.Paise(bandLower)
	o.Terms.PriceBandUpperPaise = money.Paise(bandUpper)

	// The table stores timestamptz, which pgx reads in the session's zone. Everything in this system is
	// UTC, and a time carrying a local zone would serialise with an offset that the contract forbids.
	o.Terms.OpensAt = o.Terms.OpensAt.UTC()
	o.Terms.ClosesAt = o.Terms.ClosesAt.UTC()
	o.Terms.AllotmentDueAt = o.Terms.AllotmentDueAt.UTC()
	o.CreatedAt = o.CreatedAt.UTC()

	return o, nil
}

// ByID loads one offer.
func (s Offers) ByID(ctx context.Context, id string) (Offer, error) {
	const q = `SELECT ` + offerColumns + ` FROM offers WHERE id = $1`

	o, err := scanOffer(s.q.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Offer{}, ErrNotFound
	}
	if err != nil {
		return Offer{}, err
	}
	return o, nil
}

// ForScheme lists a scheme's offers, newest first.
func (s Offers) ForScheme(ctx context.Context, schemeID string, p Pagination) ([]Offer, error) {
	p = p.normalise()

	// Ordered by opens_at then id. The id breaks the tie, so two offers opening at the same instant have a
	// deterministic order and a page boundary between them neither repeats nor skips.
	const q = `
		SELECT ` + offerColumns + `
		FROM offers
		WHERE scheme_id = $1
		ORDER BY opens_at DESC, id DESC
		LIMIT $2`

	rows, err := s.q.Query(ctx, q, schemeID, p.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Offer
	for rows.Next() {
		o, err := scanOffer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// SubscriptionFor aggregates bidding on an offer.
//
// Counts funded blocks by joining asba_blocks rather than trusting a column on bids, because a bid is only
// backed once the block is confirmed and the two can legitimately disagree while a block is in flight.
// BLOCKED and DEBITED both count as funded: the money is committed in either case, and a debited block is
// simply one where it has already moved.
//
// bidCount and distinctBidders are both selected even though `UNIQUE (offer_id, investor_id)` makes them
// equal by construction. The contract publishes both, and computing the second rather than copying the
// first means that if that constraint is ever relaxed the numbers start differing instead of one of them
// silently becoming wrong.
func (s Offers) SubscriptionFor(ctx context.Context, offerID string) (Subscription, error) {
	const q = `
		SELECT
			count(*)                                        AS bid_count,
			count(DISTINCT b.investor_id)                   AS distinct_bidders,
			coalesce(sum(b.units_bid), 0)                   AS units_bid,
			count(*) FILTER (
				WHERE a.block_status::text IN ('BLOCKED', 'DEBITED')
			)                                               AS funds_blocked
		FROM bids b
		LEFT JOIN asba_blocks a ON a.bid_id = b.id
		WHERE b.offer_id = $1`

	var sub Subscription
	err := s.q.QueryRow(ctx, q, offerID).Scan(
		&sub.BidCount, &sub.DistinctBidders, &sub.UnitsBid, &sub.FundsBlockedCount,
	)
	if err != nil {
		return Subscription{}, err
	}
	return sub, nil
}
