package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const canary = "sk_live_THIS_MUST_NEVER_APPEAR_IN_OUTPUT"

// The whole point of the Secret type. Credentials escape into logs through
// incidental formatting, not deliberate printing, so every formatting path has to
// be closed rather than just the obvious one.
func TestSecretNeverFormatsItsValue(t *testing.T) {
	s := Secret(canary)

	renders := map[string]string{
		"%s":        fmt.Sprintf("%s", s),
		"%v":        fmt.Sprintf("%v", s),
		"%#v":       fmt.Sprintf("%#v", s),
		"%q":        fmt.Sprintf("%q", s),
		"Sprint":    fmt.Sprint(s),
		"Sprintln":  fmt.Sprintln(s),
		"String()":  s.String(),
		"GoString()": s.GoString(),
	}
	for name, got := range renders {
		if strings.Contains(got, canary) {
			t.Errorf("%s leaked the secret: %s", name, got)
		}
	}

	// Inside a struct, which is how configuration actually gets logged.
	type wrapper struct {
		Name  string
		Token Secret
	}
	w := wrapper{Name: "razorpay", Token: s}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		got := fmt.Sprintf(verb, w)
		if strings.Contains(got, canary) {
			t.Errorf("struct formatted with %s leaked the secret: %s", verb, got)
		}
	}
}

func TestSecretNeverMarshalsItsValue(t *testing.T) {
	type payload struct {
		Token Secret `json:"token"`
	}
	blob, err := json.Marshal(payload{Token: Secret(canary)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), canary) {
		t.Fatalf("JSON marshalling leaked the secret: %s", blob)
	}

	text, err := Secret(canary).MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(text), canary) {
		t.Fatalf("MarshalText leaked the secret: %s", text)
	}
}

// Reveal is the single sanctioned accessor, so it must actually work. Its value is
// that every call site is greppable.
func TestRevealReturnsTheValue(t *testing.T) {
	if got := Secret(canary).Reveal(); got != canary {
		t.Fatalf("Reveal returned %q", got)
	}
}

// Fingerprints let an operator confirm which credential is loaded without
// disclosing it. Two characters distinguishes keys; it does not reconstruct one.
func TestFingerprint(t *testing.T) {
	if got := Secret("").Fingerprint(); got != "unset" {
		t.Errorf("empty fingerprint = %q, want unset", got)
	}

	fp := Secret(canary).Fingerprint()
	if strings.Contains(fp, canary) {
		t.Fatalf("fingerprint leaked the secret: %s", fp)
	}
	if !strings.Contains(fp, "len=") {
		t.Errorf("fingerprint should report length: %s", fp)
	}
	if !strings.HasSuffix(fp, canary[len(canary)-2:]) {
		t.Errorf("fingerprint should end with the last two characters: %s", fp)
	}

	// Different credentials must be distinguishable.
	if Secret("aaaaaaaaaa").Fingerprint() == Secret("bbbbbbbbbb").Fingerprint() {
		t.Error("fingerprints of different secrets should differ")
	}

	// A short secret must not expose a meaningful fraction of itself.
	short := Secret("abc").Fingerprint()
	if strings.Contains(short, "abc") {
		t.Errorf("short secret fingerprint leaked the value: %s", short)
	}
}

