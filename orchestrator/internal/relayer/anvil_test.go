package relayer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/acresync/orchestrator/internal/chain"
	"github.com/acresync/orchestrator/internal/config"
	"github.com/acresync/orchestrator/internal/idempotency"
	"github.com/acresync/orchestrator/internal/outbox"
)

// These tests run against a real Anvil node.
//
// The behaviours that matter here cannot be tested against a mock: a mock reorgs when you tell it
// to and produces receipts when you say so, which is precisely the thing under test. Anvil gives
// real nonce enforcement, real mempool semantics, and real snapshot/revert so a reorg can be
// caused rather than simulated.
//
// Anvil's first default account. Well known, funded, and worthless.
const (
	anvilKey     = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	anvilAddress = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
	anvilPort    = 8599 // off the default 8545 so a developer's own node is not disturbed
)

const schemeID = "6f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8"

// anvilNode manages a node.
type anvilNode struct {
	cmd *exec.Cmd
	url string
}

// sharedNode is one Anvil for the whole package, started by TestMain.
//
// A node per test was the first approach and it did not work. Anvil runs under WSL here, so the
// process being started is a `wsl.exe` wrapper; killing the wrapper leaves the real Anvil alive
// inside the VM holding the port, and every test after the first skipped. One shared node with
// snapshot-based isolation avoids the problem entirely and runs considerably faster.
var sharedNode *anvilNode

// requireAnvil returns the shared node with per-test isolation.
//
// Each test snapshots on entry and reverts on cleanup, so state, balances and account nonces are
// restored. Nonce restoration matters more than it looks: without it a later test would inherit an
// arbitrary starting nonce and the sequence assertions would depend on execution order.
func requireAnvil(t *testing.T) *anvilNode {
	t.Helper()
	if sharedNode == nil {
		t.Skip("anvil unavailable; skipping chain integration tests")
	}

	snap := sharedNode.snapshot(t)
	t.Cleanup(func() {
		// Best effort. A failed revert only affects isolation, and failing cleanup would mask
		// the real test result.
		_, _ = sharedNode.rpcCall("evm_revert", snap)
	})
	return sharedNode
}

// startAnvil launches the shared node, returning nil if Anvil is unavailable.
//
// Unavailability is a skip rather than a failure. Anvil is a toolchain dependency, not a project
// one, and a developer without it should still be able to run the rest of the suite. A hard
// failure here would train people to ignore red.
func startAnvil() *anvilNode {
	bin, args := anvilCommand()
	if bin == "" {
		return nil
	}

	// Clear any node left behind by an earlier interrupted run.
	killStrayAnvil()

	cmd := exec.Command(bin, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil
	}

	node := &anvilNode{cmd: cmd, url: fmt.Sprintf("http://127.0.0.1:%d", anvilPort)}

	//acresync:allow-wallclock test harness readiness polling, not a business deadline
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := node.rpcCall("eth_blockNumber"); err == nil {
			return node
		}
		time.Sleep(250 * time.Millisecond)
	}
	node.stop()
	return nil
}

// killStrayAnvil removes a node left holding the port.
//
// Necessary because killing the WSL wrapper process does not kill the Anvil process inside the
// VM, so an interrupted run leaves the port bound and every subsequent run skips.
func killStrayAnvil() {
	pattern := fmt.Sprintf("anvil --port %d", anvilPort)

	if _, err := exec.LookPath("pkill"); err == nil {
		_ = exec.Command("pkill", "-f", pattern).Run()
		return
	}
	if wsl, err := exec.LookPath("wsl"); err == nil {
		_ = exec.Command(wsl, "-d", "Ubuntu", "-e", "pkill", "-f", pattern).Run()
	}
}

// anvilCommand locates Anvil, preferring a native binary and falling back to WSL.
func anvilCommand() (string, []string) {
	base := []string{
		"--port", fmt.Sprint(anvilPort),
		"--block-time", "1",
		"--silent",
	}

	if path, err := exec.LookPath("anvil"); err == nil {
		return path, base
	}

	if wsl, err := exec.LookPath("wsl"); err == nil {
		inner := "export PATH=$HOME/.foundry/bin:$PATH; exec anvil " + strings.Join(base, " ")
		return wsl, []string{"-d", "Ubuntu", "-e", "bash", "-c", inner}
	}
	return "", nil
}

