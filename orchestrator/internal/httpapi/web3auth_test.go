package httpapi

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/acresync/orchestrator/internal/store"
)

// upstreamFixture holds a key pair and mints tokens with it.
type upstreamFixture struct {
	key      *rsa.PrivateKey
	kid      string
	verifier Web3AuthVerifier
}

func newUpstreamFixture(t *testing.T) *upstreamFixture {
	t.Helper()

	// 2048 bits: the smallest size worth testing against, and fast enough to generate per test run.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}

	f := &upstreamFixture{key: key, kid: "test-kid-1"}
	f.verifier = Web3AuthVerifier{
		Keys:     StaticKeySet{Keys: map[string]*rsa.PublicKey{f.kid: &key.PublicKey}},
		Issuer:   "https://api-auth.web3auth.io",
		Audience: "test-client-id",
		Now:      func() time.Time { return tokenNow },
	}
	return f
}

// mint signs a token with the given header and payload maps.
func (f *upstreamFixture) mint(t *testing.T, header, payload map[string]any) string {
	t.Helper()

	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}

	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(payloadJSON)

	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}

	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func (f *upstreamFixture) goodHeader() map[string]any {
	return map[string]any{"alg": "RS256", "typ": "JWT", "kid": f.kid}
}

func (f *upstreamFixture) goodPayload() map[string]any {
	return map[string]any{
		"sub": "web3auth|abc123",
		"iss": "https://api-auth.web3auth.io",
		"aud": "test-client-id",
		"exp": tokenNow.Add(time.Hour).Unix(),
		"wallets": []map[string]any{
			{"address": "0xF858a002402e968D1C7eC62c3824d30520e9Ca38", "type": "ethereum", "curve": "secp256k1"},
		},
	}
}

// TestUpstreamTokenVerifies is the baseline, and checks the wallet is lowercased.
//
// Web3Auth hands out EIP-55 checksummed addresses; the register stores lowercase and the column enforces it.
// One address must have exactly one spelling or the lookup silently misses.
func TestUpstreamTokenVerifies(t *testing.T) {
	f := newUpstreamFixture(t)

	got, err := f.verifier.Verify(context.Background(), f.mint(t, f.goodHeader(), f.goodPayload()))
	if err != nil {
		t.Fatalf("a well-formed token was refused: %v", err)
	}

	if got.Subject != "web3auth|abc123" {
		t.Errorf("subject = %q", got.Subject)
	}
	if got.WalletAddress != "0xf858a002402e968d1c7ec62c3824d30520e9ca38" {
		t.Errorf("wallet = %q, want the lowercased form", got.WalletAddress)
	}
}

// TestUpstreamAlgNoneIsRejected is the forgery that costs everything if it works.
func TestUpstreamAlgNoneIsRejected(t *testing.T) {
	f := newUpstreamFixture(t)

	header := f.goodHeader()
	header["alg"] = "none"
	headerJSON, _ := json.Marshal(header)
	payloadJSON, _ := json.Marshal(f.goodPayload())
	unsigned := base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(payloadJSON) + "."

	if _, err := f.verifier.Verify(context.Background(), unsigned); !errors.Is(err, ErrIDTokenAlg) {
		t.Fatalf("an unsigned token was not refused for its algorithm: %v", err)
	}
}

// TestUpstreamAlgConfusionIsRejected covers substituting a symmetric algorithm.
//
// The classic attack signs with HMAC using the RSA modulus as the key. A verifier that read alg from the token
// would have to decide what to do; fixing it removes the decision.
func TestUpstreamAlgConfusionIsRejected(t *testing.T) {
	f := newUpstreamFixture(t)

	for _, alg := range []string{"HS256", "RS512", "ES256", "PS256", "", "rs256"} {
		t.Run(alg, func(t *testing.T) {
			header := f.goodHeader()
			header["alg"] = alg

			// Still signed correctly with RSA, so only the algorithm claim differs.
			token := f.mint(t, header, f.goodPayload())

			if _, err := f.verifier.Verify(context.Background(), token); !errors.Is(err, ErrIDTokenAlg) {
				t.Fatalf("alg %q was accepted or misreported: %v", alg, err)
			}
		})
	}
}

// TestAnUnknownKidIsRefused stops the verifier becoming an oracle.
//
// Trying every key would let an attacker learn which keys exist from the difference between failures.
func TestAnUnknownKidIsRefused(t *testing.T) {
	f := newUpstreamFixture(t)

	header := f.goodHeader()
	header["kid"] = "a-kid-we-do-not-have"

	if _, err := f.verifier.Verify(context.Background(), f.mint(t, header, f.goodPayload())); !errors.Is(err, ErrIDTokenKid) {
		t.Fatalf("an unknown kid was not refused: %v", err)
	}
}

