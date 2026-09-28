package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AcreSync's own session tokens.
//
// # Why this is written out rather than imported
//
// A JWT library would be a new dependency for one algorithm in one direction: this service signs these
// tokens and this service verifies them. HS256 over a compact JWS is a MAC, a base64 encoding and a
// constant-time comparison, all of which are in the standard library.
//
// The risk in hand-rolling JWT is well documented and specific, so it is worth naming what is done about it:
//
//   - The algorithm is not read from the token. A verifier that trusts the header's alg accepts "none" and
//     verifies nothing, and accepts a symmetric alg against an asymmetric key so that a public key becomes a
//     signing secret. Here the expected algorithm is fixed by the verifier and the header must match it.
//   - The signature is compared with hmac.Equal, not ==, so a comparison cannot leak by timing.
//   - Expiry is required. A token without one never expires, and a session that cannot end is not a session.
//   - The signature is checked before any claim is parsed, so nothing unverified reaches a decision.
//
// This is deliberately not a general JWT implementation. It verifies exactly the tokens this service issues.
// Web3Auth's tokens are somebody else's RS256 and are handled separately.

// tokenAlg is the only algorithm accepted for our own tokens.
const tokenAlg = "HS256"

// Token failures. Each says what is wrong without saying which part of the secret was close, because a
// verifier that distinguishes "bad signature" from "wrong key" tells an attacker which to vary.
var (
	ErrTokenMalformed = errors.New("token: not a compact JWS")
	ErrTokenAlg       = errors.New("token: unexpected algorithm")
	ErrTokenSignature = errors.New("token: signature does not verify")
	ErrTokenExpired   = errors.New("token: expired")
	ErrTokenNotYet    = errors.New("token: not valid yet")
	ErrTokenClaims    = errors.New("token: claims are not usable")
	ErrNoSigningKey   = errors.New("token: no signing key is configured")
)

// minSecretLen is the shortest signing key accepted.
//
// HMAC-SHA256 has a 32-byte block-sized key, and a shorter secret is simply a weaker one. Refusing at
// construction rather than warning means a deployment cannot accidentally run on a four-character key.
const minSecretLen = 32

// Claims is the payload of an AcreSync session token.
//
// Deliberately small. A token is a bearer credential that travels through logs, proxies and browser storage,
// so it carries an identifier and an authorisation and nothing describing a person. No name, no email, no
// PAN: the same boundary the chain and the public API respect.
type Claims struct {
	// Subject is the upstream identity, the Web3Auth subject for an investor.
	Subject string `json:"sub"`
	// Kind separates an investor session from an operator session, so a token issued for one audience
	// cannot be presented to the other even if the signing key is shared.
	Kind PrincipalKind `json:"knd"`
	// InvestorID is the register identity. Empty for an operator.
	InvestorID string `json:"inv,omitempty"`
	// Role is the operator role. Empty for an investor.
	Role Role `json:"rol,omitempty"`

	IssuedAt  int64 `json:"iat"`
	ExpiresAt int64 `json:"exp"`
}

// Valid checks the claims are internally coherent.
//
// Checked on issue and on verify. A token whose kind and fields disagree, such as an investor token carrying
// an operator role, should never exist; refusing to mint one and refusing to honour one are both cheap.
func (c Claims) Valid() error {
	if c.Subject == "" {
		return fmt.Errorf("%w: subject is empty", ErrTokenClaims)
	}
	if c.ExpiresAt == 0 {
		return fmt.Errorf("%w: no expiry, and a session that cannot end is not a session", ErrTokenClaims)
	}

	switch c.Kind {
	case PrincipalInvestor:
		if c.InvestorID == "" {
			return fmt.Errorf("%w: an investor token must identify an investor", ErrTokenClaims)
		}
		if c.Role != "" {
			return fmt.Errorf("%w: an investor token must not carry an operator role", ErrTokenClaims)
		}
	case PrincipalOperator:
		if !c.Role.Valid() {
			return fmt.Errorf("%w: operator role %q is not one the contract publishes", ErrTokenClaims, c.Role)
		}
		if c.InvestorID != "" {
			return fmt.Errorf("%w: an operator token must not claim an investor identity", ErrTokenClaims)
		}
	default:
		return fmt.Errorf("%w: unknown principal kind %q", ErrTokenClaims, c.Kind)
	}
	return nil
}

