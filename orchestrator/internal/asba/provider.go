package asba

import (
	"context"
	"errors"
	"fmt"

	"github.com/acresync/orchestrator/internal/idempotency"
	"github.com/acresync/orchestrator/internal/money"
)

var (
	ErrNotFound          = errors.New("asba: no such block")
	ErrInvalidRequest    = errors.New("asba: invalid request")
	ErrIdempotencyReuse  = errors.New("asba: idempotency key reused with a different request")
	ErrInsufficientFunds = errors.New("asba: the investor's account does not hold the bid amount")
	ErrMandateMissing    = errors.New("asba: no active mandate authorises a block on this account")
)

// BlockRequest asks a bank to reserve an investor's subscription amount.
type BlockRequest struct {
	// IdempotencyKey makes a retry after a dropped connection safe.
	//
	// Required, for the same reason it is on a payout: a blind retry without one could place a second
	// block on the same account. That is less damaging than a double payment, since nothing has moved,
	// but it freezes twice the money and the investor discovers it as an unexplained shortfall in their
	// available balance.
	IdempotencyKey string

	BidID string

	// BankAccountRef is the investor's account, held by reference rather than by number.
	//
	// The account number and IFSC live with the bank under its own registration flow, so this package
	// handles an opaque handle. Keeping the details out means a leak here exposes nothing usable.
	BankAccountRef string

	// AmountPaise is the full bid amount. A block is all or nothing.
	AmountPaise money.Paise

	// MandateRef authorises the bank to place the block.
	MandateRef string
}

func (r BlockRequest) Validate() error {
	var problems []string

	if r.IdempotencyKey == "" {
		problems = append(problems, "idempotency key is required; without one a retry can double-block")
	}
	if r.BidID == "" {
		problems = append(problems, "bid id is required")
	}
	if r.BankAccountRef == "" {
		problems = append(problems, "bank account reference is required")
	}
	if r.MandateRef == "" {
		problems = append(problems, "mandate reference is required: a bank will not freeze funds "+
			"without an instruction the investor authorised")
	}
	if r.AmountPaise <= 0 {
		problems = append(problems, fmt.Sprintf("amount must be positive, got %d", r.AmountPaise))
	}

	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalidRequest, joinAll(problems))
	}
	return nil
}

// SettleRequest instructs the bank to take the allotted portion and release the rest.
type SettleRequest struct {
	IdempotencyKey string
	BankRef        string

	// DebitPaise is the allotted amount: units allotted times price per unit.
	//
	// Zero is valid and means the bid won nothing, in which case the whole block is released.
	DebitPaise money.Paise
}

// Provider reserves and settles subscription money at a bank.
type Provider interface {
	// RequestBlock asks the bank to reserve the full bid amount.
	RequestBlock(ctx context.Context, req BlockRequest) (*Block, error)

	// Settle debits the allotted portion and releases the remainder, as one instruction.
	//
	// # Why this is one call and not a debit followed by an unblock
	//
	// Two calls can be half-completed. A crash between them leaves the investor's money debited with no
	// release, or blocked with no claim against it, and in both cases the per-bid reconciliation fails
	// with no way to tell whether the second instruction was never sent or was sent and lost.
	//
	// One instruction makes the identity debited + released == blocked structural rather than something
	// the orchestrator has to maintain across a failure boundary. It also matches how the ASBA flow
	// actually works: the sponsor bank is told the allotted amount and releases the balance itself.
	Settle(ctx context.Context, req SettleRequest) (*Block, error)

	// Release returns the entire amount, for a bid that won nothing or an offer that was abandoned.
	Release(ctx context.Context, idempotencyKey, bankRef string) (*Block, error)

	// Fetch reads the current state.
	//
	// Polling exists because a bank notification is a delivery attempt rather than a guarantee. Without
	// it a block whose confirmation was lost would sit in REQUESTED forever, and the offer could never
	// freeze its book.
	Fetch(ctx context.Context, bankRef string) (*Block, error)

	Name() string
}

// DeriveBlockKey builds the idempotency key for a block request.
//
// Derived from the bid rather than from the attempt, because unlike a payout there is no legitimate
// reason to place a second block for the same bid: one bid, one reservation. A failed block is retried
// as a new bid validation cycle, not as a second block on the same one.
func DeriveBlockKey(schemeID, bidID string, amount money.Paise) (string, error) {
	k, err := idempotency.Derive(idempotency.Input{
		Action:   idempotency.ActionRequestASBABlock,
		SchemeID: schemeID,
		ScopeID:  bidID,
		Payload:  map[string]any{"amountPaise": amount},
	})
	if err != nil {
		return "", err
	}
	return k.Hex(), nil
}

// DeriveSettleKey builds the idempotency key for a settlement instruction.
//
// Includes the debit amount, so a corrected allotment produces a different key rather than being
// deduped onto the earlier instruction. An unchanged amount retried after a timeout reuses the key and
// is safe.
func DeriveSettleKey(schemeID, bidID string, debit money.Paise) (string, error) {
	k, err := idempotency.Derive(idempotency.Input{
		Action:   idempotency.ActionSettleASBABlock,
		SchemeID: schemeID,
		ScopeID:  bidID,
		Payload:  map[string]any{"debitPaise": debit},
	})
	if err != nil {
		return "", err
	}
	return k.Hex(), nil
}

func joinAll(p []string) string {
	out := ""
	for i, s := range p {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}
