// Package ballotrun orchestrates the commit-reveal ceremony and the draw.
//
// The allocation algorithm lives in internal/ballot and is not reimplemented here. What this package
// owns is the ordering: which artefact has to exist before the next step is allowed, where the secret
// comes from, and when the seed may be revealed. Those are the parts that make the draw unriggable,
// and none of them are arithmetic.
//
// # The shape of the ceremony
//
// The bid book is anchored first, so the set of bids is fixed. Then a commitment to a secret is
// anchored and the contract, not the caller, picks a target block some distance ahead. When that block
// exists its hash is mixed with the secret and the book root to make the final seed. The draw is then a
// pure function of three values, two of which were fixed before the third existed.
//
// The operator therefore cannot steer the outcome. They choose the secret but not the blockhash, and by
// the time the blockhash exists the secret is already public knowledge in committed form.
package ballotrun

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/acresync/orchestrator/internal/ballot"
	"github.com/acresync/orchestrator/internal/merkle"
)

// Stage mirrors the ballot_status Postgres enum and the contract's Stage enum.
type Stage string

const (
	StagePending         Stage = "PENDING"
	StageBidbookAnchored Stage = "BIDBOOK_ANCHORED"
	StageSeedCommitted   Stage = "SEED_COMMITTED"
	StageSeedRevealed    Stage = "SEED_REVEALED"
	StageDrawn           Stage = "DRAWN"
	StageResultAnchored  Stage = "RESULT_ANCHORED"
	StageEscalated       Stage = "ESCALATED"
	StageAbandoned       Stage = "ABANDONED"
)

func AllStages() []Stage {
	return []Stage{
		StagePending, StageBidbookAnchored, StageSeedCommitted, StageSeedRevealed,
		StageDrawn, StageResultAnchored, StageEscalated, StageAbandoned,
	}
}

func (s Stage) IsTerminal() bool {
	return s == StageResultAnchored || s == StageAbandoned
}

var (
	ErrWrongStage         = errors.New("ballotrun: wrong stage for this step")
	ErrNoPepper           = errors.New("ballotrun: no pepper to derive the seed secret from")
	ErrTargetNotReached   = errors.New("ballotrun: the target block has not been mined yet")
	ErrWindowExpired      = errors.New("ballotrun: the reveal window has lapsed")
	ErrWindowStillOpen    = errors.New("ballotrun: the reveal window is still open")
	ErrAttemptsExhausted  = errors.New("ballotrun: the commit attempt cap is exhausted")
	ErrBlockhashLost      = errors.New("ballotrun: the target blockhash is no longer retrievable")
	ErrCommitmentMismatch = errors.New("ballotrun: the secret does not match the anchored commitment")
	ErrBookNotAnchored    = errors.New("ballotrun: the bid book root is not anchored")
	ErrRootMismatch       = errors.New("ballotrun: the book does not match the anchored root")
)

// BlockhashHorizon is how far back the EVM can still return a blockhash.
//
// blockhash(n) returns zero once n is more than 256 blocks behind. A zero folded into the seed would
// produce a draw that looked valid and was derived from nothing, which is why the contract rejects it
// explicitly rather than relying on the window check alone.
const BlockhashHorizon = 256

// seedSecretDomain separates this HMAC use from investor anchoring under the same pepper.
//
// Without it, a pepper used for both would let one derivation be mistaken for the other if the inputs
// ever collided. They are different lengths and shapes today, which is an argument for the separator
// being cheap, not for it being unnecessary.
const seedSecretDomain = "acresync.ballot.seed-secret.v1"

