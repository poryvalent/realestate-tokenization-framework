package asba

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"

	"github.com/acresync/orchestrator/internal/clock"
	"github.com/acresync/orchestrator/internal/money"
)

// Mock is an in-memory ASBA provider.
//
// # What it models and what it refuses to model
//
// It models a bank that holds investors' balances, places all-or-nothing blocks against them, and
// settles a block by debiting an amount and releasing the balance in one instruction.
//
// It deliberately does not model a "collected funds" balance on our side, because there is no such
// thing. Money blocked under ASBA belongs to the investor and sits with their bank throughout. A mock
// with a house balance would be a mock of a different, and illegal, arrangement, and code written
// against it would quietly assume custody we never have.
//
// State changes happen only when a method is called. Nothing advances on a timer, for the same reasons
// as the payout mock: a wall clock makes the suite slow and then flaky, and it models the wrong thing,
// since we learn of a block's outcome from a notification or a poll rather than by waiting.
type Mock struct {
	clk clock.Business

	mu     sync.Mutex
	seq    int
	blocks map[string]*Block // bankRef -> block

	// byKey resolves a retried request to the block it already created.
	byKey   map[string]string
	keyBody map[string][32]byte

	// balances is what each investor account can cover, keyed by BankAccountRef.
	balances map[string]money.Paise

	// held tracks the amount currently blocked per account, so a second block cannot reserve money the
	// first one already took. Without this an investor with one unit of headroom could back two bids.
	held map[string]money.Paise

	// mandates records which accounts have authorised blocking.
	mandates map[string]string

	// refToAccount maps a bank reference back to the account it was placed against, so settlement can
	// release the hold on the right balance.
	refToAccount map[string]string

	failNextBlock  error
	failNextSettle error
	failNextFetch  error

	// refuseNextBlock makes the bank decline rather than error, which is the more common real outcome
	// and produces a FAILED block rather than a transport failure.
	refuseNextBlock string
}

func NewMock(clk clock.Business) *Mock {
	return &Mock{
		clk:      clk,
		blocks:   map[string]*Block{},
		byKey:    map[string]string{},
		keyBody:  map[string][32]byte{},
		balances:     map[string]money.Paise{},
		held:         map[string]money.Paise{},
		mandates:     map[string]string{},
		refToAccount: map[string]string{},
	}
}

func (m *Mock) Name() string { return "MOCK" }

// Fund sets an investor account's available balance and registers a mandate.
func (m *Mock) Fund(accountRef string, balance money.Paise, mandateRef string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.balances[accountRef] = balance
	m.mandates[accountRef] = mandateRef
}

// RevokeMandate removes blocking authorisation, modelling an investor who withdrew consent.
func (m *Mock) RevokeMandate(accountRef string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.mandates, accountRef)
}

// Available reports an account's unblocked balance.
func (m *Mock) Available(accountRef string) money.Paise {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.balances[accountRef] - m.held[accountRef]
}

// Held reports how much of an account is currently blocked.
func (m *Mock) Held(accountRef string) money.Paise {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.held[accountRef]
}

// FailNextBlock arms a one-shot transport failure, where the bank may or may not have acted.
func (m *Mock) FailNextBlock(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failNextBlock = err
}

// RefuseNextBlock makes the bank decline the next request with a reason.
//
// Distinct from FailNextBlock: a refusal is an answer, producing a FAILED block with a code, while a
// transport failure leaves the outcome unknown and is what the idempotency key has to survive.
func (m *Mock) RefuseNextBlock(code string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refuseNextBlock = code
}

func (m *Mock) FailNextSettle(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failNextSettle = err
}

func (m *Mock) FailNextFetch(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failNextFetch = err
}

