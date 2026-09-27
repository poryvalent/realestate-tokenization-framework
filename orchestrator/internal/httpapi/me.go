package httpapi

import (
	"context"
	"net/http"

	"github.com/acresync/orchestrator/internal/money"
	"github.com/acresync/orchestrator/internal/store"
)

// An investor's own records.
//
// # Every query is scoped by the token, never by the path
//
// There is no /investors/{id} in this API. The subject of every endpoint here is the caller, taken from their
// verified token, so there is no identifier for a client to substitute. That is a deliberate shape rather than
// a convenience: an endpoint that accepts an investor id has to check that the caller owns it, and the check
// that has to be remembered is the check that gets forgotten.
//
// # What is absent, and why that is honest
//
// A display name, a masked PAN and a masked account number are not returned. Those columns hold
// application-layer ciphertext under a KMS-wrapped key, and no decryption path exists in this system yet. The
// contract types each of them as optional or nullable, so omitting them is conformant, and returning a
// placeholder that looked like a mask would be a claim about data this service cannot read.
//
// demat client ids are plaintext in the schema, so those are genuinely masked.

// MeReader is the read surface the investor endpoints need.
type MeReader interface {
	Identity(ctx context.Context, investorID string) (store.Identity, error)
	Holdings(ctx context.Context, investorID string, p store.Pagination) ([]store.Holding, error)
	Entitlements(ctx context.Context, investorID string, p store.Pagination) ([]store.Entitlement, error)
	Payouts(ctx context.Context, investorID string, p store.Pagination) ([]store.Payout, error)
	Bids(ctx context.Context, investorID string, p store.Pagination) ([]store.Bid, error)
}

// wireMe is the contract's Me.
//
// panMasked and walletAddress are pointers because the contract types them as nullable, and there is a
// difference between "this investor has no wallet" and "this field was omitted". A null says the former.
type wireMe struct {
	InvestorID    string  `json:"investorId"`
	InvestorClass string  `json:"investorClass"`
	KYCStatus     string  `json:"kycStatus"`
	PanMasked     *string `json:"panMasked"`
	WalletAddress *string `json:"walletAddress"`

	DematAccounts []wireDemat `json:"dematAccounts"`
	BankAccounts  []wireBank  `json:"bankAccounts"`
}

type wireDemat struct {
	ID             string `json:"id"`
	Depository     string `json:"depository"`
	MaskedClientID string `json:"maskedClientId"`
	Verified       bool   `json:"verified"`
}

type wireBank struct {
	ID       string `json:"id"`
	IFSC     string `json:"ifsc"`
	Verified bool   `json:"verified"`
}

func identityToWire(id store.Identity) wireMe {
	out := wireMe{
		InvestorID:    id.InvestorID,
		InvestorClass: string(id.InvestorClass),
		KYCStatus:     id.KYCStatus,
		// Explicitly null. The PAN is stored encrypted and this service holds no key, so there is nothing to
		// mask; a fabricated mask would be worse than an honest null.
		PanMasked:     nil,
		DematAccounts: make([]wireDemat, 0, len(id.Demats)),
		BankAccounts:  make([]wireBank, 0, len(id.Banks)),
	}

	if id.WalletAddress != "" {
		addr := id.WalletAddress
		out.WalletAddress = &addr
	}

	for _, d := range id.Demats {
		out.DematAccounts = append(out.DematAccounts, wireDemat{
			ID: d.ID, Depository: d.Depository, MaskedClientID: d.MaskedClientID, Verified: d.Verified,
		})
	}
	for _, b := range id.Banks {
		out.BankAccounts = append(out.BankAccounts, wireBank{ID: b.ID, IFSC: b.IFSC, Verified: b.Verified})
	}
	return out
}

// wireHolding is the contract's Holding.
type wireHolding struct {
	SchemeID                string     `json:"schemeId"`
	SchemeName              string     `json:"schemeName"`
	WalletAddress           string     `json:"walletAddress"`
	Units                   int32      `json:"units"`
	ExcludedFromHolderCount bool       `json:"excludedFromHolderCount"`
	LockInExpiresAt         *timestamp `json:"lockInExpiresAt"`
	FirstCreditedAt         timestamp  `json:"firstCreditedAt"`
}