// DeriveSecret derives the seed secret for an offer.
//
// # Why the secret is derived rather than generated and stored
//
// The obvious implementation reads 32 bytes from crypto/rand and writes them down. That cannot be done
// safely here, and the reason is a database trigger: ballot_runs_enforce_reveal_order refuses to store
// seed_plaintext until commitment_anchored_tx is set. The trigger is right to do so, because a plaintext
// on disk before the commitment is anchored is an operator who can still choose which secret they
// "always intended". But it means there is a window where the secret exists, the commitment has been
// anchored to it, and nothing durable holds the secret. A crash in that window loses it permanently, the
// reveal becomes impossible, and the ceremony burns attempts until a trustee has to step in.
//
// Deriving the secret closes the window: after any crash it can be recomputed from the pepper and two
// identifiers that were already durable before the ceremony began.
//
// # Why a predictable secret is not a weakness
//
// It looks alarming and is not. The secret does not need to be unpredictable to AcreSync, because
// AcreSync chooses it; it needs to be FIXED before the blockhash exists. All the entropy that makes the
// draw fair comes from the target block, which nobody can predict at commit time. The secret's only job
// is to stop the operator swapping it afterwards, and a commitment does that whether the secret was
// random or derived.
//
// To everyone else the value is still unpredictable: HMAC-SHA256 under a key that never leaves KMS.
//
// # Keyed on the offer, not the attempt
//
// recommitSeed takes no commitment argument and the seed_commitment column is immutable once set, both
// deliberately: an abandoned window rerolls the blockhash and nothing else. Mixing the attempt number in
// would derive a new secret on the second attempt, the commitment would no longer match, and the reveal
// would revert with CommitmentMismatch.
func DeriveSecret(pepper []byte, schemeID, offerID string) (merkle.Hash, error) {
	if len(pepper) == 0 {
		return merkle.Hash{}, ErrNoPepper
	}
	if schemeID == "" || offerID == "" {
		return merkle.Hash{}, errors.New("ballotrun: deriving a secret needs both identifiers")
	}

	m := hmac.New(sha256.New, pepper)
	m.Write([]byte(seedSecretDomain))
	m.Write([]byte("|"))
	m.Write([]byte(schemeID))
	m.Write([]byte("|"))
	m.Write([]byte(offerID))

	var out merkle.Hash
	copy(out[:], m.Sum(nil))
	return out, nil
}

// Config is the ceremony timing, mirroring the contract's constructor arguments.
type Config struct {
	// DelayBlocks is how far ahead of the commit the target block sits.
	DelayBlocks uint32

	// RevealWindowBlocks is how long after the target block a reveal is accepted.
	RevealWindowBlocks uint32

	// MaxAttempts is the commit attempt cap before a trustee must intervene.
	MaxAttempts uint32
}

// Validate checks the configuration against what the contract will accept.
func (c Config) Validate() error {
	if c.DelayBlocks == 0 {
		return errors.New("ballotrun: delay blocks must be positive, or the target block is the " +
			"current one and its hash is already known at commit time")
	}
	if c.RevealWindowBlocks == 0 {
		return errors.New("ballotrun: the reveal window must be positive")
	}
	if c.MaxAttempts == 0 {
		return errors.New("ballotrun: the attempt cap must be positive")
	}

	// The contract refuses anything above 250 for the same reason.
	//
	// A window wider than the blockhash horizon is shorter than it claims: the last stretch of it is
	// unusable because blockhash has already returned to zero. Rejecting the configuration is better
	// than accepting one whose advertised deadline cannot be met.
	if c.RevealWindowBlocks > 250 {
		return fmt.Errorf("ballotrun: a reveal window of %d blocks exceeds 250; beyond the %d-block "+
			"blockhash horizon the target hash is zero and no reveal can succeed",
			c.RevealWindowBlocks, BlockhashHorizon)
	}
	return nil
}

// V1Config is the ceremony timing of the deployed contract.
//
// These are not free parameters. The contract was constructed with them and they are immutable, so a
// value here that disagrees is a Go layer reasoning about a chain that behaves differently.
// TestConfigMatchesTheDeployedContract parses them out of the deploy script to keep the two in step.
func V1Config() Config {
	return Config{DelayBlocks: 10, RevealWindowBlocks: 200, MaxAttempts: 3}
}