func (m *Mock) RequestBlock(ctx context.Context, req BlockRequest) (*Block, error) {
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
		return nil, fmt.Errorf("asba: reading business time: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Idempotency is resolved before any injected failure, for the same reason as on a payout: a retry
	// after a dropped connection must return the existing block rather than place a second one. Checking
	// the failure first would double-block the investor's account.
	if ref, ok := m.byKey[req.IdempotencyKey]; ok {
		if m.keyBody[req.IdempotencyKey] != digest {
			return nil, fmt.Errorf("%w: key %s was used for a different bid or amount",
				ErrIdempotencyReuse, req.IdempotencyKey)
		}
		cp := *m.blocks[ref]
		return &cp, nil
	}

	if m.failNextBlock != nil {
		e := m.failNextBlock
		m.failNextBlock = nil
		return nil, fmt.Errorf("asba: mock block failed: %w", e)
	}

	m.seq++
	ref := fmt.Sprintf("ASBA%08d", m.seq)

	b := &Block{
		BidID:          req.BidID,
		Provider:       m.Name(),
		BankRef:        ref,
		RequestedPaise: req.AmountPaise,
		Status:         StatusRequested,
		RequestedAt:    now,
	}

	fail := func(code string) *Block {
		b.Status = StatusFailed
		b.FailureCode = code
		m.blocks[ref] = b
		m.byKey[req.IdempotencyKey] = ref
		m.keyBody[req.IdempotencyKey] = digest
		cp := *b
		return &cp
	}

	if m.refuseNextBlock != "" {
		code := m.refuseNextBlock
		m.refuseNextBlock = ""
		return fail(code), nil
	}

	// A mandate is what authorises the bank to freeze anything. Without one the request is refused
	// rather than errored, because the bank answered.
	if mandate, ok := m.mandates[req.BankAccountRef]; !ok || mandate != req.MandateRef {
		return fail("mandate_not_found"), nil
	}

	// All or nothing. A partial block would leave the bid underfunded by exactly the amount it was
	// short, and admitting it to the book would defer the failure to debit time, after the cap table
	// was anchored.
	available := m.balances[req.BankAccountRef] - m.held[req.BankAccountRef]
	if available < req.AmountPaise {
		return fail("insufficient_funds"), nil
	}

	b.Status = StatusBlocked
	b.BlockedPaise = req.AmountPaise
	blockedAt := now
	b.BlockedAt = &blockedAt

	m.held[req.BankAccountRef] += req.AmountPaise

	// The account is remembered so settlement can release against it.
	m.accountFor(ref, req.BankAccountRef)

	m.blocks[ref] = b
	m.byKey[req.IdempotencyKey] = ref
	m.keyBody[req.IdempotencyKey] = digest

	cp := *b
	return &cp, nil
}

func (m *Mock) accountFor(bankRef, accountRef string) {
	m.refToAccount[bankRef] = accountRef
}

func (m *Mock) Settle(ctx context.Context, req SettleRequest) (*Block, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.IdempotencyKey == "" {
		return nil, fmt.Errorf("%w: idempotency key is required", ErrInvalidRequest)
	}
	if req.DebitPaise < 0 {
		return nil, fmt.Errorf("%w: debit cannot be negative, got %d", ErrInvalidRequest, req.DebitPaise)
	}

	now, err := m.clk.Now(ctx)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	b, ok := m.blocks[req.BankRef]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, req.BankRef)
	}

	// A settled block restated with the same instruction is a no-op, which is what makes a retry safe.
	if b.Status.IsTerminal() {
		if prev, seen := m.byKey[req.IdempotencyKey]; seen && prev == req.BankRef {
			cp := *b
			return &cp, nil
		}
		return nil, fmt.Errorf("%w: %s is already %s", ErrIllegalTransition, req.BankRef, b.Status)
	}

	if m.failNextSettle != nil {
		e := m.failNextSettle
		m.failNextSettle = nil
		return nil, fmt.Errorf("asba: mock settle failed: %w", e)
	}

	if b.Status != StatusBlocked {
		return nil, fmt.Errorf("%w: %s is %s, settlement requires BLOCKED",
			ErrIllegalTransition, req.BankRef, b.Status)
	}
	if req.DebitPaise > b.BlockedPaise {
		return nil, fmt.Errorf("%w: %s asked to debit %d of %d blocked",
			ErrDebitExceedsBlock, req.BankRef, req.DebitPaise, b.BlockedPaise)
	}

	released, err := b.BlockedPaise.Sub(req.DebitPaise)
	if err != nil {
		return nil, err
	}

	b.DebitedPaise = req.DebitPaise
	b.ReleasedPaise = released

	// A debit of nothing is a release, not a zero-value debit. Naming it correctly keeps the settled
	// count equal to the number of investors who actually paid for units.
	if req.DebitPaise == 0 {
		b.Status = StatusUnblocked
		t := now
		b.UnblockedAt = &t
	} else {
		b.Status = StatusDebited
		t := now
		b.DebitedAt = &t
		if released > 0 {
			u := now
			b.UnblockedAt = &u
		}
	}

	// The whole block leaves the account's held total, since both halves of the instruction are applied
	// together. The debited part leaves the balance too; the released part becomes available again.
	if acct, ok := m.refToAccount[req.BankRef]; ok {
		m.held[acct] -= b.BlockedPaise
		m.balances[acct] -= req.DebitPaise
	}

	m.byKey[req.IdempotencyKey] = req.BankRef

	cp := *b
	return &cp, nil
}

func (m *Mock) Release(ctx context.Context, idempotencyKey, bankRef string) (*Block, error) {
	// A release is a settlement that debits nothing, so it goes through the same path rather than a
	// parallel one. Two implementations of the same instruction would be two places for the
	// reconciliation identity to drift.
	return m.Settle(ctx, SettleRequest{
		IdempotencyKey: idempotencyKey,
		BankRef:        bankRef,
		DebitPaise:     0,
	})
}

func (m *Mock) Fetch(ctx context.Context, bankRef string) (*Block, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failNextFetch != nil {
		e := m.failNextFetch
		m.failNextFetch = nil
		return nil, fmt.Errorf("asba: mock fetch failed: %w", e)
	}

	b, ok := m.blocks[bankRef]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, bankRef)
	}
	cp := *b
	return &cp, nil
}

// Count reports how many blocks exist, which is how a double-block is detected.
func (m *Mock) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.blocks)
}

// requestDigest canonicalises the identifying fields for idempotency comparison.
//
// Covers the bid, the account, the mandate and the amount. Reusing a key with any of those changed is a
// different instruction and must be refused rather than silently deduped onto the earlier one.
func requestDigest(r BlockRequest) ([32]byte, error) {
	s := fmt.Sprintf("%s|%s|%s|%d", r.BidID, r.BankAccountRef, r.MandateRef, r.AmountPaise)
	return sha256.Sum256([]byte(s)), nil
}
