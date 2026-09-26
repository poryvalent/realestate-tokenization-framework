package payout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/acresync/orchestrator/internal/clock"
	"github.com/acresync/orchestrator/internal/config"
	"github.com/acresync/orchestrator/internal/money"
)

// ErrBalanceUnavailable is returned when the provider cannot report a balance.
//
// A distinct error rather than a zero balance. Zero would read as "no funds" and abort a
// distribution that could have proceeded, so the caller has to be able to tell "the account is
// empty" from "I could not ask".
var ErrBalanceUnavailable = errors.New("payout: balance is not available from this provider")

// RazorpayX is the live payout provider.
//
// # Status
//
// Written against the published API reference and never executed against live credentials. Nothing
// in M5 depends on it; the vertical slice runs entirely on Mock. When the sandbox keys land, the
// first thing to establish is whether this behaves as the mock does, and the honest expectation is
// that something here is wrong, because an unexercised HTTP client usually is.
//
// Endpoints: POST /v1/payouts, GET /v1/payouts/:id
// Reference: https://razorpay.com/docs/api/x/payouts/
type RazorpayX struct {
	keyID         string
	keySecret     config.Secret
	accountNumber string
	baseURL       string
	client        *http.Client
}

const (
	razorpayBaseURL = "https://api.razorpay.com/v1"

	// razorpayTimeout bounds one attempt.
	//
	// A payout request that times out is the genuinely dangerous case: the instruction may have been
	// accepted. The idempotency key is what makes the retry safe, so the timeout exists to bound
	// waiting, not to decide anything about the payout.
	razorpayTimeout = 60 * time.Second
)

func NewRazorpayX(cfg config.PayoutConfig) (*RazorpayX, error) {
	var missing []string
	if cfg.KeyID == "" {
		missing = append(missing, "ACRESYNC_RAZORPAY_KEY_ID")
	}
	if cfg.KeySecret.IsZero() {
		missing = append(missing, "ACRESYNC_RAZORPAY_KEY_SECRET")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("payout: RazorpayX selected but %v unset", missing)
	}

	// The source account number is deliberately not required here.
	//
	// Credentials are needed for any call; the source account is needed only to move money. Requiring
	// it at construction conflated the two and made it impossible to check whether the credentials
	// work without first having a fully completed deployment, which is exactly backwards for a
	// preflight. CreatePayout enforces it, and config.Validate(FeaturePayouts) enforces it for the
	// orchestrator's own startup, so the fail-fast is preserved where it belongs.

	return &RazorpayX{
		keyID:         cfg.KeyID,
		keySecret:     cfg.KeySecret,
		accountNumber: cfg.AccountNumber,
		baseURL:       razorpayBaseURL,
		client:        &http.Client{Timeout: razorpayTimeout},
	}, nil
}

func (r *RazorpayX) Name() string { return "RAZORPAYX" }

// IsTestMode reports whether the configured key is a sandbox key.
//
// Surfaced so the orchestrator can state which environment it is pointed at. Confusing a sandbox run
// for a live one during a rehearsal is recoverable; the reverse is not.
func (r *RazorpayX) IsTestMode() bool {
	return len(r.keyID) >= 9 && r.keyID[:9] == "rzp_test_"
}

type razorpayPayoutBody struct {
	AccountNumber     string            `json:"account_number"`
	FundAccountID     string            `json:"fund_account_id"`
	Amount            int64             `json:"amount"`
	Currency          string            `json:"currency"`
	Mode              string            `json:"mode"`
	Purpose           string            `json:"purpose"`
	QueueIfLowBalance bool              `json:"queue_if_low_balance"`
	ReferenceID       string            `json:"reference_id,omitempty"`
	Narration         string            `json:"narration,omitempty"`
	Notes             map[string]string `json:"notes,omitempty"`
}

