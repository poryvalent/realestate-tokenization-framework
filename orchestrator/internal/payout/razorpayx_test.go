package payout

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acresync/orchestrator/internal/config"
	"github.com/acresync/orchestrator/internal/money"
)

// These tests stand in for credentials we do not have yet.
//
// They cannot prove RazorpayX behaves as expected, but they can prove our side of the exchange is
// built correctly: the idempotency header is actually sent, auth is attached, the documented response
// shape parses into the right fields, and an unmodelled status is refused rather than passed through.
// Those are the failures that would otherwise be discovered while moving real money.

func newFakeRazorpay(t *testing.T, handler http.HandlerFunc) (*RazorpayX, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	p, err := NewRazorpayX(config.PayoutConfig{
		Provider:      config.PayoutRazorpayX,
		KeyID:         "rzp_test_abc123",
		KeySecret:     config.Secret("supersecretvalue"),
		AccountNumber: srcAccount,
	})
	if err != nil {
		t.Fatal(err)
	}
	p.baseURL = srv.URL
	return p, srv
}

func okPayoutJSON(status, utr string) string {
	return `{
	  "id": "pout_00000000000001",
	  "entity": "payout",
	  "fund_account_id": "` + beneA + `",
	  "amount": 50000000,
	  "currency": "INR",
	  "fees": 590,
	  "tax": 106,
	  "status": "` + status + `",
	  "utr": "` + utr + `",
	  "mode": "IMPS",
	  "purpose": "payout",
	  "reference_id": "ACRESYNC-P1-0001",
	  "narration": "AcreSync Q2 payout",
	  "created_at": 1790000000
	}`
}

// TestIdempotencyHeaderIsSent is the one that matters most.
//
// Without the header the API treats every retry as a new instruction, so a dropped connection during
// a distribution run pays a unitholder twice. The header is easy to omit and its absence is invisible
// until it costs money, so it is asserted directly on the wire.
func TestIdempotencyHeaderIsSent(t *testing.T) {
	var gotHeader, gotAuthUser, gotAuthPass, gotPath, gotMethod string
	var gotBody map[string]any

	p, _ := newFakeRazorpay(t, func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Payout-Idempotency")
		gotAuthUser, gotAuthPass, _ = r.BasicAuth()
		gotPath, gotMethod = r.URL.Path, r.Method
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)

		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, okPayoutJSON("processing", ""))
	})

	req := validRequest("0xdeadbeef", money.Paise(50_000_000))
	if _, err := p.CreatePayout(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	if gotHeader != "0xdeadbeef" {
		t.Errorf("X-Payout-Idempotency = %q, want the request key; without it a retry pays twice", gotHeader)
	}
	if gotAuthUser != "rzp_test_abc123" || gotAuthPass != "supersecretvalue" {
		t.Error("basic auth must carry the key id and secret")
	}
	if gotMethod != http.MethodPost || gotPath != "/payouts" {
		t.Errorf("got %s %s, want POST /payouts", gotMethod, gotPath)
	}

	// The body must use the provider's field names, in paise, with currency set.
	if gotBody["account_number"] != srcAccount {
		t.Errorf("account_number = %v", gotBody["account_number"])
	}
	if gotBody["fund_account_id"] != beneA {
		t.Errorf("fund_account_id = %v", gotBody["fund_account_id"])
	}
	if gotBody["amount"] != float64(50_000_000) {
		t.Errorf("amount = %v, want 50000000 paise", gotBody["amount"])
	}
	if gotBody["currency"] != "INR" {
		t.Errorf("currency = %v", gotBody["currency"])
	}
	if gotBody["mode"] != "IMPS" {
		t.Errorf("mode = %v, and the provider treats it as case-sensitive", gotBody["mode"])
	}
	if gotBody["queue_if_low_balance"] != false {
		t.Errorf("queue_if_low_balance = %v, want false", gotBody["queue_if_low_balance"])
	}
}

