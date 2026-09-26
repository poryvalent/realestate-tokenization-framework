// Command razorpayx-preflight exercises the live RazorpayX sandbox.
//
// # Why this is a command and not a test
//
// It calls a third party and, past the first stage, creates real records in a sandbox account. A test
// that did that would fail in CI, fail offline, and leave residue every time somebody ran the suite.
// This is a thing an operator runs deliberately, before a rehearsal, and reads the output of.
//
// # What it is looking for
//
// The RazorpayX client was written against published documentation and never executed. The honest
// expectation is that something in it is wrong, because an unexercised HTTP client usually is. So the
// stages are ordered to fail as early and as cheaply as possible, and each one prints what the provider
// actually returned rather than only whether it liked it.
//
// The two stages that matter most are the last two. Everything before them proves the plumbing works.
// Those prove that a retry after a dropped connection does not pay a unitholder twice, which is the
// single behaviour standing between this system and an unrecoverable mistake.
//
// Usage:
//
//	go run ./cmd/razorpayx-preflight              # auth check only, creates nothing
//	go run ./cmd/razorpayx-preflight -full        # full path, creates sandbox records
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/acresync/orchestrator/internal/clock"
	"github.com/acresync/orchestrator/internal/config"
	"github.com/acresync/orchestrator/internal/money"
	"github.com/acresync/orchestrator/internal/payout"
)

