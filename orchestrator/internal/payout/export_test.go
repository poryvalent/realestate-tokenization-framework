package payout

import (
	"context"
	"fmt"
)

// Helpers available only to tests in this package.

// count reports how many payouts the mock holds, which is how a double-payment is detected.
func (m *Mock) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.payouts)
}

// createQueuedForTest bypasses Validate to reach the provider's queued path.
//
// Validate rejects QueueIfLowBalance as AcreSync policy, since a queued payout stalls a distribution
// without alerting anyone. The provider still implements queuing, and its balance accounting has to
// be correct, so this door exists to test the provider behaviour without weakening the policy.
func (m *Mock) createQueuedForTest(ctx context.Context, req Request) (*Payout, error) {
	if !req.QueueIfLowBalance {
		return nil, fmt.Errorf("createQueuedForTest requires QueueIfLowBalance")
	}

	digest, err := requestDigest(req)
	if err != nil {
		return nil, err
	}
	now, err := m.clk.Now(ctx)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	outcome := m.outcomeFor(req.FundAccountID)
	return m.insert(req, digest, StatusQueued, outcome, now, 0,
		StatusDetails{Description: "Payout queued: insufficient balance", Source: SourceBusiness}), nil
}