// TestATokenSignedByAnotherKeyIsRefused is the point of verifying at all.
func TestATokenSignedByAnotherKeyIsRefused(t *testing.T) {
	f := newUpstreamFixture(t)

	attacker := newUpstreamFixture(t)
	// The attacker signs with their own key but names the kid we trust.
	forged := attacker.mint(t, f.goodHeader(), f.goodPayload())

	if _, err := f.verifier.Verify(context.Background(), forged); !errors.Is(err, ErrIDTokenSignature) {
		t.Fatalf("a token signed by the wrong key verified: %v", err)
	}
}

// TestATamperedUpstreamPayloadIsRefused covers changing the subject after signing.
func TestATamperedUpstreamPayloadIsRefused(t *testing.T) {
	f := newUpstreamFixture(t)
	original := f.mint(t, f.goodHeader(), f.goodPayload())
	parts := strings.Split(original, ".")

	swapped := f.goodPayload()
	swapped["wallets"] = []map[string]any{{"address": "0x0000000000000000000000000000000000000001"}}
	payloadJSON, _ := json.Marshal(swapped)

	forged := parts[0] + "." + base64.RawURLEncoding.EncodeToString(payloadJSON) + "." + parts[2]

	if _, err := f.verifier.Verify(context.Background(), forged); !errors.Is(err, ErrIDTokenSignature) {
		t.Fatalf("a token with a swapped wallet verified: %v", err)
	}
}

// TestTheWrongAudienceIsRefused is the check a signature alone does not give.
//
// A signature proves who signed, not who the token was for. Without this, a token the same provider minted for
// another application would authenticate here.
func TestTheWrongAudienceIsRefused(t *testing.T) {
	f := newUpstreamFixture(t)

	payload := f.goodPayload()
	payload["aud"] = "somebody-elses-client-id"

	if _, err := f.verifier.Verify(context.Background(), f.mint(t, f.goodHeader(), payload)); !errors.Is(err, ErrIDTokenAudience) {
		t.Fatalf("a token for another application was accepted: %v", err)
	}
}

// TestAnAudienceArrayIsHandled covers the legal array form.
func TestAnAudienceArrayIsHandled(t *testing.T) {
	f := newUpstreamFixture(t)

	payload := f.goodPayload()
	payload["aud"] = []string{"another-app", "test-client-id"}

	if _, err := f.verifier.Verify(context.Background(), f.mint(t, f.goodHeader(), payload)); err != nil {
		t.Fatalf("an aud array containing our client id was refused: %v", err)
	}

	payload["aud"] = []string{"another-app", "a-third-app"}
	if _, err := f.verifier.Verify(context.Background(), f.mint(t, f.goodHeader(), payload)); !errors.Is(err, ErrIDTokenAudience) {
		t.Fatalf("an aud array without our client id was accepted: %v", err)
	}
}

// TestTheWrongIssuerIsRefused covers iss.
func TestTheWrongIssuerIsRefused(t *testing.T) {
	f := newUpstreamFixture(t)

	payload := f.goodPayload()
	payload["iss"] = "https://evil.example"

	if _, err := f.verifier.Verify(context.Background(), f.mint(t, f.goodHeader(), payload)); !errors.Is(err, ErrIDTokenIssuer) {
		t.Fatalf("a token from another issuer was accepted: %v", err)
	}
}

// TestAnExpiredUpstreamTokenIsRefused covers exp, at the boundary.
func TestAnExpiredUpstreamTokenIsRefused(t *testing.T) {
	f := newUpstreamFixture(t)

	payload := f.goodPayload()
	payload["exp"] = tokenNow.Unix()

	if _, err := f.verifier.Verify(context.Background(), f.mint(t, f.goodHeader(), payload)); !errors.Is(err, ErrIDTokenExpired) {
		t.Fatalf("a token expiring exactly now was accepted: %v", err)
	}

	payload["exp"] = tokenNow.Add(-time.Hour).Unix()
	if _, err := f.verifier.Verify(context.Background(), f.mint(t, f.goodHeader(), payload)); !errors.Is(err, ErrIDTokenExpired) {
		t.Fatalf("an expired token was accepted: %v", err)
	}
}

// TestAnUpstreamTokenWithoutExpiryOrSubjectIsRefused covers the required claims.
func TestAnUpstreamTokenWithoutExpiryOrSubjectIsRefused(t *testing.T) {
	f := newUpstreamFixture(t)

	t.Run("no expiry", func(t *testing.T) {
		payload := f.goodPayload()
		delete(payload, "exp")
		if _, err := f.verifier.Verify(context.Background(), f.mint(t, f.goodHeader(), payload)); err == nil {
			t.Fatal("a token with no expiry was accepted")
		}
	})

	t.Run("no subject", func(t *testing.T) {
		payload := f.goodPayload()
		delete(payload, "sub")
		if _, err := f.verifier.Verify(context.Background(), f.mint(t, f.goodHeader(), payload)); err == nil {
			t.Fatal("a token with no subject was accepted")
		}
	})
}

