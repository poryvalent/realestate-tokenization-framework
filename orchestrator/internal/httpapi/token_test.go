package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// testSecret is a 32-byte key, the minimum the signer accepts.
var testSecret = []byte("0123456789abcdef0123456789abcdef")

func testSigner(t *testing.T) *signer {
	t.Helper()
	s, err := newSigner(testSecret)
	if err != nil {
		t.Fatalf("building a signer: %v", err)
	}
	return s
}

var tokenNow = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func investorClaims() Claims {
	return Claims{
		Subject:    "web3auth|abc123",
		Kind:       PrincipalInvestor,
		InvestorID: "cfa9ddd6-0377-4cbf-83b8-23b8ca6fa1fa",
		IssuedAt:   tokenNow.Unix(),
		ExpiresAt:  tokenNow.Add(30 * time.Minute).Unix(),
	}
}

func operatorClaims(role Role) Claims {
	return Claims{
		Subject:   "ops|manager-1",
		Kind:      PrincipalOperator,
		Role:      role,
		IssuedAt:  tokenNow.Unix(),
		ExpiresAt: tokenNow.Add(30 * time.Minute).Unix(),
	}
}

// TestTokenRoundTrip is the baseline.
func TestTokenRoundTrip(t *testing.T) {
	s := testSigner(t)
	want := investorClaims()

	raw, err := s.issue(want)
	if err != nil {
		t.Fatalf("issuing: %v", err)
	}

	got, err := s.verify(raw, tokenNow)
	if err != nil {
		t.Fatalf("verifying a token we just issued: %v", err)
	}
	if got != want {
		t.Fatalf("claims did not survive the round trip:\n got %+v\nwant %+v", got, want)
	}
}

// TestAlgNoneIsRejected is the classic JWT forgery and the reason the algorithm is fixed by the verifier.
//
// A verifier that reads alg from the token accepts "none", which means it verifies nothing while appearing to.
func TestAlgNoneIsRejected(t *testing.T) {
	s := testSigner(t)

	header, err := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(investorClaims())
	if err != nil {
		t.Fatal(err)
	}

	// An unsigned token, as the "none" algorithm specifies.
	forged := b64.EncodeToString(header) + "." + b64.EncodeToString(payload) + "."

	if _, err := s.verify(forged, tokenNow); !errors.Is(err, ErrTokenAlg) && !errors.Is(err, ErrTokenMalformed) {
		t.Fatalf("an alg=none token was not refused: err = %v", err)
	}
}

// TestAlgConfusionIsRejected covers the asymmetric-to-symmetric substitution.
//
// Even though this service only issues HS256, a verifier that honoured the header would accept a token
// claiming RS256 and then have to decide what to do with it. Fixing the algorithm removes the question.
func TestAlgConfusionIsRejected(t *testing.T) {
	s := testSigner(t)

	for _, alg := range []string{"RS256", "ES256", "HS384", "HS512", "", "hs256"} {
		t.Run(alg, func(t *testing.T) {
			header, err := json.Marshal(map[string]string{"alg": alg, "typ": "JWT"})
			if err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal(investorClaims())
			if err != nil {
				t.Fatal(err)
			}

			signingInput := b64.EncodeToString(header) + "." + b64.EncodeToString(payload)
			// Signed correctly with the real key, so only the algorithm claim is wrong. Without the fixed
			// algorithm check this token would verify.
			m := hmac.New(sha256.New, testSecret)
			m.Write([]byte(signingInput))
			token := signingInput + "." + b64.EncodeToString(m.Sum(nil))

			if _, err := s.verify(token, tokenNow); !errors.Is(err, ErrTokenAlg) {
				t.Fatalf("alg %q was accepted or misreported: err = %v", alg, err)
			}
		})
	}
}

// TestATamperedPayloadIsRejected is the MAC doing its job.
func TestATamperedPayloadIsRejected(t *testing.T) {
	s := testSigner(t)

	raw, err := s.issue(investorClaims())
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(raw, ".")

	// Swap the investor identity, keeping the original signature. This is the attack that matters: it would
	// let a holder of one token read another unitholder's position.
	tampered := investorClaims()
	tampered.InvestorID = "00000000-0000-0000-0000-000000000001"
	payload, err := json.Marshal(tampered)
	if err != nil {
		t.Fatal(err)
	}

	forged := parts[0] + "." + b64.EncodeToString(payload) + "." + parts[2]

	if _, err := s.verify(forged, tokenNow); !errors.Is(err, ErrTokenSignature) {
		t.Fatalf("a token with a swapped investorId was not refused: err = %v", err)
	}
}

