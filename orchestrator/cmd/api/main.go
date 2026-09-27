// Command api serves the AcreSync HTTP API described by docs/api/openapi.yaml.
//
// It attests; it never custodies. Every endpoint here reads or records against the TradFi escrow and the
// on-chain shadow ledger, and none of them moves money.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/acresync/orchestrator/internal/clock"
	"github.com/acresync/orchestrator/internal/config"
	"github.com/acresync/orchestrator/internal/db"
	"github.com/acresync/orchestrator/internal/httpapi"
	"github.com/acresync/orchestrator/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("the api could not start", "err", err)
		os.Exit(1)
	}
}

func run() error {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	cfg, err := config.Load(config.FeatureDatabase)
	if err != nil {
		return err
	}

	// Signals are wired before anything is opened, so a Ctrl-C during startup is still a clean shutdown
	// rather than a half-initialised process left holding a port.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// db.Open pings, so a bad connection string fails here rather than in the middle of the first request.
	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer pool.Close()

	deps := httpapi.Deps{
		Env:           cfg.Environment,
		Wall:          clock.Real(),
		Schemes:       store.NewSchemes(pool),
		Offers:        store.NewOffers(pool),
		Periods:       store.NewPeriods(pool),
		Ready:         pool.Ping,
		SessionSecret: []byte(cfg.API.SessionSecret.Reveal()),
		SessionTTL:    cfg.API.SessionTTL,
		Investors:     httpapi.InvestorsByWallet{Wallets: store.NewInvestors(pool)},
	}

	// The upstream verifier is attached only when a JWKS is configured. Without it POST /auth/session is not
	// registered, which is better than registering an endpoint that cannot establish who is calling.
	verifier, err := upstreamVerifier(cfg)
	if err != nil {
		return err
	}
	if verifier != nil {
		deps.IDTokens = verifier
	} else {
		slog.Warn("no Web3Auth JWKS is configured, so the session endpoint is not served",
			"hint", "set ACRESYNC_WEB3AUTH_JWKS_FILE and ACRESYNC_WEB3AUTH_CLIENT_ID")
	}

	srv := httpapi.New(deps).HTTPServer(cfg.HTTPAddr)

	// Bound before the goroutine starts, so a failure to bind is returned from run rather than logged from
	// somewhere else, and so "listening" is only ever printed after the socket is actually held. Using
	// ListenAndServe instead would log the claim first and discover the conflict afterwards, which is
	// precisely the wrong order when the thing you are debugging is a port conflict.
	listener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return err
	}

	slog.Info("listening",
		"addr", listener.Addr().String(),
		"env", string(cfg.Environment),
		"basePath", "/v1")

	// The serve error is delivered on a channel rather than logged and forgotten, so a failure exits
	// non-zero instead of leaving a process that looks healthy and serves nothing.
	listenErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			listenErr <- err
			return
		}
		listenErr <- nil
	}()

	select {
	case err := <-listenErr:
		return err

	case <-ctx.Done():
		slog.Info("shutting down", "grace", httpapi.ShutdownGrace.String())

		// A fresh context: ctx is already cancelled, and passing it would abort in-flight requests
		// immediately, which is the opposite of a graceful shutdown.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), httpapi.ShutdownGrace)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			// Report rather than swallow: a shutdown that timed out means requests were cut off, and that
			// is worth knowing when reconciling what a client believes happened.
			return err
		}

		slog.Info("stopped cleanly")
		return nil
	}
}

// upstreamVerifier builds the Web3Auth verifier, or nil when none is configured.
//
// The JWKS is read from a file rather than fetched at startup. Fetching would make the process fail to start
// when a third party is unreachable, and it would need a refresh loop and a cache with its own failure modes.
// A file is explicit, reviewable, and can be rotated by a deployment step that already exists. The cost is
// that a key rotation needs a restart, which is the right trade for a document that changes rarely.
func upstreamVerifier(cfg *config.Config) (httpapi.IDTokenVerifier, error) {
	path := os.Getenv("ACRESYNC_WEB3AUTH_JWKS_FILE")
	if path == "" {
		return nil, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		// A configured but unreadable JWKS is a hard failure, not a reason to serve without authentication.
		// Degrading here would silently remove the investor surface in a deployment that expects it.
		return nil, fmt.Errorf("reading the Web3Auth jwks at %s: %w", path, err)
	}

	keys, err := httpapi.ParseJWKS(raw)
	if err != nil {
		return nil, err
	}

	if cfg.Web3Auth.ClientID == "" {
		// The audience check is what stops a token minted for another application authenticating here, so
		// running without it is refused rather than warned about.
		return nil, fmt.Errorf("%w: ACRESYNC_WEB3AUTH_CLIENT_ID is required when a jwks is configured, "+
			"because without an audience a token issued for another application would be accepted",
			config.ErrMissing)
	}

	return httpapi.Web3AuthVerifier{
		Keys:     keys,
		Issuer:   os.Getenv("ACRESYNC_WEB3AUTH_ISSUER"),
		Audience: cfg.Web3Auth.ClientID,
		// Wall time. A third party's token expiry has nothing to do with the simulated distribution timeline,
		// so this reads the real clock through the sanctioned reader rather than business time.
		Now: func() time.Time {
			t, err := clock.Real().Now(context.Background())
			if err != nil {
				return time.Time{}
			}
			return t
		},
	}, nil
}
