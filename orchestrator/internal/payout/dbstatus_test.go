package payout

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/acresync/orchestrator/internal/money"
)

var mapAt = time.Date(2026, 9, 30, 18, 30, 0, 0, time.UTC)

// TestEveryProviderStatusMaps is the check that stops an unmapped state stalling a payout.
//
// A provider status with no stored equivalent would leave the row at whatever it was before, so the
// payout's real state would be invisible. Every one of the nine has to land somewhere.
func TestEveryProviderStatusMaps(t *testing.T) {
	want := map[Status]DBStatus{
		StatusPending:    DBQueued,
		StatusQueued:     DBQueued,
		StatusScheduled:  DBQueued,
		StatusProcessing: DBProcessing,
		StatusProcessed:  DBSettled,
		StatusReversed:   DBReversed,
		StatusCancelled:  DBFailed,
		StatusRejected:   DBFailed,
		StatusFailed:     DBFailed,
	}

	for _, s := range AllStatuses() {
		p := &Payout{ProviderID: "pout_1", Status: s, AmountPaise: money.Paise(1000)}

		// The settled path requires a bank reference, so supply one for the states that reach it.
		if s == StatusProcessed || s == StatusReversed {
			p.UTR = "MOCKN0011223344"
		}

		got, err := ToDBStatus(p, mapAt)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if got.Status != want[s] {
			t.Errorf("%s maps to %s, want %s", s, got.Status, want[s])
		}
		if !got.Status.Valid() {
			t.Errorf("%s produced an invalid stored status %q", s, got.Status)
		}
	}
}

// TestSettledRequiresABankReference covers the constraint and the reason behind it.
//
// payouts_settled_has_utr rejects a SETTLED row without a UTR. The UTR is the only identifier our
// records share with the holder's own bank statement, so a settled row without one cannot resolve an
// "I never received it" dispute either way.
func TestSettledRequiresABankReference(t *testing.T) {
	p := &Payout{ProviderID: "pout_1", Status: StatusProcessed, AmountPaise: money.Paise(1000)}

	_, err := ToDBStatus(p, mapAt)
	if err == nil {
		t.Fatal("a processed payout with no UTR must be refused rather than stored as SETTLED")
	}
	if !strings.Contains(err.Error(), "UTR") {
		t.Errorf("the error should name the missing reference, got: %v", err)
	}

	p.UTR = "HDFCN0001234567"
	got, err := ToDBStatus(p, mapAt)
	if err != nil {
		t.Fatal(err)
	}
	if got.UTR != "HDFCN0001234567" {
		t.Errorf("UTR = %q", got.UTR)
	}
	if got.SettledAt == nil {
		t.Error("a settled row needs a settlement timestamp; the constraint requires both")
	}
}

// TestFailedAlwaysCarriesACode covers the other paired constraint.
//
// payouts_failed_has_code rejects a FAILED row with no code. A row that cannot be written is worse than
// a coarse code, because the payout would stay at its previous status and look healthy.
func TestFailedAlwaysCarriesACode(t *testing.T) {
	for _, s := range []Status{StatusCancelled, StatusRejected, StatusFailed} {
		t.Run(string(s), func(t *testing.T) {
			// No status details at all, which is the worst case for producing a code.
			p := &Payout{ProviderID: "pout_1", Status: s, AmountPaise: money.Paise(1000)}

			got, err := ToDBStatus(p, mapAt)
			if err != nil {
				t.Fatal(err)
			}
			if got.FailureCode == "" {
				t.Fatal("a FAILED row must carry a code even when the provider gave no detail")
			}
			if got.FailureCode != string(s) {
				t.Errorf("code = %q, want the status as a fallback", got.FailureCode)
			}
		})
	}

	// With detail, the code carries the source and reason so the operational response is clear.
	p := &Payout{
		ProviderID: "pout_1", Status: StatusFailed, AmountPaise: money.Paise(1000),
		StatusDetails: StatusDetails{Source: SourceBeneficiaryBank, Reason: "account_closed"},
	}
	got, err := ToDBStatus(p, mapAt)
	if err != nil {
		t.Fatal(err)
	}
	if got.FailureCode != "beneficiary_bank/account_closed" {
		t.Errorf("code = %q", got.FailureCode)
	}
}

