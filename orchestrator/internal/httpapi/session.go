package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/acresync/orchestrator/internal/store"
)

// POST /auth/session: exchanging an upstream identity for one of ours.
//
// # Two identities, deliberately kept apart
//
// Web3Auth establishes *who is calling*. The investor record establishes *what they hold*. Conflating them
// would mean a change of login provider rewrote the register, and the register is the thing a unitholder's
// legal entitlement rests on.
//
// So the exchange is a lookup, never a creation. An upstream subject with no investor record is refused
// rather than silently given one: an endpoint that mints register identities on presentation of a third
// party's token makes that third party the author of the register.

// ErrNoSuchSubject is returned when an upstream identity maps to no investor.
var ErrNoSuchSubject = errors.New("session: the authenticated subject is not a registered investor")

// IDTokenVerifier verifies an upstream identity token.
//
// An interface because the real implementation talks to Web3Auth's JWKS over the network, and a test must not.
// The contract says the development setup uses a local RSA key pair, which is the same shape behind this seam.
type IDTokenVerifier interface {
	// Verify returns the upstream subject, or an error if the token does not hold up.
	Verify(ctx context.Context, idToken string) (IDTokenClaims, error)
}

// IDTokenClaims is what an upstream token establishes.
//
// Two fields, because they answer different questions. Subject is the login account and is what the session
// records for audit. WalletAddress is the derived on-chain identity and is what the register can actually be
// matched against: no column anywhere stores a Web3Auth subject, and inventing one would put the login
// provider's identifier inside the register.
//
// An email or a display name may be present in the upstream token and is deliberately not carried forward.
// Nothing downstream needs it, and a field that exists will eventually be logged.
type IDTokenClaims struct {
	Subject       string
	WalletAddress string
}

// InvestorResolver maps a verified upstream identity to a register identity.
//
// Takes the whole claims value rather than one field, so the decision about which part of an upstream identity
// links to the register belongs to the resolver and is visible there. Today it is the wallet address.
type InvestorResolver interface {
	// Resolve returns the investor, or ErrNoSuchSubject when the identity holds nothing here.
	Resolve(ctx context.Context, claims IDTokenClaims) (string, error)
}

// sessionRequest is the contract's request body.
type sessionRequest struct {
	IDToken string `json:"idToken"`
}

// sessionResponse is the contract's response body.
type sessionResponse struct {
	AccessToken string    `json:"accessToken"`
	ExpiresAt   timestamp `json:"expiresAt"`
	InvestorID  string    `json:"investorId"`
}

// handleCreateSession serves POST /v1/auth/session.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req sessionRequest
	if err := decodeBodyStrict(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if req.IDToken == "" {
		writeError(w, r, badRequest("idToken is required", nil))
		return
	}

	claims, err := s.deps.IDTokens.Verify(r.Context(), req.IDToken)
	if err != nil {
		// Logged with the cause, answered without it. Why a token failed is useful to us and useful to
		// somebody forging one; the caller's only action either way is to log in again.
		slog.Info("an upstream id token was rejected",
			"requestId", requestIDFrom(r.Context()), "reason", err)
		writeError(w, r, unauthorized("the identity token is not valid"))
		return
	}
	if claims.Subject == "" {
		slog.Error("an id token verified but carried no subject",
			"requestId", requestIDFrom(r.Context()))
		writeError(w, r, unauthorized("the identity token is not valid"))
		return
	}

	investorID, err := s.deps.Investors.Resolve(r.Context(), claims)
	if errors.Is(err, ErrNoSuchSubject) || errors.Is(err, store.ErrNoInvestorForIdentity) {
		// 403 rather than 404. The identity is genuine and was verified; what is missing is a relationship
		// with this platform. A 404 would suggest the endpoint does not exist, and a 401 would suggest the
		// token was bad, which would send the caller into a login loop that cannot succeed.
		writeError(w, r, forbidden("this identity is verified but is not a registered investor"))
		return
	}
	if err != nil {
		writeError(w, r, err)
		return
	}

	now, err := s.deps.Wall.Now(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	expiresAt := now.Add(s.deps.SessionTTL)

	token, err := s.signer.issue(Claims{
		Subject:    claims.Subject,
		Kind:       PrincipalInvestor,
		InvestorID: investorID,
		IssuedAt:   now.Unix(),
		ExpiresAt:  expiresAt.Unix(),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}

	slog.Info("session issued",
		"requestId", requestIDFrom(r.Context()),
		"investorId", investorID, "expiresAt", expiresAt.UTC().Format(time.RFC3339))

	// Not writeJSONWithETag. A session response is unique to the request and must never be cached or
	// revalidated; an ETag on a credential invites a proxy to store it.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, sessionResponse{
		AccessToken: token,
		ExpiresAt:   expiresAt.UTC(),
		InvestorID:  investorID,
	})
}

// WalletResolver is the store surface the session exchange needs.
//
// Declared here rather than taking store.Investors directly, so this package keeps depending on behaviour
// instead of on a concrete type, and a test can substitute one without a database.
type WalletResolver interface {
	ResolveWallet(ctx context.Context, address string) (string, error)
}

// InvestorsByWallet adapts a wallet resolver to the session exchange.
//
// The adapter exists because the two sides answer different questions. The store knows how to find the holder
// of a wallet; this package knows that a verified login without a wallet cannot be resolved at all, which is a
// statement about the exchange rather than about the database.
type InvestorsByWallet struct {
	Wallets WalletResolver
}

// Resolve maps a verified identity to a register identity.
func (r InvestorsByWallet) Resolve(ctx context.Context, claims IDTokenClaims) (string, error) {
	if claims.WalletAddress == "" {
		// Verified, but carrying nothing the register can be matched against. Refusing beats guessing: the
		// alternative would be picking an investor by some looser attribute, and picking the wrong one hands
		// somebody another unitholder's position.
		return "", ErrNoSuchSubject
	}

	investorID, err := r.Wallets.ResolveWallet(ctx, claims.WalletAddress)
	if errors.Is(err, store.ErrNoInvestorForIdentity) {
		return "", ErrNoSuchSubject
	}
	if err != nil {
		return "", err
	}
	return investorID, nil
}

// decodeBodyStrict decodes a JSON request body, refusing anything unexpected.
//
// DisallowUnknownFields is on deliberately. A client sending "amountPaise" where the contract says
// "amountPaise " or "amount" would otherwise have its value silently dropped and get a successful response
// describing something it did not ask for. On a mutation that moves money, quietly ignoring a field is the
// worst available behaviour.
//
// A second decode is attempted to catch trailing content, because a body of two concatenated JSON objects
// otherwise parses as the first one and discards the rest.
func decodeBodyStrict(r *http.Request, dst any) error {
	if r.Body == nil {
		return badRequest("a JSON body is required", nil)
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			return &statusError{
				status: http.StatusRequestEntityTooLarge,
				code:   CodeValidationFailed,
				msg:    "the request body is larger than this endpoint accepts",
			}
		}
		// The decoder's message names the offending field or type, which is about the caller's own request
		// and is worth passing on.
		return badRequest("the request body is not valid JSON for this endpoint", err)
	}

	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return badRequest("the request body carries content after the JSON document", nil)
	}
	return nil
}
