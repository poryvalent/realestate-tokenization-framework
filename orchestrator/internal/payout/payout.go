package payout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/acresync/orchestrator/internal/idempotency"
	"github.com/acresync/orchestrator/internal/money"
)

var (
	ErrInvalidRequest   = errors.New("payout: invalid request")
	ErrIdempotencyReuse = errors.New("payout: idempotency key reused with a different request")
	ErrNotFound         = errors.New("payout: no such payout")
	ErrInsufficientFunds = errors.New("payout: insufficient balance in the business account")
)

// Field limits transcribed from Razorpay's Create Payout reference.
//
// Enforced locally so a violation surfaces as a named validation error against a specific field,
// rather than as an opaque HTTP 400 mid-batch. During a distribution run the difference matters: a
// clear message identifies the one holder whose record is wrong, while a generic rejection means
// auditing the whole batch to find it.
const (
	// MinAmountPaise is Razorpay's documented floor of 100 paise, that is ₹1.
	MinAmountPaise = money.Paise(100)

	MaxReferenceIDLen = 40
	MaxNarrationLen   = 30
	MaxNotes          = 15
	MaxNoteValueLen   = 256
)

// Request is an instruction to move money to one beneficiary.
type Request struct {
	// IdempotencyKey is sent as the X-Payout-Idempotency header.
	//
	// Razorpay supports idempotency on Create Payout, which is what makes a retry after a network
	// timeout safe. It is required here rather than optional: a payout request without one is a
	// double-payment waiting for a dropped connection, and the safe choice must not be the one a
	// caller has to remember.
	IdempotencyKey string

	// AccountNumber is the source account the money leaves.
	AccountNumber string

	// FundAccountID is the beneficiary's registered fund account, fa_xxx.
	//
	// A fund account identifier, never raw bank details. The account number and IFSC live with
	// Razorpay under their registration flow, so this package handles an opaque reference instead of
	// beneficiary banking data. That keeps the payout path outside the blast radius of a leak here
	// and keeps bank details out of our logs by construction.
	FundAccountID string

	AmountPaise money.Paise
	Mode        Mode
	Purpose     Purpose

	// QueueIfLowBalance controls whether a shortfall queues the payout or fails it.
	//
	// AcreSync sets this false. A queued payout sits until funded, and Razorpay documents that one
	// left queued beyond three months is failed automatically. For a distribution run, a payout
	// silently waiting on balance is worse than a clean failure: the period looks like it is
	// progressing, no operator is alerted, and the regulatory clock keeps running. Failing loudly
	// means someone funds the account and reissues deliberately.
	QueueIfLowBalance bool

	// ReferenceID is our own identifier, echoed back by the provider.
	ReferenceID string

	// Narration is the text on the beneficiary's bank statement.
	Narration string

	// Notes carry structured context back on the provider record.
	//
	// Restricted to internal references. Notes are stored by a third party and returned in API
	// responses and dashboards, so a holder's name or PAN placed here would leak outside the
	// boundary the IPFS guard is built to defend, just through a different door.
	Notes map[string]string
}

// Payout is the provider's record of an instruction.
type Payout struct {
	// ProviderID is Razorpay's identifier, pout_xxx.
	ProviderID string

	Status      Status
	AmountPaise money.Paise

	// UTR is the bank's unique transaction reference.
	//
	// The only identifier a unitholder's own bank statement shares with our records, which makes it
	// the thing that resolves "I never received it" without either side taking the other's word.
	// Populated once the payout reaches processed.
	UTR string

	Mode        Mode
	ReferenceID string

	// FeesPaise and TaxPaise are charged to the business account, separately from AmountPaise.
	//
	// They are not deducted from the beneficiary, which matters for the distribution arithmetic: the
	// unitholder receives AmountPaise exactly, and the fee is a scheme expense. Netting fees out of
	// the distributed figure would understate what was distributed and misstate the 95% floor.
	FeesPaise money.Paise
	TaxPaise  money.Paise

	StatusDetails StatusDetails
	CreatedAt     time.Time

	// Raw is the provider's unmodified response.
	//
	// Kept because a reconciliation dispute is settled by what the provider actually said, not by
	// our parse of it. A field we did not model, or modelled wrongly, is still in here.
	Raw json.RawMessage
}

