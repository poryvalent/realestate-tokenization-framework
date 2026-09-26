// Package chain wraps the Ethereum JSON-RPC client and the relayer's signing key.
//
// # The single authorised relayer
//
// AcreSync's contracts accept business calls from exactly one address. That is a deliberate
// centralisation: the chain is a mirror of facts established off-chain, so there is nothing for
// a permissionless writer to contribute, and a single writer means a single nonce sequence that
// can be reasoned about.
//
// It also means the relayer key is the most sensitive credential in the system, which is why the
// contracts custody no value. The worst outcome of a compromised relayer is a corrupted
// attestation, recoverable by revoking the key and redeploying nothing. Compare that with a
// design where the same key could move funds.
//
// # Confirmation depth is not optional
//
// The orchestrator reads chain state and then irreversibly moves money through banking rails.
// A reorg after a bank transfer has cleared cannot be undone. Every read that authorises fiat
// therefore goes through ConfirmationsFor, which checks the transaction is still in the
// canonical chain at the block it claimed, not merely that a receipt once existed.
package chain

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/acresync/orchestrator/internal/config"
)

var (
	ErrNoReceipt        = errors.New("chain: no receipt yet")
	ErrReverted         = errors.New("chain: transaction reverted")
	ErrReorged          = errors.New("chain: transaction is no longer in the canonical chain")
	ErrChainIDMismatch  = errors.New("chain: node chain id does not match configuration")
	ErrBadPrivateKey    = errors.New("chain: private key is not 0x followed by 64 hex characters")
	ErrInsufficientFees = errors.New("chain: suggested fees exceed the configured ceiling")
)

// Client is a signing JSON-RPC client bound to one relayer key.
type Client struct {
	rpc     *ethclient.Client
	key     *ecdsa.PrivateKey
	from    common.Address
	chainID *big.Int
	signer  types.Signer
}

// Dial connects, verifies the chain id, and loads the relayer key.
//
// The chain id is verified rather than assumed. Signing with the wrong chain id produces a
// transaction that is valid on a different network, and the failure mode of pointing a Sepolia
// configuration at the wrong endpoint should be a refusal at startup rather than a confusing
// rejection later.
func Dial(ctx context.Context, cfg config.ChainConfig) (*Client, error) {
	rpc, err := ethclient.DialContext(ctx, cfg.RPCURL.Reveal())
	if err != nil {
		// The URL usually embeds a provider key, so it is never included in the error.
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

	key, err := parseKey(cfg.RelayerPrivateKey.Reveal())
	if err != nil {
		rpc.Close()
		return nil, err
	}

	return &Client{
		rpc:     rpc,
		key:     key,
		from:    crypto.PubkeyToAddress(key.PublicKey),
		chainID: remoteID,
		signer:  types.LatestSignerForChainID(remoteID),
	}, nil
}

func parseKey(raw string) (*ecdsa.PrivateKey, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(raw), "0x")
	if len(trimmed) != 64 {
		// The key material is never echoed, only the shape complaint.
		return nil, ErrBadPrivateKey
	}
	key, err := crypto.HexToECDSA(trimmed)
	if err != nil {
		return nil, fmt.Errorf("%w", ErrBadPrivateKey)
	}
	return key, nil
}

func (c *Client) Close() {
	if c.rpc != nil {
		c.rpc.Close()
	}
}

// From is the relayer address.
func (c *Client) From() common.Address { return c.from }

// ChainID is the verified chain id.
func (c *Client) ChainID() *big.Int { return new(big.Int).Set(c.chainID) }

// BlockNumber returns the current head.
func (c *Client) BlockNumber(ctx context.Context) (uint64, error) {
	return c.rpc.BlockNumber(ctx)
}

// BalanceWei returns the relayer's balance.
func (c *Client) BalanceWei(ctx context.Context) (*big.Int, error) {
	return c.rpc.BalanceAt(ctx, c.from, nil)
}

