// Command devtoken mints a session token for a LOCAL API, for building and testing the console.
//
// There is no operator sign-in yet. Until there is, this is how a developer gets an operator token, and how an
// investor token is obtained without a Web3Auth round trip. It refuses to run outside LOCAL.
//
//	go run ./cmd/devtoken -role MANAGER
//	go run ./cmd/devtoken -investor 5f0c...-uuid
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/acresync/orchestrator/internal/clock"
	"github.com/acresync/orchestrator/internal/config"
	"github.com/acresync/orchestrator/internal/httpapi"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "devtoken:", err)
		os.Exit(1)
	}
}

func run() error {
	role := flag.String("role", "", "operator role: MANAGER, TRUSTEE or COMPLIANCE")
	investor := flag.String("investor", "", "investor id, for an investor token")
	subject := flag.String("sub", "", "subject (defaults to dev|<role or investor>)")
	ttl := flag.Duration("ttl", 30*time.Minute, "lifetime")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.Environment != config.EnvLocal {
		return fmt.Errorf("refusing to mint a token in %s; this tool is for LOCAL only", cfg.Environment)
	}
	if cfg.API.SessionSecret.IsZero() {
		return fmt.Errorf("ACRESYNC_API_SESSION_SECRET is not set")
	}

	now, err := clock.Real().Now(context.Background())
	if err != nil {
		return err
	}
	c := httpapi.Claims{IssuedAt: now.Unix(), ExpiresAt: now.Add(*ttl).Unix()}
	switch {
	case *role != "" && *investor == "":
		c.Kind, c.Role, c.Subject = httpapi.PrincipalOperator, httpapi.Role(*role), "dev|"+*role
	case *investor != "" && *role == "":
		c.Kind, c.InvestorID, c.Subject = httpapi.PrincipalInvestor, *investor, "dev|"+*investor
	default:
		return fmt.Errorf("pass exactly one of -role or -investor")
	}
	if *subject != "" {
		c.Subject = *subject
	}

	token, err := httpapi.MintDevToken([]byte(cfg.API.SessionSecret.Reveal()), c)
	if err != nil {
		return err
	}
	fmt.Println(token)
	return nil
}
