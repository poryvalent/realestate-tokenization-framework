package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

// walletSeq keeps seeded wallet addresses distinct; the column is uniquely constrained.
var walletSeq atomic.Int64

// seedWallet attaches a wallet to an investor.
//
// The address is generated rather than fixed because wallets.address is UNIQUE, so a constant would make the
// second insert in any test fail for a reason unrelated to what it is testing.
func seedWallet(t *testing.T, ctx context.Context, q Querier, investorID string, active bool) string {
	t.Helper()

	// Deliberately contains hex letters. An address of only digits would make the checksummed-input test
	// vacuous, because upper-casing it would produce the same string and prove nothing.
	address := fmt.Sprintf("0xabcdef%034x", walletSeq.Add(1))

	// wallets_deactivation_consistent requires deactivated_at exactly when is_active is false.
	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO wallets (investor_id, address, provider, is_active, deactivated_at, rotation_reason)
		VALUES ($1, $2, 'WEB3AUTH', $3,
			CASE WHEN $3 THEN NULL ELSE now() END,
			CASE WHEN $3 THEN NULL ELSE 'rotated in a test' END)
		RETURNING id`,
		investorID, address, active).Scan(&id)
	if err != nil {
		t.Fatalf("seeding a wallet: %v", err)
	}
	return address
}

// TestResolveWalletFindsTheHolder is the happy path of the session exchange.
func TestResolveWalletFindsTheHolder(t *testing.T) {
	ctx, tx := pgTx(t)
	investorID, _, _ := seedInvestor(t, ctx, tx)
	address := seedWallet(t, ctx, tx, investorID, true)

	got, err := NewInvestors(tx).ResolveWallet(ctx, address)
	if err != nil {
		t.Fatalf("resolving an active wallet: %v", err)
	}
	if got != investorID {
		t.Fatalf("resolved to %q, want %q", got, investorID)
	}
}

// TestResolveWalletAcceptsAChecksummedAddress covers the spelling difference.
//
// Web3Auth hands out EIP-55 checksummed addresses and the column stores lowercase. Rejecting the checksummed
// form would fail for every real caller; storing it would give one address two spellings.
func TestResolveWalletAcceptsAChecksummedAddress(t *testing.T) {
	ctx, tx := pgTx(t)
	investorID, _, _ := seedInvestor(t, ctx, tx)
	address := seedWallet(t, ctx, tx, investorID, true)

	// Upper-case the hex body, leaving the 0x prefix.
	mixed := "0x" + strings.ToUpper(address[2:])
	if mixed == address {
		t.Fatalf("the fixture produced an address with no letters (%s), so this test would prove nothing", address)
	}

	got, err := NewInvestors(tx).ResolveWallet(ctx, mixed)
	if err != nil {
		t.Fatalf("a checksummed address did not resolve: %v", err)
	}
	if got != investorID {
		t.Fatalf("resolved to %q, want %q", got, investorID)
	}
}

// TestResolveWalletIgnoresARotatedWallet is the one that matters for safety.
//
// A deactivated wallet is kept because a snapshot taken while it was active still refers to it. Letting it
// authenticate would hand a session to whoever holds the address the unitholder deliberately moved off, which
// is exactly the situation a rotation exists to end.
func TestResolveWalletIgnoresARotatedWallet(t *testing.T) {
	ctx, tx := pgTx(t)
	investorID, _, _ := seedInvestor(t, ctx, tx)
	old := seedWallet(t, ctx, tx, investorID, false)
	current := seedWallet(t, ctx, tx, investorID, true)

	store := NewInvestors(tx)

	if _, err := store.ResolveWallet(ctx, old); !errors.Is(err, ErrNoInvestorForIdentity) {
		t.Fatalf("a rotated wallet resolved: err = %v", err)
	}

	got, err := store.ResolveWallet(ctx, current)
	if err != nil {
		t.Fatalf("the current wallet did not resolve: %v", err)
	}
	if got != investorID {
		t.Fatalf("resolved to %q, want %q", got, investorID)
	}
}

// TestResolveWalletOnAnUnknownAddress covers the ordinary refusal.
func TestResolveWalletOnAnUnknownAddress(t *testing.T) {
	ctx, tx := pgTx(t)

	cases := map[string]string{
		"unknown":  "0x00000000000000000000000000000000deadbeef",
		"empty":    "",
		"blank":    "   ",
		"nonsense": "not-an-address",
	}

	for name, address := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewInvestors(tx).ResolveWallet(ctx, address)
			if !errors.Is(err, ErrNoInvestorForIdentity) {
				t.Fatalf("err = %v, want ErrNoInvestorForIdentity", err)
			}
		})
	}
}

// TestResolveWalletDoesNotCrossInvestors is the isolation check.
func TestResolveWalletDoesNotCrossInvestors(t *testing.T) {
	ctx, tx := pgTx(t)
	first, _, _ := seedInvestor(t, ctx, tx)
	second, _, _ := seedInvestor(t, ctx, tx)

	firstAddr := seedWallet(t, ctx, tx, first, true)
	secondAddr := seedWallet(t, ctx, tx, second, true)

	store := NewInvestors(tx)

	gotFirst, err := store.ResolveWallet(ctx, firstAddr)
	if err != nil {
		t.Fatal(err)
	}
	gotSecond, err := store.ResolveWallet(ctx, secondAddr)
	if err != nil {
		t.Fatal(err)
	}

	if gotFirst != first || gotSecond != second {
		t.Fatalf("wallets resolved to the wrong holders: %q and %q, want %q and %q",
			gotFirst, gotSecond, first, second)
	}
	if gotFirst == gotSecond {
		t.Fatal("two different wallets resolved to the same investor")
	}
}