type razorpayPayoutResponse struct {
	ID            string `json:"id"`
	Entity        string `json:"entity"`
	FundAccountID string `json:"fund_account_id"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	Fees          int64  `json:"fees"`
	Tax           int64  `json:"tax"`
	Status        string `json:"status"`
	UTR           string `json:"utr"`
	Mode          string `json:"mode"`
	Purpose       string `json:"purpose"`
	ReferenceID   string `json:"reference_id"`
	Narration     string `json:"narration"`
	CreatedAt     int64  `json:"created_at"`
	StatusDetails *struct {
		Description string `json:"description"`
		Source      string `json:"source"`
		Reason      string `json:"reason"`
	} `json:"status_details"`
}

type razorpayError struct {
	Error struct {
		Code        string `json:"code"`
		Description string `json:"description"`
		Reason       string `json:"reason"`
	} `json:"error"`
}

func (r *RazorpayX) CreatePayout(ctx context.Context, req Request) (*Payout, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if r.accountNumber == "" {
		return nil, fmt.Errorf("%w: ACRESYNC_RAZORPAY_ACCOUNT_NUMBER is unset, so there is no source "+
			"account for the money to leave", ErrInvalidRequest)
	}
	if req.AccountNumber != r.accountNumber {
		return nil, fmt.Errorf("%w: source account %q is not the configured account",
			ErrInvalidRequest, req.AccountNumber)
	}

	body, err := json.Marshal(razorpayPayoutBody{
		AccountNumber:     req.AccountNumber,
		FundAccountID:     req.FundAccountID,
		Amount:            int64(req.AmountPaise),
		Currency:          "INR",
		Mode:              string(req.Mode),
		Purpose:           string(req.Purpose),
		QueueIfLowBalance: req.QueueIfLowBalance,
		ReferenceID:       req.ReferenceID,
		Narration:         req.Narration,
		Notes:             req.Notes,
	})
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/payouts", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// The idempotency header is what makes a blind retry safe after a dropped connection.
	httpReq.Header.Set("X-Payout-Idempotency", req.IdempotencyKey)
	httpReq.SetBasicAuth(r.keyID, r.keySecret.Reveal())

	return r.do(httpReq)
}

func (r *RazorpayX) FetchPayout(ctx context.Context, providerID string) (*Payout, error) {
	if providerID == "" {
		return nil, fmt.Errorf("%w: empty payout id", ErrNotFound)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+"/payouts/"+providerID, nil)
	if err != nil {
		return nil, err
	}
	httpReq.SetBasicAuth(r.keyID, r.keySecret.Reveal())

	return r.do(httpReq)
}

// Balance is not implemented against the live API.
//
// Left unimplemented rather than pointed at a guessed endpoint. A wrong URL would return a 404 that
// reads like a permissions problem, and an operator would spend the outage debugging credentials. The
// caller is expected to treat ErrBalanceUnavailable as "cannot pre-flight" and rely on per-payout
// failures instead, which is a degraded but correct mode.
func (r *RazorpayX) Balance(ctx context.Context) (money.Paise, error) {
	return 0, fmt.Errorf("%w: the RazorpayX balance endpoint is not wired up", ErrBalanceUnavailable)
}

func (r *RazorpayX) do(httpReq *http.Request) (*Payout, error) {
	resp, err := r.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("payout: razorpayx request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("payout: reading razorpayx response: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: razorpayx returned 404", ErrNotFound)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var re razorpayError
		if json.Unmarshal(raw, &re) == nil && re.Error.Description != "" {
			return nil, fmt.Errorf("payout: razorpayx http %d: %s (code %s, reason %s)",
				resp.StatusCode, re.Error.Description, re.Error.Code, re.Error.Reason)
		}
		return nil, fmt.Errorf("payout: razorpayx http %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}

	var pr razorpayPayoutResponse
	if err := json.Unmarshal(raw, &pr); err != nil {
		return nil, fmt.Errorf("payout: decoding razorpayx response: %w", err)
	}

	status := Status(pr.Status)
	if !status.Valid() {
		// An unmodelled state is an error rather than something to pass through.
		//
		// Tolerating it would mean the orchestrator deciding what to do with a state it does not
		// understand, and the plausible default of "keep waiting" would hang a distribution while
		// the safer-sounding "treat as failed" would risk a reissue against money already sent.
		return nil, fmt.Errorf("%w: razorpayx reported %q, which this build does not model",
			ErrUnknownStatus, pr.Status)
	}

	out := &Payout{
		ProviderID:  pr.ID,
		Status:      status,
		AmountPaise: money.Paise(pr.Amount),
		UTR:         pr.UTR,
		Mode:        Mode(pr.Mode),
		ReferenceID: pr.ReferenceID,
		FeesPaise:   money.Paise(pr.Fees),
		TaxPaise:    money.Paise(pr.Tax),
		Raw:         raw,
	}
	if pr.CreatedAt > 0 {
		out.CreatedAt = time.Unix(pr.CreatedAt, 0).UTC()
	}
	if pr.StatusDetails != nil {
		out.StatusDetails = StatusDetails{
			Description: pr.StatusDetails.Description,
			Source:      FailureSource(pr.StatusDetails.Source),
			Reason:      pr.StatusDetails.Reason,
		}
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// NewProvider builds the configured payout provider.
func NewProvider(cfg config.PayoutConfig, clk clock.Business) (Provider, error) {
	switch cfg.Provider {
	case config.PayoutMock:
		return NewMock(clk, cfg.AccountNumber, 0), nil
	case config.PayoutRazorpayX:
		return NewRazorpayX(cfg)
	default:
		return nil, fmt.Errorf("payout: unknown provider %q", cfg.Provider)
	}
}

// ---------------------------------------------------------------------------
// Beneficiary registration
// ---------------------------------------------------------------------------

// A payout cannot name a bank account directly. It names a fund account, which belongs to a contact,
// so a unitholder has to be registered with the provider before they can be paid. That registration is
// where the account number and IFSC live, which is why the payout path only ever handles an opaque
// fa_ reference and never the bank details themselves.
//
// Both endpoints are idempotent by content rather than by header: the provider returns the existing
// record when the identifying fields match. Convenient, and not something to rely on for money
// movement, which is why Create Payout has a real idempotency key.

// Contact is a registered payee.
type Contact struct {
	ID          string
	Name        string
	Type        string
	ReferenceID string
	Active      bool
	Raw         json.RawMessage
}

// FundAccount is a bank account linked to a contact.
type FundAccount struct {
	ID            string
	ContactID     string
	AccountType   string
	Active        bool
	IFSC          string
	AccountNumber string
	Raw           json.RawMessage
}

// BankDetails are the beneficiary's account details, used only at registration.
type BankDetails struct {
	// Name is the account holder's name, 3 to 120 characters.
	Name string

	// IFSC identifies the branch and must be exactly 11 characters.
	IFSC string

	// AccountNumber is 5 to 35 alphanumeric characters.
	AccountNumber string
}

// CreateContact registers a payee.
//
// ReferenceID carries our internal investor identifier so the provider's record can be matched back to
// ours during reconciliation. Name is required and is the one field here that is unavoidably personal:
// a bank will not accept a transfer to an account whose holder name does not match, so there is no
// version of this that works with a pseudonym. It lives with the provider under their registration
// flow and never reaches IPFS or the chain.
func (r *RazorpayX) CreateContact(ctx context.Context, name, contactType, referenceID string) (*Contact, error) {
	if l := len([]rune(name)); l < 3 || l > 50 {
		return nil, fmt.Errorf("%w: contact name must be 3 to 50 characters, got %d",
			ErrInvalidRequest, l)
	}

	body, err := json.Marshal(map[string]any{
		"name":         name,
		"type":         contactType,
		"reference_id": referenceID,
	})
	if err != nil {
		return nil, err
	}

	raw, err := r.postJSON(ctx, "/contacts", body)
	if err != nil {
		return nil, err
	}

	var resp struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Type        string `json:"type"`
		ReferenceID string `json:"reference_id"`
		Active      bool   `json:"active"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("payout: decoding the contact response: %w", err)
	}
	if resp.ID == "" {
		return nil, errors.New("payout: contact created with no id")
	}

	return &Contact{
		ID: resp.ID, Name: resp.Name, Type: resp.Type,
		ReferenceID: resp.ReferenceID, Active: resp.Active, Raw: raw,
	}, nil
}

