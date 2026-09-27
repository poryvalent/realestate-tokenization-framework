package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/acresync/orchestrator/internal/config"
)

// fixedClock returns a constant time, so token expiry is deterministic.
type fixedClock struct{ at time.Time }

func (f fixedClock) Now(context.Context) (time.Time, error) { return f.at, nil }

// brokenClock fails, standing in for a simulated clock whose database read errors.
type brokenClock struct{}

func (brokenClock) Now(context.Context) (time.Time, error) {
	return time.Time{}, errors.New("reading the clock: connection refused")
}

// fakeIDTokens accepts one token and rejects everything else.
type fakeIDTokens struct {
	accept  string
	subject string
}

func (f fakeIDTokens) Verify(_ context.Context, idToken string) (IDTokenClaims, error) {
	if idToken != f.accept {
		return IDTokenClaims{}, errors.New("upstream: signature does not verify")
	}
	return IDTokenClaims{Subject: f.subject}, nil
}

// fakeInvestors resolves one subject.
type fakeInvestors struct {
	subject    string
	investorID string
}

func (f fakeInvestors) Resolve(_ context.Context, claims IDTokenClaims) (string, error) {
	if claims.Subject != f.subject {
		return "", ErrNoSuchSubject
	}
	return f.investorID, nil
}

// authServer builds a server with authentication available.
func authServer(t *testing.T, env config.Environment) *Server {
	t.Helper()

	return New(Deps{
		Env:           env,
		Wall:          fixedClock{at: tokenNow},
		SessionSecret: testSecret,
		SessionTTL:    30 * time.Minute,
		IDTokens:      fakeIDTokens{accept: "good-upstream-token", subject: "web3auth|abc123"},
		Investors:     fakeInvestors{subject: "web3auth|abc123", investorID: "cfa9ddd6-0377-4cbf-83b8-23b8ca6fa1fa"},
	})
}

// tokenFor mints a token through the server's own signer.
func tokenFor(t *testing.T, s *Server, claims Claims) string {
	t.Helper()
	if s.signer == nil {
		t.Fatal("this server has no signer")
	}
	raw, err := s.signer.issue(claims)
	if err != nil {
		t.Fatalf("issuing a test token: %v", err)
	}
	return raw
}

// probe records whether the handler ran and what principal it saw.
type probe struct {
	ran       bool
	principal Principal
}

func (p *probe) handler(w http.ResponseWriter, r *http.Request) {
	p.ran = true
	p.principal, _ = principalFrom(r.Context())
	writeJSON(w, r, http.StatusOK, map[string]string{"ok": "yes"})
}

// callGuarded runs a request through a guard and reports the outcome.
func callGuarded(t *testing.T, guarded http.HandlerFunc, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/v1/guarded", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	// withRequestID so the envelope carries an id, as it would in the real stack.
	chain(http.HandlerFunc(guarded), withRequestID).ServeHTTP(rec, req)
	return rec
}

// TestMissingOrMalformedAuthorizationIs401 covers admission.
func TestMissingOrMalformedAuthorizationIs401(t *testing.T) {
	s := authServer(t, config.EnvLocal)
	p := &probe{}
	guarded := s.requireInvestor(p.handler)

	cases := map[string]map[string]string{
		"no header":            {},
		"no scheme":            {"Authorization": "abcdef"},
		"wrong scheme":         {"Authorization": "Basic dXNlcjpwYXNz"},
		"bearer with no token": {"Authorization": "Bearer "},
		"bearer only":          {"Authorization": "Bearer"},
		"garbage token":        {"Authorization": "Bearer not-a-token"},
	}

	for name, headers := range cases {
		t.Run(name, func(t *testing.T) {
			p.ran = false
			rec := callGuarded(t, guarded, headers)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401. body: %s", rec.Code, rec.Body.String())
			}
			if p.ran {
				t.Fatal("the handler ran despite failed authentication")
			}
			env := decodeError(t, rec.Result())
			if env.Error.Code != CodeUnauthorized {
				t.Errorf("code = %q, want %q", env.Error.Code, CodeUnauthorized)
			}
		})
	}
}