// Ceremony is the commit-reveal state for one offer.
type Ceremony struct {
	SchemeID string
	OfferID  string

	Stage Stage

	// BidbookRoot is the anchored book root. Part of the final seed.
	BidbookRoot merkle.Hash

	// Commitment is sha256(secret), as anchored. Immutable once set.
	Commitment merkle.Hash

	// CommitmentAnchored records whether commitSeed is confirmed on-chain.
	//
	// The gate the reveal-order trigger enforces, held here so the same rule applies before a
	// transaction is built rather than only when a row is written.
	CommitmentAnchored bool

	// TargetBlock is the block whose hash feeds the seed. Chosen by the contract, never by the caller.
	TargetBlock uint64

	// Attempt is the commit attempt, starting at 1 when commitSeed succeeds.
	Attempt uint32

	Config Config
}

// Deadline is the last block at which a reveal is still accepted.
func (c *Ceremony) Deadline() uint64 {
	return c.TargetBlock + uint64(c.Config.RevealWindowBlocks)
}

// CanCommit reports whether commitSeed may be called.
func (c *Ceremony) CanCommit() error {
	if c.Stage != StageBidbookAnchored {
		return fmt.Errorf("%w: commitSeed requires BIDBOOK_ANCHORED, this ceremony is %s",
			ErrWrongStage, c.Stage)
	}
	if c.BidbookRoot.IsZero() {
		// The same ordering the trigger enforces: seed_commitment requires bidbook_merkle_root.
		// Committing first would let the book still move under a seed already bound to it.
		return ErrBookNotAnchored
	}
	if c.Commitment.IsZero() {
		return errors.New("ballotrun: there is no commitment to anchor")
	}
	return nil
}

// CanReveal reports whether revealSeed may be called at the given block.
//
// Mirrors the contract's three checks exactly, including the strict inequality on the target block: the
// hash of a block does not exist until the block after it, so revealing AT the target would read a hash
// that is not yet determined.
func (c *Ceremony) CanReveal(currentBlock uint64) error {
	if c.Stage != StageSeedCommitted {
		return fmt.Errorf("%w: revealSeed requires SEED_COMMITTED, this ceremony is %s",
			ErrWrongStage, c.Stage)
	}
	if !c.CommitmentAnchored {
		// Mirrors ballot_runs_enforce_reveal_order. Revealing before the commitment is on-chain would
		// let an operator who disliked the resulting draw claim a different secret was always intended.
		return errors.New("ballotrun: the commitment is not confirmed on-chain, so there is nothing " +
			"the revealed secret can be checked against")
	}
	if c.TargetBlock == 0 {
		return errors.New("ballotrun: no target block is set")
	}

	if currentBlock <= c.TargetBlock {
		return fmt.Errorf("%w: target is %d, chain is at %d", ErrTargetNotReached, c.TargetBlock, currentBlock)
	}
	if deadline := c.Deadline(); currentBlock > deadline {
		return fmt.Errorf("%w: deadline was block %d, chain is at %d; the remedy is a recommit, "+
			"which rerolls the target block and reuses the same commitment",
			ErrWindowExpired, deadline, currentBlock)
	}

	// Defence in depth, exactly as the contract does it. Validate() makes this unreachable for a
	// conforming configuration, and a zero blockhash folded into the seed would produce a draw that
	// looked valid and was derived from nothing.
	if currentBlock-c.TargetBlock > BlockhashHorizon {
		return fmt.Errorf("%w: block %d is %d behind the chain head, past the %d-block horizon",
			ErrBlockhashLost, c.TargetBlock, currentBlock-c.TargetBlock, BlockhashHorizon)
	}
	return nil
}

// CanRecommit reports whether recommitSeed may be called.
//
// The window must have lapsed. Recommitting while a reveal is still possible would let an operator skip
// a draw they could already compute, which is the one thing the ceremony exists to prevent.
func (c *Ceremony) CanRecommit(currentBlock uint64) error {
	if c.Stage != StageSeedCommitted {
		return fmt.Errorf("%w: recommitSeed requires SEED_COMMITTED, this ceremony is %s",
			ErrWrongStage, c.Stage)
	}
	if deadline := c.Deadline(); currentBlock <= deadline {
		return fmt.Errorf("%w: the deadline is block %d and the chain is at %d, so a reveal is still "+
			"possible and must be attempted instead", ErrWindowStillOpen, deadline, currentBlock)
	}
	if c.Attempt >= c.Config.MaxAttempts {
		return fmt.Errorf("%w: attempt %d of %d used; only a trustee can act now",
			ErrAttemptsExhausted, c.Attempt, c.Config.MaxAttempts)
	}
	return nil
}

