// Command api serves the AcreSync HTTP API described by docs/api/openapi.yaml.
//
// It attests; it never custodies. Every endpoint here reads or records against the TradFi escrow and the
// on-chain shadow ledger, and none of them moves money.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

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

	srv := httpapi.New(httpapi.Deps{
		Env:     cfg.Environment,
		Wall:    clock.Real(),
		Schemes: store.NewSchemes(pool),
		Offers:  store.NewOffers(pool),
		Periods: store.NewPeriods(pool),
		Ready:   pool.Ping,
	}).HTTPServer(cfg.HTTPAddr)

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