// NeedsReissueNow reports whether this payout requires a fresh instruction.
//
// A method on the payout rather than a free function on the status so there is one obvious thing to
// call at the decision point, and no opportunity to reach for elapsed time instead. There is
// deliberately no duration parameter: the only input is the state the provider reports. An IMPS
// payout can sit in processing for up to T+3 working days under a deemed-success outcome, and any
// policy that reissued on a timer would pay the unitholder twice.
func (p *Payout) NeedsReissueNow() bool {
	return p.Status.NeedsReissue()
}

// StatusDetails is Razorpay's explanation for the current state.
type StatusDetails struct {
	Description string
	Source      FailureSource
	Reason      string
}

// Provider instructs and queries fiat transfers.
type Provider interface {
	// CreatePayout instructs a transfer. Safe to retry with the same Request.
	CreatePayout(ctx context.Context, req Request) (*Payout, error)

	// FetchPayout reads the current state.
	//
	// Polling exists alongside webhooks because a webhook is a delivery attempt, not a guarantee,
	// and a missed one would leave a payout in flight forever from our side. Reconciliation has to
	// be able to ask rather than only listen.
	FetchPayout(ctx context.Context, providerID string) (*Payout, error)

	// Balance reports the available balance in the source account.
	Balance(ctx context.Context) (money.Paise, error)

	Name() string
}

// DeriveIdempotencyKey builds the key for one payout attempt.
//
// # Why attempt is a parameter
//
// Idempotency has to serve two opposed requirements. A retry after a dropped connection must reuse
// the key, or the unitholder is paid twice and recovering the second credit means asking an investor
// to send money back. A genuine reissue after a failed transfer must use a different key, or the
// provider keeps returning the original failed record and the unitholder is never paid at all.
//
// One key derived only from the payout's identity satisfies the first and breaks the second. So the
// attempt generation is explicit, and the rule is narrow: the caller persists it, and increments it
// only when the previous attempt reached a state where NeedsReissue is true. Never on a timeout,
// never on an error reading the status, and never while the payout is in flight, because in all of
// those cases the money may already be moving.
//
// # Why the amount is in the key
//
// A corrected amount is a different instruction and must not be deduped onto the earlier one. The
// inverse mistake, reusing a key with an altered body, is caught by the provider, which rejects it
// rather than quietly returning the stale record.
func DeriveIdempotencyKey(schemeID, entitlementID string, periodID uint32, attempt uint32, amount money.Paise, fundAccountID string) (string, error) {
	k, err := idempotency.Derive(idempotency.Input{
		Action:   idempotency.ActionInstructPayout,
		SchemeID: schemeID,
		ScopeID:  entitlementID,
		Payload: map[string]any{
			"periodId":      periodID,
			"attempt":       attempt,
			"amountPaise":   amount,
			"fundAccountId": fundAccountID,
		},
	})
	if err != nil {
		return "", err
	}
	return k.Hex(), nil
}

