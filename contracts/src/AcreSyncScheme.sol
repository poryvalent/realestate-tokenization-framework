// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {AcreSyncRoles} from "./AcreSyncRoles.sol";
import {AcreSyncBallot} from "./AcreSyncBallot.sol";
import {MerkleLib} from "./MerkleLib.sol";

/// @title AcreSyncScheme
/// @notice The shadow ledger for one SM-REIT scheme: lifecycle, unitholder register,
/// settlement, and the distribution attestation.
///
/// @dev This contract is not the legal register of ownership. The depository is. Units are
/// held in dematerialised form and the Demat account is what a court would look at. Every
/// function here records a fact that has already happened somewhere else, which is why
/// there is no `transfer`: a user-initiated on-chain movement would desync the mirror from
/// the register that actually counts, and the register would win.
///
/// What the contract does provide is the thing an off-chain database cannot: an append-only,
/// independently verifiable record that the 95% distribution floor was met, that the
/// 200-unitholder floor was met, and that the allotment came from a draw nobody could steer.
///
/// # Deliberately a partial ERC20
///
/// `balanceOf`, `totalSupply`, `decimals`, `name` and `symbol` exist so wallets and
/// explorers render holdings. `transfer`, `approve`, `transferFrom` and `allowance` are
/// absent entirely rather than present and reverting. Omission means there is no function
/// selector for an aggregator to call, so a transfer is impossible by construction rather
/// than by guard. Standard `Transfer` events are still emitted, which is what lets Etherscan
/// index holders.
///
/// Tooling will report this as a non-compliant ERC20. That is accurate, and it is the point.
///
/// # Money on chain
///
/// Amounts are INR paise as integers. There is no ETH here and no token with value. The
/// contract attests to amounts; the orchestrator moves them through banking rails.
contract AcreSyncScheme {
    // ---------------------------------------------------------------------
    // Types
    // ---------------------------------------------------------------------

    enum Status {
        Draft,
        SpvFormed,
        Valued,
        Filed,
        OfferApproved,
        OfferOpen,
        OfferClosed,
        Allocated,
        Settled,
        Listed,
        Operational,
        Undersubscribed,
        Refunding,
        Aborted,
        WindDown,
        Dissolved
    }

    enum DocType {
        OfferDocument,
        Kis,
        TrustDeed,
        ImAgreement,
        Valuation,
        TrusteeEscrow,
        SpvControl,
        AllotmentFile,
        NdcfStatement,
        Snapshot,
        Bidbook
    }

    enum PeriodStatus {
        None,
        Anchored,
        EntitlementsAnchored,
        PayoutsConfirmed,
        Closed,
        Reversed
    }

    enum SettlementStage {
        NotStarted,
        InProgress,
        Finalised
    }

    /// @dev Mirrors the holding_entry_type Postgres enum.
    enum HoldingReason {
        Allotment,
        ImSubscription,
        DepositoryTransfer,
        Transmission,
        Correction,
        Reversal
    }

    struct Period {
        uint64 recordDate;
        uint64 ndcfPaise;
        uint64 distributedPaise;
        uint32 snapshotTotalUnits;
        uint32 supersedes;
        uint32 entitledUnitsAccrued;
        uint32 entitledHoldersAccrued;
        uint16 bps;
        uint16 snapshotHolders;
        PeriodStatus status;
        bytes32 statementHash;
        bytes32 snapshotRoot;
        bytes32 ndcfCidDigest;
    }

    struct ReversalApproval {
        address trustee;
        uint8 reason;
        bool present;
        bytes32 narrativeHash;
    }

    /// @notice Inputs to anchorPeriod, grouped into a struct.
    ///
    /// @dev Not cosmetic. Passed as eleven separate parameters this function exhausts the
    /// EVM's addressable stack slots and will not compile without via-ir. A calldata struct
    /// keeps the values in calldata and reachable by offset, which is both compilable and
    /// cheaper. It also makes the call site self-documenting, and eleven positional bytes32
    /// and uint64 arguments is exactly the shape where a transposed pair goes unnoticed.
    struct PeriodAnchorInput {
        uint32 periodId;
        uint64 recordDate;
        uint64 ndcfPaise;
        uint64 distributedPaise;
        uint32 snapshotTotalUnits;
        uint16 snapshotHolders;
        uint32 supersedes;
        bytes32 statementHash;
        bytes32 snapshotRoot;
        bytes32 ndcfCidDigest;
    }

    struct SettlementState {
        SettlementStage stage;
        uint32 expectedHolders;
        uint32 expectedUnits;
        uint32 creditedHolders;
        uint32 creditedUnits;
        bytes32 allotmentFileHash;
        bytes32 ballotResultRoot;
    }

    // ---------------------------------------------------------------------
    // Immutables
    // ---------------------------------------------------------------------

    AcreSyncRoles public immutable roles;
    AcreSyncBallot public immutable ballot;

    /// @notice SEBI scheme registration reference. Public record, stored in clear.
    bytes32 public immutable sebiSchemeRef;

    /// @notice Issue price per unit in paise. At least ten lakh rupees by regulation.
    uint64 public immutable unitPricePaise;

    uint32 public immutable totalUnits;
    uint32 public immutable imUnits;
    uint32 public immutable publicUnits;
    uint16 public immutable minPublicHolders;

    /// @notice Minimum share of NDCF that must be distributed, in basis points.
    uint16 public immutable distributionFloorBps;

    /// @notice Hard ceiling on batch size.
    /// @dev Prevents an accidentally oversized batch running out of gas and wedging the
    /// settlement cursor mid-way.
    uint32 public immutable maxBatchSize;

    bool public immutable isSimulation;
    uint8 public immutable environmentTag;

    // ---------------------------------------------------------------------
    // State
    // ---------------------------------------------------------------------

    Status public status;

    mapping(uint8 docType => bytes32 digest) public docAnchor;

    mapping(address holder => uint32 units) public unitsOf;
    mapping(address holder => bool excluded) public excludedFromCount;
    mapping(address holder => uint64 expiry) public lockInExpiry;

    address[] private _holders;
    mapping(address holder => uint256 indexPlusOne) private _holderPos;

    uint32 public totalUnitsIssued;
    uint16 public distinctHolderCount;

    /// @notice The investment manager's wallet, recorded at subscription.
    address public imWallet;

    SettlementState public settlement;

    mapping(uint32 periodId => Period) private _periods;
    mapping(uint32 periodId => ReversalApproval) private _reversalApproval;
    uint32 public latestPeriodId;

    mapping(bytes32 key => bool used) public usedKey;

    // ---------------------------------------------------------------------
    // Events
    // ---------------------------------------------------------------------

    event StatusChanged(Status indexed from, Status indexed to);
    event DocAnchored(uint8 indexed docType, bytes32 digest);
    event DocAnchorSuperseded(uint8 indexed docType, bytes32 oldDigest, bytes32 newDigest, uint8 reason, bytes32 narrativeHash);

    event HoldingChanged(
        address indexed holder,
        int64 delta,
        uint32 balanceAfter,
        HoldingReason indexed reason,
        bytes32 depositoryRef
    );
    /// @dev Standard ERC20 event, emitted so explorers index holders even though no transfer
    /// function exists.
    event Transfer(address indexed from, address indexed to, uint256 value);
    event LockInSet(address indexed holder, uint64 expiry);

    event SettlementBegun(uint32 expectedHolders, uint32 expectedUnits, bytes32 allotmentFileHash);
    event SettlementBatchApplied(uint32 cursorFrom, uint32 cursorTo, uint32 unitsInBatch);
    event SettlementFinalised(uint32 totalUnitsIssued, uint16 distinctHolderCount, address finalisedBy);

    event PeriodAnchored(
        uint32 indexed periodId,
        uint64 recordDate,
        uint64 ndcfPaise,
        uint64 distributedPaise,
        uint16 bps,
        bytes32 statementHash,
        bytes32 snapshotRoot,
        uint32 supersedes
    );
    /// @dev A dedicated event so the compliance claim is one indexed log lookup rather than a
    /// struct read.
    event DistributionFloorSatisfied(uint32 indexed periodId, uint16 bps, uint16 floorBps);
    event EntitlementAccrued(uint32 indexed periodId, address indexed holder, uint32 units, uint32 snapshotTotalUnits);
    event EntitlementsFinalised(uint32 indexed periodId, uint32 holders, uint32 units);
    event PayoutsConfirmed(uint32 indexed periodId, uint16 settled, uint16 failed, bytes32 reportHash);
    event PeriodClosed(uint32 indexed periodId);

    event ReversalApproved(uint32 indexed periodId, uint8 reason, bytes32 narrativeHash, address trustee);
    event PeriodReversed(uint32 indexed periodId, uint8 reason, bytes32 narrativeHash, address approvedBy);
    event PayoutAdjustmentRecorded(uint32 indexed sourcePeriodId, uint32 indexed targetPeriodId, address indexed holder, uint8 direction, uint8 reason);

    // ---------------------------------------------------------------------
    // Errors
    // ---------------------------------------------------------------------

    error NotRelayer(address caller);
    error NotTrustee(address caller);
    error ContractPaused();
    error KeyAlreadyUsed(bytes32 key);
    error ZeroValue();
    error InvalidTransition(Status from, Status to);
    error WrongStatus(Status expected, Status actual);
    error DocAnchorAlreadySet(uint8 docType);
    error DocAnchorMissing(uint8 docType);

    error InsufficientUnits(address holder, uint32 have, uint32 want);
    error UnitsExceedTotal(uint32 attempted, uint32 cap);
    error LockInActive(address holder, uint64 until);
    error SelfTransfer();

    error SettlementNotStarted();
    error SettlementAlreadyFinalised();
    error CursorMismatch(uint32 expected, uint32 got);
    error ArrayLengthMismatch();
    error EmptyBatch();
    error BatchTooLarge(uint32 max);
    error SettlementUnitsMismatch(uint32 expected, uint32 actual);
    error SettlementHoldersMismatch(uint32 expected, uint32 actual);
    error HolderFloorNotMet(uint16 required, uint16 actual);
    error TotalUnitsMismatch(uint32 required, uint32 actual);
    error ImHoldingMismatch(uint32 required, uint32 actual);
    error BallotNotAnchored();

    error PeriodAlreadyAnchored(uint32 periodId);
    error PeriodNotAnchored(uint32 periodId);
    error ZeroNdcf();
    error DistributedExceedsNdcf(uint64 distributed, uint64 ndcf);
    error DistributionFloorNotMet(uint16 actual, uint16 required);
    error SnapshotUnitsMismatch(uint32 expected, uint32 actual);
    error SnapshotHolderFloorNotMet(uint16 required, uint16 actual);
    error EntitlementUnitsMismatch(uint32 expected, uint32 actual);
    error PeriodNotReversible(uint32 periodId, PeriodStatus status);
    error ReversalNotApproved(uint32 periodId);
    error ReversalApprovalMismatch();
    error PayoutsAlreadyConfirmed(uint32 periodId);

    // ---------------------------------------------------------------------
    // Modifiers
    // ---------------------------------------------------------------------

    modifier onlyRelayer() {
        if (!roles.isRelayer(msg.sender)) revert NotRelayer(msg.sender);
        _;
    }

    /// @dev Applied to lifecycle, settlement and distribution. Deliberately NOT applied to
    /// reconciliation or reversal: the depository is the legal register, and blinding the
    /// mirror during an emergency compounds the problem rather than containing it.
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
    // Construction
    // ---------------------------------------------------------------------

    constructor(
        AcreSyncRoles rolesContract,
        AcreSyncBallot ballotContract,
        bytes32 schemeRef,
        uint64 pricePaise,
        uint32 total,
        uint32 imAllocation,
        uint16 holderFloor,
        uint16 floorBps,
        uint32 batchCap
    ) {
        if (address(rolesContract) == address(0) || address(ballotContract) == address(0)) {
            revert ZeroValue();
        }
        if (schemeRef == bytes32(0) || total == 0 || batchCap == 0) revert ZeroValue();
        // The statutory minimum unit price: ten lakh rupees expressed in paise.
        if (pricePaise < 100_000_000) revert ZeroValue();
        if (imAllocation >= total) revert ZeroValue();
        if (holderFloor == 0 || uint32(holderFloor) > total - imAllocation) revert ZeroValue();
        if (floorBps == 0 || floorBps > 10_000) revert ZeroValue();

        roles = rolesContract;
        ballot = ballotContract;
        sebiSchemeRef = schemeRef;
        unitPricePaise = pricePaise;
        totalUnits = total;
        imUnits = imAllocation;
        publicUnits = total - imAllocation;
        minPublicHolders = holderFloor;
        distributionFloorBps = floorBps;
        maxBatchSize = batchCap;

        isSimulation = rolesContract.isSimulation();
        environmentTag = rolesContract.environmentTag();
    }

    // ---------------------------------------------------------------------
    // Partial ERC20 read surface
    // ---------------------------------------------------------------------

    function name() external pure returns (string memory) {
        return "AcreSync Scheme Unit";
    }

    function symbol() external pure returns (string memory) {
        return "ACRE-U";
    }

    /// @notice Always zero.
    /// @dev Units are indivisible. A Demat account cannot hold a fraction of a unit, so a
    /// non-zero decimals value would let the ledger represent a state that cannot legally
    /// exist.
    function decimals() external pure returns (uint8) {
        return 0;
    }

    function totalSupply() external view returns (uint256) {
        return totalUnitsIssued;
    }

    function balanceOf(address holder) external view returns (uint256) {
        return unitsOf[holder];
    }

    // ---------------------------------------------------------------------
    // Lifecycle
    // ---------------------------------------------------------------------

    /// @notice Advances the scheme status.
    /// @dev Settled is unreachable here: only finaliseSettlement can set it, and only once
    /// the invariants hold.
    function advanceStatus(Status target, bytes32 idempotencyKey)
        external
        onlyRelayer
        notPaused
        fresh(idempotencyKey)
    {
        Status from = status;
        if (!_transitionAllowed(from, target)) revert InvalidTransition(from, target);

        if (target == Status.Valued && docAnchor[uint8(DocType.Valuation)] == bytes32(0)) {
            revert DocAnchorMissing(uint8(DocType.Valuation));
        }
        if (target == Status.OfferOpen) {
            if (docAnchor[uint8(DocType.TrusteeEscrow)] == bytes32(0)) {
                revert DocAnchorMissing(uint8(DocType.TrusteeEscrow));
            }
            if (docAnchor[uint8(DocType.OfferDocument)] == bytes32(0)) {
                revert DocAnchorMissing(uint8(DocType.OfferDocument));
            }
        }
        // Listing requires the manager's lock-in to be recorded, because the two-year hold
        // runs from listing and cannot be known before it.
        if (target == Status.Listed && (imWallet == address(0) || lockInExpiry[imWallet] == 0)) {
            revert ZeroValue();
        }

        status = target;
        emit StatusChanged(from, target);
    }

    function _transitionAllowed(Status from, Status to) private pure returns (bool) {
        if (to == Status.Settled) return false; // finaliseSettlement only

        if (from == Status.Draft) return to == Status.SpvFormed;
        if (from == Status.SpvFormed) return to == Status.Valued;
        if (from == Status.Valued) return to == Status.Filed;
        if (from == Status.Filed) return to == Status.OfferApproved;
        if (from == Status.OfferApproved) return to == Status.OfferOpen;
        if (from == Status.OfferOpen) return to == Status.OfferClosed;
        if (from == Status.OfferClosed) {
            // A scheme that fails its minimum subscription or its unitholder floor is a
            // normal outcome, not an error state.
            return to == Status.Allocated || to == Status.Undersubscribed;
        }
        if (from == Status.Undersubscribed) return to == Status.Refunding;
        if (from == Status.Refunding) return to == Status.Aborted;
        if (from == Status.Settled) return to == Status.Listed;
        if (from == Status.Listed) return to == Status.Operational;
        if (from == Status.Operational) return to == Status.WindDown;
        if (from == Status.WindDown) return to == Status.Dissolved;
        return false;
    }

    // ---------------------------------------------------------------------
    // Document anchors
    // ---------------------------------------------------------------------

    function setDocAnchor(uint8 docType, bytes32 digest, bytes32 idempotencyKey)
        external
        onlyRelayer
        notPaused
        fresh(idempotencyKey)
    {
        if (digest == bytes32(0)) revert ZeroValue();
        if (docAnchor[docType] != bytes32(0)) revert DocAnchorAlreadySet(docType);
        docAnchor[docType] = digest;
        emit DocAnchored(docType, digest);
    }

    /// @notice Replaces a document anchor, keeping the original visible in the log.
    /// @dev A restated valuation is a real occurrence. Overwriting silently would hide it;
    /// the superseding event keeps both versions discoverable.
    function supersedeDocAnchor(
        uint8 docType,
        bytes32 newDigest,
        uint8 reason,
        bytes32 narrativeHash,
        bytes32 idempotencyKey
    ) external onlyRelayer notPaused fresh(idempotencyKey) {
        bytes32 old = docAnchor[docType];
        if (old == bytes32(0)) revert DocAnchorMissing(docType);
        if (newDigest == bytes32(0) || narrativeHash == bytes32(0)) revert ZeroValue();

        docAnchor[docType] = newDigest;
        emit DocAnchorSuperseded(docType, old, newDigest, reason, narrativeHash);
    }

    // ---------------------------------------------------------------------
    // Register
    // ---------------------------------------------------------------------

    /// @notice Records the investment manager's subscription.
    /// @dev Called before the public ballot so the allocation engine starts from a clean
    /// public pool. The lock-in expiry is left unset because the two-year hold runs from
    /// listing, which has not happened yet.
    function recordImSubscription(address wallet, uint32 units, bytes32 idempotencyKey)
        external
        onlyRelayer
        notPaused
        fresh(idempotencyKey)
    {
        if (wallet == address(0)) revert ZeroValue();
        if (units != imUnits) revert ImHoldingMismatch(imUnits, units);
        if (imWallet != address(0)) revert ZeroValue();

        imWallet = wallet;
        excludedFromCount[wallet] = true;
        _setBalance(wallet, units, HoldingReason.ImSubscription, bytes32(0));
    }

    /// @notice Writes the manager's lock-in expiry at listing.
    function setLockIn(address holder, uint64 expiry, bytes32 idempotencyKey)
        external
        onlyRelayer
        notPaused
        fresh(idempotencyKey)
    {
        if (expiry == 0) revert ZeroValue();
        lockInExpiry[holder] = expiry;
        emit LockInSet(holder, expiry);
    }

    /// @notice Mirrors a transfer already effected at the depository.
    /// @dev Permitted while paused. Halting the sync would let the mirror drift further from
    /// the legal register, which is worse than the condition the pause is responding to.
    function reconcileTransfer(
        address from,
        address to,
        uint32 units,
        bytes32 depositoryRef,
        bytes32 idempotencyKey
    ) external onlyRelayer fresh(idempotencyKey) {
        if (from == to) revert SelfTransfer();
        if (to == address(0) || units == 0) revert ZeroValue();
        if (unitsOf[from] < units) revert InsufficientUnits(from, unitsOf[from], units);

        uint64 until = lockInExpiry[from];
        // A lock-in runs for two years. Validator timestamp drift is bounded at seconds, so
        // block.timestamp is a perfectly adequate clock for a comparison at that scale.
        // forge-lint: disable-next-line(block-timestamp)
        if (until != 0 && block.timestamp < until) revert LockInActive(from, until);

        _setBalance(from, unitsOf[from] - units, HoldingReason.DepositoryTransfer, depositoryRef);
        _setBalance(to, unitsOf[to] + units, HoldingReason.DepositoryTransfer, depositoryRef);
        emit Transfer(from, to, units);
    }

    /// @notice Mirrors a transmission on death or succession.
    /// @dev Bypasses the lock-in check. A transmission is not a voluntary disposal, so a
    /// lock-in intended to prevent the manager selling must not trap an estate.
    function reconcileTransmission(
        address from,
        address to,
        uint32 units,
        bytes32 depositoryRef,
        bytes32 idempotencyKey
    ) external onlyRelayer fresh(idempotencyKey) {
        if (from == to) revert SelfTransfer();
        if (to == address(0) || units == 0) revert ZeroValue();
        if (unitsOf[from] < units) revert InsufficientUnits(from, unitsOf[from], units);

        _setBalance(from, unitsOf[from] - units, HoldingReason.Transmission, depositoryRef);
        _setBalance(to, unitsOf[to] + units, HoldingReason.Transmission, depositoryRef);
        emit Transfer(from, to, units);
    }

    /// @notice Corrects a holding, with a mandatory reason and narrative digest.
    function correctHolding(
        address holder,
        uint32 newUnits,
        uint8 reason,
        bytes32 narrativeHash,
        bytes32 idempotencyKey
    ) external onlyRelayer fresh(idempotencyKey) {
        if (holder == address(0)) revert ZeroValue();
        if (narrativeHash == bytes32(0)) revert ZeroValue();

        uint32 previous = unitsOf[holder];
        _setBalance(holder, newUnits, HoldingReason.Correction, narrativeHash);

        if (newUnits > previous) {
            emit Transfer(address(0), holder, newUnits - previous);
        } else if (previous > newUnits) {
            emit Transfer(holder, address(0), previous - newUnits);
        }
        reason; // recorded via HoldingChanged
    }

    /// @dev Single point of truth for every derived register value. Keeping the holder array,
    /// the distinct count and the issued total in one place means they cannot drift apart,
    /// which matters because finaliseSettlement checks them against each other.
    function _setBalance(
        address holder,
        uint32 newUnits,
        HoldingReason reason,
        bytes32 ref
    ) private {
        uint32 previous = unitsOf[holder];
        if (newUnits == previous) return;

        if (newUnits > previous) {
            uint32 increase = newUnits - previous;
            // Reached from inside the settlement loop. Reverting the whole transaction is
            // correct: issuing more than the scheme's fixed unit count is not something to
            // partially apply, and the cursor must not advance past a batch that breached it.
            if (totalUnitsIssued + increase > totalUnits) {
                // forge-lint: disable-next-line(require-revert-in-loop)
                revert UnitsExceedTotal(totalUnitsIssued + increase, totalUnits);
            }
            totalUnitsIssued += increase;
        } else {
            totalUnitsIssued -= (previous - newUnits);
        }

        bool countable = !excludedFromCount[holder];
        if (previous == 0 && newUnits > 0) {
            _holders.push(holder);
            _holderPos[holder] = _holders.length;
            if (countable) distinctHolderCount += 1;
        } else if (previous > 0 && newUnits == 0) {
            _removeHolder(holder);
            if (countable) distinctHolderCount -= 1;
        }

        unitsOf[holder] = newUnits;

        // Both operands are uint32 widened to uint64 before the signed subtraction, so the
        // difference cannot overflow int64 in either direction.
        // forge-lint: disable-next-line(unsafe-typecast)
        int64 delta = int64(uint64(newUnits)) - int64(uint64(previous));
        emit HoldingChanged(holder, delta, newUnits, reason, ref);
    }

    function _removeHolder(address holder) private {
        uint256 pos = _holderPos[holder];
        if (pos == 0) return;
        uint256 index = pos - 1;
        uint256 last = _holders.length - 1;
        if (index != last) {
            address moved = _holders[last];
            _holders[index] = moved;
            _holderPos[moved] = index + 1;
        }
        _holders.pop();
        delete _holderPos[holder];
    }

    // ---------------------------------------------------------------------
    // Settlement
    // ---------------------------------------------------------------------

    /// @notice Opens settlement, binding it to the anchored ballot result.
    function beginSettlement(
        uint32 expectedHolders,
        uint32 expectedUnits,
        bytes32 allotmentFileHash,
        bytes32 idempotencyKey
    ) external onlyRelayer notPaused fresh(idempotencyKey) {
        if (status != Status.Allocated) revert WrongStatus(Status.Allocated, status);
        if (settlement.stage != SettlementStage.NotStarted) revert SettlementAlreadyFinalised();
        if (expectedHolders == 0 || expectedUnits == 0 || allotmentFileHash == bytes32(0)) {
            revert ZeroValue();
        }

        // Bind to the draw. Reading the result root here means this settlement is
        // permanently associated with one specific ballot outcome.
        bytes32 resultRoot = ballot.resultRoot();
        if (resultRoot == bytes32(0)) revert BallotNotAnchored();

        settlement = SettlementState({
            stage: SettlementStage.InProgress,
            expectedHolders: expectedHolders,
            expectedUnits: expectedUnits,
            creditedHolders: 0,
            creditedUnits: 0,
            allotmentFileHash: allotmentFileHash,
            ballotResultRoot: resultRoot
        });

        emit SettlementBegun(expectedHolders, expectedUnits, allotmentFileHash);
    }

    /// @notice Credits one chunk of allottees.
    ///
    /// @dev The strict cursor equality is the whole safety property. Batches cannot overlap,
    /// cannot skip, and cannot be reordered. A batch that ran out of gas did not advance the
    /// cursor, so it is retryable verbatim; a batch that succeeded cannot be replayed because
    /// both the idempotency key and the cursor would reject it.
    function settleBatch(
        address[] calldata holders_,
        uint32[] calldata units,
        uint32 cursorFrom,
        bytes32 idempotencyKey
    ) external onlyRelayer notPaused fresh(idempotencyKey) {
        if (settlement.stage != SettlementStage.InProgress) revert SettlementNotStarted();
        if (holders_.length != units.length) revert ArrayLengthMismatch();
        if (holders_.length == 0) revert EmptyBatch();
        if (holders_.length > maxBatchSize) revert BatchTooLarge(maxBatchSize);
        if (cursorFrom != settlement.creditedHolders) {
            revert CursorMismatch(settlement.creditedHolders, cursorFrom);
        }

        uint32 batchUnits = 0;
        for (uint256 i = 0; i < holders_.length; i++) {
            address holder = holders_[i];
            uint32 amount = units[i];
            // Reverting inside the loop is intended: a batch containing one bad entry must
            // fail entirely rather than apply the good entries and advance the cursor past
            // the bad one, which would leave a gap no retry could fill.
            // forge-lint: disable-next-line(require-revert-in-loop)
            if (holder == address(0) || amount == 0) revert ZeroValue();

            batchUnits += amount;
            _setBalance(holder, unitsOf[holder] + amount, HoldingReason.Allotment, bytes32(0));
            emit Transfer(address(0), holder, amount);
        }

        uint32 newCredited = settlement.creditedUnits + batchUnits;
        if (newCredited > settlement.expectedUnits) {
            revert SettlementUnitsMismatch(settlement.expectedUnits, newCredited);
        }

        // Narrowing the array length is safe: it was already bounded above by maxBatchSize,
        // which is a uint32.
        // forge-lint: disable-next-line(unsafe-typecast)
        uint32 cursorTo = cursorFrom + uint32(holders_.length);
        settlement.creditedUnits = newCredited;
        settlement.creditedHolders = cursorTo;

        emit SettlementBatchApplied(cursorFrom, cursorTo, batchUnits);
    }

    /// @notice Closes settlement once every invariant holds. Callable by anyone.
    ///
    /// @dev Permissionless on purpose. If only the operator could finalise, the honest answer
    /// to "so you control the final cap table" would be yes. Because anyone can call this and
    /// the contract accepts it only when all five checks pass, the cap table is governed by
    /// arithmetic rather than by whoever holds a key.
    ///
    /// On failure the stage stays InProgress so the operator can correct and retry. There is
    /// no abort path, and none is needed: the contract custodies nothing, so a stalled
    /// settlement strands no value.
    function finaliseSettlement(bytes32 idempotencyKey) external notPaused fresh(idempotencyKey) {
        SettlementState memory s = settlement;
        if (s.stage != SettlementStage.InProgress) revert SettlementNotStarted();

        if (totalUnitsIssued != totalUnits) {
            revert TotalUnitsMismatch(totalUnits, totalUnitsIssued);
        }
        if (distinctHolderCount < minPublicHolders) {
            revert HolderFloorNotMet(minPublicHolders, distinctHolderCount);
        }
        if (imWallet == address(0) || unitsOf[imWallet] != imUnits || !excludedFromCount[imWallet]) {
            revert ImHoldingMismatch(imUnits, imWallet == address(0) ? 0 : unitsOf[imWallet]);
        }
        if (s.creditedUnits != s.expectedUnits) {
            revert SettlementUnitsMismatch(s.expectedUnits, s.creditedUnits);
        }
        if (s.creditedHolders != s.expectedHolders) {
            revert SettlementHoldersMismatch(s.expectedHolders, s.creditedHolders);
        }

        settlement.stage = SettlementStage.Finalised;
        Status from = status;
        status = Status.Settled;

        emit SettlementFinalised(totalUnitsIssued, distinctHolderCount, msg.sender);
        emit StatusChanged(from, Status.Settled);
    }

    // ---------------------------------------------------------------------
    // Distribution
    // ---------------------------------------------------------------------

    /// @notice Anchors a distribution period and enforces the statutory floor.
    ///
    /// @dev The floor check is written as a cross-multiplication in uint256 rather than as
    /// `distributed * 10000 / ndcf >= floorBps`.
    ///
    /// The reason is overflow, not truncation. For an integer threshold, floor(x) >= k holds
    /// exactly when x >= k, so division would give the same boolean. But `distributedPaise` is
    /// uint64, and multiplying it by 10000 overflows that width for large NDCF values.
    /// Overflow in Solidity 0.8 would revert on a plain uint64 multiply, but widening first
    /// and comparing products removes the question entirely and keeps the check exact.
    function anchorPeriod(PeriodAnchorInput calldata in_, bytes32 idempotencyKey)
        external
        onlyRelayer
        notPaused
        fresh(idempotencyKey)
    {
        if (_periods[in_.periodId].status != PeriodStatus.None) {
            revert PeriodAlreadyAnchored(in_.periodId);
        }
        if (in_.ndcfPaise == 0) revert ZeroNdcf();
        if (in_.distributedPaise > in_.ndcfPaise) {
            revert DistributedExceedsNdcf(in_.distributedPaise, in_.ndcfPaise);
        }
        if (
            in_.statementHash == bytes32(0) || in_.snapshotRoot == bytes32(0)
                || in_.ndcfCidDigest == bytes32(0)
        ) {
            revert ZeroValue();
        }
        if (in_.snapshotTotalUnits != totalUnitsIssued) {
            revert SnapshotUnitsMismatch(totalUnitsIssued, in_.snapshotTotalUnits);
        }
        if (in_.snapshotHolders < minPublicHolders) {
            revert SnapshotHolderFloorNotMet(minPublicHolders, in_.snapshotHolders);
        }

        uint16 bps = _checkDistributionFloor(in_.ndcfPaise, in_.distributedPaise);

        _periods[in_.periodId] = Period({
            recordDate: in_.recordDate,
            ndcfPaise: in_.ndcfPaise,
            distributedPaise: in_.distributedPaise,
            snapshotTotalUnits: in_.snapshotTotalUnits,
            supersedes: in_.supersedes,
            entitledUnitsAccrued: 0,
            entitledHoldersAccrued: 0,
            bps: bps,
            snapshotHolders: in_.snapshotHolders,
            status: PeriodStatus.Anchored,
            statementHash: in_.statementHash,
            snapshotRoot: in_.snapshotRoot,
            ndcfCidDigest: in_.ndcfCidDigest
        });
        if (in_.periodId > latestPeriodId) latestPeriodId = in_.periodId;

        emit PeriodAnchored(
            in_.periodId,
            in_.recordDate,
            in_.ndcfPaise,
            in_.distributedPaise,
            bps,
            in_.statementHash,
            in_.snapshotRoot,
            in_.supersedes
        );
        emit DistributionFloorSatisfied(in_.periodId, bps, distributionFloorBps);
    }

    /// @dev Cross-multiplication in uint256, returning the achieved basis points.
    ///
    /// Written this way because of overflow, not truncation. For an integer threshold,
    /// floor(x) >= k holds exactly when x >= k, so dividing first would give the same boolean.
    /// The problem is that distributedPaise is uint64 and multiplying by 10000 exceeds that
    /// width for large NDCF values. Widening before multiplying removes the failure mode
    /// rather than bounding it.
    function _checkDistributionFloor(uint64 ndcfPaise, uint64 distributedPaise)
        private
        view
        returns (uint16)
    {
        uint256 lhs = uint256(distributedPaise) * 10_000;
        uint256 rhs = uint256(ndcfPaise) * uint256(distributionFloorBps);
        // distributedPaise <= ndcfPaise is checked by the caller, so the quotient is at most
        // 10000 and fits uint16 with room to spare.
        // forge-lint: disable-next-line(unsafe-typecast)
        uint16 achieved = uint16(lhs / uint256(ndcfPaise));
        if (lhs < rhs) revert DistributionFloorNotMet(achieved, distributionFloorBps);
        return achieved;
    }

    /// @notice Emits entitlements for one chunk of holders.
    /// @dev Events only, no per-holder storage. The rupee amount is deliberately absent: it
    /// is `distributedPaise * units / snapshotTotalUnits`, derivable by anyone, so writing it
    /// would spend gas to store a number that adds no information.
    function anchorEntitlementsBatch(
        uint32 periodId,
        address[] calldata holders_,
        uint32[] calldata units,
        uint32 cursorFrom,
        bytes32 idempotencyKey
    ) external onlyRelayer notPaused fresh(idempotencyKey) {
        Period storage p = _periods[periodId];
        if (p.status != PeriodStatus.Anchored) revert PeriodNotAnchored(periodId);
        if (holders_.length != units.length) revert ArrayLengthMismatch();
        if (holders_.length == 0) revert EmptyBatch();
        if (holders_.length > maxBatchSize) revert BatchTooLarge(maxBatchSize);
        if (cursorFrom != p.entitledHoldersAccrued) {
            revert CursorMismatch(p.entitledHoldersAccrued, cursorFrom);
        }

        uint32 batchUnits = 0;
        for (uint256 i = 0; i < holders_.length; i++) {
            // As in settleBatch: a partially applied batch would advance the cursor past a
            // bad entry and leave an unfillable gap.
            // forge-lint: disable-next-line(require-revert-in-loop)
            if (units[i] == 0) revert ZeroValue();
            batchUnits += units[i];
            emit EntitlementAccrued(periodId, holders_[i], units[i], p.snapshotTotalUnits);
        }

        uint32 newUnits = p.entitledUnitsAccrued + batchUnits;
        if (newUnits > p.snapshotTotalUnits) {
            revert EntitlementUnitsMismatch(p.snapshotTotalUnits, newUnits);
        }
        p.entitledUnitsAccrued = newUnits;
        // Bounded above by maxBatchSize, a uint32.
        // forge-lint: disable-next-line(unsafe-typecast)
        p.entitledHoldersAccrued = cursorFrom + uint32(holders_.length);
    }

    /// @notice Closes entitlement accrual once every unit is accounted for.
    function finaliseEntitlements(uint32 periodId, uint32 expectedHolders, bytes32 idempotencyKey)
        external
        onlyRelayer
        notPaused
        fresh(idempotencyKey)
    {
        Period storage p = _periods[periodId];
        if (p.status != PeriodStatus.Anchored) revert PeriodNotAnchored(periodId);
        if (p.entitledUnitsAccrued != p.snapshotTotalUnits) {
            revert EntitlementUnitsMismatch(p.snapshotTotalUnits, p.entitledUnitsAccrued);
        }
        if (p.entitledHoldersAccrued != expectedHolders) {
            revert SettlementHoldersMismatch(expectedHolders, p.entitledHoldersAccrued);
        }

        p.status = PeriodStatus.EntitlementsAnchored;
        emit EntitlementsFinalised(periodId, p.entitledHoldersAccrued, p.entitledUnitsAccrued);
    }

    /// @notice Records that fiat settled off-chain.
    /// @dev After this a period can no longer be reversed. The boundary matters: before it, a
    /// mistake is corrected with a compensating entry; after it, money has left the escrow and
    /// only recovery is possible.
    function confirmPayouts(
        uint32 periodId,
        uint16 settledCount,
        uint16 failedCount,
        bytes32 reportHash,
        bytes32 idempotencyKey
    ) external onlyRelayer notPaused fresh(idempotencyKey) {
        Period storage p = _periods[periodId];
        if (p.status != PeriodStatus.EntitlementsAnchored) revert PeriodNotAnchored(periodId);
        if (reportHash == bytes32(0)) revert ZeroValue();

        p.status = PeriodStatus.PayoutsConfirmed;
        emit PayoutsConfirmed(periodId, settledCount, failedCount, reportHash);
    }

    function closePeriod(uint32 periodId, bytes32 idempotencyKey)
        external
        onlyRelayer
        notPaused
        fresh(idempotencyKey)
    {
        Period storage p = _periods[periodId];
        if (p.status != PeriodStatus.PayoutsConfirmed) revert PeriodNotAnchored(periodId);
        p.status = PeriodStatus.Closed;
        emit PeriodClosed(periodId);
    }

    // ---------------------------------------------------------------------
    // Reversal: two roles, two transactions
    // ---------------------------------------------------------------------

    /// @notice Trustee half of a period reversal.
    /// @dev Four-eyes enforced in the contract rather than in an admin UI, which is the
    /// version that survives diligence. The reason and narrative digest are recorded here and
    /// must match exactly when the reversal executes.
    function approveReversal(uint32 periodId, uint8 reason, bytes32 narrativeHash) external {
        if (!roles.isTrustee(msg.sender)) revert NotTrustee(msg.sender);
        if (narrativeHash == bytes32(0)) revert ZeroValue();

        PeriodStatus st = _periods[periodId].status;
        if (st != PeriodStatus.Anchored && st != PeriodStatus.EntitlementsAnchored) {
            revert PeriodNotReversible(periodId, st);
        }

        _reversalApproval[periodId] =
            ReversalApproval({trustee: msg.sender, reason: reason, present: true, narrativeHash: narrativeHash});

        emit ReversalApproved(periodId, reason, narrativeHash, msg.sender);
    }

    /// @notice Relayer half of a period reversal.
    ///
    /// @dev Permitted while paused, because a pause is often exactly when a reversal is
    /// needed. Refused once payouts are confirmed: money has left the escrow and the remedy is
    /// a carry-forward adjustment, not a reversal.
    ///
    /// The reversed period stays visible forever. An audit trail that can hide its own
    /// corrections is worth nothing, and being able to show the error, the trustee approval,
    /// the reversal and the replacement is a stronger demonstration than a suspiciously clean
    /// history.
    function reversePeriod(
        uint32 periodId,
        uint8 reason,
        bytes32 narrativeHash,
        bytes32 idempotencyKey
    ) external onlyRelayer fresh(idempotencyKey) {
        Period storage p = _periods[periodId];
        PeriodStatus st = p.status;

        if (st == PeriodStatus.PayoutsConfirmed || st == PeriodStatus.Closed) {
            revert PayoutsAlreadyConfirmed(periodId);
        }
        if (st != PeriodStatus.Anchored && st != PeriodStatus.EntitlementsAnchored) {
            revert PeriodNotReversible(periodId, st);
        }

        ReversalApproval memory a = _reversalApproval[periodId];
        if (!a.present) revert ReversalNotApproved(periodId);
        if (a.reason != reason || a.narrativeHash != narrativeHash) {
            revert ReversalApprovalMismatch();
        }

        p.status = PeriodStatus.Reversed;
        delete _reversalApproval[periodId];

        emit PeriodReversed(periodId, reason, narrativeHash, a.trustee);
    }

    /// @notice Records a carry-forward adjustment, the only remedy once fiat has settled.
    function recordPayoutAdjustment(
        uint32 sourcePeriodId,
        uint32 targetPeriodId,
        address holder,
        uint8 direction,
        uint8 reason,
        bytes32 idempotencyKey
    ) external onlyRelayer fresh(idempotencyKey) {
        if (holder == address(0)) revert ZeroValue();
        emit PayoutAdjustmentRecorded(sourcePeriodId, targetPeriodId, holder, direction, reason);
    }

    // ---------------------------------------------------------------------
    // Public verification
    // ---------------------------------------------------------------------

    /// @notice Proves a holder's entitlement against the frozen record-date snapshot.
    ///
    /// @dev The reason the snapshot root is anchored at all. Without this the root is a number
    /// in a struct; with it, any investor can confirm from a block explorer that their unit
    /// count on the record date is the one the distribution used, with no access to our
    /// systems and no need to trust them.
    ///
    /// The leaf encoding is supplied by the caller alongside the proof because the snapshot
    /// document defines it, and that document is pinned to IPFS with its digest anchored here.
    function verifyEntitlement(uint32 periodId, bytes32 leaf, bytes32[] calldata proof)
        external
        view
        returns (bool)
    {
        bytes32 root = _periods[periodId].snapshotRoot;
        if (root == bytes32(0)) return false;
        return MerkleLib.verify(root, leaf, proof);
    }

    /// @notice Whether a period met the statutory distribution floor.
    function distributionFloorMet(uint32 periodId) external view returns (bool) {
        Period memory p = _periods[periodId];
        if (p.status == PeriodStatus.None) return false;
        return p.bps >= distributionFloorBps;
    }

    function getPeriod(uint32 periodId) external view returns (Period memory) {
        return _periods[periodId];
    }

    function getReversalApproval(uint32 periodId) external view returns (ReversalApproval memory) {
        return _reversalApproval[periodId];
    }

    function holderCount() external view returns (uint256) {
        return _holders.length;
    }

    function holderAt(uint256 index) external view returns (address) {
        return _holders[index];
    }
}
