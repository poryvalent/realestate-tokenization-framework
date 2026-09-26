// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {AcreSyncRoles} from "./AcreSyncRoles.sol";
import {BallotEncoding} from "./BallotEncoding.sol";
import {MerkleLib} from "./MerkleLib.sol";

/// @title AcreSyncBallot
/// @notice Commit-reveal ceremony and bid book anchor for an oversubscribed SM-REIT offer.
///
/// @dev Split from AcreSyncScheme to keep both contracts under the EIP-170 limit and to
/// keep the draw auditable in isolation. References are one-directional: the scheme reads
/// this contract, this contract knows nothing about the scheme.
///
/// # Why a ballot needs a ceremony at all
///
/// A unit costs ten lakh rupees, so a fifty crore scheme is 500 units with 475 public
/// after the manager's 5%. The scheme also needs 200 distinct unitholders. When 900
/// investors each apply for one unit there is no proportional answer: somebody gets zero.
/// Who gets zero has to be decided by something nobody can steer, and has to be checkable
/// afterwards by the people who lost.
///
/// # The ceremony
///
/// 1. Freeze the book and anchor its Merkle root.
/// 2. Commit sha256(secret). The contract, not the caller, sets the target block.
/// 3. Wait for the target block to pass.
/// 4. Reveal the secret. finalSeed = sha256(domain || secret || blockhash(target) || bidbookRoot).
///
/// Each input closes a different attack. The committed secret proves the operator's intent
/// predated the draw. The target block hash is unknown to everyone at commit time, so the
/// operator cannot choose the outcome. The bid book root binds the seed to one exact set of
/// bids, so inserting or removing a bid after the fact invalidates the draw rather than
/// quietly changing it.
///
/// # The abandonment vector, and why the cap is three
///
/// `blockhash` is only available for the previous 256 blocks. If the reveal window lapses,
/// the operator must be able to recommit, otherwise a missed transaction strands the offer
/// permanently. But a free recommit is a grinding attack: an operator who dislikes a draw
/// simply lets the window expire and rolls again.
///
/// Two things bound it. `recommitSeed` takes no commitment argument, so the stored
/// commitment is reused and only the target block changes; the secret is fixed forever
/// after the first commit, so abandonment can reroll the blockhash but never the secret.
/// And every expiry is publicly counted on-chain, with attempts capped at three, after
/// which the draw requires trustee escalation.
///
/// This does not reduce the manipulation surface to zero. An operator colluding with a
/// block proposer who withholds at the target block retains some influence. That residual
/// risk is stated rather than papered over: on a testnet demo it is negligible, and on
/// mainnet it would warrant a VRF or a longer delay.
contract AcreSyncBallot {
    // ---------------------------------------------------------------------
    // Types
    // ---------------------------------------------------------------------

    enum Stage {
        None,
        BidbookAnchored,
        SeedCommitted,
        SeedRevealed,
        ResultAnchored,
        Escalated
    }

    // ---------------------------------------------------------------------
    // Immutables
    // ---------------------------------------------------------------------

    AcreSyncRoles public immutable roles;

    /// @notice Blocks between the commit and the target block whose hash feeds the seed.
    /// @dev Any positive value denies the operator knowledge of the hash at commit time.
    /// Ten blocks is roughly two minutes on Sepolia, long enough that a proposer cannot
    /// casually line up the target and short enough to demonstrate live.
    uint32 public immutable seedDelayBlocks;

    /// @notice Blocks after the target within which the reveal must land.
    /// @dev Hard-bounded by the EVM: `blockhash` returns zero beyond 256 blocks, so a
    /// reveal after that is impossible regardless of what this is set to. Kept below 256 to
    /// leave margin.
    uint32 public immutable seedRevealWindowBlocks;

    /// @notice Maximum commit attempts before trustee escalation is required.
    uint8 public immutable maxSeedAttempts;

    // ---------------------------------------------------------------------
    // State
    // ---------------------------------------------------------------------

    Stage public stage;

    bytes32 public bidbookRoot;
    bytes32 public bidbookCidDigest;
    uint32 public bidLeafCount;
    uint32 public totalUnitsBid;
    uint32 public distinctBidders;

    bytes32 public seedCommitment;
    uint64 public targetBlock;
    uint8 public attempt;

    bytes32 public revealedSecret;
    bytes32 public targetBlockHash;
    bytes32 public finalSeed;

    bytes32 public resultRoot;
    bytes32 public resultCidDigest;
    uint32 public unitsAllotted;
    uint32 public distinctAllottees;
    uint32 public algoVersion;

    /// @notice Replay guard, mirroring the orchestrator's unique index on idempotency_key.
    /// @dev Two layers rather than one: the database catches a retry before broadcast, and
    /// this catches anything that reaches the contract by another path.
    mapping(bytes32 key => bool used) public usedKey;

    // ---------------------------------------------------------------------
    // Events
    // ---------------------------------------------------------------------

    event BidbookAnchored(
        bytes32 indexed root,
        bytes32 cidDigest,
        uint32 leafCount,
        uint32 totalUnitsBid,
        uint32 distinctBidders
    );
    event SeedCommitted(bytes32 indexed commitment, uint64 targetBlock, uint8 attempt);
    event SeedWindowExpired(uint64 previousTarget, uint8 attempt);
    event SeedRevealed(bytes32 secret, bytes32 targetBlockHash, bytes32 indexed finalSeed);
    event BallotEscalated(address trustee, uint8 attempts);
    event BallotResultAnchored(
        bytes32 indexed root, bytes32 cidDigest, uint32 unitsAllotted, uint32 distinctAllottees
    );

    // ---------------------------------------------------------------------
    // Errors
    // ---------------------------------------------------------------------

    error NotRelayer(address caller);
    error NotTrustee(address caller);
    error ContractPaused();
    error KeyAlreadyUsed(bytes32 key);
    error WrongStage(Stage expected, Stage actual);
    error ZeroValue();
    error TargetBlockNotReached(uint64 target, uint64 current);
    error RevealWindowExpired(uint64 deadline, uint64 current);
    error BlockhashUnavailable(uint64 target);
    error CommitmentMismatch(bytes32 expected, bytes32 actual);
    error WindowStillOpen(uint64 deadline, uint64 current);
    error MaxSeedAttemptsReached(uint8 max);
    error AllotteeFloorNotMet(uint32 required, uint32 actual);
    error UnitsExceedOffer(uint32 allotted, uint32 onOffer);
    error InvalidCounts();

    // ---------------------------------------------------------------------
    // Modifiers
    // ---------------------------------------------------------------------

    modifier onlyRelayer() {
        if (!roles.isRelayer(msg.sender)) revert NotRelayer(msg.sender);
        _;
    }

    modifier notPaused() {
        if (roles.paused()) revert ContractPaused();
        _;
    }

    modifier fresh(bytes32 key) {
        if (usedKey[key]) revert KeyAlreadyUsed(key);
        usedKey[key] = true;
        _;
    }

    // ---------------------------------------------------------------------
    // Block number
    // ---------------------------------------------------------------------

    /// @dev Block numbers are stored and compared as uint64 to keep the struct packed.
    ///
    /// Narrowing from uint256 is safe by an enormous margin: uint64 holds about 1.8e19
    /// blocks, and at twelve seconds a block Ethereum reaches that in roughly seven
    /// trillion years. The cast is funnelled through one helper so the justification is
    /// written once rather than repeated at six call sites, where it would inevitably be
    /// copied without being re-read.
    // forge-lint: disable-next-line(unsafe-typecast)
    function _blockNumber() private view returns (uint64) {
        // forge-lint: disable-next-line(unsafe-typecast)
        return uint64(block.number);
    }

    // ---------------------------------------------------------------------
    // Construction
    // ---------------------------------------------------------------------

    constructor(
        AcreSyncRoles rolesContract,
        uint32 delayBlocks,
        uint32 revealWindowBlocks,
        uint8 attemptCap
    ) {
        if (address(rolesContract) == address(0)) revert ZeroValue();
        if (delayBlocks == 0 || revealWindowBlocks == 0 || attemptCap == 0) revert ZeroValue();
        // Beyond 255 blocks past the target, blockhash returns zero and no reveal can
        // succeed. Rejecting the configuration is better than accepting a window that is
        // silently shorter than it claims.
        if (revealWindowBlocks > 250) revert ZeroValue();

        roles = rolesContract;
        seedDelayBlocks = delayBlocks;
        seedRevealWindowBlocks = revealWindowBlocks;
        maxSeedAttempts = attemptCap;
    }

    // ---------------------------------------------------------------------
    // Step 1: anchor the frozen bid book
    // ---------------------------------------------------------------------

    /// @notice Anchors the frozen bid book before any seed exists.
    ///
    /// @dev Ordering is the security property. Once this root is recorded the set of bids is
    /// fixed, and because the root feeds the seed, a later edit to the book produces a
    /// different seed and a visibly different draw.
    function anchorBidbook(
        bytes32 root,
        bytes32 cidDigest,
        uint32 leafCount,
        uint32 unitsBid,
        uint32 bidders,
        bytes32 idempotencyKey
    ) external onlyRelayer notPaused fresh(idempotencyKey) {
        if (stage != Stage.None) revert WrongStage(Stage.None, stage);
        if (root == bytes32(0) || cidDigest == bytes32(0)) revert ZeroValue();
        if (leafCount == 0 || bidders == 0 || unitsBid == 0) revert ZeroValue();
        // One bid per investor per offer, enforced upstream by a unique index. More
        // bidders than leaves would mean the book is malformed.
        if (bidders > leafCount) revert InvalidCounts();

        bidbookRoot = root;
        bidbookCidDigest = cidDigest;
        bidLeafCount = leafCount;
        totalUnitsBid = unitsBid;
        distinctBidders = bidders;
        stage = Stage.BidbookAnchored;

        emit BidbookAnchored(root, cidDigest, leafCount, unitsBid, bidders);
    }

    // ---------------------------------------------------------------------
    // Step 2: commit
    // ---------------------------------------------------------------------

    /// @notice Commits sha256(secret) and fixes the target block.
    ///
    /// @dev The caller supplies no block number. If it could, it would pick one whose hash
    /// it had reason to prefer, and the ceremony would prove nothing.
    function commitSeed(bytes32 commitment, bytes32 idempotencyKey)
        external
        onlyRelayer
        notPaused
        fresh(idempotencyKey)
    {
        if (stage != Stage.BidbookAnchored) revert WrongStage(Stage.BidbookAnchored, stage);
        if (commitment == bytes32(0)) revert ZeroValue();

        seedCommitment = commitment;
        targetBlock = _blockNumber() + seedDelayBlocks;
        attempt = 1;
        stage = Stage.SeedCommitted;

        emit SeedCommitted(commitment, targetBlock, attempt);
    }

    // ---------------------------------------------------------------------
    // Step 3: reveal
    // ---------------------------------------------------------------------

    /// @notice Reveals the secret and derives the final seed.
    function revealSeed(bytes32 secret, bytes32 idempotencyKey)
        external
        onlyRelayer
        notPaused
        fresh(idempotencyKey)
    {
        if (stage != Stage.SeedCommitted) revert WrongStage(Stage.SeedCommitted, stage);

        uint64 current = _blockNumber();
        if (current <= targetBlock) revert TargetBlockNotReached(targetBlock, current);

        uint64 deadline = targetBlock + seedRevealWindowBlocks;
        if (current > deadline) revert RevealWindowExpired(deadline, current);

        bytes32 observed = blockhash(targetBlock);
        // Defence in depth. The window check above should make this unreachable, but a zero
        // blockhash silently folded into the seed would produce a draw that looked valid
        // and was derived from nothing.
        if (observed == bytes32(0)) revert BlockhashUnavailable(targetBlock);

        bytes32 computed = BallotEncoding.commitmentOf(secret);
        if (computed != seedCommitment) revert CommitmentMismatch(seedCommitment, computed);

        revealedSecret = secret;
        targetBlockHash = observed;
        finalSeed = BallotEncoding.finalSeed(secret, observed, bidbookRoot);
        stage = Stage.SeedRevealed;

        emit SeedRevealed(secret, observed, finalSeed);
    }

    // ---------------------------------------------------------------------
    // The expiry path
    // ---------------------------------------------------------------------

    /// @notice Sets a fresh target block after a lapsed reveal window.
    ///
    /// @dev Takes no commitment argument, and that absence is the whole point. The stored
    /// commitment is reused, so the secret is fixed from the first commit onward and an
    /// operator who abandons a window can reroll only the blockhash. Every expiry is
    /// counted and emitted, so the attempts are public.
    function recommitSeed(bytes32 idempotencyKey)
        external
        onlyRelayer
        notPaused
        fresh(idempotencyKey)
    {
        if (stage != Stage.SeedCommitted) revert WrongStage(Stage.SeedCommitted, stage);

        uint64 current = _blockNumber();
        uint64 deadline = targetBlock + seedRevealWindowBlocks;
        // Recommitting while a reveal is still possible would let an operator skip a draw
        // they could see coming.
        if (current <= deadline) revert WindowStillOpen(deadline, current);

        if (attempt >= maxSeedAttempts) revert MaxSeedAttemptsReached(maxSeedAttempts);

        emit SeedWindowExpired(targetBlock, attempt);

        targetBlock = current + seedDelayBlocks;
        attempt += 1;

        emit SeedCommitted(seedCommitment, targetBlock, attempt);
    }

    /// @notice Escalates a ballot that has exhausted its commit attempts.
    /// @dev Trustee only. After three public expiries the operator loses the ability to keep
    /// rolling, and a second party has to put their name to whatever happens next.
    function escalateBallot() external {
        if (!roles.isTrustee(msg.sender)) revert NotTrustee(msg.sender);
        if (stage != Stage.SeedCommitted) revert WrongStage(Stage.SeedCommitted, stage);

        if (attempt < maxSeedAttempts) revert MaxSeedAttemptsReached(maxSeedAttempts);

        uint64 deadline = targetBlock + seedRevealWindowBlocks;
        if (_blockNumber() <= deadline) {
            revert WindowStillOpen(deadline, _blockNumber());
        }

        stage = Stage.Escalated;
        emit BallotEscalated(msg.sender, attempt);
    }

    // ---------------------------------------------------------------------
    // Step 4: anchor the result
    // ---------------------------------------------------------------------

    /// @notice Anchors the allotment produced off-chain from the revealed seed.
    ///
    /// @dev The contract does not run the allocation. It could not usefully: the algorithm
    /// needs sorting and a water-filling loop over hundreds of bids, which is neither cheap
    /// nor necessary on-chain. What matters is that the inputs are pinned before the draw
    /// and the output is pinned after, so anyone can recompute the allocation and compare
    /// this root.
    function anchorBallotResult(
        bytes32 root,
        bytes32 cidDigest,
        uint32 allotted,
        uint32 allottees,
        uint32 unitsOnOffer,
        uint32 minDistinctHolders,
        uint32 algo,
        bytes32 idempotencyKey
    ) external onlyRelayer notPaused fresh(idempotencyKey) {
        if (stage != Stage.SeedRevealed && stage != Stage.Escalated) {
            revert WrongStage(Stage.SeedRevealed, stage);
        }
        if (root == bytes32(0) || cidDigest == bytes32(0)) revert ZeroValue();
        if (allotted == 0 || allottees == 0) revert ZeroValue();
        if (allotted > unitsOnOffer) revert UnitsExceedOffer(allotted, unitsOnOffer);
        // The statutory floor, checked here as well as at settlement. An allotment that
        // cannot reach 200 unitholders produces a scheme that cannot list, and catching it
        // at the anchor is cheaper than catching it three transactions later.
        if (allottees < minDistinctHolders) {
            revert AllotteeFloorNotMet(minDistinctHolders, allottees);
        }
        // Every holder needs at least one whole unit, so allottees can never exceed units.
        if (allottees > allotted) revert InvalidCounts();

        resultRoot = root;
        resultCidDigest = cidDigest;
        unitsAllotted = allotted;
        distinctAllottees = allottees;
        algoVersion = algo;
        stage = Stage.ResultAnchored;

        emit BallotResultAnchored(root, cidDigest, allotted, allottees);
    }

    // ---------------------------------------------------------------------
    // Public verification
    // ---------------------------------------------------------------------

    /// @notice Confirms a bid was in the frozen book the draw consumed.
    /// @dev Callable by anyone from a block explorer. Without this the bid book root is a
    /// number in a log; with it, an investor can prove their own bid was included.
    function verifyBidInclusion(
        uint32 leafIndex,
        bytes16 bidRef,
        bytes32 investorAnchor,
        uint32 unitsBid,
        uint64 pricePerUnitPaise,
        bytes32[] calldata proof
    ) external view returns (bool) {
        if (bidbookRoot == bytes32(0)) return false;
        bytes32 leaf =
            BallotEncoding.bidLeaf(leafIndex, bidRef, investorAnchor, unitsBid, pricePerUnitPaise);
        return MerkleLib.verify(bidbookRoot, leaf, proof);
    }

    /// @notice Confirms an allotment matches the anchored result.
    function verifyAllotment(
        uint32 leafIndex,
        bytes32 investorAnchor,
        uint32 allotted,
        uint8 outcomeCode,
        uint32 ballotRank,
        bytes32[] calldata proof
    ) external view returns (bool) {
        if (resultRoot == bytes32(0)) return false;
        bytes32 leaf = BallotEncoding.allotmentLeaf(
            leafIndex, investorAnchor, allotted, outcomeCode, ballotRank
        );
        return MerkleLib.verify(resultRoot, leaf, proof);
    }

    /// @notice Recomputes a bid's ballot ranking key.
    /// @dev The mechanism by which a losing bidder checks they were treated the same as
    /// everyone else: one hash, compared against the published rank.
    function rankKeyOf(bytes32 investorAnchor, uint32 leafIndex) external view returns (bytes32) {
        return BallotEncoding.rankKey(finalSeed, investorAnchor, leafIndex);
    }

    /// @notice Computes the commitment for a candidate secret.
    ///
    /// @dev Exposed so an operator can confirm a secret matches the stored commitment before
    /// spending gas on a reveal that would revert. It discloses nothing: the caller already
    /// holds the secret they are passing in, and the function is pure.
    ///
    /// It also gives the ceremony runbook an authoritative source for the derivation rather
    /// than reimplementing sha256 framing in a shell script, which is exactly the kind of
    /// duplication that drifts.
    function commitmentFor(bytes32 secret) external pure returns (bytes32) {
        return BallotEncoding.commitmentOf(secret);
    }

    /// @notice Whether a candidate secret opens the stored commitment.
    function secretMatchesCommitment(bytes32 secret) external view returns (bool) {
        return seedCommitment != bytes32(0) && BallotEncoding.commitmentOf(secret) == seedCommitment;
    }

    /// @notice Whether a reveal is currently possible, and the window bounds.
    function revealWindow()
        external
        view
        returns (bool open, uint64 target, uint64 deadline, uint64 current)
    {
        target = targetBlock;
        deadline = targetBlock + seedRevealWindowBlocks;
        current = _blockNumber();
        open = stage == Stage.SeedCommitted && current > target && current <= deadline;
    }
}