// signer issues and verifies AcreSync session tokens.
type signer struct {
	secret []byte
}

// newSigner refuses a key that is absent or too short.
//
// Returning an error rather than falling back to a generated or empty key is the point. An empty signing key
// must never mean "sign with nothing"; it has to mean authentication is unavailable, so that a
// misconfiguration removes the authenticated endpoints instead of leaving them forgeable.
func newSigner(secret []byte) (*signer, error) {
	if len(secret) == 0 {
		return nil, ErrNoSigningKey
	}
	if len(secret) < minSecretLen {
		return nil, fmt.Errorf("%w: the signing key is %d bytes, which is below the %d-byte minimum",
			ErrNoSigningKey, len(secret), minSecretLen)
	}
	return &signer{secret: secret}, nil
}

// b64 is base64url without padding, as compact JWS requires.
var b64 = base64.RawURLEncoding

// issue mints a signed token.
func (s *signer) issue(c Claims) (string, error) {
	if err := c.Valid(); err != nil {
		return "", err
	}

	header, err := json.Marshal(struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}{Alg: tokenAlg, Typ: "JWT"})
	if err != nil {
		return "", err
	}

	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}

	signingInput := b64.EncodeToString(header) + "." + b64.EncodeToString(payload)
	return signingInput + "." + b64.EncodeToString(s.mac(signingInput)), nil
}

func (s *signer) mac(signingInput string) []byte {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(signingInput))
	return m.Sum(nil)
}

// verify checks a token and returns its claims.
//
// The order is deliberate: structure, then algorithm, then signature, then claims. Nothing from the payload
// is interpreted until the signature has been shown to hold, so an attacker cannot influence a decision with
// a claim in an unverified token.
func (s *signer) verify(raw string, now time.Time) (Claims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Claims{}, ErrTokenMalformed
	}

	headerJSON, err := b64.DecodeString(parts[0])
	if err != nil {
		return Claims{}, ErrTokenMalformed
	}
	var header struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return Claims{}, ErrTokenMalformed
	}

	// Fixed by the verifier, not taken from the token. This is the check that makes "alg":"none" and
	// algorithm confusion impossible rather than merely unlikely.
	if header.Alg != tokenAlg {
		return Claims{}, fmt.Errorf("%w: %q, want %s", ErrTokenAlg, header.Alg, tokenAlg)
	}

	signature, err := b64.DecodeString(parts[2])
	if err != nil {
		return Claims{}, ErrTokenMalformed
	}
	if !hmac.Equal(signature, s.mac(parts[0]+"."+parts[1])) {
		return Claims{}, ErrTokenSignature
	}

	payloadJSON, err := b64.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrTokenMalformed
	}
	var c Claims
	if err := json.Unmarshal(payloadJSON, &c); err != nil {
		return Claims{}, ErrTokenMalformed
	}

	if err := c.Valid(); err != nil {
		return Claims{}, err
	}

	// Expiry is exclusive at the boundary: a token expiring exactly now is expired. Treating the boundary as
	// still valid would extend every session by however coarse the clock happens to be.
	if !now.Before(time.Unix(c.ExpiresAt, 0)) {
		return Claims{}, ErrTokenExpired
	}
	if c.IssuedAt != 0 && now.Add(clockSkewAllowance).Before(time.Unix(c.IssuedAt, 0)) {
		return Claims{}, ErrTokenNotYet
	}

	return c, nil
}

// clockSkewAllowance tolerates a small disagreement on issued-at.
//
// Applied only to issued-at, never to expiry. Allowing skew on expiry would keep a revoked-by-expiry token
// working past its end, which is the direction that matters.
const clockSkewAllowance = 60 * time.Second

// MintDevToken signs a token under the session secret, for local development tooling only.
//
// There is no operator sign-in yet, so an operator console run against a LOCAL API has no other way to obtain
// a token. It grants nothing the secret does not already grant: whoever holds the secret can sign tokens with
// or without this function. The command that calls it refuses to run outside LOCAL.
func MintDevToken(secret []byte, c Claims) (string, error) {
	s, err := newSigner(secret)
	if err != nil {
		return "", err
	}
	return s.issue(c)
}
