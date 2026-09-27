package httpapi

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"

	"github.com/acresync/orchestrator/internal/store"
)

// The public verification surface.
//
// None of these endpoints requires a token, which is the point. The claim this platform makes is that the
// register, the ballot and the distribution arithmetic can be checked by somebody with no relationship to
// the operator, and an endpoint that needs credentials to read cannot support that claim.
//
// Consequently nothing here may serve anything investor-identifying. The register is published as wallet
// addresses and unit counts; no name, no PAN, no bank detail and no email passes through this file. That is
// the same boundary the chain respects, for the same reason.

// SchemeReader is the read surface the scheme endpoints need.
//
// Declared here rather than in Deps so each handler's requirements are visible at the point of use, and so a
// test can substitute a reader that returns an error without needing a database.
type SchemeReader interface {
	ByID(ctx context.Context, id string) (store.Scheme, error)
	List(ctx context.Context, p store.Pagination) ([]store.Scheme, error)
}

// OfferReader is the read surface the offer endpoints need.
type OfferReader interface {
	ByID(ctx context.Context, id string) (store.Offer, error)
	ForScheme(ctx context.Context, schemeID string, p store.Pagination) ([]store.Offer, error)
	SubscriptionFor(ctx context.Context, offerID string) (store.Subscription, error)
}

// PeriodReader is the read surface the period endpoints need.
type PeriodReader interface {
	ByID(ctx context.Context, id string) (store.Period, error)
	ForScheme(ctx context.Context, schemeID string, p store.Pagination) ([]store.Period, error)
	SnapshotFor(ctx context.Context, periodID string) (store.Snapshot, error)
}

// uuidLength is the canonical textual length of a UUID, including hyphens.
const uuidLength = 36

// pathUUID reads a path wildcard and checks it looks like a UUID.
//
// Validated before it reaches a query, not for safety from injection, which parameterised queries already
// give, but so a malformed identifier produces a 400 explaining what was wrong instead of a 500 from the
// driver rejecting the cast. A caller who mistyped an id should be told that.
func pathUUID(r *http.Request, name string) (string, error) {
	raw := r.PathValue(name)
	if raw == "" {
		return "", badRequest(name+" is missing from the path", nil)
	}
	if !looksLikeUUID(raw) {
		return "", badRequest(name+" is not a uuid", nil)
	}
	return raw, nil
}

// looksLikeUUID checks the 8-4-4-4-12 shape without allocating.
func looksLikeUUID(s string) bool {
	if len(s) != uuidLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch i {
		case 8, 13, 18, 23:
			if s[i] != '-' {
				return false
			}
		default:
			c := s[i]
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// pagination reads the limit and cursor query parameters.
//
// A non-numeric limit is refused rather than silently defaulted. Quietly ignoring it would let a client
// believe it had asked for 500 rows and received all of them, when it received the default.
func pagination(r *http.Request) (store.Pagination, error) {
	p := store.Pagination{Cursor: r.URL.Query().Get("cursor")}

	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return store.Pagination{}, badRequest("limit is not a number", err)
		}
		if n < 1 {
			return store.Pagination{}, badRequest("limit must be at least 1", nil)
		}
		p.Limit = n
	}
	return p, nil
}

// resourceErr names what was missing when a store reports a missing row.
//
// store.ErrNotFound reads "store: no such row", which is true and useless: it names an internal layer and
// says nothing about what the caller asked for. Passing a domain sentinel's message through verbatim is right
// because those sentences explain a business rule, but this one explains nothing, so the handler supplies the
// noun.
//
// The table still maps store.ErrNotFound to a 404 as a safety net. A handler that forgets to call this
// produces a correct status with a poor message, rather than a 500.
func resourceErr(what string, err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return notFound(what)
	}
	return err
}

// hexBytes renders a digest as the contract's 0x-prefixed lowercase hex.
//
// Lowercase and always prefixed, so one digest has exactly one spelling across the database, the chain and
// this API. A client comparing a value it computed against one served here must not have to normalise first.
func hexBytes(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return "0x" + hex.EncodeToString(b)
}

