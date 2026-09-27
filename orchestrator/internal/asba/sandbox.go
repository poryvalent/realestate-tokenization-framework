package asba

import (
	"context"

	"github.com/acresync/orchestrator/internal/clock"
)

// Sandbox is the simulated bank the HTTP API uses until a real ASBA provider is contracted.
//
// # What is simulated, stated plainly
//
// Mock refuses a block unless the account was funded first, which is right for tests: they control exactly
// how much money each investor has. An investor placing a bid through the API has no way to fund a mock
// account, so Sandbox funds it on demand with exactly the amount the block needs, on top of whatever that
// account already holds.
//
// So every account can always cover its bid. That is a simulation of a bank, not of investors' finances, and
// it is the one thing about the funds block that is not real. Everything else is the Mock's genuine behaviour:
// all-or-nothing blocks, one reservation per idempotency key, a debit of the allotted portion and a release of
// the rest, and no house balance anywhere.
//
// State lives in memory. A restart forgets the blocks, and settling a block placed before the restart fails
// loudly with an unknown bank reference rather than quietly succeeding.
type Sandbox struct {
	*Mock
}

// NewSandbox returns a simulated bank in which every account can cover its bid.
func NewSandbox(clk clock.Business) *Sandbox {
	return &Sandbox{Mock: NewMock(clk)}
}

// Name reports the provider. It is MOCK so every record it produces says so.
func (s *Sandbox) Name() string { return "MOCK" }

// RequestBlock funds the account with what the block needs, then asks the Mock to place it.
//
// A retry with the same key returns the original block without funding again, because the Mock resolves the
// key before looking at the balance. The top-up on a retry is harmless either way: it raises a simulated
// balance that is never debited beyond what was blocked.
func (s *Sandbox) RequestBlock(ctx context.Context, req BlockRequest) (*Block, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	s.Mock.Fund(req.BankAccountRef, s.Mock.Held(req.BankAccountRef)+req.AmountPaise, req.MandateRef)
	return s.Mock.RequestBlock(ctx, req)
}
