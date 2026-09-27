package store

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ErrNoInvestorForIdentity is returned when a verified login holds nothing on this platform.
//
// Distinct from ErrNotFound because it is not a missing row addressed by id: the caller is genuine and simply
// has no relationship with the register. The HTTP layer turns it into a 403 rather than a 404, so the caller
// is not sent back to log in again on a loop that cannot succeed.
var ErrNoInvestorForIdentity = errors.New("store: no investor holds that wallet")

// Investors resolves identities to register entries.
type Investors struct {
	q Querier
}

// NewInvestors binds a store to a pool or transaction.
func NewInvestors(q Querier) Investors { return Investors{q: q} }

// ResolveWallet returns the investor holding an active wallet.
//
// # Why the wallet and not the login subject
//
// Nothing in the schema stores a Web3Auth subject, and that absence is deliberate rather than an oversight.
// Web3Auth establishes who is calling; the register establishes what they hold. Putting the login provider's
// identifier into the investor row would tie the register to that provider, so that changing it, or a user
// changing how they log in, would rewrite who owns units.
//
// The wallet address is already the identity the register and the chain agree on, it is uniquely constrained,
// and it is what a Web3Auth token derives. So it is the link.
//
// Only an active wallet resolves. A deactivated one is kept for history, because a snapshot taken while it was
// active still refers to it, and letting a rotated-away wallet authenticate would hand a session to whoever
// held the address the holder deliberately moved off.
func (s Investors) ResolveWallet(ctx context.Context, address string) (string, error) {
	if address == "" {
		return "", ErrNoInvestorForIdentity
	}

	// Addresses are stored lowercase and the column enforces it, so one address has exactly one spelling.
	// Lowercasing the input rather than rejecting a mixed-case one is the right call here: EIP-55 checksummed
	// addresses are what most wallets hand out, and they name the same account.
	normalised := strings.ToLower(strings.TrimSpace(address))

	const q = `
		SELECT w.investor_id
		FROM wallets w
		WHERE w.address = $1 AND w.is_active
		LIMIT 1`

	var investorID string
	err := s.q.QueryRow(ctx, q, normalised).Scan(&investorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoInvestorForIdentity
	}
	if err != nil {
		return "", err
	}
	return investorID, nil
}
