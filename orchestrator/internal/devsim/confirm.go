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
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/acresync/orchestrator/internal/chain"
	"github.com/acresync/orchestrator/internal/config"
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
		ok, err := confirmOne(ctx, db, c.ID, block)
		if err != nil {
			return out, err
		}
		if ok {
			c.BlockNumber = block
			out = append(out, c)
		}
	}
	return out, nil
}

// confirmOne marks one row confirmed on the next free nonce.
//
// Two confirmers running at once (devseed and devconfirm -watch) can both read the same max(nonce) and the
// loser hits outbox_one_tx_per_nonce. The loser retries with a fresh read rather than failing, because a lost
// race here is not an error: the other process simply got there first.
func confirmOne(ctx context.Context, db DB, id string, block int64) (bool, error) {
	tx := sha256.Sum256([]byte("acresync/simulated-tx/v1/" + id))
	for attempt := 0; ; attempt++ {
		tag, err := db.Exec(ctx, `
			UPDATE chain_outbox o
			   SET status = 'CONFIRMED', tx_hash = $2, block_number = $3, confirmations = 12,
			       submitted_at = now(), confirmed_at = now(), updated_at = now(),
			       attempt_count = attempt_count + 1, last_error = NULL,
			       nonce = (SELECT coalesce(max(nonce), -1) + 1 FROM chain_outbox WHERE scheme_id = o.scheme_id)
			 WHERE id = $1 AND status = 'QUEUED' AND environment_tag = 'LOCAL'`,
			id, "0x"+hex.EncodeToString(tx[:]), block)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && attempt < 5 {
			continue
		}
		if err != nil {
			return false, err
		}
		return tag.RowsAffected() == 1, nil
	}
}

// Head reads the chain head.
type Head interface {
	Head(ctx context.Context) (uint64, error)
}

// HeadFromConfig picks the same chain the API reads: the simulated one when ACRESYNC_CHAIN_SIMULATED=true, or
// the configured RPC. Using a different source from the API would record target blocks it cannot agree with.
func HeadFromConfig(ctx context.Context, cfg *config.Config) (Head, func(), error) {
	switch {
	case os.Getenv("ACRESYNC_CHAIN_SIMULATED") == "true":
		return chain.Simulated{}, func() {}, nil
	case !cfg.Chain.RPCURL.IsZero():
		r, err := chain.DialReader(ctx, cfg.Chain)
		if err != nil {
			return nil, nil, err
		}
		return r, r.Close, nil
	}
	return nil, nil, errors.New("no chain head to record against: set ACRESYNC_CHAIN_SIMULATED=true or " +
		"ACRESYNC_CHAIN_RPC_URL, the same as the API")
}
