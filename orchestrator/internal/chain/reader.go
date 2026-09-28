package chain

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/acresync/orchestrator/internal/config"
	"github.com/acresync/orchestrator/internal/merkle"
)

// Reader reads the chain head and block hashes, and holds no key.
//
// The API needs exactly two facts from the chain for the ballot ceremony: where the head is, to judge the reveal
// window, and the hash of the target block, which feeds the seed. Neither needs the relayer key, and a process
// that only serves HTTP should not hold the one credential that can write to the contracts.
type Reader struct {
	rpc *ethclient.Client
}

// DialReader connects and verifies the chain id, for the same reason Dial does.
func DialReader(ctx context.Context, cfg config.ChainConfig) (*Reader, error) {
	rpc, err := ethclient.DialContext(ctx, cfg.RPCURL.Reveal())
	if err != nil {
		return nil, fmt.Errorf("chain: dialling the RPC endpoint failed: %w", err)
	}
	remoteID, err := rpc.ChainID(ctx)
	if err != nil {
		rpc.Close()
		return nil, fmt.Errorf("chain: reading chain id: %w", err)
	}
	if remoteID.Int64() != cfg.ChainID {
		rpc.Close()
		return nil, fmt.Errorf("%w: node reports %d, configuration says %d",
			ErrChainIDMismatch, remoteID.Int64(), cfg.ChainID)
	}
	return &Reader{rpc: rpc}, nil
}

func (r *Reader) Close() { r.rpc.Close() }

// Head is the current block number.
func (r *Reader) Head(ctx context.Context) (uint64, error) {
	return r.rpc.BlockNumber(ctx)
}

// BlockHash is the hash of block n.
func (r *Reader) BlockHash(ctx context.Context, n uint64) (merkle.Hash, error) {
	h, err := r.rpc.HeaderByNumber(ctx, new(big.Int).SetUint64(n))
	if err != nil {
		return merkle.Hash{}, fmt.Errorf("chain: reading block %d: %w", n, err)
	}
	if h == nil {
		return merkle.Hash{}, errors.New("chain: the node returned no header")
	}
	return merkle.Hash(h.Hash()), nil
}