func main() {
	register := flag.Bool("register", false,
		"also register a contact and a fund account; needs no source account number")
	full := flag.Bool("full", false,
		"register a beneficiary and create a real sandbox payout; needs a source account number")
	amount := flag.Int64("amount", 100,
		"payout amount in paise for the full run; the provider minimum is 100")
	account := flag.String("account", "",
		"source account number to try, overriding ACRESYNC_RAZORPAY_ACCOUNT_NUMBER")
	flag.Parse()

	if *account != "" {
		// Set into the environment rather than threaded through, so every stage sees the same value the
		// orchestrator would and there is no separate code path for a tried value.
		_ = os.Setenv("ACRESYNC_RAZORPAY_ACCOUNT_NUMBER", *account)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// -full implies -register: a payout cannot name a bank account directly, only a fund account, so
	// the beneficiary has to exist first.
	if err := run(ctx, *register || *full, *full, money.Paise(*amount)); err != nil {
		fmt.Fprintf(os.Stderr, "\npreflight failed: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, register, full bool, amount money.Paise) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	section("Configuration")

	// Credentials are reported by fingerprint. The length and last two characters are enough for an
	// operator to tell one key from another and far too little to reconstruct one.
	fmt.Printf("  payout provider          %s\n", cfg.Payouts.Provider)
	fmt.Printf("  ACRESYNC_RAZORPAY_KEY_ID %s\n", describeKeyID(cfg.Payouts.KeyID))
	fmt.Printf("  key secret               %s\n", cfg.Payouts.KeySecret.Fingerprint())
	fmt.Printf("  source account number    %s\n", describeAccount(cfg.Payouts.AccountNumber))

	if cfg.Payouts.KeyID == "" || cfg.Payouts.KeySecret.IsZero() {
		return errors.New("ACRESYNC_RAZORPAY_KEY_ID and ACRESYNC_RAZORPAY_KEY_SECRET must both be set")
	}

	// Constructed directly rather than through NewProvider, because the provider selector is still
	// pointed at MOCK and this tool has to be able to check the credentials before that switch is
	// flipped. Pointing production traffic at a provider nobody has preflighted is the thing this
	// command exists to prevent.
	client, err := payout.NewRazorpayX(config.PayoutConfig{
		Provider:      config.PayoutRazorpayX,
		KeyID:         cfg.Payouts.KeyID,
		KeySecret:     cfg.Payouts.KeySecret,
		AccountNumber: cfg.Payouts.AccountNumber,
	})
	if err != nil {
		// The constructor requires an account number. Report the credential state that is known good
		// before failing, so a missing account number is not mistaken for a bad key.
		fmt.Printf("\n  provider not constructible yet: %v\n", err)
		return err
	}

	if !client.IsTestMode() {
		fmt.Printf("\n  WARNING: the key id does not carry the rzp_test_ prefix. This looks like a "+
			"LIVE key. Refusing to continue.\n")
		return errors.New("refusing to preflight against what appears to be a live key")
	}
	fmt.Printf("  mode                     SANDBOX (rzp_test_ prefix)\n")

	// ---------------------------------------------------------------------
	section("Stage 1: which APIs do these keys actually reach")
	// ---------------------------------------------------------------------

	// Status codes rather than a pass or fail.
	//
	// The rzp_test_ prefix is shared between Razorpay Payments keys and RazorpayX keys, so a key can
	// authenticate perfectly while having no access to payouts. Reporting only "not 401" would call
	// such a key healthy and the gap would surface during a rehearsal.
	results := client.Probe(ctx)
	for _, p := range results {
		fmt.Printf("  %s\n", p.Summary())
	}

	var unauthorised, payoutReachable bool
	for _, p := range results {
		if p.Reachable && !p.Authorised {
			unauthorised = true
		}
		if p.Name == "payouts (read)" && p.Reachable && p.Authorised {
			payoutReachable = true
		}
	}

	// Registration endpoints returning 200 is the strongest signal available here: contacts and fund
	// accounts do not exist in the Razorpay Payments API at all, so a 200 from either means these are
	// RazorpayX credentials rather than payment-gateway ones.
	var registrationOK bool
	for _, p := range results {
		if (p.Name == "contacts (list)" || p.Name == "fund_accounts (list)") && p.StatusCode == 200 {
			registrationOK = true
		}
	}

	switch {
	case unauthorised:
		fmt.Printf("\n  At least one endpoint returned 401 or 403.\n")
		fmt.Printf("  A 401 means the key id or secret is wrong. A 403 usually means the key is valid\n")
		fmt.Printf("  but RazorpayX is not enabled on the account, which is a dashboard change rather\n")
		fmt.Printf("  than a code change.\n")
		return errors.New("credentials or product access rejected")

	case !registrationOK:
		return errors.New("neither contacts nor fund_accounts answered with 200; these may be " +
			"Razorpay Payments keys rather than RazorpayX keys")

	default:
		fmt.Printf("\n  Contacts and fund accounts answered. Those endpoints do not exist in the\n")
		fmt.Printf("  Razorpay Payments API, so these are RazorpayX credentials.\n")
	}

	// What can honestly be said about payouts, separated from what cannot.
	var listPlain, listWithAcct string
	for _, p := range results {
		switch p.Name {
		case "payouts (list)":
			listPlain = p.ErrorDescription
		case "payouts (list, acct)":
			listWithAcct = p.ErrorDescription
		}
	}

	fmt.Printf("\n  On payouts specifically:\n")
	if payoutReachable {
		fmt.Printf("    reading a non-existent payout returned 404, so the resource is reachable.\n")
	}
	switch {
	case listWithAcct == "" && listPlain != "":
		fmt.Printf("    the listing succeeded once an account number was supplied, so the earlier\n")
		fmt.Printf("    complaint was the missing parameter and payouts are available.\n")
	case listWithAcct != "" && listWithAcct == listPlain:
		fmt.Printf("    the listing gives the same message with and without an account number:\n")
		fmt.Printf("      %q\n", listPlain)
		fmt.Printf("    so the parameter was never the issue. This is unproven either way and the\n")
		fmt.Printf("    first real payout attempt is what will settle it.\n")
	case listWithAcct != "":
		fmt.Printf("    with an account number the message changed to:\n")
		fmt.Printf("      %q\n", listWithAcct)
		fmt.Printf("    which reads like the account number being wrong rather than access being\n")
		fmt.Printf("    denied, and a wrong one is expected here.\n")
	}

	// ---------------------------------------------------------------------
	section("Stage 2: look for the source account number")
	// ---------------------------------------------------------------------

	accountNumber := cfg.Payouts.AccountNumber

	if accountNumber == "" {
		fmt.Printf("  ACRESYNC_RAZORPAY_ACCOUNT_NUMBER is unset, so trying to read it from the API.\n\n")

		discovered, probes := client.DiscoverAccountNumbers(ctx)
		for _, p := range probes {
			fmt.Printf("  %s\n", p.Summary())
		}

		switch len(discovered) {
		case 0:
			fmt.Printf("\n  Nothing found. RazorpayX publishes no endpoint whose purpose is to list your\n")
			fmt.Printf("  own accounts, so this was inference rather than a documented lookup and a\n")
			fmt.Printf("  blank result means nothing is wrong.\n")
		case 1:
			accountNumber = discovered[0]
			fmt.Printf("\n  Found one candidate: %s\n", accountNumber)
			fmt.Printf("  Not confirmed as the payout source account. Treat it as a suggestion.\n")
		default:
			fmt.Printf("\n  Found %d candidates, so none can be chosen automatically:\n", len(discovered))
			for _, n := range discovered {
				fmt.Printf("    %s\n", n)
			}
		}
	} else {
		fmt.Printf("  configured: %s\n", describeAccount(accountNumber))

		// A shape check before anything is attempted with it.
		//
		// The payouts API wants a bank account number: digits, no prefix. An acc_ prefixed value is a
		// Razorpay account identifier from the Partner and Route surface, which is a different product
		// with different semantics. Saying so up front is more useful than letting it fail as a generic
		// rejection, because the two need completely different fixes.
		if strings.HasPrefix(accountNumber, "acc_") {
			fmt.Printf("\n  This looks like a Razorpay account identifier rather than a bank account\n")
			fmt.Printf("  number. The acc_ prefix belongs to the Partner and Route APIs, which identify\n")
			fmt.Printf("  a merchant account. The payouts API wants the account number money leaves\n")
			fmt.Printf("  from, which is a plain digit string with no prefix.\n")
			fmt.Printf("  Trying it anyway, because the response is worth having.\n")
		} else if strings.ContainsFunc(accountNumber, func(r rune) bool { return r < '0' || r > '9' }) {
			fmt.Printf("\n  Note: this contains non-digit characters. A RazorpayX source account number\n")
			fmt.Printf("  is normally all digits.\n")
		}
	}

	if !register {
		fmt.Printf("\n  Stopping here. Nothing was created.\n")
		fmt.Printf("  Re-run with -register to exercise beneficiary registration, which needs no\n")
		fmt.Printf("  account number, or -full to also move %s paise once one is available.\n", amount)
		return nil
	}

	// ---------------------------------------------------------------------
	section("Stage 3: register a contact")
	// ---------------------------------------------------------------------

	// Real time, read through the one sanctioned door.
	//
	// A CLI an operator runs genuinely wants the wall clock: the timestamp is only there to make the
	// preflight's sandbox records distinguishable between runs. The wall-clock lint has no way to tell
	// that from a business deadline being read outside the scheme clock, and it is right not to try, so
	// this goes through clock.Real like everything else.
	now, err := clock.Real().Now(ctx)
	if err != nil {
		return err
	}
	stamp := now.Format("20060102T150405")

	// A stable reference, so repeated runs reuse one beneficiary instead of accumulating records.
	//
	// Both registration endpoints are idempotent by content: the provider returns the existing record
	// when the identifying fields match. A timestamped reference defeated that and left a new contact
	// behind on every run. Keeping it fixed also exercises that content-idempotency, which is worth
	// knowing about even though the payout path relies on a real key rather than on it.
	const ref = "acresync-preflight-fixed"

	contact, err := client.CreateContact(ctx, "AcreSync Preflight Holder", "customer", ref)
	if err != nil {
		return fmt.Errorf("creating the contact: %w", err)
	}
	fmt.Printf("  contact id     %s\n", contact.ID)
	fmt.Printf("  reference id   %s\n", contact.ReferenceID)
	fmt.Printf("  active         %v\n", contact.Active)

	// ---------------------------------------------------------------------
	section("Stage 4: register a fund account")
	// ---------------------------------------------------------------------

	// Razorpay publishes these as documentation values for sandbox testing. Using a documented test
	// account keeps this command from depending on anybody's real bank details.
	fa, err := client.CreateFundAccount(ctx, contact.ID, payout.BankDetails{
		Name:          "AcreSync Preflight Holder",
		IFSC:          "HDFC0000053",
		AccountNumber: "765432123456789",
	})
	if err != nil {
		return fmt.Errorf("creating the fund account: %w", err)
	}
	fmt.Printf("  fund account   %s\n", fa.ID)
	fmt.Printf("  contact        %s\n", fa.ContactID)
	fmt.Printf("  ifsc           %s\n", fa.IFSC)
	fmt.Printf("  active         %v\n", fa.Active)

	// ---------------------------------------------------------------------
	section("Stage 5: read the registration back")
	// ---------------------------------------------------------------------

	// Parsing a list response built from real data, which a fixture cannot vouch for.
	contacts, err := client.ListContacts(ctx, 5)
	if err != nil {
		return fmt.Errorf("listing contacts: %w", err)
	}
	fmt.Printf("  %d contact(s) read back and parsed\n", len(contacts))

	var seen bool
	for _, c := range contacts {
		if c.ID == contact.ID {
			seen = true
		}
	}
	if !seen {
		fmt.Printf("  NOTE: the contact just created was not in the first page. Ordering is the likely\n")
		fmt.Printf("  reason rather than a failure, but the registration is worth confirming by id.\n")
	} else {
		fmt.Printf("  the contact created above is present\n")
	}

	// ---------------------------------------------------------------------
	section("Stage 5b: are payouts actually enabled")
	// ---------------------------------------------------------------------

	// The read probes were ambiguous, so this settles it with a write attempt that cannot succeed.
	probe, err := client.DiagnosePayoutAvailability(ctx, fa.ID)
	if err != nil {
		return err
	}
	fmt.Printf("  with an impossible source account:\n    %s\n", probe.Summary())

	// And again with whatever the operator supplied, so the two responses can be compared. If a real
	// account number were the only thing missing, these would differ.
	if accountNumber != "" {
		supplied, err := client.DiagnosePayoutWithAccount(ctx, fa.ID, accountNumber)
		if err != nil {
			return err
		}
		fmt.Printf("  with the supplied account number:\n    %s\n", supplied.Summary())

		if supplied.ErrorDescription == probe.ErrorDescription {
			fmt.Printf("\n  Identical responses. The account number is not what the API is objecting to.\n")
		} else {
			fmt.Printf("\n  The responses differ, so the account number does reach the validation layer.\n")
			probe = supplied
		}
	}

	enabled, reason := probe.PayoutsEnabled()
	fmt.Printf("\n  %s\n", reason)

	if !enabled {
		fmt.Printf("\n  Nothing in the code can work around this. There is no source account for money\n")
		fmt.Printf("  to leave, so no value of ACRESYNC_RAZORPAY_ACCOUNT_NUMBER would help.\n")
	}

	if !full {
		fmt.Printf("\n  Stopping before the payout.\n")
		fmt.Printf("  Everything reachable without a source account number has now been exercised\n")
		fmt.Printf("  against the live sandbox: authentication, product access, request construction,\n")
		fmt.Printf("  response parsing, field validation and beneficiary registration.\n")
		fmt.Printf("\n  Sandbox records created:\n")
		fmt.Printf("    contact       %s\n", contact.ID)
		fmt.Printf("    fund account  %s\n", fa.ID)
		fmt.Printf("\n  Use this fund account id when a source account number becomes available:\n")
		fmt.Printf("    go run ./cmd/razorpayx-preflight -full\n")
		return nil
	}

	if accountNumber == "" {
		return errors.New("a payout needs ACRESYNC_RAZORPAY_ACCOUNT_NUMBER, which is the source " +
			"account the money leaves. Nothing above required it; this is the first step that does")
	}

	// ---------------------------------------------------------------------
	section("Stage 6: create a payout")
	// ---------------------------------------------------------------------

	key, err := payout.DeriveIdempotencyKey(
		"6f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8", // a fixed scheme id for the preflight
		"0123abcd-4567-89ef-0123-456789abcdef", // a fixed entitlement id
		1, 0, amount, fa.ID,
	)
	if err != nil {
		return err
	}

	req := payout.Request{
		IdempotencyKey: key,
		AccountNumber:  accountNumber,
		FundAccountID:  fa.ID,
		AmountPaise:    amount,
		Mode:           payout.ModeIMPS,
		Purpose:        payout.PurposePayout,
		ReferenceID:    "AS-PREFLIGHT-" + stamp,
		Narration:      payout.SanitiseNarration("AcreSync preflight"),
		Notes:          map[string]string{"preflight": stamp},
	}

	fmt.Printf("  amount         %s paise\n", amount)
	fmt.Printf("  mode           %s\n", req.Mode)
	fmt.Printf("  narration      %q\n", req.Narration)
	fmt.Printf("  idempotency    %s\n", req.IdempotencyKey)

	first, err := client.CreatePayout(ctx, req)
	if err != nil {
		return fmt.Errorf("creating the payout: %w", err)
	}

	fmt.Printf("\n  payout id      %s\n", first.ProviderID)
	fmt.Printf("  status         %s\n", first.Status)
	fmt.Printf("  amount         %s paise\n", first.AmountPaise)
	fmt.Printf("  fees / tax     %s / %s paise\n", first.FeesPaise, first.TaxPaise)
	fmt.Printf("  utr            %s\n", orNone(first.UTR))
	describeStatus(first.Status)

	// ---------------------------------------------------------------------
	section("Stage 7: fetch it back")
	// ---------------------------------------------------------------------

	fetched, err := client.FetchPayout(ctx, first.ProviderID)
	if err != nil {
		return fmt.Errorf("fetching the payout: %w", err)
	}
	fmt.Printf("  status         %s\n", fetched.Status)
	fmt.Printf("  utr            %s\n", orNone(fetched.UTR))
	if fetched.StatusDetails.Description != "" {
		fmt.Printf("  detail         %s (source %s)\n",
			fetched.StatusDetails.Description, fetched.StatusDetails.Source)
	}

	// The stored status mapping, exercised against a real response rather than a fixture.
	mappedAt, err := clock.Real().Now(ctx)
	if err != nil {
		return err
	}
	persisted, err := payout.ToDBStatus(fetched, mappedAt)
	if err != nil {
		fmt.Printf("  MAPPING PROBLEM: %v\n", err)
	} else {
		fmt.Printf("  maps to        %s\n", persisted.Status)
	}

	// ---------------------------------------------------------------------
	section("Stage 8: retry the same request (the double-payment check)")
	// ---------------------------------------------------------------------

	fmt.Printf("  Resending the identical request with the same idempotency key.\n")
	fmt.Printf("  This is what happens after a dropped connection: the caller cannot tell whether the\n")
	fmt.Printf("  first attempt reached the provider, so it retries blind.\n\n")

	second, err := client.CreatePayout(ctx, req)
	if err != nil {
		return fmt.Errorf("the retry failed, which means a dropped connection would leave a payout "+
			"in an unknown state: %w", err)
	}

	if second.ProviderID != first.ProviderID {
		return fmt.Errorf("THE RETRY CREATED A SECOND PAYOUT: %s then %s. A unitholder would be paid "+
			"twice. Do not run a distribution until this is understood",
			first.ProviderID, second.ProviderID)
	}
	fmt.Printf("  the retry returned the same payout (%s). No second transfer was created.\n",
		second.ProviderID)

	// ---------------------------------------------------------------------
	section("Stage 9: reuse the key with a different amount")
	// ---------------------------------------------------------------------

	fmt.Printf("  Resending with the same key and a larger amount.\n")
	fmt.Printf("  Our mock rejects this outright. Whether the live API does is an open question, and\n")
	fmt.Printf("  the answer changes what the orchestrator has to guard against.\n\n")

	altered := req
	altered.AmountPaise = amount + 100

	third, err := client.CreatePayout(ctx, altered)
	switch {
	case err != nil:
		fmt.Printf("  the API rejected it: %v\n", err)
		fmt.Printf("  This matches the mock's behaviour.\n")
	case third.ProviderID == first.ProviderID:
		fmt.Printf("  NOTE: the API silently returned the ORIGINAL payout (%s) for a changed amount.\n",
			third.ProviderID)
		fmt.Printf("  It did not reject and it did not apply the new amount. The mock is stricter than\n")
		fmt.Printf("  the live API here. That strictness now matters: a caller that recomputed an amount\n")
		fmt.Printf("  and reused the key would believe the new figure was sent while the old one stood,\n")
		fmt.Printf("  and only the local check would catch it.\n")
	default:
		return fmt.Errorf("A CHANGED AMOUNT UNDER THE SAME KEY CREATED A SECOND PAYOUT: %s. "+
			"The idempotency key does not cover the body, and every amount correction is a "+
			"double-payment risk", third.ProviderID)
	}

	section("Summary")
	fmt.Printf("  Authentication, beneficiary registration, payout creation, readback and the\n")
	fmt.Printf("  idempotent retry all behaved. Sandbox records created:\n")
	fmt.Printf("    contact       %s\n", contact.ID)
	fmt.Printf("    fund account  %s\n", fa.ID)
	fmt.Printf("    payout        %s\n", first.ProviderID)
	fmt.Printf("\n  A sandbox payout does not settle on a real rail, so the status will not progress\n")
	fmt.Printf("  the way a live one does. Re-run Stage 5 against the payout id above to watch it.\n")

	return nil
}

func section(title string) {
	fmt.Printf("\n%s\n%s\n", title, dashes(len(title)))
}

func dashes(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '-'
	}
	return string(b)
}

// describeKeyID reports enough to identify the key without disclosing it.
func describeKeyID(id string) string {
	if id == "" {
		return "UNSET"
	}
	if len(id) < 12 {
		return fmt.Sprintf("len=%d (suspiciously short)", len(id))
	}
	// ASCII only. This runs in a Windows console by default, where a multi-byte ellipsis renders as
	// mojibake and makes a diagnostic tool look broken at the exact moment somebody is using it to
	// work out whether something is broken.
	return fmt.Sprintf("%s... len=%d", id[:9], len(id))
}

func describeAccount(n string) string {
	if n == "" {
		return "UNSET  <- required before any payout can be created"
	}
	if len(n) <= 4 {
		return fmt.Sprintf("len=%d", len(n))
	}
	return fmt.Sprintf("...%s len=%d", n[len(n)-4:], len(n))
}

func orNone(s string) string {
	if s == "" {
		return "(none yet)"
	}
	return s
}

// describeStatus explains what the returned state means for a distribution.
func describeStatus(s payout.Status) {
	switch {
	case s.IsInFlight():
		fmt.Printf("  reading        in flight. Must not be reissued: for IMPS this can persist for\n")
		fmt.Printf("                 up to three working days under a deemed-success outcome, and\n")
		fmt.Printf("                 reissuing inside that window pays the holder twice.\n")
	case s.IsCreditConfirmed():
		fmt.Printf("  reading        credited. Still reversible: a bank or clearing house can reverse\n")
		fmt.Printf("                 a processed payout afterwards, which is why the period cannot be\n")
		fmt.Printf("                 unwound once fiat settles.\n")
	case s.NeedsReissue():
		fmt.Printf("  reading        failed without moving money. Safe to reissue, and the reissue\n")
		fmt.Printf("                 must use an incremented attempt so the key changes.\n")
	default:
		fmt.Printf("  reading        terminal.\n")
	}
}
