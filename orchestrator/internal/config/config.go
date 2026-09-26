// Package config loads AcreSync configuration from the environment.
//
// Two design choices worth stating.
//
// Credentials are typed as Secret, not string, so that printing a Config cannot
// leak one. See secret.go.
//
// Requirements are declared per feature rather than globally. The build reaches
// Sepolia at M3 and banking at M5, so a loader that demanded every credential up
// front would force placeholder values into a developer's environment months
// before they mean anything, and placeholders are how a real deployment ends up
// pointing at nothing.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Feature names a capability group whose credentials must be present and valid.
type Feature string

const (
	FeatureDatabase Feature = "database"
	FeatureChain    Feature = "chain"
	FeatureIPFS     Feature = "ipfs"
	FeaturePayouts  Feature = "payouts"
	FeatureKYC      Feature = "kyc"
	FeatureAnchor   Feature = "anchor"
)

// Environment distinguishes a local run from the Sepolia demo. It is written into
// every on-chain anchor and every admin action, so a simulated record can never
// be mistaken for a live one.
type Environment string

const (
	EnvLocal      Environment = "LOCAL"
	EnvSepoliaSim Environment = "SEPOLIA_SIM"
)

func (e Environment) Valid() bool { return e == EnvLocal || e == EnvSepoliaSim }

// Provider selections. MOCK implementations exist for every external dependency
// so the full pipeline is exercisable before any account is approved. That is not
// only convenience: RazorpayX sandbox access needs a business approval with real
// lead time, and the build cannot be blocked on it.
type (
	IPFSProvider   string
	PayoutProvider string
	KYCProvider    string
)

const (
	IPFSMock   IPFSProvider = "MOCK"
	IPFSPinata IPFSProvider = "PINATA"

	PayoutMock       PayoutProvider = "MOCK"
	PayoutRazorpayX  PayoutProvider = "RAZORPAYX_SANDBOX"

	KYCMock     KYCProvider = "MOCK"
	KYCDecentro KYCProvider = "DECENTRO"
	KYCSetu     KYCProvider = "SETU"
)

type Config struct {
	Environment Environment
	HTTPAddr    string

	Database DatabaseConfig
	Anchor   AnchorConfig
	Chain    ChainConfig
	IPFS     IPFSConfig
	Payouts  PayoutConfig
	KYC      KYCConfig
	Web3Auth Web3AuthConfig
}

type DatabaseConfig struct {
	URL Secret
}

type AnchorConfig struct {
	// Identifier of the KMS key that peppers investor anchors.
	PepperKeyID string

	// Local development pepper. Refused outside LOCAL: a pepper in an env file is
	// a pepper in every backup of that file, and the whole point of the pepper is
	// that destroying it severs on-chain linkage irreversibly.
	DevPepper Secret
}

type ChainConfig struct {
	RPCURL            Secret
	ChainID           int64
	RelayerPrivateKey Secret
	ConfirmationDepth int

	RolesAddress   string
	BallotAddress  string
	SchemeAddress  string
	EtherscanAPIKey Secret
}

type IPFSConfig struct {
	Provider  IPFSProvider
	PinataJWT Secret
	Gateway   string
}

type PayoutConfig struct {
	Provider      PayoutProvider
	KeyID         string
	KeySecret     Secret
	AccountNumber string
}

type KYCConfig struct {
	Provider     KYCProvider
	ClientID     string
	ClientSecret Secret
}

type Web3AuthConfig struct {
	ClientID string
	Network  string
}

var (
	ErrMissing     = errors.New("config: required value is absent")
	ErrPlaceholder = errors.New("config: value looks like an unfilled placeholder")
	ErrInvalid     = errors.New("config: value is not valid")
)

var (
	addressRe = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
	privKeyRe = regexp.MustCompile(`^0x[0-9a-fA-F]{64}$`)
)