// TestATokenWithNoWalletVerifiesButResolvesToNothing separates the two failures.
//
// The token is genuine, so verification succeeds. What it cannot do is identify a holder, and the resolver is
// where that is refused rather than guessed at.
func TestATokenWithNoWalletVerifiesButResolvesToNothing(t *testing.T) {
	f := newUpstreamFixture(t)

	payload := f.goodPayload()
	delete(payload, "wallets")

	claims, err := f.verifier.Verify(context.Background(), f.mint(t, f.goodHeader(), payload))
	if err != nil {
		t.Fatalf("a token without wallets should still verify: %v", err)
	}
	if claims.WalletAddress != "" {
		t.Fatalf("wallet = %q, want empty", claims.WalletAddress)
	}

	resolver := InvestorsByWallet{Wallets: stubWallets{}}
	if _, err := resolver.Resolve(context.Background(), claims); !errors.Is(err, ErrNoSuchSubject) {
		t.Fatalf("a verified identity with no wallet should resolve to nothing, got %v", err)
	}
}

type stubWallets struct {
	address    string
	investorID string
}

func (s stubWallets) ResolveWallet(_ context.Context, address string) (string, error) {
	if address != s.address {
		return "", store.ErrNoInvestorForIdentity
	}
	return s.investorID, nil
}

// TestMalformedUpstreamTokensAreRejected covers the shapes an attacker sends.
func TestMalformedUpstreamTokensAreRejected(t *testing.T) {
	f := newUpstreamFixture(t)

	for name, raw := range map[string]string{
		"empty":         "",
		"one segment":   "abc",
		"two segments":  "abc.def",
		"four segments": "a.b.c.d",
		"not base64":    "!!!.???.###",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.verifier.Verify(context.Background(), raw); err == nil {
				t.Fatalf("a malformed token verified: %q", raw)
			}
		})
	}
}

// TestParseJWKS reads a document in the shape a provider publishes.
func TestParseJWKS(t *testing.T) {
	f := newUpstreamFixture(t)

	doc := map[string]any{
		"keys": []map[string]any{
			{
				"kty": "RSA",
				"kid": f.kid,
				"n":   base64.RawURLEncoding.EncodeToString(f.key.PublicKey.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(f.key.PublicKey.E)).Bytes()),
			},
			// A key of another type, which must be skipped rather than breaking the document.
			{"kty": "EC", "kid": "ec-key", "crv": "P-256"},
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	set, err := ParseJWKS(raw)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if len(set.Keys) != 1 {
		t.Fatalf("got %d keys, want only the RSA one", len(set.Keys))
	}

	key, err := set.KeyByID(context.Background(), f.kid)
	if err != nil {
		t.Fatalf("looking up the kid: %v", err)
	}
	if key.N.Cmp(f.key.PublicKey.N) != 0 || key.E != f.key.PublicKey.E {
		t.Fatal("the parsed key does not match the original")
	}

	// And a token signed by that key verifies through the parsed set.
	verifier := f.verifier
	verifier.Keys = set
	if _, err := verifier.Verify(context.Background(), f.mint(t, f.goodHeader(), f.goodPayload())); err != nil {
		t.Fatalf("a token did not verify against the parsed jwks: %v", err)
	}
}

// TestParseJWKSRejectsAnUnusableDocument covers the empty case.
//
// A document with no usable key must be an error, not an empty set. An empty set would make every token fail
// with an unknown-kid error, which reads as a token problem rather than a configuration one.
func TestParseJWKSRejectsAnUnusableDocument(t *testing.T) {
	for name, raw := range map[string]string{
		"not json":        `nope`,
		"no keys":         `{"keys":[]}`,
		"no rsa keys":     `{"keys":[{"kty":"EC","kid":"x"}]}`,
		"rsa without kid": `{"keys":[{"kty":"RSA","n":"AQAB","e":"AQAB"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseJWKS([]byte(raw)); err == nil {
				t.Fatalf("an unusable jwks parsed cleanly: %s", raw)
			}
		})
	}
}

// TestAVerifierWithNoClockRefuses stops a nil clock reading as "never expires".
func TestAVerifierWithNoClockRefuses(t *testing.T) {
	f := newUpstreamFixture(t)
	v := f.verifier
	v.Now = nil

	if _, err := v.Verify(context.Background(), f.mint(t, f.goodHeader(), f.goodPayload())); err == nil {
		t.Fatal("a verifier with no clock accepted a token, so expiry was never checked")
	}
}
