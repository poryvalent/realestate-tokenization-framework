package httpapi

import "net/http"

// writeRoutes registers the mutations.
//
// Each group is registered only when everything it needs is present. A mutation whose dependency is missing
// is absent, a 404, rather than present and failing at request time with a 500 that looks like a bug.
func (s *Server) writeRoutes(mux *http.ServeMux) {
	// Every mutation needs a way to authenticate the caller, a transaction to run in, and business time.
	if s.signer == nil || s.deps.DB == nil || s.deps.BusinessClock == nil {
		return
	}

	op := func(roles ...Role) func(http.HandlerFunc) http.HandlerFunc { return s.requireOperator(roles...) }

	// Readiness is a read, but it needs the same evidence loader and business clock as the writes, so it is
	// registered with them.
	mux.HandleFunc("GET /v1/admin/offers/{offerId}/readiness", op()(s.handleGetReadiness))

	mux.HandleFunc("POST /v1/admin/offers",
		op(rolesRunOffer...)(s.write("POST /v1/admin/offers", s.handleCreateOffer)))

	// Abort widens the role boundary to the trustee, which depends on the body, so the route admits both and
	// the handler narrows it.
	mux.HandleFunc("POST /v1/admin/offers/{offerId}/transitions",
		op(rolesAbort...)(s.write("POST /v1/admin/offers/{offerId}/transitions", s.handleAdvanceOffer)))

	if s.deps.ASBA != nil {
		mux.HandleFunc("POST /v1/offers/{offerId}/bids",
			s.requireInvestor(s.write("POST /v1/offers/{offerId}/bids", s.handlePlaceBid)))
	}

	manager := func(route string, fn writeFunc) {
		mux.HandleFunc(route, op(rolesRunOffer...)(s.write(route, fn)))
	}

	// Freezing pins the book, so it needs somewhere to pin it.
	if s.deps.Publisher != nil {
		manager("POST /v1/admin/offers/{offerId}/book/freeze", s.handleFreezeBook)
	}

	// The ceremony derives its secret from the pepper. Without one there is no secret to commit to, and a
	// ceremony started under one pepper can only be finished under the same one.
	if len(s.deps.SeedPepper) > 0 {
		manager("POST /v1/admin/offers/{offerId}/ballot/commit", s.handleCommitSeed)
		// Reveal and recommit are judged against the chain head, and reveal reads the target block's hash.
		if s.deps.Chain != nil {
			manager("POST /v1/admin/offers/{offerId}/ballot/reveal", s.handleRevealSeed)
			manager("POST /v1/admin/offers/{offerId}/ballot/recommit", s.handleRecommitSeed)
		}
		if s.deps.Publisher != nil {
			manager("POST /v1/admin/offers/{offerId}/ballot/draw", s.handleDrawBallot)
		}
	}
}
