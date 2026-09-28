// Package devsim fakes the relayer's confirmations for LOCAL demos.
//
// # What this pretends
//
// The relayer signs a queued chain call, broadcasts it, and once the receipt is five blocks deep marks the
// outbox row CONFIRMED with the transaction hash and block number. Every guard in the API reads that row. This
// package writes the same row without any transaction existing: the hash is invented, the block number is
// chosen, and nothing reached any chain.
//
// # Why it cannot touch a real row
//
// Only rows tagged environment_tag = 'LOCAL' are eligible, which is fixed in the SQL rather than passed in.
// A Sepolia row is never matched however this is called. cmd/devseed creates its scheme as LOCAL for exactly
// this reason, and cmd/devconfirm additionally refuses to start outside LOCAL.
//
// # The block number
//
// Every confirmed row is recorded eleven blocks behind the head it is given. For commitSeed and recommitSeed
// that makes the contract's target block (the row's block plus the ten-block delay) one block behind the head,
// so a reveal is immediately possible and stays possible for the 200-block window. Backdating like this is the
// clearest sign that the ceremony is simulated: on a real chain nobody can choose the block a commitment lands
// in, which is the whole point of it.
package devsim

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Backdate is how far behind the head a faked confirmation is recorded.
const Backdate = 11

// DB is what Confirm needs.
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Options narrows what is confirmed.
type Options struct {
	// Head is the chain head the confirmations are recorded against.
	Head uint64
	// MinAge leaves a row queued until it is at least this old, so a UI shows its waiting state briefly.
	MinAge time.Duration
	// SchemeID confirms only this scheme's rows when set.
	SchemeID string
}

// Confirmed is one row this call marked confirmed.
type Confirmed struct {
	ID           string
	FunctionName string
	BlockNumber  int64
}

// Confirm marks every eligible queued LOCAL row confirmed, oldest first.
func Confirm(ctx context.Context, db DB, o Options) ([]Confirmed, error) {
	if o.Head <= Backdate {
		return nil, errors.New("devsim: the chain head is too low to backdate a confirmation")
	}
	rows, err := db.Query(ctx, `
		SELECT id::text, function_name FROM chain_outbox
		 WHERE status = 'QUEUED' AND environment_tag = 'LOCAL'
		   AND created_at <= now() - make_interval(secs => $1)
		   AND ($2 = '' OR scheme_id::text = $2)
		 ORDER BY created_at, id`, o.MinAge.Seconds(), o.SchemeID)
	if err != nil {
		return nil, err
	}
	var queued []Confirmed
	for rows.Next() {
		var c Confirmed
		if err := rows.Scan(&c.ID, &c.FunctionName); err != nil {
			rows.Close()
			return nil, err
		}
		queued = append(queued, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	block := int64(o.Head - Backdate)
	out := make([]Confirmed, 0, len(queued))
	for _, c := range queued {
		tx := sha256.Sum256([]byte("acresync/simulated-tx/v1/" + c.ID))
		tag, err := db.Exec(ctx, `
			UPDATE chain_outbox o
			   SET status = 'CONFIRMED', tx_hash = $2, block_number = $3, confirmations = 12,
			       submitted_at = now(), confirmed_at = now(), updated_at = now(),
			       attempt_count = attempt_count + 1, last_error = NULL,
			       nonce = (SELECT coalesce(max(nonce), -1) + 1 FROM chain_outbox WHERE scheme_id = o.scheme_id)
			 WHERE id = $1 AND status = 'QUEUED' AND environment_tag = 'LOCAL'`,
			c.ID, "0x"+hex.EncodeToString(tx[:]), block)
		if err != nil {
			return out, err
		}
		if tag.RowsAffected() == 1 {
			c.BlockNumber = block
			out = append(out, c)
		}
	}
	return out, nil
}
