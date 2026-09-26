package ipfs

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/acresync/orchestrator/internal/ipfsguard"
)

// MockProvider is a content-addressed store backed by a directory.
//
// It is not a stub that returns plausible strings. It computes real SHA-256 digests, derives real
// CIDs through the same published rule the production path uses, and stores each document under its
// own address, so re-pinning identical content is idempotent and lands on the same key the way a
// real content-addressed store does. That fidelity is what lets the M5 integration test prove the
// full verification path — fetch by CID, hash, compare against the anchored digest — without a
// network dependency. A mock that faked the addresses would let the pipeline pass while the one
// property that matters went untested.
//
// It pins single raw blocks by construction, so CodecVerified is always true against it. Whether
// that also holds against a real pinning service is an empirical question the Pinata provider
// answers, and deliberately does not assume.
type MockProvider struct {
	dir string

	mu     sync.Mutex
	counts map[string]int

	// failNext makes the next Pin fail. Pinning is a network call in production and the period
	// pipeline has to survive one failing midway, so the failure path needs to be reachable in a
	// test rather than only in theory.
	failNext error

	// fetchFailNext makes the next Fetch fail, which models the case that actually bites: the pin
	// reported success but the content is not retrievable.
	fetchFailNext error
}

func NewMockProvider(dir string) (*MockProvider, error) {
	if dir == "" {
		return nil, errors.New("ipfs: mock provider needs a directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("ipfs: mock provider directory: %w", err)
	}
	return &MockProvider{dir: dir, counts: make(map[string]int)}, nil
}

func (m *MockProvider) Name() string { return "MOCK" }

// Dir exposes the backing directory so tests can inspect what was written.
func (m *MockProvider) Dir() string { return m.dir }

// FailNextPin arms a one-shot pin failure.
func (m *MockProvider) FailNextPin(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failNext = err
}

// FailNextFetch arms a one-shot fetch failure.
func (m *MockProvider) FailNextFetch(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fetchFailNext = err
}

// Corrupt overwrites stored content without changing its address.
//
// Impossible in a real content-addressed store, which is exactly why it is worth being able to do
// here: it is the only way to exercise the branch where a fetched document does not match its
// anchored digest, and that branch is the one standing between a silent mismatch and a loud
// failure.
func (m *MockProvider) Corrupt(cid string, replacement []byte) error {
	return os.WriteFile(filepath.Join(m.dir, cid), replacement, 0o644)
}

func (m *MockProvider) Pin(ctx context.Context, docType ipfsguard.DocType, canonical []byte) (*Pin, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	if m.failNext != nil {
		err := m.failNext
		m.failNext = nil
		m.mu.Unlock()
		return nil, fmt.Errorf("ipfs: mock pin failed: %w", err)
	}
	m.mu.Unlock()

	if len(canonical) == 0 {
		return nil, ErrEmptyDocument
	}

	digest := sha256.Sum256(canonical)
	cid := CIDFromDigest(digest)

	path := filepath.Join(m.dir, cid)

	// Written through a temp file and renamed, so a crash mid-write cannot leave a truncated
	// document sitting at a valid address. A partial file at the right name is worse than a missing
	// one: the fetch succeeds and the digest check fails, pointing the investigation at the wrong
	// thing entirely.
	tmp, err := os.CreateTemp(m.dir, ".pin-*")
	if err != nil {
		return nil, fmt.Errorf("ipfs: mock pin: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(canonical); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return nil, fmt.Errorf("ipfs: mock pin: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return nil, fmt.Errorf("ipfs: mock pin: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return nil, fmt.Errorf("ipfs: mock pin: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return nil, fmt.Errorf("ipfs: mock pin: %w", err)
	}

	m.mu.Lock()
	m.counts[cid]++
	count := m.counts[cid]
	m.mu.Unlock()

	return &Pin{
		DocType:     docType,
		ByteSize:    len(canonical),
		Digest:      digest,
		DerivedCID:  cid,
		ProviderCID: cid,
		Provider:    m.Name(),
		ProviderRef: cid,
		PinCount:    count,
		SingleBlock: len(canonical) <= SingleBlockMaxBytes,
	}, nil
}

func (m *MockProvider) Fetch(ctx context.Context, cid string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	if m.fetchFailNext != nil {
		err := m.fetchFailNext
		m.fetchFailNext = nil
		m.mu.Unlock()
		return nil, fmt.Errorf("ipfs: mock fetch failed: %w", err)
	}
	m.mu.Unlock()

	// The address is validated before it touches the filesystem. CIDs arrive from stored records
	// and, in the verification tooling, potentially from user input, and a CID is a path component
	// here. Rejecting anything that is not a well-formed CIDv1 raw address means traversal
	// sequences never reach filepath.Join.
	if _, err := DigestFromCID(cid); err != nil {
		return nil, fmt.Errorf("ipfs: mock fetch: %w", err)
	}

	b, err := os.ReadFile(filepath.Join(m.dir, cid))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, cid)
	}
	if err != nil {
		return nil, fmt.Errorf("ipfs: mock fetch: %w", err)
	}
	return b, nil
}

// PinCountFor reports how many times content was pinned.
func (m *MockProvider) PinCountFor(cid string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[cid]
}