// Load reads configuration from the environment, then validates the groups named
// in required.
//
// If a .env file is present at or above the working directory it is loaded first,
// but existing environment variables always win. That ordering matters: a
// container or CI runner sets real values in the environment, and a stale .env
// left in a working copy must not silently override them.
func Load(required ...Feature) (*Config, error) {
	if path, err := findDotEnv(); err == nil && path != "" {
		if err := loadDotEnv(path); err != nil {
			return nil, fmt.Errorf("config: loading %s: %w", path, err)
		}
	}

	c := &Config{
		Environment: Environment(getEnv("ACRESYNC_ENVIRONMENT", string(EnvLocal))),
		HTTPAddr:    getEnv("ACRESYNC_HTTP_ADDR", "127.0.0.1:8080"),

		Database: DatabaseConfig{
			URL: Secret(os.Getenv("ACRESYNC_DATABASE_URL")),
		},
		Anchor: AnchorConfig{
			PepperKeyID: os.Getenv("ACRESYNC_ANCHOR_PEPPER_KEY_ID"),
			DevPepper:   Secret(os.Getenv("ACRESYNC_ANCHOR_PEPPER_DEV")),
		},
		Chain: ChainConfig{
			RPCURL:            Secret(os.Getenv("ACRESYNC_CHAIN_RPC_URL")),
			ChainID:           getEnvInt64("ACRESYNC_CHAIN_ID", 11155111),
			RelayerPrivateKey: Secret(os.Getenv("ACRESYNC_RELAYER_PRIVATE_KEY")),
			ConfirmationDepth: int(getEnvInt64("ACRESYNC_CONFIRMATION_DEPTH", 5)),
			RolesAddress:      os.Getenv("ACRESYNC_ROLES_ADDRESS"),
			BallotAddress:     os.Getenv("ACRESYNC_BALLOT_ADDRESS"),
			SchemeAddress:     os.Getenv("ACRESYNC_SCHEME_ADDRESS"),
			EtherscanAPIKey:   Secret(os.Getenv("ACRESYNC_ETHERSCAN_API_KEY")),
		},
		IPFS: IPFSConfig{
			Provider:  IPFSProvider(getEnv("ACRESYNC_IPFS_PROVIDER", string(IPFSMock))),
			PinataJWT: Secret(os.Getenv("ACRESYNC_PINATA_JWT")),
			Gateway:   getEnv("ACRESYNC_IPFS_GATEWAY", "https://gateway.pinata.cloud"),
		},
		Payouts: PayoutConfig{
			Provider:      PayoutProvider(getEnv("ACRESYNC_PAYOUT_PROVIDER", string(PayoutMock))),
			KeyID:         os.Getenv("ACRESYNC_RAZORPAY_KEY_ID"),
			KeySecret:     Secret(os.Getenv("ACRESYNC_RAZORPAY_KEY_SECRET")),
			AccountNumber: os.Getenv("ACRESYNC_RAZORPAY_ACCOUNT_NUMBER"),
		},
		KYC: KYCConfig{
			Provider:     KYCProvider(getEnv("ACRESYNC_KYC_PROVIDER", string(KYCMock))),
			ClientID:     os.Getenv("ACRESYNC_KYC_CLIENT_ID"),
			ClientSecret: Secret(os.Getenv("ACRESYNC_KYC_CLIENT_SECRET")),
		},
		Web3Auth: Web3AuthConfig{
			ClientID: os.Getenv("ACRESYNC_WEB3AUTH_CLIENT_ID"),
			Network:  getEnv("ACRESYNC_WEB3AUTH_NETWORK", "sapphire_devnet"),
		},
	}

	if err := c.Validate(required...); err != nil {
		return nil, err
	}
	return c, nil
}