// TestADifferentKeyDoesNotVerify covers the obvious case explicitly.
func TestADifferentKeyDoesNotVerify(t *testing.T) {
	issuer := testSigner(t)
	other, err := newSigner([]byte("ffffffffffffffffffffffffffffffff"))
	if err != nil {
		t.Fatal(err)
	}

	raw, err := issuer.issue(investorClaims())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := other.verify(raw, tokenNow); !errors.Is(err, ErrTokenSignature) {
		t.Fatalf("a token verified under the wrong key: err = %v", err)
	}
}

// TestExpiryIsEnforcedAtTheBoundary pins the comparison.
//
// A token expiring exactly now is expired. Treating the boundary as valid extends every session by however
// coarse the clock is, which is a silent change to the session length.
func TestExpiryIsEnforcedAtTheBoundary(t *testing.T) {
	s := testSigner(t)
	claims := investorClaims()
	expiry := time.Unix(claims.ExpiresAt, 0)

	raw, err := s.issue(claims)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.verify(raw, expiry.Add(-time.Second)); err != nil {
		t.Fatalf("a token one second before expiry was refused: %v", err)
	}
	if _, err := s.verify(raw, expiry); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("a token exactly at expiry was accepted: err = %v", err)
	}
	if _, err := s.verify(raw, expiry.Add(time.Hour)); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("an hour-expired token was accepted: err = %v", err)
	}
}

// TestClaimsWithoutAnExpiryAreRefused covers both directions.
//
// Refused on issue so one cannot be minted, and refused on verify so a hand-made one cannot be used.
func TestClaimsWithoutAnExpiryAreRefused(t *testing.T) {
	s := testSigner(t)

	claims := investorClaims()
	claims.ExpiresAt = 0

	if _, err := s.issue(claims); !errors.Is(err, ErrTokenClaims) {
		t.Fatalf("issuing a token with no expiry was allowed: err = %v", err)
	}

	// And a correctly signed token with no expiry, as an insider might mint by bypassing issue.
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	header, err := json.Marshal(map[string]string{"alg": tokenAlg, "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	signingInput := b64.EncodeToString(header) + "." + b64.EncodeToString(payload)
	m := hmac.New(sha256.New, testSecret)
	m.Write([]byte(signingInput))
	raw := signingInput + "." + b64.EncodeToString(m.Sum(nil))

	if _, err := s.verify(raw, tokenNow); !errors.Is(err, ErrTokenClaims) {
		t.Fatalf("a signed token with no expiry verified: err = %v", err)
	}
}

// TestKindConfusionIsRefused is why Kind is in the claims.
//
// One signing key serves both audiences, so without a kind an operator token would be a valid investor token
// and the reverse. The coherence rules make each shape impossible to mistake for the other.
func TestKindConfusionIsRefused(t *testing.T) {
	s := testSigner(t)

	cases := []struct {
		name   string
		claims Claims
	}{
		{
			name: "an investor token carrying an operator role",
			claims: Claims{
				Subject: "x", Kind: PrincipalInvestor, InvestorID: "id", Role: RoleTrustee,
				ExpiresAt: tokenNow.Add(time.Hour).Unix(),
			},
		},
		{
			name: "an operator token claiming an investor identity",
			claims: Claims{
				Subject: "x", Kind: PrincipalOperator, Role: RoleManager, InvestorID: "id",
				ExpiresAt: tokenNow.Add(time.Hour).Unix(),
			},
		},
		{
			name: "an investor token with no investor",
			claims: Claims{
				Subject: "x", Kind: PrincipalInvestor,
				ExpiresAt: tokenNow.Add(time.Hour).Unix(),
			},
		},
		{
			name: "an operator token with no role",
			claims: Claims{
				Subject: "x", Kind: PrincipalOperator,
				ExpiresAt: tokenNow.Add(time.Hour).Unix(),
			},
		},
		{
			name: "an operator token with an invented role",
			claims: Claims{
				Subject: "x", Kind: PrincipalOperator, Role: Role("SUPERUSER"),
				ExpiresAt: tokenNow.Add(time.Hour).Unix(),
			},
		},
		{
			name: "no kind at all",
			claims: Claims{
				Subject: "x", ExpiresAt: tokenNow.Add(time.Hour).Unix(),
			},
		},
		{
			name: "no subject",
			claims: Claims{
				Kind: PrincipalInvestor, InvestorID: "id",
				ExpiresAt: tokenNow.Add(time.Hour).Unix(),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.issue(tc.claims); !errors.Is(err, ErrTokenClaims) {
				t.Fatalf("these claims should not be issuable: err = %v", err)
			}
		})
	}
}

// TestMalformedTokensAreRejected covers the shapes a client or an attacker might send.
func TestMalformedTokensAreRejected(t *testing.T) {
	s := testSigner(t)

	cases := map[string]string{
		"empty":                   "",
		"one segment":             "abc",
		"two segments":            "abc.def",
		"four segments":           "a.b.c.d",
		"not base64":              "!!!.???.###",
		"base64 but not json":     b64.EncodeToString([]byte("nope")) + "." + b64.EncodeToString([]byte("nope")) + ".x",
		"standard base64 padding": base64.StdEncoding.EncodeToString([]byte(`{"alg":"HS256"}`)) + ".e30=.x",
		"only separators":         "..",
	}

	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := s.verify(raw, tokenNow); err == nil {
				t.Fatalf("a malformed token verified: %q", raw)
			}
		})
	}
}