// CreateFundAccount links a bank account to a contact.
//
// The field limits are enforced locally so a violation names the offending field rather than arriving
// as an opaque rejection while onboarding a batch of holders.
func (r *RazorpayX) CreateFundAccount(ctx context.Context, contactID string, bank BankDetails) (*FundAccount, error) {
	var problems []string

	if contactID == "" {
		problems = append(problems, "contact id is required")
	}
	if l := len([]rune(bank.Name)); l < 3 || l > 120 {
		problems = append(problems, fmt.Sprintf("account holder name must be 3 to 120 characters, got %d", l))
	}
	if len(bank.IFSC) != 11 {
		problems = append(problems, fmt.Sprintf("IFSC must be exactly 11 characters, got %d", len(bank.IFSC)))
	}
	if l := len(bank.AccountNumber); l < 5 || l > 35 {
		problems = append(problems, fmt.Sprintf("account number must be 5 to 35 characters, got %d", l))
	}
	for _, c := range bank.AccountNumber {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') {
			problems = append(problems, "account number may contain only letters and digits")
			break
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrInvalidRequest, strings.Join(problems, "; "))
	}

	body, err := json.Marshal(map[string]any{
		"contact_id":   contactID,
		"account_type": "bank_account",
		"bank_account": map[string]string{
			"name":           bank.Name,
			"ifsc":           bank.IFSC,
			"account_number": bank.AccountNumber,
		},
	})
	if err != nil {
		return nil, err
	}

	raw, err := r.postJSON(ctx, "/fund_accounts", body)
	if err != nil {
		return nil, err
	}

	var resp struct {
		ID          string `json:"id"`
		ContactID   string `json:"contact_id"`
		AccountType string `json:"account_type"`
		Active      bool   `json:"active"`
		BankAccount struct {
			IFSC          string `json:"ifsc"`
			AccountNumber string `json:"account_number"`
		} `json:"bank_account"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("payout: decoding the fund account response: %w", err)
	}
	if resp.ID == "" {
		return nil, errors.New("payout: fund account created with no id")
	}

	return &FundAccount{
		ID: resp.ID, ContactID: resp.ContactID, AccountType: resp.AccountType,
		Active: resp.Active,
		IFSC:   resp.BankAccount.IFSC, AccountNumber: resp.BankAccount.AccountNumber,
		Raw: raw,
	}, nil
}

// postJSON issues an authenticated POST and returns the raw body.
//
// Shared by the registration calls and deliberately not used by CreatePayout, which needs to set the
// idempotency header and must not be able to lose it to a refactor of a shared helper.
func (r *RazorpayX) postJSON(ctx context.Context, path string, body []byte) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(r.keyID, r.keySecret.Reveal())

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("payout: %s: %w", path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var re razorpayError
		if json.Unmarshal(raw, &re) == nil && re.Error.Description != "" {
			return nil, fmt.Errorf("payout: %s returned http %d: %s (code %s, reason %s)",
				path, resp.StatusCode, re.Error.Description, re.Error.Code, re.Error.Reason)
		}
		return nil, fmt.Errorf("payout: %s returned http %d: %s",
			path, resp.StatusCode, truncate(string(raw), 400))
	}
	return raw, nil
}

// Ping verifies the credentials without moving anything.
//
// Requests a payout identifier that cannot exist. A 401 means the credentials are wrong; anything else,
// including the 400 or 404 this is expected to provoke, means authentication succeeded. There is no
// dedicated identity endpoint, and this distinction is what a preflight needs: it separates "the keys
// are wrong" from "the keys are fine and something else is misconfigured", which are very different
// conversations.
//
// Deliberately read-only. A preflight that created something would leave residue in the account every
// time somebody checked their configuration.
func (r *RazorpayX) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		r.baseURL+"/payouts/pout_0000000000000000", nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(r.keyID, r.keySecret.Reveal())

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("payout: reaching RazorpayX: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("payout: RazorpayX rejected the credentials (http 401). Check "+
			"ACRESYNC_RAZORPAY_KEY_ID and ACRESYNC_RAZORPAY_KEY_SECRET: %s", truncate(string(raw), 200))
	case http.StatusForbidden:
		return fmt.Errorf("payout: RazorpayX authenticated but refused the request (http 403). The "+
			"key is probably valid without payout permissions: %s", truncate(string(raw), 200))
	default:
		return nil
	}
}

// ---------------------------------------------------------------------------
// Diagnostics
// ---------------------------------------------------------------------------

// ProbeResult is what one endpoint said when asked.
type ProbeResult struct {
	Name   string
	Method string
	Path   string

	StatusCode int

	// ErrorCode and ErrorDescription come from Razorpay's error envelope when present.
	ErrorCode        string
	ErrorDescription string

	// Body is a truncated copy for anything the envelope did not explain.
	Body string

	// Reachable is true when the endpoint answered at all, regardless of what it said.
	Reachable bool

	// Authorised distinguishes a credential problem from every other outcome.
	Authorised bool
}

// Summary renders one line for an operator.
func (p ProbeResult) Summary() string {
	switch {
	case !p.Reachable:
		return fmt.Sprintf("%-22s unreachable: %s", p.Name, p.Body)
	case p.ErrorDescription != "":
		return fmt.Sprintf("%-22s http %d  %s (%s)", p.Name, p.StatusCode, p.ErrorDescription, p.ErrorCode)
	default:
		return fmt.Sprintf("%-22s http %d", p.Name, p.StatusCode)
	}
}

// Probe reports what each endpoint says, without creating anything.
//
// # Why the status codes matter rather than a pass or fail
//
// The rzp_test_ prefix is shared by Razorpay Payments keys and RazorpayX keys, so a key can
// authenticate perfectly while having no access to payouts at all. A probe that only asked "did this
// return 401" would report healthy credentials for a key that can never move money, and the gap would
// surface during a rehearsal.
//
// So each endpoint is reported with its actual status and Razorpay's own error code. A 400 on a
// malformed identifier means the endpoint exists and the key reaches it. A 401 means the credentials
// are wrong. A 403 means the key is valid and the product is not enabled, which is a different problem
// with a different fix.
func (r *RazorpayX) Probe(ctx context.Context) []ProbeResult {
	type target struct {
		name, method, path string
	}

	targets := []target{
		// A payout identifier that cannot exist. Reaching this proves the payout API is enabled.
		{"payouts (read)", http.MethodGet, "/payouts/pout_0000000000000000"},

		// Contacts and fund accounts are the registration surface and need no account number, which
		// makes them the deepest check available without one.
		{"contacts (list)", http.MethodGet, "/contacts"},
		{"fund_accounts (list)", http.MethodGet, "/fund_accounts"},

		// Listing payouts requires the source account number as a query parameter.
		{"payouts (list)", http.MethodGet, "/payouts"},

		// The Partner and Onboarding accounts endpoint. Not part of RazorpayX payouts, probed because an
		// acc_ prefixed identifier belongs to that surface rather than to this one, and confirming
		// which product an identifier comes from is faster than guessing.
		{"accounts (partner)", http.MethodGet, "/accounts"},

		// The same listing with a syntactically plausible but wrong account number.
		//
		// This pair disambiguates a message that is otherwise ambiguous. "Access to requested resource
		// not available" could mean the product is not enabled, or it could just be how the API
		// complains about a missing required parameter. If supplying a bogus account number changes the
		// error, the parameter was the problem and payouts are available. If the message is identical,
		// the parameter was never the issue.
		{"payouts (list, acct)", http.MethodGet, "/payouts?account_number=0000000000000000&count=1"},
	}

	out := make([]ProbeResult, 0, len(targets))
	for _, t := range targets {
		out = append(out, r.probeOne(ctx, t.name, t.method, t.path))
	}
	return out
}

func (r *RazorpayX) probeOne(ctx context.Context, name, method, path string) ProbeResult {
	res := ProbeResult{Name: name, Method: method, Path: path}

	req, err := http.NewRequestWithContext(ctx, method, r.baseURL+path, nil)
	if err != nil {
		res.Body = err.Error()
		return res
	}
	req.SetBasicAuth(r.keyID, r.keySecret.Reveal())

	resp, err := r.client.Do(req)
	if err != nil {
		res.Body = err.Error()
		return res
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	res.Reachable = true
	res.StatusCode = resp.StatusCode
	res.Authorised = resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden

	var re razorpayError
	if json.Unmarshal(raw, &re) == nil && re.Error.Description != "" {
		res.ErrorCode = re.Error.Code
		res.ErrorDescription = re.Error.Description
	} else {
		res.Body = truncate(string(raw), 300)
	}
	return res
}

// DiscoverAccountNumbers attempts to find the source account number from the API.
//
// # Why this is worth trying and why it may not work
//
// The account number is normally read off the dashboard, which is inconvenient and easy to get wrong:
// it is a long digit string with no checksum, and a transposed pair produces a valid-looking value that
// fails at payout time. Reading it from the API instead removes that transcription step.
//
// RazorpayX publishes no endpoint whose purpose is to list your own accounts, so this works by
// inference rather than by design. Some error responses name the account they expected, and the
// Payments virtual-accounts endpoint sometimes exposes the same underlying number when the account is a
// RazorpayX Lite. Neither is documented as a discovery mechanism, so a failure here means nothing is
// wrong: it means the dashboard is the only source.
func (r *RazorpayX) DiscoverAccountNumbers(ctx context.Context) ([]string, []ProbeResult) {
	probes := []ProbeResult{
		r.probeOne(ctx, "virtual_accounts", http.MethodGet, "/virtual_accounts"),
		r.probeOne(ctx, "transactions", http.MethodGet, "/transactions"),
	}

	found := map[string]bool{}

	// The virtual-accounts listing, when available, carries receiver descriptors that include an
	// account number.
	raw, err := r.getRaw(ctx, "/virtual_accounts?count=10")
	if err == nil {
		var resp struct {
			Items []struct {
				Receivers []struct {
					Entity        string `json:"entity"`
					AccountNumber string `json:"account_number"`
				} `json:"receivers"`
			} `json:"items"`
		}
		if json.Unmarshal(raw, &resp) == nil {
			for _, it := range resp.Items {
				for _, rc := range it.Receivers {
					if rc.AccountNumber != "" {
						found[rc.AccountNumber] = true
					}
				}
			}
		}
	}

	out := make([]string, 0, len(found))
	for n := range found {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, probes
}

func (r *RazorpayX) getRaw(ctx context.Context, path string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(r.keyID, r.keySecret.Reveal())

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return raw, nil
}

// ListContacts reads back registered payees, which verifies response parsing against real data.
func (r *RazorpayX) ListContacts(ctx context.Context, count int) ([]Contact, error) {
	if count <= 0 || count > 100 {
		count = 10
	}
	raw, err := r.getRaw(ctx, fmt.Sprintf("/contacts?count=%d", count))
	if err != nil {
		return nil, fmt.Errorf("payout: listing contacts: %w", err)
	}

	var resp struct {
		Count int `json:"count"`
		Items []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Type        string `json:"type"`
			ReferenceID string `json:"reference_id"`
			Active      bool   `json:"active"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("payout: decoding the contacts list: %w", err)
	}

	out := make([]Contact, 0, len(resp.Items))
	for _, it := range resp.Items {
		out = append(out, Contact{
			ID: it.ID, Name: it.Name, Type: it.Type,
			ReferenceID: it.ReferenceID, Active: it.Active,
		})
	}
	return out, nil
}

