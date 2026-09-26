package payout

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/acresync/orchestrator/internal/canonical"
	"github.com/acresync/orchestrator/internal/clock"
	"github.com/acresync/orchestrator/internal/money"
)

// Mock is an in-memory payout provider modelling RazorpayX.
//
// # Why it does not advance on a timer
//
// State changes happen only when Advance, ForceReversal or a similar method is called. Nothing moves
// on a wall clock. Two reasons.
//
// A timer-driven mock makes tests depend on real elapsed time, so the distribution suite becomes
// slow and then flaky, and a flaky test covering fiat movement gets muted, which is worse than not
// having written it.
//
// More importantly, a timer would be modelling the wrong thing. We never observe a payout change
// state; we observe a webhook arriving or a poll returning. Explicit advancement is the faithful
// model, and it lets a test place a late reversal exactly where it hurts, such as after the period
// anchor has already been confirmed on-chain.
//
// # What it refuses to do
//
// The mock validates every transition against the documented lifecycle and rejects illegal ones. A
// permissive mock would let the orchestrator depend on transitions the real provider never performs,
// and the discrepancy would surface against live money.
type Mock struct {
	clk clock.Business

	mu       sync.Mutex
	seq      int
	payouts  map[string]*Payout            // providerID -> payout
	byKey    map[string]string             // idempotency key -> providerID
	keyBody  map[string][32]byte           // idempotency key -> request digest
	scripts  map[string]*script            // providerID -> remaining path
	outcomes map[string]Outcome            // fundAccountID -> scripted outcome
	debited  map[string]money.Paise        // providerID -> amount taken from the balance
	balance  money.Paise
	accountNumber string

	defaultOutcome Outcome
	failNextCreate error
	failNextFetch  error
}

// Outcome scripts how a payout will progress.
type Outcome struct {
	// Path is the sequence of states the payout moves through after creation, one per Advance call.
	Path []Status

	// Source and Description populate status_details on a failure state.
	Source      FailureSource
	Description string

	// FeesPaise and TaxPaise are charged once the payout reaches processing, matching the documented
	// behaviour that these appear only from that state onward.
	FeesPaise money.Paise
	TaxPaise  money.Paise
}

type script struct {
	remaining []Status
	outcome   Outcome
}

// SuccessfulOutcome is the ordinary path: accepted, processed by the bank, credited.
func SuccessfulOutcome() Outcome {
	return Outcome{
		Path:      []Status{StatusProcessing, StatusProcessed},
		FeesPaise: 590, // ₹5 plus 18% GST, the shape of a real per-payout charge.
		TaxPaise:  106,
	}
}

// ReversedOutcome models a transfer the beneficiary bank rejects.
func ReversedOutcome(source FailureSource, description string) Outcome {
	return Outcome{
		Path:        []Status{StatusProcessing, StatusReversed},
		Source:      source,
		Description: description,
		FeesPaise:   590,
		TaxPaise:    106,
	}
}

// LateReversalOutcome models the case the contract's reversal path exists for: the payout completes,
// and only afterwards does the clearing house or the beneficiary's bank reverse the credit.
//
// This is the scenario that breaks a naive design. By the time the reversal lands, the period may be
// anchored on-chain with a distributed figure that is now wrong, and the ledger has to be corrected
// in public rather than quietly repaired in the database.
func LateReversalOutcome() Outcome {
	return Outcome{
		Path:        []Status{StatusProcessing, StatusProcessed, StatusReversed},
		Source:      SourceBeneficiaryBank,
		Description: "Credit reversed by the clearing house after settlement",
		FeesPaise:   590,
		TaxPaise:    106,
	}
}

// DeemedSuccessOutcome models an IMPS payout held in processing while NPCI reconciles, for the given
// number of observation rounds before it resolves.
//
// The lingering is the point. Any policy that treats a long processing state as failure and reissues
// would double-pay here, and this outcome is how that bug gets caught in a test rather than against
// real money. The round count is a parameter so a caller can state how long the window is instead of
// depending on a number buried in this function.
func DeemedSuccessOutcome(lingerRounds int) Outcome {
	path := make([]Status, 0, lingerRounds+2)
	// The first entry is consumed as the state the payout is created in.
	path = append(path, StatusProcessing)
	for i := 0; i < lingerRounds; i++ {
		path = append(path, StatusProcessing)
	}
	path = append(path, StatusProcessed)

	return Outcome{Path: path, FeesPaise: 590, TaxPaise: 106}
}