// PendingNonce returns the next nonce the node expects, counting queued transactions.
//
// Used only to seed the relayer's own sequence at startup. During normal operation the outbox is
// the source of truth for which nonces are consumed, because the node's view lags a transaction
// that has been signed but not yet accepted, and trusting it would hand the same nonce to two
// different calls.
func (c *Client) PendingNonce(ctx context.Context) (uint64, error) {
	return c.rpc.PendingNonceAt(ctx, c.from)
}

// SendResult describes a broadcast transaction.
type SendResult struct {
	TxHash      common.Hash
	Nonce       uint64
	GasLimit    uint64
	GasFeeCap   *big.Int
	GasTipCap   *big.Int
	BroadcastAt time.Time
}

// SendOpts tunes one send.
type SendOpts struct {
	// Nonce is assigned by the outbox rather than read from the node.
	Nonce uint64

	// GasLimitOverride skips estimation. Useful when a call is known to revert under current
	// state but must still be attempted, since estimation would fail first.
	GasLimitOverride uint64

	// MaxFeeCapWei refuses to send if the suggested fee exceeds this. A fee spike during an
	// unattended run should stall the queue rather than silently drain the relayer.
	MaxFeeCapWei *big.Int
}

// Call is an encoded contract call.
type Call struct {
	To       common.Address
	Calldata []byte
}

// Send signs and broadcasts a call.
//
// EIP-1559 dynamic fees. Legacy pricing would require guessing a gas price that is either wasteful
// or too low to be included, and a transaction stuck for hours blocks every later nonce behind it.
func (c *Client) Send(ctx context.Context, call Call, opts SendOpts) (*SendResult, error) {
	tipCap, err := c.rpc.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, fmt.Errorf("chain: suggesting a gas tip: %w", err)
	}

	head, err := c.rpc.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("chain: reading the latest header: %w", err)
	}

	// Base fee can rise 12.5% per block. Doubling it plus the tip gives room for several blocks
	// of increase without overpaying, since unused fee is refunded under EIP-1559.
	baseFee := head.BaseFee
	if baseFee == nil {
		baseFee = big.NewInt(0)
	}
	feeCap := new(big.Int).Add(new(big.Int).Mul(baseFee, big.NewInt(2)), tipCap)

	if opts.MaxFeeCapWei != nil && feeCap.Cmp(opts.MaxFeeCapWei) > 0 {
		return nil, fmt.Errorf("%w: suggested %s wei exceeds the ceiling %s wei",
			ErrInsufficientFees, feeCap, opts.MaxFeeCapWei)
	}

	gasLimit := opts.GasLimitOverride
	if gasLimit == 0 {
		estimated, estErr := c.rpc.EstimateGas(ctx, ethereumCallMsg(c.from, call))
		if estErr != nil {
			return nil, fmt.Errorf("chain: estimating gas (the call would likely revert): %w", estErr)
		}
		// A 25% buffer. Estimation runs against current state, and state can move between
		// estimation and inclusion.
		gasLimit = estimated + estimated/4
	}

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   c.chainID,
		Nonce:     opts.Nonce,
		GasTipCap: tipCap,
		GasFeeCap: feeCap,
		Gas:       gasLimit,
		To:        &call.To,
		Value:     big.NewInt(0), // the contracts custody nothing; a non-zero value would be a bug
		Data:      call.Calldata,
	})

	signed, err := types.SignTx(tx, c.signer, c.key)
	if err != nil {
		return nil, fmt.Errorf("chain: signing: %w", err)
	}

	if err := c.rpc.SendTransaction(ctx, signed); err != nil {
		return nil, fmt.Errorf("chain: broadcasting: %w", err)
	}

	return &SendResult{
		TxHash:    signed.Hash(),
		Nonce:     opts.Nonce,
		GasLimit:  gasLimit,
		GasFeeCap: feeCap,
		GasTipCap: tipCap,
		//acresync:allow-wallclock broadcast time is audit metadata, not a business deadline
		BroadcastAt: time.Now().UTC(),
	}, nil
}