func holdingToWire(h store.Holding) wireHolding {
	return wireHolding{
		SchemeID:                h.SchemeID,
		SchemeName:              h.SchemeName,
		WalletAddress:           h.WalletAddress,
		Units:                   h.Units,
		ExcludedFromHolderCount: h.ExcludedFromHolderCount,
		LockInExpiresAt:         h.LockInExpiresAt,
		FirstCreditedAt:         h.FirstCreditedAt,
	}
}

// wireTax is the contract's MyEntitlement.tax.
type wireTax struct {
	InvestorClass  string      `json:"investorClass"`
	Section        string      `json:"section"`
	RateBps        int32       `json:"rateBps"`
	AmountPaise    money.Paise `json:"amountPaise"`
	Form15GHOnFile bool        `json:"form15GHOnFile"`
}

// wireEntitlement is the contract's MyEntitlement.
type wireEntitlement struct {
	PeriodID           string      `json:"periodId"`
	PeriodEnd          string      `json:"periodEnd"`
	LeafIndex          int32       `json:"leafIndex"`
	Units              int32       `json:"units"`
	SnapshotTotalUnits int32       `json:"snapshotTotalUnits"`
	GrossPaise         money.Paise `json:"grossPaise"`
	Tax                *wireTax    `json:"tax,omitempty"`
	NetPayablePaise    money.Paise `json:"netPayablePaise"`
	Payable            bool        `json:"payable"`
	Payout             *wirePayout `json:"payout"`
}

// minimumPayablePaise is the smallest amount a payout provider will move.
//
// One rupee. An entitlement between 1 and 99 paise is storable and unpayable, and the contract is explicit that
// such an amount carries forward rather than being silently dropped. Reporting it as payable would produce a
// payout instruction the provider refuses, and the holder would see a failure rather than a carry-forward.
const minimumPayablePaise money.Paise = 100

func entitlementToWire(e store.Entitlement) wireEntitlement {
	out := wireEntitlement{
		PeriodID:           e.PeriodID,
		PeriodEnd:          dateOnly(e.PeriodEnd),
		LeafIndex:          e.LeafIndex,
		Units:              e.Units,
		SnapshotTotalUnits: e.SnapshotTotalUnits,
		GrossPaise:         e.GrossPaise,
		NetPayablePaise:    e.NetPayablePaise,
		Payable:            e.NetPayablePaise >= minimumPayablePaise,
	}

	if e.Tax != nil {
		out.Tax = &wireTax{
			InvestorClass:  string(e.Tax.InvestorClass),
			Section:        e.Tax.Section,
			RateBps:        e.Tax.RateBps,
			AmountPaise:    e.Tax.AmountPaise,
			Form15GHOnFile: e.Tax.Form15GHOnFile,
		}
	}
	if e.Payout != nil {
		p := payoutToWire(*e.Payout)
		out.Payout = &p
	}
	return out
}

// wirePayout is the contract's Payout.
type wirePayout struct {
	ID          string      `json:"id"`
	PeriodID    string      `json:"periodId"`
	AmountPaise money.Paise `json:"amountPaise"`
	Status      string      `json:"status"`
	UTR         *string     `json:"utr"`
	SettledAt   *timestamp  `json:"settledAt"`
	FailureCode *string     `json:"failureCode"`
	Provider    string      `json:"provider"`

	// Simulated is surfaced so a demo can never be mistaken for a payment. The distribution arithmetic and the
	// on-chain record are real; the final fiat hop is simulated while corporate banking KYC is outstanding, and
	// the API says so on every payout rather than leaving it to a footnote.
	Simulated bool `json:"simulated"`
}

func payoutToWire(p store.Payout) wirePayout {
	out := wirePayout{
		ID:          p.ID,
		PeriodID:    p.PeriodID,
		AmountPaise: p.AmountPaise,
		// DBStatus, ours. payout carries two vocabularies and the provider's lowercase one is theirs to change;
		// exposing it would make a third party's wording part of our published contract.
		Status:    string(p.Status),
		SettledAt: p.SettledAt,
		Provider:  p.Provider,
		Simulated: p.Simulated(),
	}
	if p.UTR != "" {
		utr := p.UTR
		out.UTR = &utr
	}
	if p.FailureCode != "" {
		code := p.FailureCode
		out.FailureCode = &code
	}
	return out
}

