// Package db holds the Postgres connection pool and small helpers shared by the
// stores.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/acresync/orchestrator/internal/config"
)

// Pool wraps pgxpool so callers depend on this package rather than on pgx
// directly, which keeps the driver swappable and gives one place to set
// connection policy.
type Pool struct {
	*pgxpool.Pool
}

// Open creates a pool and verifies it with a ping.
//
// Connecting lazily would move a bad connection string from a startup failure to
// a failure in the middle of a distribution run, so the ping is deliberate.
func Open(ctx context.Context, cfg config.DatabaseConfig) (*Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.URL.Reveal())
	if err != nil {
		// The URL carries a password, so it is never included in the error.
		return nil, fmt.Errorf("db: parsing connection string: %w", err)
	}

	pc.MaxConns = 10
	pc.MinConns = 1
	pc.MaxConnLifetime = 30 * time.Minute
	pc.MaxConnIdleTime = 5 * time.Minute

	// Application name shows up in pg_stat_activity, which is how you find out
	// which process is holding a lock during an incident.
	if pc.ConnConfig.RuntimeParams == nil {
		pc.ConnConfig.RuntimeParams = map[string]string{}
	}
	pc.ConnConfig.RuntimeParams["application_name"] = "acresync-orchestrator"

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("db: creating pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping failed: %w", err)
	}
	return &Pool{Pool: pool}, nil
}
