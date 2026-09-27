package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"

	"github.com/acresync/orchestrator/internal/asba"
	"github.com/acresync/orchestrator/internal/idempotency"
	"github.com/acresync/orchestrator/internal/money"
	"github.com/acresync/orchestrator/internal/store"
)

// POST /offers/{offerId}/bids: an investor bids, and their bank blocks the money.
//
// # The order of checks
//
// Everything that can be refused without touching the bank is refused first: the offer's status and window,
// ownership of the accounts, KYC, the bid's bounds. Only then is the bank asked to block funds, because a block
// is a real reservation on a real account, and asking for one on a bid that was always going to be refused
// freezes an investor's money for nothing.
//
// # What the caller cannot choose
//
// The investor is taken from the token. The accounts in the body are checked to belong to that investor in the
// same query, so a bid cannot be placed against somebody else's demat or bank account by naming its id.

type placeBidRequest struct {
	UnitsBid          uint32      `json:"unitsBid"`
	PricePerUnitPaise money.Paise `json:"pricePerUnitPaise"`
	DematAccountID    string      `json:"dematAccountId"`
	BankAccountID     string      `json:"bankAccountId"`
}

// newBidRef draws the bid reference.
//
// Random rather than derived. The book is ordered by bidRef, so a reference an investor could predict from
// their own identity would let them predict their leaf position, and nothing about the ballot should be
// influenceable by choosing who you are. A retry is covered by the HTTP replay and, behind it, by the
// one-bid-per-investor-per-offer constraint, so the reference never needs to be reproducible.
func newBidRef() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// handlePlaceBid serves POST /v1/offers/{offerId}/bids.
func (s *Server) handlePlaceBid(c *writeCtx) (int, any, error) {
	investorID := c.principal.InvestorID
	offerID, err := pathUUID(c.r, "offerId")
	if err != nil {
		return 0, nil, err
	}

	var req placeBidRequest
	if err := c.decode(&req); err != nil {
		return 0, nil, err
	}
	if req.UnitsBid < 1 {
		return 0, nil, validation("unitsBid must be at least 1")
	}
	if req.PricePerUnitPaise <= 0 {
		return 0, nil, validation("pricePerUnitPaise must be positive")
	}
	if !looksLikeUUID(req.DematAccountID) || !looksLikeUUID(req.BankAccountID) {
		return 0, nil, validation("dematAccountId and bankAccountId must be uuids")
	}

	o, err := store.NewOffers(c.tx).ByID(c.ctx(), offerID)
	if err != nil {
		return 0, nil, resourceErr("offer", err)
	}
	if !o.Status.AcceptsBids() {
		return 0, nil, conflict(CodePreconditionFailed,
			fmt.Sprintf("the offer is %s and is not accepting bids", o.Status))
	}

	now, err := s.nowFor(c.ctx(), o.SchemeID)
	if err != nil {
		return 0, nil, err
	}
	if now.Before(o.Terms.OpensAt) || !now.Before(o.Terms.ClosesAt) {
		return 0, nil, conflict(CodePreconditionFailed, fmt.Sprintf(
			"bids are accepted from %s until %s; business time is %s",
			o.Terms.OpensAt.Format("2006-01-02T15:04:05Z"), o.Terms.ClosesAt.Format("2006-01-02T15:04:05Z"),
			now.Format("2006-01-02T15:04:05Z")))
	}

	// The same bounds bids_within_offer_bounds enforces, checked first so the refusal names the rule rather
	// than surfacing as a trigger message.
	t := o.Terms
	switch {
	case req.UnitsBid < t.MinBidUnits:
		return 0, nil, validation(fmt.Sprintf("a bid of %d units is below the minimum of %d", req.UnitsBid, t.MinBidUnits))
	case req.UnitsBid > t.MaxBidUnits:
		return 0, nil, validation(fmt.Sprintf("a bid of %d units exceeds the maximum of %d; the cap exists so "+
			"the %d-holder floor stays reachable", req.UnitsBid, t.MaxBidUnits, t.MinDistinctHolders))
	case req.PricePerUnitPaise < t.PriceBandLowerPaise || req.PricePerUnitPaise > t.PriceBandUpperPaise:
		return 0, nil, validation(fmt.Sprintf("a price of %d paise is outside the band [%d, %d]",
			req.PricePerUnitPaise, t.PriceBandLowerPaise, t.PriceBandUpperPaise))
	}

	dematOK, bankOK, asbaEnabled, err := store.AccountOwnership(c.ctx(), c.tx, investorID, req.DematAccountID, req.BankAccountID)
	if err != nil {
		return 0, nil, err
	}
	if !dematOK || !bankOK {
		// One message for both, and no confirmation of whether the id exists at all: the caller learns
		// that the accounts are not theirs, which is all they are entitled to learn.
		return 0, nil, validation("the demat and bank accounts must both be your own")
	}
	if !asbaEnabled {
		return 0, nil, validation("the bank account is not enabled for ASBA, so funds cannot be blocked against it")
	}

	identity, err := store.NewMe(c.tx).Identity(c.ctx(), investorID)
	if err != nil {
		return 0, nil, resourceErr("investor", err)
	}
	if identity.KYCStatus != "VERIFIED" {
		return 0, nil, conflict(CodePreconditionFailed, fmt.Sprintf(
			"KYC is %s; a bid requires a verified KYC record", identity.KYCStatus))
	}

	anchor, err := store.InvestorAnchor(c.ctx(), c.tx, o.SchemeID, investorID)
	if errors.Is(err, store.ErrNotFound) {
		return 0, nil, conflict(CodePreconditionFailed,
			"you are not onboarded to this scheme yet, so there is no anonymised anchor to place in the public bid book")
	}
	if err != nil {
		return 0, nil, err
	}

	ref, err := newBidRef()
	if err != nil {
		return 0, nil, err
	}

	bidKey, err := key(idempotency.ActionSubmitBid, o.SchemeID, offerID, map[string]any{"investor": investorID})
	if err != nil {
		return 0, nil, err
	}
	bidID, _, err := store.InsertBid(c.ctx(), c.tx, store.NewBid{
		OfferID: offerID, InvestorID: investorID, InvestorAnchor: anchor,
		DematID: req.DematAccountID, BankID: req.BankAccountID,
		Units: req.UnitsBid, Price: req.PricePerUnitPaise, BidRef: ref, Key: bidKey,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return 0, nil, conflict(CodePreconditionFailed, "you already have a bid on this offer; one bid per investor")
		}
		return 0, nil, err
	}

	// Keyed on the bid, so one bid gets one reservation. A second block on the same bid would freeze twice
	// the money, and the provider refuses a key reused with a different amount.
	blockKey, err := key(idempotency.ActionRequestASBABlock, o.SchemeID, offerID, map[string]any{"bidRef": ref})
	if err != nil {
		return 0, nil, err
	}
	blk, err := s.deps.ASBA.RequestBlock(c.ctx(), asba.BlockRequest{
		IdempotencyKey: hex.EncodeToString(blockKey),
		BidID:          bidID,
		BankAccountRef: req.BankAccountID,
		AmountPaise:    money.Paise(int64(req.UnitsBid) * int64(req.PricePerUnitPaise)),
		MandateRef:     req.BankAccountID,
	})
	if err != nil {
		// The transaction rolls back, so no bid is recorded. If the bank did act before the error, the
		// same key on the retry returns that block rather than placing another.
		return 0, nil, fmt.Errorf("requesting the funds block: %w", err)
	}

	recordKey, err := key(idempotency.ActionRequestASBABlock, o.SchemeID, offerID, map[string]any{"block": ref})
	if err != nil {
		return 0, nil, err
	}
	if err := store.RecordBlock(c.ctx(), c.tx, bidID, blk, recordKey); err != nil {
		return 0, nil, err
	}

	placed, err := store.NewMe(c.tx).BidFor(c.ctx(), investorID, bidID)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, bidToWire(placed), nil
}