func TestResponseParsing(t *testing.T) {
	p, _ := newFakeRazorpay(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, okPayoutJSON("processed", "HDFCN0001234567"))
	})

	got, err := p.FetchPayout(context.Background(), "pout_00000000000001")
	if err != nil {
		t.Fatal(err)
	}

	if got.Status != StatusProcessed {
		t.Errorf("status = %s", got.Status)
	}
	if got.AmountPaise != money.Paise(50_000_000) {
		t.Errorf("amount = %d", got.AmountPaise)
	}
	if got.UTR != "HDFCN0001234567" {
		t.Errorf("utr = %q", got.UTR)
	}
	if got.FeesPaise != 590 || got.TaxPaise != 106 {
		t.Errorf("fees = %d, tax = %d; these are charged to the scheme separately from the amount",
			got.FeesPaise, got.TaxPaise)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at should be parsed from the unix timestamp")
	}
	if len(got.Raw) == 0 {
		t.Error("the raw response must be retained; a reconciliation dispute is settled by what the " +
			"provider actually said, not by our parse of it")
	}
}

func TestStatusDetailsParsed(t *testing.T) {
	p, _ := newFakeRazorpay(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{
		  "id": "pout_00000000000001", "entity": "payout", "amount": 50000000,
		  "status": "reversed", "mode": "IMPS",
		  "status_details": {
		    "description": "IMPS is not enabled on the beneficiary account",
		    "source": "beneficiary_bank",
		    "reason": "beneficiary_bank_unavailable"
		  }
		}`)
	})

	got, err := p.FetchPayout(context.Background(), "pout_00000000000001")
	if err != nil {
		t.Fatal(err)
	}
	if got.StatusDetails.Source != SourceBeneficiaryBank {
		t.Errorf("source = %q", got.StatusDetails.Source)
	}
	if got.StatusDetails.Source.IsOurFault() {
		t.Error("a beneficiary bank failure is not fixed by reissuing to the same account")
	}
	if !strings.Contains(got.StatusDetails.Description, "IMPS") {
		t.Errorf("description = %q", got.StatusDetails.Description)
	}
}

// TestUnknownStatusRefused covers the state RazorpayX might add tomorrow.
//
// Passing it through would leave the orchestrator deciding what to do with a state it does not
// understand. "Keep waiting" hangs the distribution; "treat as failed" risks reissuing against money
// already sent. Neither default is acceptable, so the provider refuses and an operator decides.
func TestUnknownStatusRefused(t *testing.T) {
	p, _ := newFakeRazorpay(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, okPayoutJSON("settled", ""))
	})

	_, err := p.FetchPayout(context.Background(), "pout_00000000000001")
	if !errors.Is(err, ErrUnknownStatus) {
		t.Fatalf("want ErrUnknownStatus, got %v", err)
	}
	if !strings.Contains(err.Error(), "settled") {
		t.Errorf("the error should name the unrecognised status, got %v", err)
	}
}

func TestErrorBodySurfaced(t *testing.T) {
	p, _ := newFakeRazorpay(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"code":"BAD_REQUEST_ERROR",
		  "description":"The narration may only contain alphanumeric characters and spaces",
		  "reason":"input_validation_failed"}}`)
	})

	_, err := p.CreatePayout(context.Background(), validRequest("0xkey", money.Paise(50_000_000)))
	if err == nil {
		t.Fatal("an HTTP 400 must be an error")
	}
	if !strings.Contains(err.Error(), "narration") {
		t.Errorf("the provider's description should reach the operator, got %v", err)
	}
}

// TestSecretNeverAppearsInErrors checks the credential does not leak through an error path.
func TestSecretNeverAppearsInErrors(t *testing.T) {
	p, _ := newFakeRazorpay(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":{"code":"SERVER_ERROR","description":"something broke"}}`)
	})

	_, err := p.CreatePayout(context.Background(), validRequest("0xkey", money.Paise(50_000_000)))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "supersecretvalue") {
		t.Fatal("the key secret leaked into an error message")
	}
}