// TestReversedKeepsTheOriginalReference matters for tying two bank entries together.
//
// The credit did happen and was undone. Dropping the UTR would leave the reversal unmatchable against
// the original debit on a statement.
func TestReversedKeepsTheOriginalReference(t *testing.T) {
	p := &Payout{
		ProviderID: "pout_1", Status: StatusReversed, AmountPaise: money.Paise(1000),
		UTR:           "HDFCN0009999999",
		StatusDetails: StatusDetails{Source: SourceGateway, Reason: "clearing_house_reversal"},
	}

	got, err := ToDBStatus(p, mapAt)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != DBReversed {
		t.Errorf("status = %s", got.Status)
	}
	if got.UTR != "HDFCN0009999999" {
		t.Error("a reversal must keep the original bank reference")
	}
	if got.FailureCode == "" {
		t.Error("a reversal should record why")
	}
}

func TestUnknownStatusRefusedByMapper(t *testing.T) {
	p := &Payout{ProviderID: "pout_1", Status: Status("settled"), AmountPaise: money.Paise(1000)}
	if _, err := ToDBStatus(p, mapAt); !errors.Is(err, ErrUnknownStatus) {
		t.Fatalf("want ErrUnknownStatus, got %v", err)
	}
	if _, err := ToDBStatus(nil, mapAt); err == nil {
		t.Error("a nil payout must be refused")
	}
}

// TestSubmittedHasNoProviderCounterpart records a deliberate asymmetry.
//
// SUBMITTED is ours: the instruction was handed over and no response has been seen. It exists to
// describe the window a crash between the request and the response leaves behind, so nothing maps onto
// it from a provider reply.
func TestSubmittedHasNoProviderCounterpart(t *testing.T) {
	for _, s := range AllStatuses() {
		p := &Payout{ProviderID: "pout_1", Status: s, AmountPaise: money.Paise(1000)}
		if s == StatusProcessed || s == StatusReversed {
			p.UTR = "MOCKN0011223344"
		}
		got, err := ToDBStatus(p, mapAt)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == DBSubmitted {
			t.Fatalf("%s mapped to SUBMITTED, which is ours to set and not the provider's to report", s)
		}
	}
}

// TestSettledTallyCountsOnlyConfirmedCredits guards the figure anchored on-chain.
//
// A payout in flight has not settled and one that failed moved no money, so counting either would
// anchor a settled figure the bank statements do not support.
func TestSettledTallyCountsOnlyConfirmedCredits(t *testing.T) {
	payouts := []*Payout{
		{ProviderID: "a", Status: StatusProcessed, AmountPaise: money.Paise(1000), UTR: "u1"},
		{ProviderID: "b", Status: StatusProcessed, AmountPaise: money.Paise(2500), UTR: "u2"},
		{ProviderID: "c", Status: StatusProcessing, AmountPaise: money.Paise(9999)},
		{ProviderID: "d", Status: StatusFailed, AmountPaise: money.Paise(7777)},
		{ProviderID: "e", Status: StatusReversed, AmountPaise: money.Paise(4444)},
		{ProviderID: "f", Status: StatusQueued, AmountPaise: money.Paise(1111)},
	}

	count, total, err := SettledCountAndAmount(payouts)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
	if total != 3500 {
		t.Errorf("total = %d, want 3500", total)
	}
}

// TestReversedCreditIsNotCountedAsSettled is the case that would overstate a distribution.
//
// A reversal means the money came back. Including it would anchor a settled total the escrow account
// does not reflect, and the discrepancy would only surface at the next reconciliation.
func TestReversedCreditIsNotCountedAsSettled(t *testing.T) {
	count, total, err := SettledCountAndAmount([]*Payout{
		{ProviderID: "a", Status: StatusReversed, AmountPaise: money.Paise(5000), UTR: "u1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 || total != 0 {
		t.Fatalf("a reversed credit counted as settled: %d payouts, %d paise", count, total)
	}
}

func TestSettledTallyRejectsBadInput(t *testing.T) {
	if _, _, err := SettledCountAndAmount([]*Payout{nil}); err == nil {
		t.Error("a nil entry must be refused")
	}
	if _, _, err := SettledCountAndAmount([]*Payout{
		{ProviderID: "a", Status: Status("done")},
	}); !errors.Is(err, ErrUnknownStatus) {
		t.Error("an unmodelled status must be refused rather than skipped")
	}
}

func TestDBStatusEnumMatchesPostgres(t *testing.T) {
	pg := []string{"QUEUED", "SUBMITTED", "PROCESSING", "SETTLED", "FAILED", "REVERSED"}
	for _, name := range pg {
		if !DBStatus(name).Valid() {
			t.Errorf("%s is in the payout_status enum but unmapped here", name)
		}
	}
	if len(AllDBStatuses()) != len(pg) {
		t.Errorf("AllDBStatuses has %d entries, the enum has %d", len(AllDBStatuses()), len(pg))
	}
	if DBStatus("PENDING").Valid() {
		t.Error("PENDING is a provider state, not one of ours")
	}
}