// Validate checks the always-required values plus the named feature groups.
func (c *Config) Validate(required ...Feature) error {
	var problems []string

	if !c.Environment.Valid() {
		problems = append(problems, fmt.Sprintf("ACRESYNC_ENVIRONMENT=%q must be LOCAL or SEPOLIA_SIM", c.Environment))
	}
	if c.HTTPAddr == "" {
		problems = append(problems, "ACRESYNC_HTTP_ADDR is empty")
	}

	// A development pepper outside LOCAL is refused rather than warned about.
	// Erasure guarantees rest on that key being destroyable in a KMS, and a value
	// that has sat in an env file cannot be proven destroyed.
	if c.Environment != EnvLocal && !c.Anchor.DevPepper.IsZero() {
		problems = append(problems,
			"ACRESYNC_ANCHOR_PEPPER_DEV is set outside LOCAL: the anchor pepper must live in a KMS, "+
				"because DPDP erasure depends on being able to destroy it")
	}

	for _, f := range required {
		switch f {
		case FeatureDatabase:
			problems = append(problems, requireSecret("ACRESYNC_DATABASE_URL", c.Database.URL)...)

		case FeatureAnchor:
			problems = append(problems, requireString("ACRESYNC_ANCHOR_PEPPER_KEY_ID", c.Anchor.PepperKeyID)...)
			if c.Environment == EnvLocal && c.Anchor.DevPepper.IsZero() {
				problems = append(problems, "ACRESYNC_ANCHOR_PEPPER_DEV must be set in LOCAL mode")
			}

		case FeatureChain:
			problems = append(problems, requireSecret("ACRESYNC_CHAIN_RPC_URL", c.Chain.RPCURL)...)
			problems = append(problems, requireSecret("ACRESYNC_RELAYER_PRIVATE_KEY", c.Chain.RelayerPrivateKey)...)
			if !c.Chain.RelayerPrivateKey.IsZero() && !privKeyRe.MatchString(c.Chain.RelayerPrivateKey.Reveal()) {
				// The key itself is never echoed, only the shape complaint.
				problems = append(problems, "ACRESYNC_RELAYER_PRIVATE_KEY must be 0x followed by 64 hex characters")
			}
			if c.Chain.ChainID <= 0 {
				problems = append(problems, "ACRESYNC_CHAIN_ID must be positive")
			}
			// Below five confirmations, a reorg can invalidate an anchor that a
			// fiat instruction was already gated on, and fiat does not reorg back.
			if c.Chain.ConfirmationDepth < 1 {
				problems = append(problems, "ACRESYNC_CONFIRMATION_DEPTH must be at least 1")
			}
			for name, addr := range map[string]string{
				"ACRESYNC_ROLES_ADDRESS":  c.Chain.RolesAddress,
				"ACRESYNC_BALLOT_ADDRESS": c.Chain.BallotAddress,
				"ACRESYNC_SCHEME_ADDRESS": c.Chain.SchemeAddress,
			} {
				if addr != "" && !addressRe.MatchString(addr) {
					problems = append(problems, name+" must be 0x followed by 40 hex characters")
				}
			}

		case FeatureIPFS:
			switch c.IPFS.Provider {
			case IPFSMock:
			case IPFSPinata:
				problems = append(problems, requireSecret("ACRESYNC_PINATA_JWT", c.IPFS.PinataJWT)...)
				problems = append(problems, requireString("ACRESYNC_IPFS_GATEWAY", c.IPFS.Gateway)...)
			default:
				problems = append(problems, fmt.Sprintf("ACRESYNC_IPFS_PROVIDER=%q must be MOCK or PINATA", c.IPFS.Provider))
			}

		case FeaturePayouts:
			switch c.Payouts.Provider {
			case PayoutMock:
			case PayoutRazorpayX:
				problems = append(problems, requireString("ACRESYNC_RAZORPAY_KEY_ID", c.Payouts.KeyID)...)
				problems = append(problems, requireSecret("ACRESYNC_RAZORPAY_KEY_SECRET", c.Payouts.KeySecret)...)
				problems = append(problems, requireString("ACRESYNC_RAZORPAY_ACCOUNT_NUMBER", c.Payouts.AccountNumber)...)
			default:
				problems = append(problems, fmt.Sprintf("ACRESYNC_PAYOUT_PROVIDER=%q must be MOCK or RAZORPAYX_SANDBOX", c.Payouts.Provider))
			}

		case FeatureKYC:
			switch c.KYC.Provider {
			case KYCMock:
			case KYCDecentro, KYCSetu:
				problems = append(problems, requireString("ACRESYNC_KYC_CLIENT_ID", c.KYC.ClientID)...)
				problems = append(problems, requireSecret("ACRESYNC_KYC_CLIENT_SECRET", c.KYC.ClientSecret)...)
			default:
				problems = append(problems, fmt.Sprintf("ACRESYNC_KYC_PROVIDER=%q must be MOCK, DECENTRO or SETU", c.KYC.Provider))
			}

		default:
			problems = append(problems, fmt.Sprintf("unknown required feature %q", f))
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("config: %d problem(s):\n  - %s", len(problems), strings.Join(problems, "\n  - "))
	}
	return nil
}

// Summary renders a boot-time description of the loaded configuration.
//
// Secrets appear only as fingerprints. An operator needs to confirm which
// credential is loaded, which a length and two trailing characters answers; they
// never need the value, and a log line that carries one is a permanent leak into
// whatever aggregates those logs.
func (c *Config) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "environment       %s\n", c.Environment)
	fmt.Fprintf(&b, "http              %s\n", c.HTTPAddr)
	fmt.Fprintf(&b, "database          %s\n", c.Database.URL.Fingerprint())
	fmt.Fprintf(&b, "anchor pepper key %s (dev pepper %s)\n", orNone(c.Anchor.PepperKeyID), c.Anchor.DevPepper.Fingerprint())
	fmt.Fprintf(&b, "chain             id=%d rpc=%s confirmations=%d\n", c.Chain.ChainID, c.Chain.RPCURL.Fingerprint(), c.Chain.ConfirmationDepth)
	fmt.Fprintf(&b, "  roles           %s\n", orNone(c.Chain.RolesAddress))
	fmt.Fprintf(&b, "  ballot          %s\n", orNone(c.Chain.BallotAddress))
	fmt.Fprintf(&b, "  scheme          %s\n", orNone(c.Chain.SchemeAddress))
	fmt.Fprintf(&b, "relayer key       %s\n", c.Chain.RelayerPrivateKey.Fingerprint())
	fmt.Fprintf(&b, "ipfs              %s jwt=%s\n", c.IPFS.Provider, c.IPFS.PinataJWT.Fingerprint())
	fmt.Fprintf(&b, "payouts           %s key=%s\n", c.Payouts.Provider, orNone(c.Payouts.KeyID))
	fmt.Fprintf(&b, "kyc               %s client=%s\n", c.KYC.Provider, orNone(c.KYC.ClientID))
	return b.String()
}

