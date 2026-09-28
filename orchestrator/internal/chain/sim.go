package chain

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"time"

	"github.com/acresync/orchestrator/internal/clock"
	"github.com/acresync/orchestrator/internal/merkle"
)

// Simulated is a chain that does not exist, for LOCAL demos only.
//
// The head advances one block every twelve seconds of wall time, as Sepolia does, and a block's hash is a
// fixed function of its number. Nothing about it is unpredictable: anyone can compute every future block hash,
// so a ballot seeded from it proves nothing about fairness. It exists so the ceremony's state machine can be
// clicked through without an RPC key, and cmd/devconfirm reads the same head so the block numbers it records
// agree with what the API judges the reveal window against.
type Simulated struct{}

// simGenesis is block zero of the simulated chain.
var simGenesis = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const simBlockTime = 12 * time.Second

func (Simulated) Head(ctx context.Context) (uint64, error) {
	now, err := clock.Real().Now(ctx)
	if err != nil {
		return 0, err
	}
	return uint64(now.Sub(simGenesis) / simBlockTime), nil
}

func (Simulated) BlockHash(_ context.Context, n uint64) (merkle.Hash, error) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], n)
	return merkle.Hash(sha256.Sum256(append([]byte("acresync/simulated-block/v1/"), buf[:]...))), nil
}