func portOpen(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (a *anvilNode) stop() {
	if a.cmd != nil && a.cmd.Process != nil {
		_ = a.cmd.Process.Kill()
		_, _ = a.cmd.Process.Wait()
	}
	// Killing the wrapper is not enough when Anvil runs inside WSL, so the real process is
	// targeted by name as well. Leaving it running would make the next run skip everything.
	killStrayAnvil()
}

// rpcCall issues a raw JSON-RPC request, which is how the Anvil-specific cheat methods are
// reached since they are not part of the standard client.
func (a *anvilNode) rpcCall(method string, params ...any) (json.RawMessage, error) {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	if err != nil {
		return nil, err
	}

	resp, err := http.Post(a.url, "application/json", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, fmt.Errorf("rpc %s: %s", method, out.Error.Message)
	}
	return out.Result, nil
}

func (a *anvilNode) snapshot(t *testing.T) string {
	t.Helper()
	raw, err := a.rpcCall("evm_snapshot")
	if err != nil {
		t.Fatalf("evm_snapshot: %v", err)
	}
	var id string
	if err := json.Unmarshal(raw, &id); err != nil {
		t.Fatalf("decoding the snapshot id: %v", err)
	}
	return id
}

func (a *anvilNode) revert(t *testing.T, id string) {
	t.Helper()
	if _, err := a.rpcCall("evm_revert", id); err != nil {
		t.Fatalf("evm_revert: %v", err)
	}
}

func (a *anvilNode) mine(t *testing.T, blocks int) {
	t.Helper()
	for range blocks {
		if _, err := a.rpcCall("evm_mine"); err != nil {
			t.Fatalf("evm_mine: %v", err)
		}
	}
}

func (a *anvilNode) client(t *testing.T) *chain.Client {
	t.Helper()
	c, err := chain.Dial(context.Background(), config.ChainConfig{
		RPCURL:            config.Secret(a.url),
		ChainID:           31337, // Anvil's default
		RelayerPrivateKey: config.Secret(anvilKey),
		ConfirmationDepth: 2,
	})
	if err != nil {
		t.Fatalf("dialling anvil: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// ---------------------------------------------------------------------------
// A trivial encoder: a plain value transfer to a burn address.
//
// The relayer's job is nonce management, confirmation tracking and reorg handling. Exercising
// those needs transactions that land, not transactions that call AcreSync methods, and a transfer
// keeps the test independent of contract deployment.
// ---------------------------------------------------------------------------

type transferEncoder struct{ to common.Address }

func (e transferEncoder) Encode(entry *outbox.Entry) (chain.Call, error) {
	return chain.Call{To: e.to, Calldata: nil}, nil
}

type failingEncoder struct{}

func (failingEncoder) Encode(*outbox.Entry) (chain.Call, error) {
	return chain.Call{}, fmt.Errorf("this call can never be encoded")
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testKey derives a distinct idempotency key per test entry, using the real derivation so these
// tests exercise the same path production does.
func testKey(n int) idempotency.Key {
	return idempotency.MustDerive(idempotency.Input{
		Action:   idempotency.ActionAnchorPeriod,
		SchemeID: schemeID,
		Payload:  map[string]any{"n": n},
	})
}

func newRelayer(t *testing.T, store outbox.Store, c *chain.Client, enc Encoder, cfg outbox.Config) *Relayer {
	t.Helper()
	r, err := New(store, c, enc, Options{
		SchemeID: schemeID,
		Config:   cfg,
		Logger:   quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func testConfig() outbox.Config {
	return outbox.Config{
		ConfirmationDepth: 2,
		MaxAttempts:       3,
		PollInterval:      100 * time.Millisecond,
		ReceiptTimeout:    5 * time.Second,
	}
}

func enqueue(t *testing.T, s *outbox.Store, n int) *outbox.Entry {
	t.Helper()
	e, err := (*s).Enqueue(context.Background(), outbox.NewEntry{
		SchemeID:       schemeID,
		TargetContract: "0x000000000000000000000000000000000000dead",
		FunctionName:   "noop",
		Payload:        []byte(fmt.Sprintf(`{"n":%d}`, n)),
		IdempotencyKey: testKey(n),
		EnvironmentTag: "LOCAL",
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// ---------------------------------------------------------------------------
// Baseline
// ---------------------------------------------------------------------------

func TestAnvil_BroadcastAndConfirm(t *testing.T) {
	node := requireAnvil(t)
	client := node.client(t)
	ctx := context.Background()

	var store outbox.Store = outbox.NewMemoryStore()
	enc := transferEncoder{to: common.HexToAddress("0x000000000000000000000000000000000000dEaD")}
	r := newRelayer(t, store, client, enc, testConfig())

	entry := enqueue(t, &store, 1)

	// Broadcast.
	if err := r.Step(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get(ctx, entry.ID)
	if got.Status != outbox.StatusBroadcast {
		t.Fatalf("status = %s, want BROADCAST", got.Status)
	}
	if got.TxHash == "" || got.Nonce == nil {
		t.Fatal("broadcast must record a tx hash and a nonce")
	}

	// Mine enough blocks to satisfy the confirmation depth.
	node.mine(t, 4)

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if err := r.Step(ctx); err != nil {
			t.Fatal(err)
		}
		got, _ = store.Get(ctx, entry.ID)
		if got.Status == outbox.StatusConfirmed {
			break
		}
		node.mine(t, 1)
		time.Sleep(100 * time.Millisecond)
	}

	if got.Status != outbox.StatusConfirmed {
		t.Fatalf("status = %s after mining, want CONFIRMED (confirmations %d)", got.Status, got.Confirmations)
	}
	if !got.ReadyForFiat(testConfig().ConfirmationDepth) {
		t.Fatal("a confirmed entry with sufficient depth must satisfy the fiat gate")
	}
	if got.BlockNumber == nil {
		t.Fatal("the block number is the evidence the fiat gate checks; it must be recorded")
	}
}

// The gate that stops money moving on an anchor that could still disappear.
func TestAnvil_NotReadyForFiatBeforeDepth(t *testing.T) {
	node := requireAnvil(t)
	client := node.client(t)
	ctx := context.Background()

	cfg := testConfig()
	cfg.ConfirmationDepth = 6

	var store outbox.Store = outbox.NewMemoryStore()
	enc := transferEncoder{to: common.HexToAddress("0x000000000000000000000000000000000000dEaD")}
	r := newRelayer(t, store, client, enc, cfg)

	entry := enqueue(t, &store, 1)
	if err := r.Step(ctx); err != nil {
		t.Fatal(err)
	}

	// One block: mined, but nowhere near six confirmations.
	node.mine(t, 1)
	if err := r.Step(ctx); err != nil {
		t.Fatal(err)
	}

	got, _ := store.Get(ctx, entry.ID)
	if got.Status == outbox.StatusConfirmed {
		t.Fatal("must not confirm before reaching the configured depth")
	}
	if got.ReadyForFiat(cfg.ConfirmationDepth) {
		t.Fatal("fiat must not be authorised before the confirmation depth is reached")
	}
}

// ---------------------------------------------------------------------------
// Nonce discipline
// ---------------------------------------------------------------------------

func TestAnvil_SequentialNoncesNoGaps(t *testing.T) {
	node := requireAnvil(t)
	client := node.client(t)
	ctx := context.Background()

	var store outbox.Store = outbox.NewMemoryStore()
	enc := transferEncoder{to: common.HexToAddress("0x000000000000000000000000000000000000dEaD")}
	r := newRelayer(t, store, client, enc, testConfig())

	const count = 5
	for i := range count {
		enqueue(t, &store, i)
	}

	start, err := client.PendingNonce(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for range count {
		if err := r.Step(ctx); err != nil {
			t.Fatal(err)
		}
	}

	entries, _ := store.ListByStatus(ctx, schemeID)
	seen := map[uint64]string{}
	for _, e := range entries {
		if e.Nonce == nil {
			t.Fatalf("entry %s was never assigned a nonce", e.ID)
		}
		if prior, dup := seen[*e.Nonce]; dup {
			t.Fatalf("nonce %d assigned to both %s and %s", *e.Nonce, prior, e.ID)
		}
		seen[*e.Nonce] = e.ID
	}

	// Contiguous from the starting nonce. A gap stalls every later transaction behind it.
	for i := range uint64(count) {
		if _, ok := seen[start+i]; !ok {
			t.Errorf("nonce %d is missing; the sequence has a gap", start+i)
		}
	}
}

// The crash-recovery case. After a restart the node has not seen a transaction that was signed
// but never broadcast, so its pending nonce would reissue a slot the outbox considers spent.
func TestAnvil_NonceSeedPrefersOutboxWhenAhead(t *testing.T) {
	node := requireAnvil(t)
	client := node.client(t)
	ctx := context.Background()

	var store outbox.Store = outbox.NewMemoryStore()
	enc := transferEncoder{to: common.HexToAddress("0x000000000000000000000000000000000000dEaD")}

	nodeNonce, err := client.PendingNonce(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a crash after signing: the entry holds a nonce well ahead of the node's view and
	// was never broadcast.
	enqueue(t, &store, 1)
	claimed, err := store.ClaimNextQueued(ctx, schemeID, nodeNonce+7)
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil {
		t.Fatal("expected to claim the queued entry")
	}

	r := newRelayer(t, store, client, enc, testConfig())
	if err := r.SeedNonce(ctx); err != nil {
		t.Fatal(err)
	}

	if got := r.NextNonce(); got != nodeNonce+8 {
		t.Fatalf("seeded nonce = %d, want %d: the outbox must win when it is ahead of the node",
			got, nodeNonce+8)
	}
}

// ---------------------------------------------------------------------------
// Reorg
// ---------------------------------------------------------------------------

// The behaviour that cannot be tested without a real node. Anvil's snapshot and revert remove the
// block a transaction was mined in, which is exactly what a reorg does.
func TestAnvil_ReorgRequeuesTheEntry(t *testing.T) {
	node := requireAnvil(t)
	client := node.client(t)
	ctx := context.Background()

	var store outbox.Store = outbox.NewMemoryStore()
	enc := transferEncoder{to: common.HexToAddress("0x000000000000000000000000000000000000dEaD")}

	// A deep confirmation requirement keeps the entry in CONFIRMING long enough for the reorg to
	// be observable. With a shallow depth it reaches CONFIRMED, which is terminal by design, and
	// the requeue path never runs. That is itself the argument for a non-trivial depth: it is what
	// makes the window in which a reorg can still be handled wide enough to matter.
	cfg := testConfig()
	cfg.ConfirmationDepth = 50

	r := newRelayer(t, store, client, enc, cfg)

	// Snapshot before anything is sent, so reverting unmines the transaction entirely.
	snap := node.snapshot(t)

	entry := enqueue(t, &store, 1)
	if err := r.Step(ctx); err != nil {
		t.Fatal(err)
	}

	node.mine(t, 3)
	if err := r.Step(ctx); err != nil {
		t.Fatal(err)
	}

	got, _ := store.Get(ctx, entry.ID)
	if got.Status != outbox.StatusConfirming && got.Status != outbox.StatusConfirmed {
		t.Fatalf("expected the entry to be mined first, got %s", got.Status)
	}
	minedStatus := got.Status
	originalTx := got.TxHash
	t.Logf("before the reorg the entry was %s with %d confirmations", minedStatus, got.Confirmations)

	// Reorg it away.
	node.revert(t, snap)
	node.mine(t, 2)

	if err := r.Step(ctx); err != nil {
		t.Fatal(err)
	}

	got, _ = store.Get(ctx, entry.ID)

	// A confirmed entry is terminal by design, so only one that was still confirming can be
	// rescued. That asymmetry is the reason the confirmation depth exists at all: it makes the
	// window in which a reorg can still be handled wide enough to matter.
	if minedStatus == outbox.StatusConfirmed {
		if got.Status != outbox.StatusConfirmed {
			t.Fatalf("a confirmed entry must stay confirmed, got %s", got.Status)
		}
		t.Log("entry had already reached CONFIRMED, which is terminal; this is why depth matters")
		return
	}

	// The reorg must be recorded regardless of how far recovery got, because "this anchor was
	// mined, then unmined, then re-sent" is exactly the history an incident review needs.
	if !strings.Contains(strings.ToLower(got.LastError), "reorg") {
		t.Errorf("the reorg should be recorded on the entry, got %q", got.LastError)
	}

	switch got.Status {
	case outbox.StatusReorged, outbox.StatusQueued:
		// Detected but not yet re-sent.
		if got.Nonce != nil {
			t.Error("a requeued entry must have its nonce cleared so it is re-signed")
		}
		if got.TxHash != "" {
			t.Error("a requeued entry must have its tx hash cleared")
		}

	case outbox.StatusBroadcast, outbox.StatusPending, outbox.StatusConfirming:
		// Already recovered. A single Step advances in-flight work and then broadcasts, so a
		// requeued entry can be re-sent in the same pass that noticed the reorg. That is the
		// desired behaviour, not a race.
		//
		// What matters is that it was re-signed rather than rebroadcast: after a reorg the old
		// nonce may have been consumed by a different transaction that survived, so replaying the
		// old signed bytes would either be rejected or replace unrelated work.
		if got.TxHash == originalTx {
			t.Errorf("the entry was re-sent with the original transaction %s; it must be re-signed",
				originalTx)
		}
		if got.TxHash == "" {
			t.Error("a re-broadcast entry should carry its new transaction hash")
		}

	default:
		t.Fatalf("unexpected status after a reorg: %s (%s)", got.Status, got.LastError)
	}

	t.Logf("reorg handled: entry is now %s, tx %s -> %s (%s)",
		got.Status, originalTx, got.TxHash, got.LastError)
}

// ---------------------------------------------------------------------------
// Failure paths
// ---------------------------------------------------------------------------

// A call that cannot be encoded will never encode, so retrying burns nothing but time and hides
// the real error.
func TestAnvil_EncodingFailureDeadLetters(t *testing.T) {
	node := requireAnvil(t)
	client := node.client(t)
	ctx := context.Background()

	var store outbox.Store = outbox.NewMemoryStore()
	r := newRelayer(t, store, client, failingEncoder{}, testConfig())

	entry := enqueue(t, &store, 1)
	if err := r.Step(ctx); err != nil {
		t.Fatal(err)
	}

	got, _ := store.Get(ctx, entry.ID)
	if got.Status != outbox.StatusDeadLetter {
		t.Fatalf("status = %s, want DEAD_LETTER", got.Status)
	}
	if !strings.Contains(got.LastError, "encoded") {
		t.Errorf("the reason should name the encoding failure: %q", got.LastError)
	}
}

// A fee spike during an unattended run must stall the queue rather than silently drain the
// relayer.
func TestAnvil_FeeCeilingStallsRatherThanDrains(t *testing.T) {
	node := requireAnvil(t)
	client := node.client(t)
	ctx := context.Background()

	var store outbox.Store = outbox.NewMemoryStore()
	enc := transferEncoder{to: common.HexToAddress("0x000000000000000000000000000000000000dEaD")}

	r, err := New(store, client, enc, Options{
		SchemeID:     schemeID,
		Config:       testConfig(),
		Logger:       quietLogger(),
		MaxFeeCapWei: big.NewInt(1), // absurdly low, so every send is refused
	})
	if err != nil {
		t.Fatal(err)
	}

	entry := enqueue(t, &store, 1)
	if err := r.Step(ctx); err != nil {
		t.Fatal(err)
	}

	got, _ := store.Get(ctx, entry.ID)
	if got.Status == outbox.StatusBroadcast || got.Status == outbox.StatusConfirmed {
		t.Fatal("a send above the fee ceiling must not be broadcast")
	}
	if got.TxHash != "" {
		t.Error("nothing should have been broadcast")
	}
	t.Logf("fee ceiling honoured; entry is %s (%s)", got.Status, got.LastError)
}

// The retry ceiling. Without it a permanent failure becomes an infinite gas burn.
func TestAnvil_RetryCeilingDeadLetters(t *testing.T) {
	node := requireAnvil(t)
	client := node.client(t)
	ctx := context.Background()

	cfg := testConfig()
	cfg.MaxAttempts = 2

	var store outbox.Store = outbox.NewMemoryStore()
	enc := transferEncoder{to: common.HexToAddress("0x000000000000000000000000000000000000dEaD")}

	r, err := New(store, client, enc, Options{
		SchemeID:     schemeID,
		Config:       cfg,
		Logger:       quietLogger(),
		MaxFeeCapWei: big.NewInt(1), // forces every send to fail
	})
	if err != nil {
		t.Fatal(err)
	}

	entry := enqueue(t, &store, 1)

	for range cfg.MaxAttempts + 2 {
		if err := r.Step(ctx); err != nil {
			t.Fatal(err)
		}
		got, _ := store.Get(ctx, entry.ID)
		if got.Status == outbox.StatusDeadLetter {
			t.Logf("dead lettered after %d attempts", got.AttemptCount)
			return
		}
	}

	got, _ := store.Get(ctx, entry.ID)
	t.Fatalf("expected DEAD_LETTER within the retry ceiling, still %s after %d attempts",
		got.Status, got.AttemptCount)
}

// ---------------------------------------------------------------------------
// Crash safety
// ---------------------------------------------------------------------------

// A crash between broadcasting and recording is the one genuinely dangerous window: the
// transaction is on the network but the orchestrator does not know to watch it. This confirms the
// nonce is not reissued, which is what would turn a lost record into a lost anchor.
func TestAnvil_RestartDoesNotReuseABroadcastNonce(t *testing.T) {
	node := requireAnvil(t)
	client := node.client(t)
	ctx := context.Background()

	var store outbox.Store = outbox.NewMemoryStore()
	enc := transferEncoder{to: common.HexToAddress("0x000000000000000000000000000000000000dEaD")}

	r1 := newRelayer(t, store, client, enc, testConfig())
	entry := enqueue(t, &store, 1)
	if err := r1.Step(ctx); err != nil {
		t.Fatal(err)
	}

	first, _ := store.Get(ctx, entry.ID)
	if first.Nonce == nil {
		t.Fatal("expected a nonce to be assigned")
	}
	usedNonce := *first.Nonce

	// A brand new relayer, as if the process had restarted.
	r2 := newRelayer(t, store, client, enc, testConfig())
	if err := r2.SeedNonce(ctx); err != nil {
		t.Fatal(err)
	}

	if r2.NextNonce() <= usedNonce {
		t.Fatalf("after a restart the next nonce is %d but %d is already broadcast",
			r2.NextNonce(), usedNonce)
	}

	enqueue(t, &store, 2)
	if err := r2.Step(ctx); err != nil {
		t.Fatal(err)
	}

	entries, _ := store.ListByStatus(ctx, schemeID)
	seen := map[uint64]string{}
	for _, e := range entries {
		if e.Nonce == nil {
			continue
		}
		if prior, dup := seen[*e.Nonce]; dup {
			t.Fatalf("nonce %d reused across a restart: %s and %s", *e.Nonce, prior, e.ID)
		}
		seen[*e.Nonce] = e.ID
	}
}

func TestMain(m *testing.M) {
	sharedNode = startAnvil()

	code := m.Run()

	if sharedNode != nil {
		sharedNode.stop()
	}
	os.Exit(code)
}
