// Command migrate applies the AcreSync SQL migrations.
//
// Design decisions worth knowing:
//
// The runner owns transaction boundaries, so the migration files contain no BEGIN
// or COMMIT. Each file is applied inside one transaction together with the insert
// that records it, which means a failed migration leaves neither its schema
// changes nor its version row behind. A file that manages its own transaction
// would break that coupling and allow a migration to be applied but unrecorded.
//
// Every applied file's SHA-256 is stored and re-checked on each run. Editing a
// migration that has already been applied is the classic way for two environments
// to silently diverge, so it is treated as an error rather than ignored.
//
// Statements are sent using the simple protocol because migration files contain
// multiple statements and dollar-quoted function bodies. That precludes bound
// parameters inside migration SQL, which is correct: a migration is static DDL,
// not a query with user input.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/acresync/orchestrator/internal/config"
	"github.com/acresync/orchestrator/internal/db"
)

const bootstrapSQL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version     TEXT        PRIMARY KEY,
    checksum    BYTEA       NOT NULL,
    applied_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    duration_ms INTEGER     NOT NULL,
    CONSTRAINT schema_migrations_checksum_len CHECK (octet_length(checksum) = 32)
);`

type migration struct {
	version  string
	path     string
	sql      string
	checksum [32]byte
}

func main() {
	action := flag.String("action", "up", "up | status")
	dir := flag.String("dir", "", "migrations directory (default: nearest db/migrations)")
	timeout := flag.Duration("timeout", 5*time.Minute, "overall timeout")
	flag.Parse()

	if err := run(*action, *dir, *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "\nmigrate: %v\n", err)
		os.Exit(1)
	}
}

func run(action, dir string, timeout time.Duration) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, timeout)
	defer cancelTimeout()

	cfg, err := config.Load(config.FeatureDatabase)
	if err != nil {
		return err
	}

	if dir == "" {
		dir, err = findMigrationsDir()
		if err != nil {
			return err
		}
	}

	migrations, err := loadMigrations(dir)
	if err != nil {
		return err
	}
	if len(migrations) == 0 {
		return fmt.Errorf("no .sql files found in %s", dir)
	}

	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, bootstrapSQL); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}

	applied, err := loadApplied(ctx, pool.Pool)
	if err != nil {
		return err
	}

	if err := checkDrift(migrations, applied); err != nil {
		return err
	}

	fmt.Printf("environment %s\ndirectory   %s\nfiles       %d\napplied     %d\n\n",
		cfg.Environment, dir, len(migrations), len(applied))

	switch action {
	case "status":
		return printStatus(migrations, applied)
	case "up":
		return applyPending(ctx, pool.Pool, migrations, applied)
	default:
		return fmt.Errorf("unknown action %q (want up or status)", action)
	}
}

type appliedRecord struct {
	checksum  [32]byte
	appliedAt time.Time
}

func loadApplied(ctx context.Context, pool *pgxpool.Pool) (map[string]appliedRecord, error) {
	rows, err := pool.Query(ctx, `SELECT version, checksum, applied_at FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("reading schema_migrations: %w", err)
	}
	defer rows.Close()

	out := map[string]appliedRecord{}
	for rows.Next() {
		var version string
		var sum []byte
		var at time.Time
		if err := rows.Scan(&version, &sum, &at); err != nil {
			return nil, err
		}
		var arr [32]byte
		copy(arr[:], sum)
		out[version] = appliedRecord{checksum: arr, appliedAt: at}
	}
	return out, rows.Err()
}

