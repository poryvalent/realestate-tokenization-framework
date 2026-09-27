package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/acresync/orchestrator/internal/config"
)

// Authentication and the role boundary.
//
// The contract publishes two security schemes: investorAuth and operatorAuth. Operator roles are a claim on
// one token rather than three separate schemes, because there is one credential; modelling three would
// suggest three exist and leave the frontend juggling key sets to represent something the backend does not
// have.
//
// # Roles are a boundary, not a ladder
//
// MANAGER is not a weaker TRUSTEE. Where a step needs a second party, such as trustee approval of a
// distribution, the manager cannot satisfy it by being more privileged, and no amount of authority collapses
// the two. The roles mirror the on-chain roles so that an action the chain would refuse is refused here
// rather than attempted, reverted and paid for in gas.
//
// Four eyes is a further thing again, and it is not enforced here: it requires two different *people*, which
// a role check cannot see. That lives in the domain, where both approvals are compared.

// PrincipalKind separates the two audiences.
type PrincipalKind string

const (
	PrincipalInvestor PrincipalKind = "INVESTOR"
	PrincipalOperator PrincipalKind = "OPERATOR"
)

// Role is an operator role, mirroring the on-chain roles.
type Role string

const (
	RoleManager    Role = "MANAGER"
	RoleTrustee    Role = "TRUSTEE"
	RoleCompliance Role = "COMPLIANCE"
)

// AllRoles is the published set, in the contract's order.
func AllRoles() []Role { return []Role{RoleManager, RoleTrustee, RoleCompliance} }

// Valid reports whether the role is one the contract publishes.
func (r Role) Valid() bool {
	for _, known := range AllRoles() {
		if r == known {
			return true
		}
	}
	return false
}

// Principal is the authenticated caller.
type Principal struct {
	Kind       PrincipalKind
	Subject    string
	InvestorID string
	Role       Role
}

// IsOperator reports whether the caller is an operator.
func (p Principal) IsOperator() bool { return p.Kind == PrincipalOperator }

// principalFrom returns the caller, and whether one was authenticated.
func principalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxPrincipal).(Principal)
	return p, ok
}

// bearerToken extracts a token from the Authorization header.
//
// The scheme is matched case-insensitively because RFC 7235 makes it so, and a client sending "bearer" is
// correct even though most send "Bearer". Rejecting it would be a confusing failure for a conforming client.
func bearerToken(r *http.Request) (string, error) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", unauthorized("an Authorization bearer token is required")
	}

	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", unauthorized("the Authorization header must be a Bearer token")
	}

	token = strings.TrimSpace(token)
	if token == "" {
		return "", unauthorized("the Bearer token is empty")
	}
	return token, nil
}

// authenticate verifies a bearer token and returns the principal.
func (s *Server) authenticate(r *http.Request) (Principal, error) {
	if s.signer == nil {
		// No signing key configured. Refusing is the only safe answer: the alternative is verifying against
		// an empty key, which would accept a token anybody could mint.
		return Principal{}, unauthorized("this deployment has no session signing key, so tokens cannot be verified")
	}

	raw, err := bearerToken(r)
	if err != nil {
		return Principal{}, err
	}

	now, err := s.deps.Wall.Now(r.Context())
	if err != nil {
		// A clock failure must not be an open door. Without a trustworthy now, expiry cannot be checked, and
		// an unexpired-by-default token is exactly what an expiry is for.
		slog.Error("the clock failed while verifying a token",
			"requestId", requestIDFrom(r.Context()), "err", err)
		return Principal{}, unauthorized("the token's expiry cannot be checked right now")
	}

	claims, err := s.signer.verify(raw, now)
	if err != nil {
		// The reason is logged, not returned. Telling a caller whether the signature or the expiry failed
		// helps somebody probing for a valid key more than it helps a legitimate client, whose only useful
		// action either way is to obtain a new token.
		slog.Info("token rejected",
			"requestId", requestIDFrom(r.Context()), "path", r.URL.Path, "reason", err)
		return Principal{}, unauthorized("the token is not valid")
	}

	return Principal{
		Kind:       claims.Kind,
		Subject:    claims.Subject,
		InvestorID: claims.InvestorID,
		Role:       claims.Role,
	}, nil
}

// requireInvestor admits an authenticated investor.
func (s *Server) requireInvestor(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.authenticate(r)
		if err != nil {
			writeError(w, r, err)
			return
		}

		// An operator token is not a weaker investor token. An operator has no holdings, and letting one
		// through would mean an endpoint scoped to "my units" had no meaningful subject.
		if p.Kind != PrincipalInvestor {
			writeError(w, r, forbidden("this endpoint is for investors; the token identifies an operator"))
			return
		}

		next(w, r.WithContext(context.WithValue(r.Context(), ctxPrincipal, p)))
	}
}

// requireOperator admits an operator holding one of the given roles.
//
// The roles are listed per endpoint rather than checked inside handlers, so the boundary is visible in the
// routing table. An empty list means any operator role, which is spelled out at each call site rather than
// being the default.
func (s *Server) requireOperator(allowed ...Role) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			p, err := s.authenticate(r)
			if err != nil {
				writeError(w, r, err)
				return
			}
			if !p.IsOperator() {
				writeError(w, r, forbidden("this endpoint is for operators"))
				return
			}

			p.Role = s.effectiveRole(r, p.Role)

			if len(allowed) > 0 && !roleIn(p.Role, allowed) {
				// The required role is named. This is not a hint that helps an attacker: the role model is
				// published in the contract, and an operator who used the wrong persona needs to know which
				// one the step wants.
				writeError(w, r, forbidden(
					"this endpoint requires the "+roleList(allowed)+" role; the token carries "+string(p.Role)))
				return
			}

			next(w, r.WithContext(context.WithValue(r.Context(), ctxPrincipal, p)))
		}
	}
}

// mockRoleHeader lets a developer act as another persona without a second token.
const mockRoleHeader = "X-Mock-Role"

// effectiveRole applies X-Mock-Role, but only where that cannot matter.
//
// Honoured in LOCAL only. The header exists so the frontend can exercise the trustee path against the mock
// without a separate credential, and it is a complete bypass of the role boundary, so anywhere that boundary
// means anything it is ignored. Its presence outside LOCAL is logged: a client sending it there is either
// misconfigured or probing, and both are worth seeing.
func (s *Server) effectiveRole(r *http.Request, fromToken Role) Role {
	requested := Role(r.Header.Get(mockRoleHeader))
	if requested == "" {
		return fromToken
	}

	if s.deps.Env != config.EnvLocal {
		slog.Warn("X-Mock-Role was ignored outside LOCAL",
			"requestId", requestIDFrom(r.Context()),
			"env", string(s.deps.Env), "requested", string(requested), "path", r.URL.Path)
		return fromToken
	}

	if !requested.Valid() {
		// An unknown value is ignored rather than honoured. Treating it as a role would invent one.
		slog.Warn("X-Mock-Role was not a published role and was ignored",
			"requestId", requestIDFrom(r.Context()), "requested", string(requested))
		return fromToken
	}

	slog.Info("acting under X-Mock-Role",
		"requestId", requestIDFrom(r.Context()), "role", string(requested), "tokenRole", string(fromToken))
	return requested
}

func roleIn(role Role, allowed []Role) bool {
	for _, a := range allowed {
		if role == a {
			return true
		}
	}
	return false
}

func roleList(roles []Role) string {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		out = append(out, string(r))
	}
	return strings.Join(out, " or ")
}
