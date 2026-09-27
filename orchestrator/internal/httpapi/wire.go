package httpapi

import (
	"time"

	"github.com/acresync/orchestrator/internal/money"
	"github.com/acresync/orchestrator/internal/ndcf"
	"github.com/acresync/orchestrator/internal/store"
)

// The wire types in this file are the contract's schemas, spelled in Go.
//
// # Why they are separate from the store's types
//
// A store type is shaped by the table. A wire type is shaped by the published contract. Serving a store
// type directly would tie the API's field names to the column names, so a migration that renamed a column
// would silently change the API, and a JSON tag would become the only thing standing between a schema change
// and every client breaking.
//
// # Money
//
// Every monetary field is an integer number of paise and named with a Paise suffix, as the contract requires.
// money.Paise marshals as a JSON number because its underlying type is int64. Rupees appear nowhere: a
// float rupee amount cannot represent a paise exactly, and a distribution that is short by a paise fails the
// 95% floor by exactly that paise.
//
// # Times
//
// Timestamps are RFC3339 in UTC with a Z. time.Time marshals that way only when its location is UTC, which
// is why the stores normalise on read rather than leaving it to each handler.

// timestamp marshals a time as the contract's Timestamp.
type timestamp = time.Time

// wireScheme is the contract's Scheme.
type wireScheme struct {
	ID                   string         `json:"id"`
	SebiSchemeRef        string         `json:"sebiSchemeRef"`
	Name                 string         `json:"name"`
	AssetValuePaise      money.Paise    `json:"assetValuePaise"`
	UnitPricePaise       money.Paise    `json:"unitPricePaise"`
	TotalUnits           int32          `json:"totalUnits"`
	IMUnits              int32          `json:"imUnits"`
	PublicUnits          int32          `json:"publicUnits"`
	MinPublicHolders     int32          `json:"minPublicHolders"`
	DistributionFloorBps int            `json:"distributionFloorBps"`
	SchemeStatus         string         `json:"schemeStatus"`
	EnvironmentTag       string         `json:"environmentTag"`
	Contracts            *wireContracts `json:"contracts,omitempty"`
}

// wireContracts is the deployed-address block.
//
// A pointer so it is omitted entirely before deployment. An empty object with blank addresses would suggest
// the scheme is on a chain at address zero, which is a worse answer than saying nothing.
type wireContracts struct {
	ChainID int32  `json:"chainId"`
	Roles   string `json:"roles"`
	Ballot  string `json:"ballot"`
	Scheme  string `json:"scheme"`
}

// schemeToWire converts a stored scheme.
//
// distributionFloorBps comes from ndcf.FloorBps, the constant the distribution maths actually applies, not
// from a column. Serving a stored copy would allow the published figure and the enforced one to differ, and
// the published figure is the claim the whole platform rests on.
func schemeToWire(s store.Scheme) wireScheme {
	w := wireScheme{
		ID:                   s.ID,
		SebiSchemeRef:        s.SebiSchemeRef,
		Name:                 s.Name,
		AssetValuePaise:      s.AssetValue,
		UnitPricePaise:       s.UnitPrice,
		TotalUnits:           s.TotalUnits,
		IMUnits:              s.IMUnits,
		PublicUnits:          s.PublicUnits,
		MinPublicHolders:     s.MinPublicHolders,
		DistributionFloorBps: int(ndcf.FloorBps),
		SchemeStatus:         s.Status,
		EnvironmentTag:       s.EnvironmentTag,
	}

	if s.Deployed() {
		w.Contracts = &wireContracts{
			ChainID: s.ChainID,
			Roles:   s.RolesAddress,
			Ballot:  s.BallotAddress,
			Scheme:  s.SchemeAddress,
		}
	}
	return w
}

// wireOfferTerms is the contract's OfferTerms.
type wireOfferTerms struct {
	UnitsOnOffer         uint32      `json:"unitsOnOffer"`
	MinBidUnits          uint32      `json:"minBidUnits"`
	MaxBidUnits          uint32      `json:"maxBidUnits"`
	MinSubscriptionUnits uint32      `json:"minSubscriptionUnits"`
	MinDistinctHolders   uint32      `json:"minDistinctHolders"`
	PriceBandLowerPaise  money.Paise `json:"priceBandLowerPaise"`
	PriceBandUpperPaise  money.Paise `json:"priceBandUpperPaise"`
	OpensAt              timestamp   `json:"opensAt"`
	ClosesAt             timestamp   `json:"closesAt"`
	AllotmentDueAt       timestamp   `json:"allotmentDueAt"`
}

// wireSubscription is the contract's Offer.subscription.
type wireSubscription struct {
	BidCount          int64 `json:"bidCount"`
	DistinctBidders   int64 `json:"distinctBidders"`
	UnitsBid          int64 `json:"unitsBid"`
	FundsBlockedCount int64 `json:"fundsBlockedCount"`

	// The oversubscription ratio is published as a numerator and a denominator rather than a decimal.
	// A ratio of 980 to 475 is exact; 2.0631578947368423 is not, and a client comparing it against 1 to
	// decide whether a ballot is needed should not be doing that on a rounded value.
	OversubscriptionNumerator   int64 `json:"oversubscriptionNumerator"`
	OversubscriptionDenominator int64 `json:"oversubscriptionDenominator"`
}