// TestLowercaseBearerSchemeIsAccepted follows RFC 7235, where the scheme is case-insensitive.
func TestLowercaseBearerSchemeIsAccepted(t *testing.T) {
	s := authServer(t, config.EnvLocal)
	p := &probe{}

	rec := callGuarded(t, s.requireInvestor(p.handler), map[string]string{
		"Authorization": "bearer " + tokenFor(t, s, investorClaims()),
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; the scheme is case-insensitive. body: %s", rec.Code, rec.Body.String())
	}
}

// TestAValidInvestorTokenReachesTheHandler is the happy path.
func TestAValidInvestorTokenReachesTheHandler(t *testing.T) {
	s := authServer(t, config.EnvLocal)
	p := &probe{}

	rec := callGuarded(t, s.requireInvestor(p.handler), map[string]string{
		"Authorization": "Bearer " + tokenFor(t, s, investorClaims()),
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}
	if !p.ran {
		t.Fatal("the handler did not run")
	}
	if p.principal.Kind != PrincipalInvestor {
		t.Errorf("kind = %q, want INVESTOR", p.principal.Kind)
	}
	if p.principal.InvestorID != "cfa9ddd6-0377-4cbf-83b8-23b8ca6fa1fa" {
		t.Errorf("investorId = %q, not the one in the token", p.principal.InvestorID)
	}
}

// TestAnOperatorCannotUseAnInvestorEndpoint is the audience boundary.
//
// An operator is not a privileged investor. An endpoint scoped to "my units" has no meaningful subject for
// somebody who holds none, and letting one through would make that endpoint answer about nobody.
func TestAnOperatorCannotUseAnInvestorEndpoint(t *testing.T) {
	s := authServer(t, config.EnvLocal)
	p := &probe{}

	rec := callGuarded(t, s.requireInvestor(p.handler), map[string]string{
		"Authorization": "Bearer " + tokenFor(t, s, operatorClaims(RoleManager)),
	})

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403. body: %s", rec.Code, rec.Body.String())
	}
	if p.ran {
		t.Fatal("an operator token reached an investor handler")
	}
}

// TestAnInvestorCannotUseAnOperatorEndpoint is the same boundary the other way.
func TestAnInvestorCannotUseAnOperatorEndpoint(t *testing.T) {
	s := authServer(t, config.EnvLocal)
	p := &probe{}

	rec := callGuarded(t, s.requireOperator()(p.handler), map[string]string{
		"Authorization": "Bearer " + tokenFor(t, s, investorClaims()),
	})

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403. body: %s", rec.Code, rec.Body.String())
	}
	if p.ran {
		t.Fatal("an investor token reached an operator handler")
	}
}

// TestRoleBoundaryIsNotALadder is the point of the role model.
//
// MANAGER is not a weaker TRUSTEE. Where a step requires the trustee, the manager cannot satisfy it by being
// more privileged, and no ordering of roles makes that true.
func TestRoleBoundaryIsNotALadder(t *testing.T) {
	s := authServer(t, config.EnvLocal)

	for _, holder := range AllRoles() {
		for _, required := range AllRoles() {
			name := string(holder) + " against " + string(required)
			t.Run(name, func(t *testing.T) {
				p := &probe{}
				rec := callGuarded(t, s.requireOperator(required)(p.handler), map[string]string{
					"Authorization": "Bearer " + tokenFor(t, s, operatorClaims(holder)),
				})

				if holder == required {
					if rec.Code != http.StatusOK {
						t.Fatalf("%s was refused its own endpoint: %d %s", holder, rec.Code, rec.Body.String())
					}
					return
				}

				if rec.Code != http.StatusForbidden {
					t.Fatalf("%s was admitted to a %s endpoint with status %d", holder, required, rec.Code)
				}
				if p.ran {
					t.Fatalf("%s reached a %s handler", holder, required)
				}
				// The message names the role the step wants, which is published in the contract anyway.
				env := decodeError(t, rec.Result())
				if !strings.Contains(env.Error.Message, string(required)) {
					t.Errorf("message = %q, want it to name %s", env.Error.Message, required)
				}
			})
		}
	}
}