// DiagnosePayoutAvailability determines whether payouts are usable, without moving money.
//
// # Why a write attempt is the only way to know
//
// Read probes on this account are ambiguous. Contacts and fund accounts answer with 200 while listing
// payouts and transactions both return "Access to requested resource not available", and that message
// is used both for a missing parameter and for a product that is not enabled. Nothing read-only
// separates the two.
//
// So this posts a real create-payout request with a source account number that cannot exist. The
// response distinguishes the cases precisely:
//
//   - a complaint about the account number means the payouts product works and only the correct number
//     is missing
//   - a repeat of "access to requested resource not available" means payouts are not enabled, which
//     also explains why no account number can be found: there is no account yet
//
// Safe by construction. The source account is sixteen zeros, so the request cannot succeed, and a
// rejected create-payout leaves nothing behind.
func (r *RazorpayX) DiagnosePayoutAvailability(ctx context.Context, fundAccountID string) (ProbeResult, error) {
	return r.DiagnosePayoutWithAccount(ctx, fundAccountID, "0000000000000000")
}

// DiagnosePayoutWithAccount runs the availability probe against a specific source account.
//
// Exists so a candidate account number can be tried and its response compared against the impossible
// one. Identical responses mean the API is not objecting to the account number, which rules it out as
// the missing piece; different responses mean the value reached the validation layer and is worth
// refining.
//
// Still safe: a create-payout that is rejected leaves nothing behind, and a 2xx is reported as an error
// rather than a success precisely so this cannot quietly move money while claiming to diagnose.
func (r *RazorpayX) DiagnosePayoutWithAccount(ctx context.Context, fundAccountID, sourceAccount string) (ProbeResult, error) {
	body, err := json.Marshal(razorpayPayoutBody{
		AccountNumber:     sourceAccount,
		FundAccountID:     fundAccountID,
		Amount:            int64(MinAmountPaise),
		Currency:          "INR",
		Mode:              string(ModeIMPS),
		Purpose:           string(PurposePayout),
		QueueIfLowBalance: false,
		Narration:         "AcreSync probe",
	})
	if err != nil {
		return ProbeResult{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/payouts", bytes.NewReader(body))
	if err != nil {
		return ProbeResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	// A distinct idempotency key, so this probe can never collide with a real payout and can never be
	// mistaken for one on a retry.
	req.Header.Set("X-Payout-Idempotency", "acresync-availability-probe-"+sourceAccount)
	req.SetBasicAuth(r.keyID, r.keySecret.Reveal())

	res := ProbeResult{Name: "payouts (create probe)", Method: http.MethodPost, Path: "/payouts"}

	resp, err := r.client.Do(req)
	if err != nil {
		res.Body = err.Error()
		return res, nil
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	res.Reachable = true
	res.StatusCode = resp.StatusCode
	res.Authorised = resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden

	var re razorpayError
	if json.Unmarshal(raw, &re) == nil && re.Error.Description != "" {
		res.ErrorCode = re.Error.Code
		res.ErrorDescription = re.Error.Description
	} else {
		res.Body = truncate(string(raw), 400)
	}

	// A 2xx here would mean sixteen zeros was accepted as a source account, which should be impossible.
	// Reported as an error rather than a success, because the alternative is a tool that quietly created
	// a payout while claiming to be a read-only diagnostic.
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return res, fmt.Errorf("payout: the availability probe was ACCEPTED with an impossible source "+
			"account. A payout may have been created. Response: %s", truncate(string(raw), 400))
	}

	return res, nil
}

// PayoutsEnabled interprets the availability probe.
func (p ProbeResult) PayoutsEnabled() (enabled bool, reason string) {
	d := strings.ToLower(p.ErrorDescription)

	switch {
	case !p.Reachable:
		return false, "the endpoint could not be reached"

	case !p.Authorised:
		return false, "the credentials were rejected for this endpoint"

	case strings.Contains(d, "requested url was not found"):
		// The route itself is absent. This is the same message the Payments-only virtual-accounts
		// endpoint returns, and it is a stronger signal than a permissions error: the API is not
		// exposing create-payout to this account at all, so there is nothing to configure.
		return false, "the create-payout route is not exposed on this account at all. This is the " +
			"same response an endpoint outside the product returns, so RazorpayX payouts are not " +
			"activated. It also explains why no source account number can be found: no banking " +
			"account has been provisioned"

	case strings.Contains(d, "access to requested resource"):
		return false, "payouts are not enabled on this account. This also explains why no source " +
			"account number exists to find: RazorpayX has not provisioned a banking account yet"

	case strings.Contains(d, "account_number") || strings.Contains(d, "account number"):
		return true, "payouts are enabled; the request was rejected on the source account number, " +
			"which is expected because the probe supplies an impossible one"

	case strings.Contains(d, "balance") || strings.Contains(d, "insufficient"):
		return true, "payouts are enabled and the account needs funding"

	default:
		return false, fmt.Sprintf("inconclusive: %q", p.ErrorDescription)
	}
}