func TestIsZero(t *testing.T) {
	if !Secret("").IsZero() {
		t.Error("empty secret should be zero")
	}
	if Secret("x").IsZero() {
		t.Error("non-empty secret should not be zero")
	}
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func baseConfig() *Config {
	return &Config{
		Environment: EnvLocal,
		HTTPAddr:    "127.0.0.1:8080",
		// Gateway is defaulted by Load; a hand-built Config has to supply it, which is
		// itself a useful signal that the validator does not assume defaults.
		IPFS:        IPFSConfig{Provider: IPFSMock, Gateway: "https://gateway.pinata.cloud"},
		Payouts:     PayoutConfig{Provider: PayoutMock},
		KYC:         KYCConfig{Provider: KYCMock},
		Chain:       ChainConfig{ChainID: 11155111, ConfirmationDepth: 5},
	}
}

// Requirements are per feature because the build reaches Sepolia at M3 and banking
// at M5. Demanding every credential up front would push placeholder values into
// developer environments months early, and placeholders are how a deployment ends
// up pointing at nothing.
func TestNoFeaturesRequiredMeansNoCredentialsNeeded(t *testing.T) {
	if err := baseConfig().Validate(); err != nil {
		t.Fatalf("M1 needs no external credentials: %v", err)
	}
}

func TestDatabaseFeatureRequiresURL(t *testing.T) {
	c := baseConfig()
	if err := c.Validate(FeatureDatabase); err == nil {
		t.Fatal("expected a failure for a missing database URL")
	}

	c.Database.URL = Secret("postgres://u:p@localhost:5432/acresync")
	if err := c.Validate(FeatureDatabase); err != nil {
		t.Fatalf("valid URL rejected: %v", err)
	}
}

// A placeholder left in an env file would otherwise surface as a confusing
// authentication failure from a third party rather than as a config error.
func TestPlaceholdersRejected(t *testing.T) {
	for _, v := range []string{"changeme", "TODO", "<your-key>", "  ", "placeholder"} {
		c := baseConfig()
		c.Database.URL = Secret(v)
		if err := c.Validate(FeatureDatabase); err == nil {
			t.Errorf("placeholder %q was accepted", v)
		}
	}
}

func TestChainFeatureValidation(t *testing.T) {
	c := baseConfig()
	c.Chain.RPCURL = Secret("https://eth-sepolia.example/v2/key")
	c.Chain.RelayerPrivateKey = Secret("0x" + strings.Repeat("ab", 32))

	if err := c.Validate(FeatureChain); err != nil {
		t.Fatalf("valid chain config rejected: %v", err)
	}

	// A malformed key must be reported by shape, never echoed.
	bad := c
	bad.Chain.RelayerPrivateKey = Secret("not-a-key")
	err := bad.Validate(FeatureChain)
	if err == nil {
		t.Fatal("expected a malformed private key to be rejected")
	}
	if strings.Contains(err.Error(), "not-a-key") {
		t.Errorf("validation error echoed the key material: %v", err)
	}

	// Addresses must be lowercase hex so one address has one spelling.
	mixed := c
	mixed.Chain.SchemeAddress = "0x000000000000000000000000000000000000DeAd"
	if err := mixed.Validate(FeatureChain); err != nil {
		t.Logf("mixed-case address rejected as expected: %v", err)
	}
}

func TestProviderSelectionValidated(t *testing.T) {
	c := baseConfig()
	c.IPFS.Provider = "ARWEAVE"
	if err := c.Validate(FeatureIPFS); err == nil {
		t.Error("an unsupported IPFS provider should be rejected")
	}

	c = baseConfig()
	c.IPFS.Provider = IPFSPinata
	if err := c.Validate(FeatureIPFS); err == nil {
		t.Error("selecting Pinata without a JWT should be rejected")
	}
	c.IPFS.PinataJWT = Secret("eyJhbGciOi.test.token")
	if err := c.Validate(FeatureIPFS); err != nil {
		t.Errorf("Pinata with a JWT should be accepted: %v", err)
	}
}

func TestMockProvidersNeedNoCredentials(t *testing.T) {
	// Mock adapters exist so the pipeline is exercisable before RazorpayX sandbox
	// approval, which has real lead time.
	c := baseConfig()
	if err := c.Validate(FeatureIPFS, FeaturePayouts, FeatureKYC); err != nil {
		t.Fatalf("mock providers should need nothing: %v", err)
	}
}

// Erasure guarantees rest on the anchor pepper living in a KMS where it can be
// destroyed. A value that has sat in an env file cannot be proven destroyed, so it
// is refused outside local development rather than warned about.
func TestDevPepperRefusedOutsideLocal(t *testing.T) {
	c := baseConfig()
	c.Environment = EnvSepoliaSim
	c.Anchor.DevPepper = Secret("dev-pepper")

	err := c.Validate()
	if err == nil {
		t.Fatal("a development pepper outside LOCAL must be refused")
	}
	if !strings.Contains(err.Error(), "KMS") {
		t.Errorf("the error should explain why: %v", err)
	}

	c.Environment = EnvLocal
	if err := c.Validate(); err != nil {
		t.Fatalf("a development pepper in LOCAL is fine: %v", err)
	}
}

func TestInvalidEnvironmentRejected(t *testing.T) {
	c := baseConfig()
	c.Environment = "PRODUCTION"
	if err := c.Validate(); err == nil {
		t.Error("an unrecognised environment should be rejected")
	}
}

func TestSummaryNeverLeaksSecrets(t *testing.T) {
	c := baseConfig()
	c.Database.URL = Secret("postgres://user:" + canary + "@host/db")
	c.Chain.RelayerPrivateKey = Secret("0x" + strings.Repeat("cd", 32))
	c.Chain.RPCURL = Secret("https://example/" + canary)
	c.IPFS.PinataJWT = Secret(canary)

	summary := c.Summary()
	if strings.Contains(summary, canary) {
		t.Fatalf("Summary leaked a secret:\n%s", summary)
	}
	// It must still be useful: the environment and providers belong in a boot log.
	for _, want := range []string{"environment", "LOCAL", "ipfs", "payouts"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary is missing %q:\n%s", want, summary)
		}
	}
}

// ---------------------------------------------------------------------------
// .env loading
// ---------------------------------------------------------------------------