// FailedOutcome models a payout the partner bank fails outright, where money never moved.
func FailedOutcome(description string) Outcome {
	return Outcome{
		Path:        []Status{StatusProcessing, StatusFailed},
		Source:      SourceGateway,
		Description: description,
	}
}

func NewMock(clk clock.Business, accountNumber string, openingBalance money.Paise) *Mock {
	return &Mock{
		clk:            clk,
		payouts:        map[string]*Payout{},
		byKey:          map[string]string{},
		keyBody:        map[string][32]byte{},
		scripts:        map[string]*script{},
		outcomes:       map[string]Outcome{},
		debited:        map[string]money.Paise{},
		balance:        openingBalance,
		accountNumber:  accountNumber,
		defaultOutcome: SuccessfulOutcome(),
	}
}

func (m *Mock) Name() string { return "MOCK" }

// SetDefaultOutcome sets the path for beneficiaries with no specific script.
func (m *Mock) SetDefaultOutcome(o Outcome) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.defaultOutcome = o
}

// ScriptOutcome sets the path for one beneficiary.
func (m *Mock) ScriptOutcome(fundAccountID string, o Outcome) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outcomes[fundAccountID] = o
}

// SetBalance sets the available balance in the source account.
func (m *Mock) SetBalance(p money.Paise) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.balance = p
}

// FailNextCreate arms a one-shot transport failure on CreatePayout.
//
// Models the dangerous case rather than a clean rejection: the request may have reached the provider
// and created a payout before the connection dropped. The caller cannot tell, which is exactly why
// the idempotency key has to make a blind retry safe.
func (m *Mock) FailNextCreate(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failNextCreate = err
}

func (m *Mock) FailNextFetch(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failNextFetch = err
}