// wireBid is an investor's own bid.
type wireBid struct {
	ID                 string       `json:"id"`
	OfferID            string       `json:"offerId"`
	BidRef             string       `json:"bidRef"`
	Units              int32        `json:"units"`
	PricePerUnitPaise  money.Paise  `json:"pricePerUnitPaise"`
	TotalAmountPaise   money.Paise  `json:"totalAmountPaise"`
	Status             string       `json:"status"`
	RejectionReason    *string      `json:"rejectionReason"`
	SubmittedAt        timestamp    `json:"submittedAt"`
	BlockStatus        *string      `json:"blockStatus"`
	BlockedAmountPaise *money.Paise `json:"blockedAmountPaise"`
}

func bidToWire(b store.Bid) wireBid {
	out := wireBid{
		ID:                 b.ID,
		OfferID:            b.OfferID,
		BidRef:             b.BidReference,
		Units:              b.Units,
		PricePerUnitPaise:  b.PricePerUnit,
		TotalAmountPaise:   b.TotalAmount,
		Status:             b.Status,
		SubmittedAt:        b.SubmittedAt,
		BlockedAmountPaise: b.BlockedPaise,
	}
	if b.RejectionReason != "" {
		reason := b.RejectionReason
		out.RejectionReason = &reason
	}
	if b.BlockStatus != "" {
		status := b.BlockStatus
		out.BlockStatus = &status
	}
	return out
}

// callerInvestorID returns the authenticated investor.
//
// A missing principal is an internal error rather than a 401, because these handlers only run behind
// requireInvestor. Reaching here without one means the route was wired without its guard, and answering 401
// would make that look like a client problem.
func callerInvestorID(r *http.Request) (string, error) {
	p, ok := principalFrom(r.Context())
	if !ok || p.InvestorID == "" {
		return "", &statusError{
			status: http.StatusInternalServerError,
			code:   CodeInternal,
			msg:    "internal error",
		}
	}
	return p.InvestorID, nil
}

// handleGetMe serves GET /v1/me.
func (s *Server) handleGetMe(w http.ResponseWriter, r *http.Request) {
	investorID, err := callerInvestorID(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	identity, err := s.deps.Me.Identity(r.Context(), investorID)
	if err != nil {
		writeError(w, r, resourceErr("investor", err))
		return
	}

	// No ETag on an identity response. It is cheap, it is per-caller, and a conditional request on a
	// credentialled resource invites a shared cache to key it wrongly.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, identityToWire(identity))
}

// handleListMyHoldings serves GET /v1/me/holdings.
func (s *Server) handleListMyHoldings(w http.ResponseWriter, r *http.Request) {
	investorID, err := callerInvestorID(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	p, err := pagination(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	holdings, err := s.deps.Me.Holdings(r.Context(), investorID, p)
	if err != nil {
		writeError(w, r, err)
		return
	}

	items := make([]wireHolding, 0, len(holdings))
	for _, h := range holdings {
		items = append(items, holdingToWire(h))
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, newWireList(items, ""))
}

// handleListMyEntitlements serves GET /v1/me/entitlements.
func (s *Server) handleListMyEntitlements(w http.ResponseWriter, r *http.Request) {
	investorID, err := callerInvestorID(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	p, err := pagination(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	entitlements, err := s.deps.Me.Entitlements(r.Context(), investorID, p)
	if err != nil {
		writeError(w, r, err)
		return
	}

	items := make([]wireEntitlement, 0, len(entitlements))
	for _, e := range entitlements {
		items = append(items, entitlementToWire(e))
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, newWireList(items, ""))
}

// handleListMyPayouts serves GET /v1/me/payouts.
func (s *Server) handleListMyPayouts(w http.ResponseWriter, r *http.Request) {
	investorID, err := callerInvestorID(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	p, err := pagination(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	payouts, err := s.deps.Me.Payouts(r.Context(), investorID, p)
	if err != nil {
		writeError(w, r, err)
		return
	}

	items := make([]wirePayout, 0, len(payouts))
	for _, po := range payouts {
		items = append(items, payoutToWire(po))
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, newWireList(items, ""))
}

// handleListMyBids serves GET /v1/me/bids.
func (s *Server) handleListMyBids(w http.ResponseWriter, r *http.Request) {
	investorID, err := callerInvestorID(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	p, err := pagination(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	bids, err := s.deps.Me.Bids(r.Context(), investorID, p)
	if err != nil {
		writeError(w, r, err)
		return
	}

	items := make([]wireBid, 0, len(bids))
	for _, b := range bids {
		items = append(items, bidToWire(b))
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, newWireList(items, ""))
}