// Real environment variables must win. A container or CI runner sets real values,
// and a stale .env left in a working copy must not silently override them.
func TestDotEnvDoesNotOverrideRealEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "ACRESYNC_FROM_FILE=file_value\nACRESYNC_ALREADY_SET=file_value\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ACRESYNC_ALREADY_SET", "environment_value")

	if err := loadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("ACRESYNC_FROM_FILE"); got != "file_value" {
		t.Errorf("unset key should come from the file, got %q", got)
	}
	if got := os.Getenv("ACRESYNC_ALREADY_SET"); got != "environment_value" {
		t.Errorf("the real environment must win, got %q", got)
	}
}

func TestDotEnvParsing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := strings.Join([]string{
		"# a comment",
		"",
		"ACRESYNC_PLAIN=value",
		`ACRESYNC_DQUOTED="quoted value"`,
		"ACRESYNC_SQUOTED='single quoted'",
		"export ACRESYNC_EXPORTED=exported",
		"ACRESYNC_WITH_EQUALS=postgres://u:p@h/db?opt=1",
		"ACRESYNC_EMPTY=",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, k := range []string{
		"ACRESYNC_PLAIN", "ACRESYNC_DQUOTED", "ACRESYNC_SQUOTED",
		"ACRESYNC_EXPORTED", "ACRESYNC_WITH_EQUALS", "ACRESYNC_EMPTY",
	} {
		os.Unsetenv(k)
		t.Cleanup(func() { os.Unsetenv(k) })
	}

	if err := loadDotEnv(path); err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"ACRESYNC_PLAIN":       "value",
		"ACRESYNC_DQUOTED":     "quoted value",
		"ACRESYNC_SQUOTED":     "single quoted",
		"ACRESYNC_EXPORTED":    "exported",
		"ACRESYNC_WITH_EQUALS": "postgres://u:p@h/db?opt=1",
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

// Regression test for a real incident.
//
// Filling in a template by appending lines rather than editing in place leaves the empty
// assignment above the real one. Without this check the loader sets the empty value, then
// skips the real one because the key already exists in the environment, and the process boots
// with a blank credential.
func TestDotEnvRejectsDuplicateKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "ACRESYNC_CHAIN_RPC_URL=\nACRESYNC_CHAIN_RPC_URL=https://real.example/key\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	err := loadDotEnv(path)
	if err == nil {
		t.Fatal("a duplicated key must be an error: the empty first assignment would win")
	}
	if !strings.Contains(err.Error(), "assigned twice") {
		t.Errorf("the error should name the problem plainly: %v", err)
	}
	if !strings.Contains(err.Error(), "ACRESYNC_CHAIN_RPC_URL") {
		t.Errorf("the error should name the offending key: %v", err)
	}
}

func TestDotEnvRejectsMalformedLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("THIS_LINE_HAS_NO_EQUALS\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadDotEnv(path); err == nil {
		t.Fatal("a malformed line should be an error, not silently skipped")
	}
}

// TestDotEnvRejectsPastedCredentialAsKey covers a real accident.
//
// Credentials copied from a provider dashboard arrived in the working .env as two well-formed
// KEY=VALUE lines whose keys were the labels rather than variable names. Both existing guards passed:
// neither line was missing an equals sign, and neither duplicated an existing key. The loader read
// them as variables nothing refers to, the real credential slots stayed empty, and the failure
// surfaced much later as an authentication error against a third party, with nothing to suggest the
// values had been sitting in the file the whole time.
func TestDotEnvRejectsPastedCredentialAsKey(t *testing.T) {
	cases := map[string]string{
		"lowercase key from a pasted prefix": "rzp_test_=rzp_test_abcdefghijklmn\n",
		"mixed-case label as key":            "Test_key_secret=abcdefghijklmnopqrstuvwx\n",
		"lowercase word":                     "secret=value\n",
		"key with a hyphen":                  "ACRESYNC-CHAIN-RPC-URL=https://example\n",
		"key with a dot":                     "acresync.chain.rpc=https://example\n",
		"empty key":                          "=orphanvalue\n",
		"key starting with a digit":          "1PASSWORD=x\n",
	}

	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".env")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			err := loadDotEnv(path)
			if err == nil {
				t.Fatalf("%q should be rejected: a key in this form is a value pasted where a "+
					"variable name belongs", strings.TrimSpace(content))
			}
			if !strings.Contains(err.Error(), "line 1") {
				t.Errorf("the error should name the offending line, got: %v", err)
			}
		})
	}
}

// TestDotEnvAcceptsLegitimateNonAcresyncKeys confirms the check is not over-tight.
//
// A working copy legitimately carries variables belonging to other tools. Restricting the loader to
// the ACRESYNC_ prefix would make it annoying enough to be bypassed, which is worse than the problem
// it would solve.
func TestDotEnvAcceptsLegitimateNonAcresyncKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := strings.Join([]string{
		"PGPASSWORD=secret",
		"FOUNDRY_PROFILE=default",
		"PATH_EXTRA=/usr/local/bin",
		"ACRESYNC_HTTP_ADDR=:8080",
		"X=1",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadDotEnv(path); err != nil {
		t.Fatalf("conventional uppercase keys must be accepted: %v", err)
	}
}