func (m *Mock) CreatePayout(ctx context.Context, req Request) (*Payout, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}

	digest, err := requestDigest(req)
	if err != nil {
		return nil, err
	}

	now, err := m.clk.Now(ctx)
	if err != nil {
		return nil, fmt.Errorf("payout: reading business time: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Idempotency is resolved before the injected failure is considered.
	//
	// Order matters. A retry following a dropped connection has to return the existing payout even
	// though the previous attempt reported an error, because the first attempt may well have
	// succeeded server-side. Checking the failure first would make the retry create a second payout,
	// which is the exact double-payment the key exists to prevent.
	if existingID, ok := m.byKey[req.IdempotencyKey]; ok {
		if m.keyBody[req.IdempotencyKey] != digest {
			// Strictness beyond what the docs spell out, chosen deliberately.
			//
			// Returning the original payout for an altered request would be the dangerous
			// alternative: a caller who changed the amount would believe the new amount was sent
			// while the old one stood. Failing loudly turns a silent misstatement into a bug report.
			return nil, fmt.Errorf("%w: key %s was used for a different amount or beneficiary",
				ErrIdempotencyReuse, req.IdempotencyKey)
		}
		cp := *m.payouts[existingID]
		return &cp, nil
	}

	if m.failNextCreate != nil {
		e := m.failNextCreate
		m.failNextCreate = nil
		return nil, fmt.Errorf("payout: mock create failed: %w", e)
	}

	if req.AccountNumber != m.accountNumber {
		return nil, fmt.Errorf("%w: source account %q is not the configured account",
			ErrInvalidRequest, req.AccountNumber)
	}

	// Balance is checked against amount plus fees, because fees are charged to the business account
	// on top of the transfer. A balance that covers only the amounts would fail partway through a
	// batch, and the first failure would land on an arbitrary holder.
	outcome := m.outcomeFor(req.FundAccountID)
	needed, err := req.AmountPaise.Add(outcome.FeesPaise)
	if err != nil {
		return nil, err
	}
	if needed, err = needed.Add(outcome.TaxPaise); err != nil {
		return nil, err
	}
	if m.balance < needed {
		if req.QueueIfLowBalance {
			// Unreachable via Validate, which rejects the flag. Implemented anyway so the mock
			// models the provider rather than our policy: a future caller that legitimately needs
			// queuing finds the behaviour here instead of an omission.
			return m.insert(req, digest, StatusQueued, outcome, now, 0,
				StatusDetails{Description: "Payout queued: insufficient balance", Source: SourceBusiness}), nil
		}
		return nil, fmt.Errorf("%w: need %d paise including fees, balance is %d",
			ErrInsufficientFunds, needed, m.balance)
	}

	m.balance -= needed

	// A payout with no approval workflow and sufficient balance begins in processing, which is what
	// Razorpay documents for an account without the queued or approval features enabled.
	return m.insert(req, digest, StatusProcessing, outcome, now, needed, StatusDetails{}), nil
}

func (m *Mock) insert(req Request, digest [32]byte, initial Status, outcome Outcome, now time.Time, debited money.Paise, details StatusDetails) *Payout {
	m.seq++
	id := fmt.Sprintf("pout_%014d", m.seq)

	p := &Payout{
		ProviderID:    id,
		Status:        initial,
		AmountPaise:   req.AmountPaise,
		Mode:          req.Mode,
		ReferenceID:   req.ReferenceID,
		StatusDetails: details,
		CreatedAt:     now,
	}

	if initial == StatusProcessing {
		p.FeesPaise = outcome.FeesPaise
		p.TaxPaise = outcome.TaxPaise
	}

	// The scripted path begins after whatever state the payout was created in, so a path that opens
	// with processing does not replay it.
	remaining := outcome.Path
	if len(remaining) > 0 && remaining[0] == initial {
		remaining = remaining[1:]
	}
	m.scripts[id] = &script{remaining: remaining, outcome: outcome}

	m.payouts[id] = p
	m.byKey[req.IdempotencyKey] = id
	m.keyBody[req.IdempotencyKey] = digest
	m.debited[id] = debited

	cp := *p
	return &cp
}

func (m *Mock) outcomeFor(fundAccountID string) Outcome {
	if o, ok := m.outcomes[fundAccountID]; ok {
		return o
	}
	return m.defaultOutcome
}

func (m *Mock) FetchPayout(ctx context.Context, providerID string) (*Payout, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failNextFetch != nil {
		e := m.failNextFetch
		m.failNextFetch = nil
		return nil, fmt.Errorf("payout: mock fetch failed: %w", e)
	}

	p, ok := m.payouts[providerID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, providerID)
	}
	cp := *p
	return &cp, nil
}

func (m *Mock) Balance(ctx context.Context) (money.Paise, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.balance, nil
}

// Advance moves every payout one step along its scripted path and returns the IDs that changed.
//
// This is the test's stand-in for a batch of webhooks arriving, or for a reconciliation poll. A
// payout whose path is exhausted stays where it is, so calling Advance repeatedly settles into a
// stable state rather than drifting.
func (m *Mock) Advance(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Sorted so the order of observed changes is reproducible. Map iteration order would make a
	// failure depend on the run, and a reconciliation bug that appears one run in five is a bug
	// nobody can bisect.
	ids := make([]string, 0, len(m.payouts))
	for id := range m.payouts {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var changed []string
	for _, id := range ids {
		sc := m.scripts[id]
		if sc == nil || len(sc.remaining) == 0 {
			continue
		}
		next := sc.remaining[0]
		p := m.payouts[id]

		if err := CheckTransition(p.Status, next); err != nil {
			return changed, fmt.Errorf("payout: mock script for %s is not a legal path: %w", id, err)
		}
		sc.remaining = sc.remaining[1:]

		if next == p.Status {
			// A repeated state models a payout still sitting in processing, which is what deemed
			// success looks like from outside. Not reported as a change, because nothing changed,
			// and a reconciler that treated it as an event would act on nothing.
			continue
		}

		if err := m.applyLocked(p, next, sc.outcome); err != nil {
			return changed, err
		}
		changed = append(changed, id)
	}
	return changed, nil
}

// ForceReversal reverses a payout regardless of its script.
//
// Present so a test can reverse a payout at a chosen moment, in particular after the period anchor is
// confirmed on-chain. The interesting failure is not that a reversal happens, it is when it happens.
func (m *Mock) ForceReversal(ctx context.Context, providerID string, source FailureSource, description string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	p, ok := m.payouts[providerID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, providerID)
	}
	if err := CheckTransition(p.Status, StatusReversed); err != nil {
		return err
	}
	sc := m.scripts[providerID]
	if sc != nil {
		sc.remaining = nil
	}
	return m.applyLocked(p, StatusReversed, Outcome{Source: source, Description: description,
		FeesPaise: p.FeesPaise, TaxPaise: p.TaxPaise})
}