// Validate checks a request against the provider's documented constraints.
func (r Request) Validate() error {
	var problems []string

	if r.IdempotencyKey == "" {
		problems = append(problems, "idempotency key is required; without one a retried request can pay twice")
	}
	if r.AccountNumber == "" {
		problems = append(problems, "source account number is required")
	}
	if r.FundAccountID == "" {
		problems = append(problems, "fund account id is required")
	}
	if !strings.HasPrefix(r.FundAccountID, "fa_") && r.FundAccountID != "" {
		problems = append(problems, fmt.Sprintf("fund account id %q should carry the fa_ prefix", r.FundAccountID))
	}

	if r.AmountPaise < MinAmountPaise {
		problems = append(problems, fmt.Sprintf(
			"amount %d paise is below the provider minimum of %d paise", r.AmountPaise, MinAmountPaise))
	}
	if !r.Mode.Valid() {
		problems = append(problems, fmt.Sprintf(
			"mode %q is not one of IMPS, NEFT, RTGS; the provider treats these as case-sensitive", r.Mode))
	}
	if !r.Purpose.Valid() {
		problems = append(problems, fmt.Sprintf(
			"purpose %q is not a built-in classification, and new purposes cannot be created over the API", r.Purpose))
	}

	if len(r.ReferenceID) > MaxReferenceIDLen {
		problems = append(problems, fmt.Sprintf(
			"reference id is %d characters, limit %d", len(r.ReferenceID), MaxReferenceIDLen))
	}

	// The narration is checked strictly because the allowed set is narrow and a rejection here is
	// silent in the worst way: the obvious value to put on a statement is a period or holder
	// reference, and those carry hyphens, which are not permitted.
	if len(r.Narration) > MaxNarrationLen {
		problems = append(problems, fmt.Sprintf(
			"narration is %d characters, limit %d", len(r.Narration), MaxNarrationLen))
	}
	if bad := invalidNarrationChars(r.Narration); bad != "" {
		problems = append(problems, fmt.Sprintf(
			"narration contains %s; only letters, digits and spaces are accepted", bad))
	}

	if len(r.Notes) > MaxNotes {
		problems = append(problems, fmt.Sprintf("%d notes, limit %d", len(r.Notes), MaxNotes))
	}
	for k, v := range r.Notes {
		if len(v) > MaxNoteValueLen {
			problems = append(problems, fmt.Sprintf(
				"note %q is %d characters, limit %d", k, len(v), MaxNoteValueLen))
		}
	}

	if r.QueueIfLowBalance {
		problems = append(problems, "queueIfLowBalance must be false: a queued payout stalls a "+
			"distribution without alerting anyone and is auto-failed after three months")
	}

	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalidRequest, strings.Join(problems, "; "))
	}
	return nil
}

func invalidNarrationChars(s string) string {
	var bad []string
	seen := map[rune]bool{}
	for _, r := range s {
		ok := r == ' ' || unicode.IsDigit(r) || (r < unicode.MaxASCII && unicode.IsLetter(r))
		if !ok && !seen[r] {
			seen[r] = true
			bad = append(bad, fmt.Sprintf("%q", r))
		}
	}
	if len(bad) == 0 {
		return ""
	}
	return strings.Join(bad, ", ")
}

// SanitiseNarration reduces a string to the characters the provider accepts.
//
// Offered as an explicit call rather than applied inside Validate. Silently rewriting a caller's
// narration would mean the text on an investor's bank statement differs from what the code appears
// to send, and the first person to notice would be the investor.
func SanitiseNarration(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == ' ' || unicode.IsDigit(r) || (r < unicode.MaxASCII && unicode.IsLetter(r)):
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '/' || r == '.':
			b.WriteRune(' ')
		}
	}
	// Truncation happens at a word boundary rather than at the character limit.
	//
	// Cutting mid-word leaves text like "AcreSync Distribution Period T" on an investor's bank
	// statement, which reads as corruption. Razorpay also notes that banks may truncate further and
	// that the important text should sit in the first nine characters, so the useful content is at
	// the front and dropping a trailing partial word costs nothing.
	words := strings.Fields(b.String())
	out := ""
	for _, w := range words {
		candidate := w
		if out != "" {
			candidate = out + " " + w
		}
		if len(candidate) > MaxNarrationLen {
			break
		}
		out = candidate
	}
	// A single word longer than the limit has no boundary to break on, so it is cut directly. The
	// alternative would be an empty narration, which loses more.
	if out == "" && len(words) > 0 {
		out = words[0][:MaxNarrationLen]
	}
	return out
}
