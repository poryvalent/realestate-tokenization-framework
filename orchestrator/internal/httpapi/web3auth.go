package httpapi

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// Verification of Web3Auth's identity tokens.
//
// These are somebody else's tokens, signed with a key we do not hold, so verification is RS256 against a
// published JWKS. The same reasoning as for our own tokens applies and then some, because here the input is
// entirely attacker-controlled:
//
//   - The algorithm is fixed by the verifier. A token claiming "none" or claiming HS256 with the modulus as
//     the key must not be entertained.
//   - The key is chosen by kid from the JWKS, and a token naming an unknown kid is refused rather than tried
//     against every key. Trying them all turns an unknown key into an oracle.
//   - Issuer and audience are checked. A signature proves who signed, not who it was for; a token minted for
//     another application by the same provider would otherwise be accepted here.
//   - Expiry is required.
//
// The JWKS itself arrives through an interface. In development that is a local RSA key pair, which the
// contract already says; in a deployment it is fetched and cached. Keeping it behind a seam means this logic is
// testable without a network and without pinning a third party's current keys into the test suite.

// Web3Auth verification failures.
var (
	ErrIDTokenMalformed = errors.New("web3auth: token is not a compact JWS")
	ErrIDTokenAlg       = errors.New("web3auth: unexpected algorithm")
	ErrIDTokenKid       = errors.New("web3auth: no key matches the token's kid")
	ErrIDTokenSignature = errors.New("web3auth: signature does not verify")
	ErrIDTokenExpired   = errors.New("web3auth: expired")
	ErrIDTokenIssuer    = errors.New("web3auth: unexpected issuer")
	ErrIDTokenAudience  = errors.New("web3auth: token was not issued for this application")
)

// idTokenAlg is the only algorithm accepted for upstream tokens.
const idTokenAlg = "RS256"

// KeySet supplies the public keys an upstream token may be signed with.
//
// An interface so the development path can be a local key pair and a deployment can fetch and cache a JWKS,
// without this verification logic differing between them. A verifier that behaved differently in development
// would be tested in a configuration nobody runs.
type KeySet interface {
	// KeyByID returns the public key for a kid, or an error if none matches.
	KeyByID(ctx context.Context, kid string) (*rsa.PublicKey, error)
}

// Web3AuthVerifier verifies an upstream token against a key set.
type Web3AuthVerifier struct {
	// Keys supplies signing keys.
	Keys KeySet
	// Issuer is the expected iss claim. Empty skips the check, which is only appropriate in development.
	Issuer string
	// Audience is the expected aud claim, normally the Web3Auth client id.
	Audience string
	// Now reads the current time. The wall clock, not business time: a third party's token expiry has nothing
	// to do with a simulated distribution timeline.
	Now func() time.Time
}

// upstreamClaims is the subset of an upstream token that is used.
type upstreamClaims struct {
	Subject  string `json:"sub"`
	Issuer   string `json:"iss"`
	Audience any    `json:"aud"`
	Expires  int64  `json:"exp"`

	// Web3Auth carries the derived key material in a wallets array. The address is what links to the
	// register, because that is the identity the register and the chain already agree on.
	Wallets []struct {
		Address   string `json:"address"`
		PublicKey string `json:"public_key"`
		Type      string `json:"type"`
		Curve     string `json:"curve"`
	} `json:"wallets"`
}