// Cancel cancels a queued payout, which is the only state the provider permits cancellation from.
func (m *Mock) Cancel(ctx context.Context, providerID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	p, ok := m.payouts[providerID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, providerID)
	}
	if p.Status != StatusQueued {
		return fmt.Errorf("%w: cancellation is only possible from queued, this payout is %s",
			ErrIllegalTransition, p.Status)
	}
	return m.applyLocked(p, StatusCancelled, Outcome{})
}

func (m *Mock) applyLocked(p *Payout, next Status, outcome Outcome) error {
	if err := CheckTransition(p.Status, next); err != nil {
		return err
	}

	switch next {
	case StatusProcessing:
		p.FeesPaise = outcome.FeesPaise
		p.TaxPaise = outcome.TaxPaise

	case StatusProcessed:
		// The UTR appears only on a confirmed credit, because that is the only point at which a
		// bank reference exists. Emitting one earlier would let reconciliation match a transfer that
		// had not happened.
		p.UTR = mockUTR(p.ProviderID)

	case StatusReversed, StatusFailed, StatusCancelled:
		// The refund is exactly what was debited, tracked per payout rather than recomputed.
		//
		// Recomputing it as amount plus fees plus tax looks equivalent and is not. A queued payout is
		// never debited, because the whole reason it queued is that the balance could not cover it,
		// so cancelling one would have credited the account with money it never spent. Refunding the
		// recorded debit and clearing it also makes a second terminal transition a no-op instead of a
		// second credit.
		//
		// Razorpay documents the reversal transaction as returning the amount together with fees and
		// tax, which is what the recorded debit contains.
		refund := m.debited[p.ProviderID]
		if refund > 0 {
			newBal, err := m.balance.Add(refund)
			if err != nil {
				return err
			}
			m.balance = newBal
			m.debited[p.ProviderID] = 0
		}

		p.StatusDetails = StatusDetails{
			Description: outcome.Description,
			Source:      outcome.Source,
			Reason:      string(next),
		}
	}

	p.Status = next

	raw, err := json.Marshal(map[string]any{
		"id":     p.ProviderID,
		"entity": "payout",
		"status": string(next),
		"amount": int64(p.AmountPaise),
		"utr":    p.UTR,
		"fees":   int64(p.FeesPaise),
		"tax":    int64(p.TaxPaise),
	})
	if err != nil {
		return err
	}
	p.Raw = raw
	return nil
}

// mockUTR produces a stable, bank-shaped reference.
//
// Derived from the payout id so a rerun yields the same value, which keeps reconciliation fixtures
// reproducible. Real UTRs look like a bank prefix followed by a serial.
func mockUTR(providerID string) string {
	h := sha256.Sum256([]byte("acresync.mock.utr|" + providerID))
	return "MOCKN" + hex.EncodeToString(h[:5])
}

// requestDigest canonicalises a request for idempotency comparison.
//
// Canonical encoding is used rather than struct equality so the comparison does not depend on map
// ordering, and so the digest is stable across a refactor of the Go types. The key itself is
// excluded, since it is the lookup, not part of the body being compared.
func requestDigest(r Request) ([32]byte, error) {
	notes := map[string]any{}
	for k, v := range r.Notes {
		notes[k] = v
	}
	b, err := canonical.Marshal(map[string]any{
		"accountNumber":     r.AccountNumber,
		"fundAccountId":     r.FundAccountID,
		"amountPaise":       r.AmountPaise,
		"mode":              string(r.Mode),
		"purpose":           string(r.Purpose),
		"queueIfLowBalance": r.QueueIfLowBalance,
		"referenceId":       r.ReferenceID,
		"narration":         r.Narration,
		"notes":             notes,
	})
	if err != nil {
		return [32]byte{}, fmt.Errorf("payout: canonicalising request: %w", err)
	}
	return sha256.Sum256(b), nil
}