// checkDrift refuses to proceed when an already-applied migration's content has
// changed on disk.
//
// The failure this prevents is subtle and expensive: a developer edits an applied
// migration, it runs clean on a fresh database, and the two environments now have
// different schemas while both report fully migrated.
func checkDrift(migrations []migration, applied map[string]appliedRecord) error {
	var drifted []string
	for _, m := range migrations {
		rec, ok := applied[m.version]
		if !ok {
			continue
		}
		if rec.checksum != m.checksum {
			drifted = append(drifted, fmt.Sprintf(
				"  %s\n      applied %s with checksum %s\n      on disk now            %s",
				m.version, rec.appliedAt.UTC().Format(time.RFC3339),
				hex.EncodeToString(rec.checksum[:8]), hex.EncodeToString(m.checksum[:8])))
		}
	}
	if len(drifted) > 0 {
		return fmt.Errorf(
			"%d already-applied migration(s) have been modified:\n%s\n\n"+
				"An applied migration is history and must not be edited. Add a new migration that\n"+
				"makes the change forward, or drop and rebuild the database if it is disposable.",
			len(drifted), strings.Join(drifted, "\n"))
	}

	// A version recorded in the database with no file on disk usually means a
	// checkout is missing a migration, which is worth saying out loud.
	onDisk := map[string]bool{}
	for _, m := range migrations {
		onDisk[m.version] = true
	}
	var orphans []string
	for v := range applied {
		if !onDisk[v] {
			orphans = append(orphans, v)
		}
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		fmt.Printf("warning: %d applied migration(s) have no file in this checkout: %s\n\n",
			len(orphans), strings.Join(orphans, ", "))
	}
	return nil
}

func printStatus(migrations []migration, applied map[string]appliedRecord) error {
	for _, m := range migrations {
		if rec, ok := applied[m.version]; ok {
			fmt.Printf("  applied  %-40s %s\n", m.version, rec.appliedAt.UTC().Format(time.RFC3339))
		} else {
			fmt.Printf("  PENDING  %-40s\n", m.version)
		}
	}
	return nil
}

func applyPending(ctx context.Context, pool *pgxpool.Pool, migrations []migration, applied map[string]appliedRecord) error {
	pending := 0
	for _, m := range migrations {
		if _, ok := applied[m.version]; ok {
			continue
		}
		pending++
		if err := applyOne(ctx, pool, m); err != nil {
			return err
		}
	}
	if pending == 0 {
		fmt.Println("  nothing to do; schema is up to date")
	} else {
		fmt.Printf("\n  applied %d migration(s)\n", pending)
	}
	return nil
}

func applyOne(ctx context.Context, pool *pgxpool.Pool, m migration) error {
	// Migration duration is elapsed real time for an operator's benefit. It is not
	// a business deadline, and schema migrations run outside any scheme's
	// simulated clock, so there is no business time to read here.
	//acresync:allow-wallclock measuring how long DDL took, not evaluating a deadline
	start := time.Now()

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%s: begin: %w", m.version, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, m.sql); err != nil {
		return fmt.Errorf("%s: %w", m.version, annotate(err))
	}

	//acresync:allow-wallclock paired with the start above; wall-clock elapsed time
	elapsed := time.Since(start)
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, checksum, duration_ms) VALUES ($1, $2, $3)`,
		m.version, m.checksum[:], int32(elapsed.Milliseconds()),
	); err != nil {
		return fmt.Errorf("%s: recording version: %w", m.version, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%s: commit: %w", m.version, err)
	}

	fmt.Printf("  applied  %-40s %dms\n", m.version, elapsed.Milliseconds())
	return nil
}

// annotate prefixes the SQLSTATE, which pgx carries but does not surface in the
// default error string. Knowing 42601 from 23514 is the difference between a typo
// and a violated constraint.
func annotate(err error) error {
	var p interface{ SQLState() string }
	if errors.As(err, &p) {
		return fmt.Errorf("[%s] %w", p.SQLState(), err)
	}
	return err
}

func loadMigrations(dir string) ([]migration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}

	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}

		// Guard the runner's core assumption. A stray BEGIN would silently take
		// transaction control away from the runner and decouple the schema change
		// from its version record.
		if hasTopLevelTxControl(string(content)) {
			return nil, fmt.Errorf(
				"%s contains a top-level BEGIN or COMMIT: the runner owns transaction boundaries "+
					"so that each migration and its version record commit together", e.Name())
		}

		out = append(out, migration{
			version:  strings.TrimSuffix(e.Name(), ".sql"),
			path:     path,
			sql:      string(content),
			checksum: sha256.Sum256(content),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func hasTopLevelTxControl(sql string) bool {
	for _, line := range strings.Split(sql, "\n") {
		t := strings.ToUpper(strings.TrimSpace(line))
		if t == "BEGIN;" || t == "COMMIT;" || t == "ROLLBACK;" {
			return true
		}
	}
	return false
}

func findMigrationsDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for range 8 {
		candidate := filepath.Join(dir, "db", "migrations")
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", errors.New("could not locate a db/migrations directory; pass -dir")
}