// CanEscalate reports whether a trustee may escalate.
func (c *Ceremony) CanEscalate(currentBlock uint64) error {
	if c.Stage != StageSeedCommitted {
		return fmt.Errorf("%w: escalateBallot requires SEED_COMMITTED, this ceremony is %s",
			ErrWrongStage, c.Stage)
	}
	if c.Attempt < c.Config.MaxAttempts {
		return fmt.Errorf("%w: attempt %d of %d, so a recommit is still available",
			ErrAttemptsExhausted, c.Attempt, c.Config.MaxAttempts)
	}
	if deadline := c.Deadline(); currentBlock <= deadline {
		return fmt.Errorf("%w: the deadline is block %d and the chain is at %d",
			ErrWindowStillOpen, deadline, currentBlock)
	}
	return nil
}

// VerifySecret checks a secret against the anchored commitment.
//
// # Why this is checked locally and not left to the contract
//
// The contract does check it, and reverts with CommitmentMismatch. But with a derived secret there is a
// specific way to reach that revert which deserves a better message: the pepper changed. Rotate the KMS
// key between the commit and the reveal and the derivation produces a different secret for the same
// offer, the commitment no longer matches, and the ceremony is unrecoverable through the normal path
// because the commitment is immutable.
//
// That is the cost of deriving rather than storing, and it is worth naming at the point of failure
// rather than leaving an operator to work backwards from a Solidity selector.
func (c *Ceremony) VerifySecret(secret merkle.Hash) error {
	if c.Commitment.IsZero() {
		return errors.New("ballotrun: no commitment is recorded to check the secret against")
	}
	got := ballot.Commitment(secret)
	if got != c.Commitment {
		return fmt.Errorf("%w: the secret hashes to %s, the anchored commitment is %s. If the secret "+
			"is derived, the most likely cause is that the anchor pepper was rotated after the "+
			"commitment was anchored",
			ErrCommitmentMismatch, got.Hex(), c.Commitment.Hex())
	}
	return nil
}

// FinalSeed derives the seed from the revealed secret and the observed blockhash.
func (c *Ceremony) FinalSeed(secret, targetBlockHash merkle.Hash) (merkle.Hash, error) {
	if err := c.VerifySecret(secret); err != nil {
		return merkle.Hash{}, err
	}
	if targetBlockHash.IsZero() {
		return merkle.Hash{}, fmt.Errorf("%w: the hash of block %d read as zero",
			ErrBlockhashLost, c.TargetBlock)
	}
	if c.BidbookRoot.IsZero() {
		return merkle.Hash{}, ErrBookNotAnchored
	}
	return ballot.DeriveFinalSeed(secret, targetBlockHash, c.BidbookRoot), nil
}

// CanPersistPlaintext reports whether the revealed secret may be written to ballot_runs.
//
// Mirrors ballot_runs_enforce_reveal_order so the Go layer refuses before Postgres does. The trigger is
// the real enforcement; this exists so the failure names the ordering rule rather than surfacing as a
// constraint violation from a function the caller did not write.
func (c *Ceremony) CanPersistPlaintext() error {
	if c.Commitment.IsZero() {
		return errors.New("ballotrun: the seed plaintext cannot be stored before the commitment")
	}
	if !c.CommitmentAnchored {
		return errors.New("ballotrun: the seed plaintext cannot be stored before the commitment is " +
			"anchored on-chain")
	}
	if c.TargetBlock == 0 {
		return errors.New("ballotrun: the seed plaintext cannot be stored before a target block is set")
	}
	return nil
}