// TestAnyOperatorRoleIsAdmittedWhenNoneIsRequired covers the empty allow-list.
func TestAnyOperatorRoleIsAdmittedWhenNoneIsRequired(t *testing.T) {
	s := authServer(t, config.EnvLocal)

	for _, role := range AllRoles() {
		t.Run(string(role), func(t *testing.T) {
			p := &probe{}
			rec := callGuarded(t, s.requireOperator()(p.handler), map[string]string{
				"Authorization": "Bearer " + tokenFor(t, s, operatorClaims(role)),
			})
			if rec.Code != http.StatusOK {
				t.Fatalf("%s was refused: %d %s", role, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestMockRoleIsHonouredOnlyInLocal is the security property of that header.
//
// X-Mock-Role is a complete bypass of the role boundary. It exists so the frontend can exercise the trustee
// path against the mock without a second credential, and anywhere the boundary means anything it must do
// nothing at all.
func TestMockRoleIsHonouredOnlyInLocal(t *testing.T) {
	t.Run("LOCAL honours it", func(t *testing.T) {
		s := authServer(t, config.EnvLocal)
		p := &probe{}

		rec := callGuarded(t, s.requireOperator(RoleTrustee)(p.handler), map[string]string{
			"Authorization": "Bearer " + tokenFor(t, s, operatorClaims(RoleManager)),
			mockRoleHeader:  string(RoleTrustee),
		})

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 in LOCAL. body: %s", rec.Code, rec.Body.String())
		}
		if p.principal.Role != RoleTrustee {
			t.Errorf("role = %q, want the mocked TRUSTEE", p.principal.Role)
		}
	})

	t.Run("SEPOLIA_SIM ignores it", func(t *testing.T) {
		s := authServer(t, config.EnvSepoliaSim)
		p := &probe{}

		rec := callGuarded(t, s.requireOperator(RoleTrustee)(p.handler), map[string]string{
			"Authorization": "Bearer " + tokenFor(t, s, operatorClaims(RoleManager)),
			mockRoleHeader:  string(RoleTrustee),
		})

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: X-Mock-Role must not cross the role boundary outside LOCAL. body: %s",
				rec.Code, rec.Body.String())
		}
		if p.ran {
			t.Fatal("X-Mock-Role escalated a MANAGER to TRUSTEE outside LOCAL")
		}
	})
}

// TestMockRoleCannotInventARole stops the header creating authority that does not exist.
func TestMockRoleCannotInventARole(t *testing.T) {
	s := authServer(t, config.EnvLocal)
	p := &probe{}

	rec := callGuarded(t, s.requireOperator(RoleTrustee)(p.handler), map[string]string{
		"Authorization": "Bearer " + tokenFor(t, s, operatorClaims(RoleManager)),
		mockRoleHeader:  "SUPERUSER",
	})

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; an unpublished role must be ignored, not honoured", rec.Code)
	}
	if p.ran {
		t.Fatal("an invented role was honoured")
	}
}

// TestMockRoleDoesNotBypassAuthentication is worth stating separately.
//
// The header changes which role an authenticated operator acts as. It is not a way in.
func TestMockRoleDoesNotBypassAuthentication(t *testing.T) {
	s := authServer(t, config.EnvLocal)
	p := &probe{}

	rec := callGuarded(t, s.requireOperator(RoleTrustee)(p.handler), map[string]string{
		mockRoleHeader: string(RoleTrustee),
	})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; X-Mock-Role is not a credential", rec.Code)
	}
	if p.ran {
		t.Fatal("X-Mock-Role alone admitted a caller")
	}
}

// TestWithoutASigningKeyAuthenticatedRoutesAreRefused is the fail-safe.
//
// An absent key must mean authentication is unavailable, never verification against an empty key. The second
// would accept a token anybody could mint.
func TestWithoutASigningKeyAuthenticatedRoutesAreRefused(t *testing.T) {
	s := New(Deps{Env: config.EnvLocal, Wall: fixedClock{at: tokenNow}})
	if s.signer != nil {
		t.Fatal("a server with no secret should have no signer")
	}

	p := &probe{}
	// A token minted with the key the deployment failed to configure.
	sg, err := newSigner(testSecret)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sg.issue(investorClaims())
	if err != nil {
		t.Fatal(err)
	}

	rec := callGuarded(t, s.requireInvestor(p.handler), map[string]string{"Authorization": "Bearer " + raw})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if p.ran {
		t.Fatal("a handler ran on a server with no signing key")
	}
}

// TestSessionRouteIsAbsentWithoutItsDependencies mirrors the store-gated routes.
func TestSessionRouteIsAbsentWithoutItsDependencies(t *testing.T) {
	cases := map[string]Deps{
		"no secret":   {Env: config.EnvLocal, IDTokens: fakeIDTokens{}, Investors: fakeInvestors{}},
		"no verifier": {Env: config.EnvLocal, SessionSecret: testSecret, Investors: fakeInvestors{}},
		"no resolver": {Env: config.EnvLocal, SessionSecret: testSecret, IDTokens: fakeIDTokens{}},
	}

	for name, deps := range cases {
		t.Run(name, func(t *testing.T) {
			deps.Wall = fixedClock{at: tokenNow}
			s := New(deps)

			req := httptest.NewRequest(http.MethodPost, "/v1/auth/session", strings.NewReader(`{"idToken":"x"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(idempotencyKeyHeader, "idem-key-1234567890")
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 when a dependency is missing. body: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestAClockFailureDoesNotOpenTheDoor covers the degradation direction.
//
// Without a trustworthy now, expiry cannot be checked, and an unexpired-by-default token is exactly what an
// expiry exists to prevent.
func TestAClockFailureDoesNotOpenTheDoor(t *testing.T) {
	s := New(Deps{Env: config.EnvLocal, Wall: brokenClock{}, SessionSecret: testSecret})
	p := &probe{}

	rec := callGuarded(t, s.requireInvestor(p.handler), map[string]string{
		"Authorization": "Bearer " + tokenFor(t, s, investorClaims()),
	})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when the clock cannot be read", rec.Code)
	}
	if p.ran {
		t.Fatal("a broken clock let a request through without an expiry check")
	}
}

// TestAnExpiredTokenIsRefusedThroughTheMiddleware joins the token check to the HTTP layer.
func TestAnExpiredTokenIsRefusedThroughTheMiddleware(t *testing.T) {
	s := authServer(t, config.EnvLocal)
	p := &probe{}

	expired := investorClaims()
	expired.IssuedAt = tokenNow.Add(-2 * time.Hour).Unix()
	expired.ExpiresAt = tokenNow.Add(-time.Hour).Unix()

	rec := callGuarded(t, s.requireInvestor(p.handler), map[string]string{
		"Authorization": "Bearer " + tokenFor(t, s, expired),
	})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if p.ran {
		t.Fatal("an expired token reached the handler")
	}
}

// TestAuthFailuresDoNotExplainWhy is a deliberate restraint.
//
// Telling a caller whether the signature or the expiry failed helps somebody probing for a valid key more than
// it helps a legitimate client, whose action either way is to obtain a new token. The reason is logged.
func TestAuthFailuresDoNotExplainWhy(t *testing.T) {
	s := authServer(t, config.EnvLocal)

	expired := investorClaims()
	expired.ExpiresAt = tokenNow.Add(-time.Hour).Unix()

	bodies := []string{}
	for _, headers := range []map[string]string{
		{"Authorization": "Bearer " + tokenFor(t, s, expired)},
		{"Authorization": "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.e30.wrong"},
	} {
		rec := callGuarded(t, s.requireInvestor((&probe{}).handler), headers)
		env := decodeError(t, rec.Result())
		bodies = append(bodies, env.Error.Message)

		for _, leak := range []string{"expired", "signature", "hmac", "alg"} {
			if strings.Contains(strings.ToLower(env.Error.Message), leak) {
				t.Errorf("the response named the reason (%q): %q", leak, env.Error.Message)
			}
		}
	}

	// Both failures must look identical from outside.
	if bodies[0] != bodies[1] {
		t.Errorf("an expired token and a forged one gave different messages, %q and %q, which distinguishes them",
			bodies[0], bodies[1])
	}
}

// TestSessionExchangeIssuesAUsableToken is the end-to-end of POST /auth/session.
func TestSessionExchangeIssuesAUsableToken(t *testing.T) {
	s := authServer(t, config.EnvLocal)

	req := httptest.NewRequest(http.MethodPost, "/v1/auth/session",
		strings.NewReader(`{"idToken":"good-upstream-token"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(idempotencyKeyHeader, "session-idem-0001")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}

	var got sessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("the response is not a session: %v\nbody: %s", err, rec.Body.String())
	}
	if got.AccessToken == "" {
		t.Fatal("no access token was issued")
	}
	if got.InvestorID != "cfa9ddd6-0377-4cbf-83b8-23b8ca6fa1fa" {
		t.Errorf("investorId = %q", got.InvestorID)
	}
	if !got.ExpiresAt.After(tokenNow) {
		t.Errorf("expiresAt = %v, which is not in the future", got.ExpiresAt)
	}

	// A credential must not be cached.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on a response carrying a credential", cc)
	}
	if rec.Header().Get("ETag") != "" {
		t.Error("a session response must not carry an ETag; it would invite a proxy to store a credential")
	}

	// And the token actually works.
	p := &probe{}
	guarded := callGuarded(t, s.requireInvestor(p.handler), map[string]string{
		"Authorization": "Bearer " + got.AccessToken,
	})
	if guarded.Code != http.StatusOK {
		t.Fatalf("the issued token was refused: %d %s", guarded.Code, guarded.Body.String())
	}
	if p.principal.InvestorID != got.InvestorID {
		t.Errorf("the token resolves to %q, the session said %q", p.principal.InvestorID, got.InvestorID)
	}
}

// TestSessionExchangeRefusesAnUnverifiableToken covers the upstream failure.
func TestSessionExchangeRefusesAnUnverifiableToken(t *testing.T) {
	s := authServer(t, config.EnvLocal)

	req := httptest.NewRequest(http.MethodPost, "/v1/auth/session",
		strings.NewReader(`{"idToken":"forged"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(idempotencyKeyHeader, "session-idem-0002")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401. body: %s", rec.Code, rec.Body.String())
	}
}

// TestAVerifiedIdentityThatIsNotAnInvestorIs403 is a distinction that matters.
//
// The identity is genuine and was verified; what is absent is a relationship with this platform. A 401 would
// send the caller into a login loop that cannot succeed, and a 404 would suggest the endpoint is missing.
//
// It is also the point at which the two identities stay apart: the exchange is a lookup, never a creation. An
// endpoint that minted register identities on presentation of a third party's token would make that third
// party the author of the register.
func TestAVerifiedIdentityThatIsNotAnInvestorIs403(t *testing.T) {
	s := New(Deps{
		Env:           config.EnvLocal,
		Wall:          fixedClock{at: tokenNow},
		SessionSecret: testSecret,
		IDTokens:      fakeIDTokens{accept: "good-upstream-token", subject: "web3auth|stranger"},
		Investors:     fakeInvestors{subject: "web3auth|somebody-else", investorID: "x"},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/auth/session",
		strings.NewReader(`{"idToken":"good-upstream-token"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(idempotencyKeyHeader, "session-idem-0003")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403. body: %s", rec.Code, rec.Body.String())
	}
	env := decodeError(t, rec.Result())
	if env.Error.Code != CodeForbidden {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeForbidden)
	}
}

// TestSessionExchangeRequiresAnIdToken covers the body check.
func TestSessionExchangeRequiresAnIdToken(t *testing.T) {
	s := authServer(t, config.EnvLocal)

	for name, body := range map[string]string{
		"empty object": `{}`,
		"empty string": `{"idToken":""}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/auth/session", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(idempotencyKeyHeader, "session-idem-0004")
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400. body: %s", rec.Code, rec.Body.String())
			}
		})
	}
}
