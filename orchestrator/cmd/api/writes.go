package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/acresync/orchestrator/internal/asba"
	"github.com/acresync/orchestrator/internal/chain"
	"github.com/acresync/orchestrator/internal/clock"
	"github.com/acresync/orchestrator/internal/config"
	"github.com/acresync/orchestrator/internal/httpapi"
	"github.com/acresync/orchestrator/internal/ipfs"
	"github.com/acresync/orchestrator/internal/uploads"
)

// writeDeps attaches what the mutations need.
//
// Each dependency that is absent removes the endpoints that need it, and is logged, rather than failing the
// process: the read surface is still worth serving. One that is configured but broken fails startup, because a
// deployment that expects it would otherwise quietly lose part of its API.
//
// What is and is not real here:
//   - Funds blocks go to asba.Sandbox, an in-memory bank. A restart forgets its blocks. Payments are mocked
//     until the bank integration exists.
//   - Chain calls are queued to chain_outbox. Nothing in this process sends them; that is the relayer's job.
//   - The ballot seed pepper is the LOCAL development pepper. Outside LOCAL it must come from a KMS, which is
//     not integrated, so the ceremony and settlement endpoints are absent there.
func writeDeps(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool, deps *httpapi.Deps) (func(), error) {
	closers := []func(){}
	closeAll := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}

	clocks := clock.NewPostgresStore(pool)
	deps.DB = pool
	deps.BusinessClock = func(schemeID string) clock.Business { return clock.For(clocks, schemeID) }
	deps.ASBA = asba.NewSandbox(clock.Real())

	provider, err := ipfs.NewProvider(cfg.IPFS, getEnv("ACRESYNC_IPFS_MOCK_DIR", "var/ipfs"))
	if err != nil {
		return closeAll, fmt.Errorf("building the IPFS provider: %w", err)
	}
	deps.Publisher = ipfs.NewPublisher(provider)

	store, err := uploads.NewDir(getEnv("ACRESYNC_UPLOAD_DIR", "var/uploads"))
	if err != nil {
		return closeAll, err
	}
	deps.Uploads = store
	deps.UploadBaseURL = getEnv("ACRESYNC_PUBLIC_BASE_URL", "http://"+cfg.HTTPAddr)

	if cfg.Environment == config.EnvLocal && !cfg.Anchor.DevPepper.IsZero() {
		deps.SeedPepper = []byte(cfg.Anchor.DevPepper.Reveal())
	} else {
		slog.Warn("no ballot seed pepper is available, so the ceremony and settlement endpoints are not served",
			"hint", "LOCAL uses ACRESYNC_ANCHOR_PEPPER_DEV; other environments need a KMS, which is not integrated")
	}

	if !cfg.Chain.RPCURL.IsZero() {
		reader, err := chain.DialReader(ctx, cfg.Chain)
		if err != nil {
			return closeAll, err
		}
		closers = append(closers, reader.Close)
		deps.Chain = reader
	} else {
		slog.Warn("no chain RPC is configured, so revealSeed and recommitSeed are not served",
			"hint", "set ACRESYNC_CHAIN_RPC_URL")
	}

	slog.Warn("funds blocks use the in-memory ASBA sandbox; a restart forgets them, and no real bank is involved")
	return closeAll, nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