// handleListSchemes serves GET /v1/schemes.
func (s *Server) handleListSchemes(w http.ResponseWriter, r *http.Request) {
	p, err := pagination(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	schemes, err := s.deps.Schemes.List(r.Context(), p)
	if err != nil {
		writeError(w, r, err)
		return
	}

	items := make([]wireScheme, 0, len(schemes))
	for _, sc := range schemes {
		items = append(items, schemeToWire(sc))
	}

	// The cursor is the last row's ordering key, which List pages on. Only sent when the page was filled,
	// because a short page means there is nothing after it and a cursor would invite a pointless extra
	// request that returns an empty list.
	var next string
	if len(schemes) == p.Limit && len(schemes) > 0 {
		next = schemes[len(schemes)-1].SebiSchemeRef
	}

	writeJSONWithETag(w, r, http.StatusOK, newWireList(items, next))
}

// handleGetScheme serves GET /v1/schemes/{schemeId}.
func (s *Server) handleGetScheme(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "schemeId")
	if err != nil {
		writeError(w, r, err)
		return
	}

	scheme, err := s.deps.Schemes.ByID(r.Context(), id)
	if err != nil {
		writeError(w, r, resourceErr("scheme", err))
		return
	}

	writeJSONWithETag(w, r, http.StatusOK, schemeToWire(scheme))
}

// handleListSchemeOffers serves GET /v1/schemes/{schemeId}/offers.
func (s *Server) handleListSchemeOffers(w http.ResponseWriter, r *http.Request) {
	schemeID, err := pathUUID(r, "schemeId")
	if err != nil {
		writeError(w, r, err)
		return
	}

	p, err := pagination(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	// The scheme is loaded first so a request for offers under a scheme that does not exist is a 404 rather
	// than an empty list. An empty list would say "this scheme has no offers", which is a different and
	// misleading claim.
	if _, err := s.deps.Schemes.ByID(r.Context(), schemeID); err != nil {
		writeError(w, r, resourceErr("scheme", err))
		return
	}

	offers, err := s.deps.Offers.ForScheme(r.Context(), schemeID, p)
	if err != nil {
		writeError(w, r, err)
		return
	}

	// The subscription is not attached to list entries. It is a separate aggregate query per offer, and a
	// list of twenty offers would become twenty extra round trips for a summary view that does not show them.
	items := make([]wireOffer, 0, len(offers))
	for _, o := range offers {
		items = append(items, offerToWire(o, nil))
	}

	writeJSONWithETag(w, r, http.StatusOK, newWireList(items, ""))
}

// handleGetOffer serves GET /v1/offers/{offerId}.
func (s *Server) handleGetOffer(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "offerId")
	if err != nil {
		writeError(w, r, err)
		return
	}

	o, err := s.deps.Offers.ByID(r.Context(), id)
	if err != nil {
		writeError(w, r, resourceErr("offer", err))
		return
	}

	// Subscription figures are the point of watching an open offer, so the detail view pays for the extra
	// query. A failure here fails the request rather than degrading to an offer without them: silently
	// omitting the block is indistinguishable from an offer nobody has bid on.
	sub, err := s.deps.Offers.SubscriptionFor(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}

	writeJSONWithETag(w, r, http.StatusOK, offerToWire(o, &sub))
}

// handleListSchemePeriods serves GET /v1/schemes/{schemeId}/periods.
func (s *Server) handleListSchemePeriods(w http.ResponseWriter, r *http.Request) {
	schemeID, err := pathUUID(r, "schemeId")
	if err != nil {
		writeError(w, r, err)
		return
	}

	p, err := pagination(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	if _, err := s.deps.Schemes.ByID(r.Context(), schemeID); err != nil {
		writeError(w, r, resourceErr("scheme", err))
		return
	}

	periods, err := s.deps.Periods.ForScheme(r.Context(), schemeID, p)
	if err != nil {
		writeError(w, r, err)
		return
	}

	items := make([]wirePeriod, 0, len(periods))
	for _, pd := range periods {
		items = append(items, periodToWire(pd, nil))
	}

	writeJSONWithETag(w, r, http.StatusOK, newWireList(items, ""))
}

// handleGetPeriod serves GET /v1/periods/{periodId}.
func (s *Server) handleGetPeriod(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "periodId")
	if err != nil {
		writeError(w, r, err)
		return
	}

	p, err := s.deps.Periods.ByID(r.Context(), id)
	if err != nil {
		writeError(w, r, resourceErr("period", err))
		return
	}

	// A period with no snapshot yet is the normal state of an open period, so the absence is not an error.
	// Any other failure is, and is not swallowed: treating a real read failure as "no snapshot" would
	// publish a period as unfrozen when the register had in fact been frozen and anchored.
	var snapshot *store.Snapshot
	snap, err := s.deps.Periods.SnapshotFor(r.Context(), id)
	switch {
	case err == nil:
		snapshot = &snap
	case errors.Is(err, store.ErrNotFound):
		snapshot = nil
	default:
		writeError(w, r, err)
		return
	}

	writeJSONWithETag(w, r, http.StatusOK, periodToWire(p, snapshot))
}
