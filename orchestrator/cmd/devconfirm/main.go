// Command devconfirm fakes the relayer for LOCAL demos: it marks queued chain calls confirmed.
//
// No transaction is sent anywhere. The hash is invented and the block number is chosen. See internal/devsim for
// why that cannot reach a real row, and why the recorded block is backdated.
//
//	go run ./cmd/devconfirm                 # confirm everything queued, once
//	go run ./cmd/devconfirm -watch          # keep confirming, like a relayer would
//	go run ./cmd/devconfirm -scheme <uuid>  # one scheme only
//
// It reads the chain head the same way the API does (ACRESYNC_CHAIN_SIMULATED=true, or the configured RPC), so
// the target blocks it produces are ones the API will accept a reveal against.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/acresync/orchestrator/internal/config"
	"github.com/acresync/orchestrator/internal/db"
	"github.com/acresync/orchestrator/internal/devsim"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "devconfirm:", err)
		os.Exit(1)
	}
}

func run() error {
	watch := flag.Bool("watch", false, "keep running and confirm new calls as they are queued")
	every := flag.Duration("every", 2*time.Second, "poll interval with -watch")
	delay := flag.Duration("delay", 5*time.Second, "leave a call queued this long first, so the UI shows its waiting state")
	schemeID := flag.String("scheme", "", "confirm only this scheme's calls")
	flag.Parse()

	cfg, err := config.Load(config.FeatureDatabase)
	if err != nil {
		return err
	}
	if cfg.Environment != config.EnvLocal {
		return fmt.Errorf("refusing to fake confirmations in %s; this tool is for LOCAL only", cfg.Environment)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer pool.Close()

	head, closeHead, err := devsim.HeadFromConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeHead()

	fmt.Println("devconfirm: SIMULATED confirmations. Nothing is sent to any chain.")
	for {
		n, err := head.Head(ctx)
		if err != nil {
			return fmt.Errorf("reading the chain head: %w", err)
		}
		opts := devsim.Options{Head: n, SchemeID: *schemeID}
		if *watch {
			opts.MinAge = *delay
		}
		done, err := devsim.Confirm(ctx, pool, opts)
		if err != nil {
			return err
		}
		for _, c := range done {
			slog.Info("confirmed (simulated)", "function", c.FunctionName, "outbox", c.ID, "block", c.BlockNumber)
		}
		if !*watch {
			fmt.Printf("devconfirm: %d call(s) confirmed\n", len(done))
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*every):
		}
	}
}