func orNone(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}

func requireSecret(name string, v Secret) []string {
	if v.IsZero() {
		return []string{name + " is required but empty"}
	}
	if looksLikePlaceholder(v.Reveal()) {
		return []string{name + " still holds a placeholder value"}
	}
	return nil
}

func requireString(name, v string) []string {
	if strings.TrimSpace(v) == "" {
		return []string{name + " is required but empty"}
	}
	if looksLikePlaceholder(v) {
		return []string{name + " still holds a placeholder value"}
	}
	return nil
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func getEnvInt64(key string, fallback int64) int64 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return fallback
	}
	return n
}

// ---------------------------------------------------------------------------
// .env loading
// ---------------------------------------------------------------------------

// findDotEnv walks up from the working directory looking for a .env, so the
// orchestrator can be run from a subdirectory during development.
func findDotEnv() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for range 6 {
		candidate := filepath.Join(dir, ".env")
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", nil
}

// validEnvKey checks that a .env key is shaped like an environment variable.
//
// Uppercase letters, digits and underscores, starting with a letter. Deliberately not restricted to
// the ACRESYNC_ prefix: a working copy legitimately carries things like PGPASSWORD or FOUNDRY_PROFILE,
// and rejecting those would make the loader annoying enough to be bypassed.
func validEnvKey(key string) error {
	if key == "" {
		return errors.New("empty key before '='")
	}
	if key[0] < 'A' || key[0] > 'Z' {
		return fmt.Errorf("key %q must start with an uppercase letter. Environment variables are "+
			"conventionally uppercase, and a key in this form is usually a value pasted where a "+
			"variable name belongs", key)
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
		default:
			return fmt.Errorf("key %q contains %q; expected uppercase letters, digits and "+
				"underscores only", key, rune(c))
		}
	}
	return nil
}

// loadDotEnv applies KEY=VALUE pairs without overwriting anything already set in
// the environment.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	line := 0

	// Keys seen in this file, so a duplicate is an error rather than a silent win for
	// whichever came first.
	//
	// This is not hypothetical. Filling in a template by appending lines rather than editing
	// in place leaves the original empty assignment above the real one. Without this check the
	// loader sets the empty value, then skips the real one because the key now exists in the
	// environment, and the process starts with a blank credential and a confusing downstream
	// failure instead of a clear one here.
	seen := make(map[string]int)

	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		raw = strings.TrimPrefix(raw, "export ")

		key, value, found := strings.Cut(raw, "=")
		if !found {
			return fmt.Errorf("line %d: expected KEY=VALUE", line)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		// The key has to look like an environment variable.
		//
		// Environment variables are conventionally uppercase, so a lowercase or mixed-case key in a
		// .env is almost always a paste accident rather than a deliberate setting. The accident this
		// catches actually happened: credentials copied from a provider dashboard arrived as
		//
		//	rzp_test_=rzp_test_XXXXXXXXXXXXXX
		//	Test_key_secret=yyyyyyyyyyyyyyyyyyyyyyyy
		//
		// Both lines are well-formed KEY=VALUE pairs, so the duplicate check and the missing-equals
		// check both passed. The loader read them as two variables nothing refers to, the real
		// credential slots stayed empty, and the failure surfaced much later as an authentication
		// error against a third party with no hint that the values had been present all along.
		if err := validEnvKey(key); err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}

		if first, dup := seen[key]; dup {
			return fmt.Errorf(
				"line %d: %s is assigned twice (first at line %d). Remove the duplicate: "+
					"the earlier assignment would win and is usually the empty template line",
				line, key, first)
		}
		seen[key] = line

		// Strip one matched pair of surrounding quotes.
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}

		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
	}
	return sc.Err()
}