// Receipt reports the outcome of a transaction.
type Receipt struct {
	TxHash      common.Hash
	BlockNumber uint64
	BlockHash   common.Hash
	Success     bool
	GasUsed     uint64
}

// GetReceipt fetches a receipt, returning ErrNoReceipt while the transaction is still pending.
func (c *Client) GetReceipt(ctx context.Context, txHash common.Hash) (*Receipt, error) {
	r, err := c.rpc.TransactionReceipt(ctx, txHash)
	if err != nil {
		if strings.Contains(err.Error(), "not found") || errors.Is(err, ethereum.NotFound) {
			return nil, ErrNoReceipt
		}
		return nil, fmt.Errorf("chain: fetching the receipt: %w", err)
	}
	return &Receipt{
		TxHash:      txHash,
		BlockNumber: r.BlockNumber.Uint64(),
		BlockHash:   r.BlockHash,
		Success:     r.Status == types.ReceiptStatusSuccessful,
		GasUsed:     r.GasUsed,
	}, nil
}

// ConfirmationsFor returns how many blocks have been built on top of a transaction, and verifies
// it is still in the canonical chain.
//
// The reorg check is the point. A receipt proves a transaction was mined in some block; it does
// not prove that block is still part of the chain. Comparing the canonical block hash at that
// height against the hash the receipt named catches a reorg, and doing so before releasing fiat
// is the difference between a recoverable incident and an unrecoverable one.
func (c *Client) ConfirmationsFor(ctx context.Context, txHash common.Hash) (int, *Receipt, error) {
	receipt, err := c.GetReceipt(ctx, txHash)
	if err != nil {
		return 0, nil, err
	}

	head, err := c.rpc.BlockNumber(ctx)
	if err != nil {
		return 0, receipt, fmt.Errorf("chain: reading the head: %w", err)
	}
	if head < receipt.BlockNumber {
		// The head is behind the receipt, which means the node is serving an inconsistent view.
		// Reporting zero is safer than a negative count.
		return 0, receipt, nil
	}

	canonical, err := c.rpc.HeaderByNumber(ctx, new(big.Int).SetUint64(receipt.BlockNumber))
	if err != nil {
		return 0, receipt, fmt.Errorf("chain: reading the canonical header at %d: %w", receipt.BlockNumber, err)
	}
	if canonical.Hash() != receipt.BlockHash {
		return 0, receipt, fmt.Errorf("%w: block %d is now %s, the receipt named %s",
			ErrReorged, receipt.BlockNumber, canonical.Hash().Hex(), receipt.BlockHash.Hex())
	}

	return int(head-receipt.BlockNumber) + 1, receipt, nil
}

// ethereumCallMsg builds the message used for gas estimation.
//
// Value is explicitly zero. The contracts custody nothing, so a call carrying value would be a
// bug, and stating that here means an accidental non-zero value cannot slip through estimation.
func ethereumCallMsg(from common.Address, call Call) ethereum.CallMsg {
	return ethereum.CallMsg{
		From:  from,
		To:    &call.To,
		Value: big.NewInt(0),
		Data:  call.Calldata,
	}
}

// EncodeCall ABI-encodes a call against a parsed contract ABI.
func EncodeCall(contractABI *abi.ABI, method string, args ...any) ([]byte, error) {
	packed, err := contractABI.Pack(method, args...)
	if err != nil {
		return nil, fmt.Errorf("chain: encoding %s: %w", method, err)
	}
	return packed, nil
}

// ParseABI parses a JSON ABI document.
func ParseABI(jsonABI string) (*abi.ABI, error) {
	parsed, err := abi.JSON(strings.NewReader(jsonABI))
	if err != nil {
		return nil, fmt.Errorf("chain: parsing the ABI: %w", err)
	}
	return &parsed, nil
}