// Verify checks an upstream token and returns what it establishes.
func (v Web3AuthVerifier) Verify(ctx context.Context, idToken string) (IDTokenClaims, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return IDTokenClaims{}, ErrIDTokenMalformed
	}

	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return IDTokenClaims{}, ErrIDTokenMalformed
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return IDTokenClaims{}, ErrIDTokenMalformed
	}

	// Fixed by the verifier. This is what makes algorithm substitution impossible rather than unlikely.
	if header.Alg != idTokenAlg {
		return IDTokenClaims{}, fmt.Errorf("%w: %q, want %s", ErrIDTokenAlg, header.Alg, idTokenAlg)
	}

	// Selected by kid, never tried against every key. Trying them all would let an attacker learn which keys
	// exist by observing which tokens are rejected differently.
	key, err := v.Keys.KeyByID(ctx, header.Kid)
	if err != nil {
		return IDTokenClaims{}, fmt.Errorf("%w: kid %q", ErrIDTokenKid, header.Kid)
	}

	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return IDTokenClaims{}, ErrIDTokenMalformed
	}

	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return IDTokenClaims{}, ErrIDTokenSignature
	}

	// Only now is the payload interpreted.
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return IDTokenClaims{}, ErrIDTokenMalformed
	}
	var claims upstreamClaims
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return IDTokenClaims{}, ErrIDTokenMalformed
	}

	if claims.Subject == "" {
		return IDTokenClaims{}, fmt.Errorf("%w: no subject", ErrIDTokenMalformed)
	}
	if claims.Expires == 0 {
		return IDTokenClaims{}, fmt.Errorf("%w: no expiry", ErrIDTokenMalformed)
	}

	now := v.Now
	if now == nil {
		// clock.Real is the sanctioned wall-clock reader, but this type is constructed with an explicit Now
		// so that a test never depends on the host clock. A nil one is a programming error, not a default.
		return IDTokenClaims{}, errors.New("web3auth: verifier has no clock")
	}
	if !now().Before(time.Unix(claims.Expires, 0)) {
		return IDTokenClaims{}, ErrIDTokenExpired
	}

	if v.Issuer != "" && claims.Issuer != v.Issuer {
		return IDTokenClaims{}, fmt.Errorf("%w: %q", ErrIDTokenIssuer, claims.Issuer)
	}
	// A signature proves who signed, not who the token was for. Without this, a token the same provider minted
	// for a different application would authenticate here.
	if v.Audience != "" && !audienceContains(claims.Audience, v.Audience) {
		return IDTokenClaims{}, ErrIDTokenAudience
	}

	out := IDTokenClaims{Subject: claims.Subject}
	for _, w := range claims.Wallets {
		if w.Address != "" {
			out.WalletAddress = strings.ToLower(w.Address)
			break
		}
	}
	return out, nil
}

// audienceContains handles aud being either a string or an array, both of which are legal.
func audienceContains(aud any, want string) bool {
	switch typed := aud.(type) {
	case string:
		return typed == want
	case []any:
		for _, entry := range typed {
			if s, ok := entry.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

// StaticKeySet is a fixed set of RSA public keys.
//
// This is the development path the contract describes, and the shape a cached JWKS collapses to once fetched.
type StaticKeySet struct {
	Keys map[string]*rsa.PublicKey
}

// KeyByID returns the key for a kid.
func (s StaticKeySet) KeyByID(_ context.Context, kid string) (*rsa.PublicKey, error) {
	key, ok := s.Keys[kid]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrIDTokenKid, kid)
	}
	return key, nil
}

// ParseJWKS reads an RFC 7517 JWKS document into a key set.
//
// Only RSA keys are read, because only RS256 is accepted, and a key whose parameters do not decode is skipped
// rather than failing the whole document: one unusable key in a rotation set should not take down verification
// for the others.
func ParseJWKS(raw []byte) (StaticKeySet, error) {
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return StaticKeySet{}, fmt.Errorf("parsing a jwks: %w", err)
	}

	out := StaticKeySet{Keys: make(map[string]*rsa.PublicKey, len(doc.Keys))}
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		modulus, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		exponent, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		out.Keys[k.Kid] = &rsa.PublicKey{
			N: new(big.Int).SetBytes(modulus),
			E: int(new(big.Int).SetBytes(exponent).Int64()),
		}
	}

	if len(out.Keys) == 0 {
		return StaticKeySet{}, errors.New("parsing a jwks: it contains no usable RSA keys")
	}
	return out, nil
}