func TestNotFoundMapped(t *testing.T) {
	p, _ := newFakeRazorpay(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":{"code":"BAD_REQUEST_ERROR","description":"not found"}}`)
	})

	if _, err := p.FetchPayout(context.Background(), "pout_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestBalanceReportsUnavailableRatherThanZero(t *testing.T) {
	p, _ := newFakeRazorpay(t, func(w http.ResponseWriter, r *http.Request) {})

	got, err := p.Balance(context.Background())
	if !errors.Is(err, ErrBalanceUnavailable) {
		t.Fatalf("want ErrBalanceUnavailable, got %v", err)
	}
	if got != 0 {
		t.Error("the amount is meaningless when the error is set")
	}
	// The distinction matters: a zero balance would abort a distribution that could have proceeded.
	if errors.Is(err, ErrInsufficientFunds) {
		t.Error("an unavailable balance must not be confused with an empty account")
	}
}

func TestTestModeDetection(t *testing.T) {
	p, _ := newFakeRazorpay(t, func(w http.ResponseWriter, r *http.Request) {})
	if !p.IsTestMode() {
		t.Error("an rzp_test_ key must be recognised as sandbox")
	}

	live, err := NewRazorpayX(config.PayoutConfig{
		KeyID: "rzp_live_abc123", KeySecret: config.Secret("x"), AccountNumber: srcAccount,
	})
	if err != nil {
		t.Fatal(err)
	}
	if live.IsTestMode() {
		t.Error("an rzp_live_ key must not be reported as sandbox")
	}
}

func TestMissingCredentialsRejected(t *testing.T) {
	cases := map[string]config.PayoutConfig{
		"no key id": {KeySecret: config.Secret("x"), AccountNumber: srcAccount},
		"no secret": {KeyID: "rzp_test_a", AccountNumber: srcAccount},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewRazorpayX(cfg); err == nil {
				t.Fatal("missing credentials must be refused at construction")
			}
		})
	}
}

// TestConstructionWithoutAccountNumberIsAllowed covers a deliberate split.
//
// Credentials are needed for any call; the source account is needed only to move money. Requiring both
// at construction made it impossible to check whether the keys work before the deployment was otherwise
// complete, which is backwards for a preflight. config.Validate(FeaturePayouts) still requires all
// three for the orchestrator's own startup.
func TestConstructionWithoutAccountNumberIsAllowed(t *testing.T) {
	p, err := NewRazorpayX(config.PayoutConfig{
		KeyID: "rzp_test_abc123", KeySecret: config.Secret("secret"),
	})
	if err != nil {
		t.Fatalf("credentials alone should be enough to construct a client: %v", err)
	}

	// But a payout must still be refused, naming the missing variable.
	_, err = p.CreatePayout(context.Background(), validRequest("0xkey", money.Paise(50_000_000)))
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("want ErrInvalidRequest, got %v", err)
	}
	if !strings.Contains(err.Error(), "ACRESYNC_RAZORPAY_ACCOUNT_NUMBER") {
		t.Errorf("the error should name the missing variable, got: %v", err)
	}
}

// TestValidationRunsBeforeAnyNetworkCall confirms a bad request never reaches the provider. A
// rejection from our own validator names the offending field; an HTTP 400 mid-batch does not.
func TestValidationRunsBeforeAnyNetworkCall(t *testing.T) {
	called := false
	p, _ := newFakeRazorpay(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	bad := validRequest("0xkey", money.Paise(50))
	if _, err := p.CreatePayout(context.Background(), bad); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("want ErrInvalidRequest, got %v", err)
	}
	if called {
		t.Error("an invalid request must not be sent")
	}
}