// TestAShortSigningKeyIsRefused stops a weak deployment.
//
// An empty or short key must not be silently accepted. The consequence of accepting one is a token anybody
// can mint, which is indistinguishable from having no authentication while looking like having some.
func TestAShortSigningKeyIsRefused(t *testing.T) {
	for _, secret := range [][]byte{nil, {}, []byte("short"), []byte("0123456789abcdef0123456789abcde")} {
		if _, err := newSigner(secret); !errors.Is(err, ErrNoSigningKey) {
			t.Errorf("a %d-byte key was accepted: err = %v", len(secret), err)
		}
	}

	// Exactly the minimum is acceptable.
	if _, err := newSigner([]byte("0123456789abcdef0123456789abcdef")); err != nil {
		t.Errorf("a 32-byte key was refused: %v", err)
	}
}

// TestOperatorTokenRoundTripsEveryRole covers the published set.
func TestOperatorTokenRoundTripsEveryRole(t *testing.T) {
	s := testSigner(t)

	for _, role := range AllRoles() {
		t.Run(string(role), func(t *testing.T) {
			raw, err := s.issue(operatorClaims(role))
			if err != nil {
				t.Fatalf("issuing a %s token: %v", role, err)
			}
			got, err := s.verify(raw, tokenNow)
			if err != nil {
				t.Fatalf("verifying a %s token: %v", role, err)
			}
			if got.Role != role {
				t.Fatalf("role = %q, want %q", got.Role, role)
			}
			if got.Kind != PrincipalOperator {
				t.Fatalf("kind = %q, want OPERATOR", got.Kind)
			}
		})
	}
}

// TestATokenFromTheFutureIsRefusedBeyondSkew covers issued-at.
func TestATokenFromTheFutureIsRefusedBeyondSkew(t *testing.T) {
	s := testSigner(t)

	claims := investorClaims()
	claims.IssuedAt = tokenNow.Add(10 * time.Minute).Unix()
	claims.ExpiresAt = tokenNow.Add(40 * time.Minute).Unix()

	raw, err := s.issue(claims)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.verify(raw, tokenNow); !errors.Is(err, ErrTokenNotYet) {
		t.Fatalf("a token issued ten minutes in the future was accepted: err = %v", err)
	}

	// Inside the skew allowance it is fine, because a small disagreement between two hosts is normal.
	claims.IssuedAt = tokenNow.Add(30 * time.Second).Unix()
	raw, err = s.issue(claims)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.verify(raw, tokenNow); err != nil {
		t.Fatalf("a token within the skew allowance was refused: %v", err)
	}
}

// TestSkewIsNotAppliedToExpiry is the asymmetry that matters.
//
// Tolerating skew on expiry would keep a token working past its end, and expiry is the only way one of these
// sessions ever ends.
func TestSkewIsNotAppliedToExpiry(t *testing.T) {
	s := testSigner(t)
	claims := investorClaims()
	expiry := time.Unix(claims.ExpiresAt, 0)

	raw, err := s.issue(claims)
	if err != nil {
		t.Fatal(err)
	}

	// One second past expiry, well inside the skew allowance applied to issued-at.
	if _, err := s.verify(raw, expiry.Add(time.Second)); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("skew leaked into the expiry check: err = %v", err)
	}
}

// TestTokenCarriesNoPersonalData is the boundary check on the credential itself.
//
// A bearer token travels through logs, proxies and browser storage. It carries an identifier and an
// authorisation; a name or an email in there would be personal data in every one of those places.
func TestTokenCarriesNoPersonalData(t *testing.T) {
	s := testSigner(t)

	raw, err := s.issue(investorClaims())
	if err != nil {
		t.Fatal(err)
	}

	payload, err := b64.DecodeString(strings.Split(raw, ".")[1])
	if err != nil {
		t.Fatal(err)
	}

	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}

	allowed := map[string]bool{"sub": true, "knd": true, "inv": true, "rol": true, "iat": true, "exp": true}
	for name := range fields {
		if !allowed[name] {
			t.Errorf("the token carries an unexpected claim %q; a credential should hold an identifier and an authorisation and nothing else", name)
		}
	}

	lowered := strings.ToLower(string(payload))
	for _, term := range []string{"name", "email", "phone", "pan", "address"} {
		if strings.Contains(lowered, term) {
			t.Errorf("the token payload mentions %q: %s", term, payload)
		}
	}
}