// wireOffer is the contract's Offer.
type wireOffer struct {
	ID           string            `json:"id"`
	SchemeID     string            `json:"schemeId"`
	OfferType    string            `json:"offerType"`
	Status       string            `json:"status"`
	Terms        wireOfferTerms    `json:"terms"`
	Subscription *wireSubscription `json:"subscription,omitempty"`
	CreatedAt    timestamp         `json:"createdAt"`
}

// offerToWire converts a stored offer, attaching a subscription only once there is one.
//
// The contract documents subscription as "present once bidding has begun". Omitted rather than zeroed,
// because a block of zeroes on an offer that has not opened reads as "nobody bid", which is a statement
// about demand rather than about the offer not having opened.
func offerToWire(o store.Offer, sub *store.Subscription) wireOffer {
	w := wireOffer{
		ID:        o.ID,
		SchemeID:  o.SchemeID,
		OfferType: o.OfferType,
		Status:    string(o.Status),
		Terms: wireOfferTerms{
			UnitsOnOffer:         o.Terms.UnitsOnOffer,
			MinBidUnits:          o.Terms.MinBidUnits,
			MaxBidUnits:          o.Terms.MaxBidUnits,
			MinSubscriptionUnits: o.Terms.MinSubscriptionUnits,
			MinDistinctHolders:   o.Terms.MinDistinctHolders,
			PriceBandLowerPaise:  o.Terms.PriceBandLowerPaise,
			PriceBandUpperPaise:  o.Terms.PriceBandUpperPaise,
			OpensAt:              o.Terms.OpensAt,
			ClosesAt:             o.Terms.ClosesAt,
			AllotmentDueAt:       o.Terms.AllotmentDueAt,
		},
		CreatedAt: o.CreatedAt,
	}

	if sub != nil && sub.BidCount > 0 {
		w.Subscription = &wireSubscription{
			BidCount:                    sub.BidCount,
			DistinctBidders:             sub.DistinctBidders,
			UnitsBid:                    sub.UnitsBid,
			FundsBlockedCount:           sub.FundsBlockedCount,
			OversubscriptionNumerator:   sub.UnitsBid,
			OversubscriptionDenominator: int64(o.Terms.UnitsOnOffer),
		}
	}
	return w
}

// wireSnapshot is the contract's Period.snapshot.
type wireSnapshot struct {
	MerkleRoot      string    `json:"merkleRoot"`
	TotalUnits      int32     `json:"totalUnits"`
	DistinctHolders int32     `json:"distinctHolders"`
	LineCount       int64     `json:"lineCount"`
	TakenAt         timestamp `json:"takenAt"`
}

// wirePeriod is the contract's Period.
type wirePeriod struct {
	ID          string        `json:"id"`
	SchemeID    string        `json:"schemeId"`
	PeriodSeq   int32         `json:"periodSeq"`
	PeriodStart string        `json:"periodStart"`
	PeriodEnd   string        `json:"periodEnd"`
	RecordDate  *string       `json:"recordDate"`
	Status      string        `json:"status"`
	Snapshot    *wireSnapshot `json:"snapshot,omitempty"`
}

// dateOnly formats a date as the contract's YYYY-MM-DD.
//
// A period boundary is a date, not an instant. Serving it as a timestamp would invite a client to apply a
// timezone to it and land on the previous day, which for a record date decides who is paid.
func dateOnly(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

func dateOnlyPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := dateOnly(*t)
	return &s
}

// periodToWire converts a stored period.
func periodToWire(p store.Period, snap *store.Snapshot) wirePeriod {
	w := wirePeriod{
		ID:          p.ID,
		SchemeID:    p.SchemeID,
		PeriodSeq:   p.PeriodSeq,
		PeriodStart: dateOnly(p.PeriodStart),
		PeriodEnd:   dateOnly(p.PeriodEnd),
		RecordDate:  dateOnlyPtr(p.RecordDate),
		Status:      string(p.Status),
	}

	if snap != nil {
		w.Snapshot = &wireSnapshot{
			MerkleRoot:      hexBytes(snap.MerkleRoot),
			TotalUnits:      snap.TotalUnits,
			DistinctHolders: snap.DistinctHolders,
			LineCount:       snap.LineCount,
			TakenAt:         snap.TakenAt,
		}
	}
	return w
}

// wireList is the envelope for a collection.
//
// Every list is wrapped in an object with an items array rather than being a bare array. A bare array cannot
// gain a field, so adding a cursor later would be a breaking change to every client; and a top-level array
// is the shape that makes a JSON response unable to carry paging at all.
type wireList[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// newWireList guarantees items is an empty array rather than null.
//
// A nil slice marshals as null, and a client iterating the result would fault on it. An empty collection is
// an empty array.
func newWireList[T any](items []T, next string) wireList[T] {
	if items == nil {
		items = []T{}
	}
	return wireList[T]{Items: items, NextCursor: next}
}