// TestPayoutsEnabledInterpretation pins the diagnosis logic.
//
// These strings came from the live sandbox, and the distinctions matter operationally: one of them
// means "fund the account", another means "get the right number off the dashboard", and another means
// "the product is not switched on and no configuration will help". Sending an operator down the wrong
// one of those three wastes a day.
func TestPayoutsEnabledInterpretation(t *testing.T) {
	cases := []struct {
		name        string
		probe       ProbeResult
		wantEnabled bool
		wantReason  string
	}{
		{
			// Observed on an account without RazorpayX banking provisioned. The route is absent
			// entirely, which is the same response an endpoint outside the product gives.
			name: "route not exposed",
			probe: ProbeResult{
				Reachable: true, Authorised: true, StatusCode: 400,
				ErrorDescription: "The requested URL was not found on the server.",
			},
			wantEnabled: false,
			wantReason:  "not activated",
		},
		{
			name: "product not enabled",
			probe: ProbeResult{
				Reachable: true, Authorised: true, StatusCode: 400,
				ErrorDescription: "Access to requested resource not available",
			},
			wantEnabled: false,
			wantReason:  "not enabled on this account",
		},
		{
			// The good case: the product works and only the source account is wrong, which is expected
			// because the probe deliberately supplies an impossible one.
			name: "rejected on the account number",
			probe: ProbeResult{
				Reachable: true, Authorised: true, StatusCode: 400,
				ErrorDescription: "The account_number provided does not exist",
			},
			wantEnabled: true,
			wantReason:  "payouts are enabled",
		},
		{
			name: "needs funding",
			probe: ProbeResult{
				Reachable: true, Authorised: true, StatusCode: 400,
				ErrorDescription: "Your account does not have enough balance to carry out the payout",
			},
			wantEnabled: true,
			wantReason:  "needs funding",
		},
		{
			name:        "credentials rejected",
			probe:       ProbeResult{Reachable: true, Authorised: false, StatusCode: 401},
			wantEnabled: false,
			wantReason:  "credentials were rejected",
		},
		{
			name:        "unreachable",
			probe:       ProbeResult{Reachable: false},
			wantEnabled: false,
			wantReason:  "could not be reached",
		},
		{
			name: "unrecognised message is not guessed at",
			probe: ProbeResult{
				Reachable: true, Authorised: true, StatusCode: 400,
				ErrorDescription: "Something nobody has seen before",
			},
			wantEnabled: false,
			wantReason:  "inconclusive",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			enabled, reason := c.probe.PayoutsEnabled()
			if enabled != c.wantEnabled {
				t.Errorf("enabled = %v, want %v (reason: %s)", enabled, c.wantEnabled, reason)
			}
			if !strings.Contains(reason, c.wantReason) {
				t.Errorf("reason %q does not mention %q", reason, c.wantReason)
			}
		})
	}
}

// TestUnrecognisedMessageDefaultsToUnavailable covers the direction of the default.
//
// An unknown response reports payouts as unavailable rather than available. Guessing the optimistic way
// would let a distribution run start against a provider whose behaviour nobody has established.
func TestUnrecognisedMessageDefaultsToUnavailable(t *testing.T) {
	enabled, _ := ProbeResult{
		Reachable: true, Authorised: true, ErrorDescription: "unheard-of error",
	}.PayoutsEnabled()

	if enabled {
		t.Fatal("an unrecognised response must not be read as payouts being available")
	}
}

func TestProbeResultSummary(t *testing.T) {
	if got := (ProbeResult{Name: "x", Reachable: false, Body: "dial error"}).Summary(); !strings.Contains(got, "unreachable") {
		t.Errorf("got %q", got)
	}
	if got := (ProbeResult{Name: "x", Reachable: true, StatusCode: 200}).Summary(); !strings.Contains(got, "http 200") {
		t.Errorf("got %q", got)
	}
	if got := (ProbeResult{
		Name: "x", Reachable: true, StatusCode: 400,
		ErrorCode: "BAD_REQUEST_ERROR", ErrorDescription: "nope",
	}).Summary(); !strings.Contains(got, "nope") || !strings.Contains(got, "BAD_REQUEST_ERROR") {
		t.Errorf("got %q", got)
	}
}
