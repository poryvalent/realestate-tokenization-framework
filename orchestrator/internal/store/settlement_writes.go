package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/acresync/orchestrator/internal/asba"
)

// Reads and writes for settlement.
//
// # The mirror is written when the credit is instructed
//
// unit_holdings mirrors the depository register, which credits an allottee at allotment. The chain is a
// second, public copy of that register, and its credit is a queued call. So the mirror row is written when
// the batch is submitted, and each ledger entry says QUEUED: the holding exists, and whether the chain agrees
// yet is recorded next to it rather than implied by it.
//
// depository_register is deliberately not written here. It stands for the depository's own books, fed by the
// depository simulator, and writing it from the same transaction that writes the mirror would make the
// reconciliation between them compare a table with itself.

// InvestmentManager returns the investor holding the manager's units and its active wallet.
//
// ("", "", nil) when the scheme has no manager recorded. An error names a manager with no active wallet,
// because the contract must be told an address and there is none to tell it.
func InvestmentManager(ctx context.Context, q Querier, schemeID string) (investorID, wallet string, err error) {
	var id, addr *string
	err = q.QueryRow(ctx, `
		SELECT s.im_investor_id::text, w.address
		  FROM schemes s
		  LEFT JOIN wallets w ON w.investor_id = s.im_investor_id AND w.is_active
		 WHERE s.id = $1`, schemeID).Scan(&id, &addr)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	if id == nil {
		return "", "", nil
	}
	if addr == nil {
		return *id, "", nil
	}
	return *id, *addr, nil
}

// ActiveWallets maps each investor to their active wallet. An investor with none is absent from the map.
func ActiveWallets(ctx context.Context, q Querier, investorIDs []string) (map[string]string, error) {
	rows, err := q.Query(ctx, `
		SELECT investor_id::text, address FROM wallets
		 WHERE is_active AND investor_id = ANY($1::uuid[])`, investorIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]string, len(investorIDs))
	for rows.Next() {
		var id, addr string
		if err := rows.Scan(&id, &addr); err != nil {
			return nil, err
		}
		out[id] = addr
	}
	return out, rows.Err()
}

// RecordASBASettlement records what the bank did with one block at settlement.
//
// A bid that won units moves to FUNDS_DEBITED. A bid that won nothing keeps its REJECTED_* status and reason:
// bids_rejection_reason_present permits a reason only on a rejected status, so advancing it would erase why it
// lost. The block's own status records that its money was released.
func RecordASBASettlement(ctx context.Context, w Writer, bidID string, b *asba.Block) error {
	status := "UNBLOCKED"
	if b.DebitedPaise > 0 {
		status = "DEBITED"
	}
	tag, err := w.Exec(ctx, `
		UPDATE asba_blocks
		   SET block_status = $2::asba_block_status, debited_amount_paise = $3,
		       debited_at = $4, unblocked_at = $5
		 WHERE bid_id = $1 AND block_status = 'BLOCKED'`,
		bidID, status, int64(b.DebitedPaise), b.DebitedAt, b.UnblockedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	if b.DebitedPaise == 0 {
		return nil
	}
	tag, err = w.Exec(ctx, `
		UPDATE bids SET status = 'FUNDS_DEBITED', updated_at = now()
		 WHERE id = $1 AND status IN ('ALLOTTED_FULL', 'ALLOTTED_PARTIAL')`, bidID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("store: bid %s was debited but is not an allotted bid", bidID)
	}
	return nil
}

// Credit is one holding written to the mirror.
type Credit struct {
	SchemeID   string
	InvestorID string
	Wallet     string
	Units      uint32

	// EntryType is ALLOTMENT for a public allottee, IM_SUBSCRIPTION for the manager.
	EntryType string

	// Excluded marks the manager's holding, which the contract excludes from the holder count.
	Excluded bool

	// At is business time.
	At time.Time

	Key []byte
}

// CreditHolding writes a first credit to the mirror and its ledger entry.
//
// A primary allotment is the first credit an investor receives in a scheme, so an existing holding is refused
// rather than added to: it would mean the same allottee credited twice, which a batch key collision cannot
// cause and a bug could.
func CreditHolding(ctx context.Context, w Writer, c Credit) error {
	tag, err := w.Exec(ctx, `
		INSERT INTO unit_holdings (
			scheme_id, investor_id, wallet_address, units, is_excluded_from_holder_count, first_credited_at
		) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (scheme_id, investor_id) DO NOTHING`,
		c.SchemeID, c.InvestorID, c.Wallet, int32(c.Units), c.Excluded, c.At.UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: investor %s already holds units in this scheme", ErrConflict, c.InvestorID)
	}

	// balance_after is checked against the running sum by trigger, so a first credit states the balance
	// it produces.
	_, err = w.Exec(ctx, `
		INSERT INTO holding_ledger (
			scheme_id, investor_id, entry_type, units_delta, balance_after,
			source, occurred_at, simulated_clock_value, anchor_status, idempotency_key
		) VALUES ($1, $2, $3::holding_entry_type, $4, $4, 'BALLOT', $5, $5, 'QUEUED', $6)`,
		c.SchemeID, c.InvestorID, c.EntryType, int32(c.Units), c.At.UTC(), c.Key)
	return err
}

// MarkUnitsCredited moves debited bids to UNITS_CREDITED once their holder's credit is instructed.
func MarkUnitsCredited(ctx context.Context, w Writer, bidIDs []string) error {
	tag, err := w.Exec(ctx, `
		UPDATE bids SET status = 'UNITS_CREDITED', updated_at = now()
		 WHERE id = ANY($1::uuid[]) AND status = 'FUNDS_DEBITED'`, bidIDs)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != int64(len(bidIDs)) {
		return fmt.Errorf("%w: %d of %d bids in the batch were debited and uncredited",
			ErrConflict, tag.RowsAffected(), len(bidIDs))
	}
	return nil
}
