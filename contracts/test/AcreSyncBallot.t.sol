// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {Test} from "forge-std/Test.sol";
import {AcreSyncRoles} from "../src/AcreSyncRoles.sol";
import {AcreSyncBallot} from "../src/AcreSyncBallot.sol";
import {BallotEncoding} from "../src/BallotEncoding.sol";
import {MerkleLib} from "../src/MerkleLib.sol";

contract AcreSyncBallotTest is Test {
    AcreSyncRoles internal roles;
    AcreSyncBallot internal ballot;

    address internal constant ADMIN_ACC = address(0xA1);
    address internal constant RELAYER_ACC = address(0xB2);
    address internal constant PAUSER_ACC = address(0xC3);
    address internal constant TRUSTEE_ACC = address(0xD4);
    address internal constant STRANGER = address(0xE5);

    uint32 internal constant DELAY = 10;
    uint32 internal constant WINDOW = 200;
    uint8 internal constant MAX_ATTEMPTS = 3;

    // The v1 offer parameters.
    uint32 internal constant UNITS_ON_OFFER = 475;
    uint32 internal constant MIN_HOLDERS = 200;

    bytes32 internal constant BOOK_ROOT = bytes32(uint256(0xB00C));
    bytes32 internal constant BOOK_CID = bytes32(uint256(0xC1D));
    bytes32 internal constant SECRET = keccak256("the operator's secret");

    uint256 internal keyNonce;

    /// @dev The commitment is precomputed in setUp rather than derived inline at each call
    /// site, and the reason is a real trap rather than style.
    ///
    /// sha256 is an EVM precompile, so computing a commitment is an actual staticcall to
    /// address 0x02. `vm.prank` applies to the next call from the current context, and that
    /// staticcall is the next call, so an expression like
    /// `ballot.commitSeed(BallotEncoding.commitmentOf(SECRET), key)` written after a prank
    /// silently spends the prank on the precompile and invokes commitSeed as the test
    /// contract. It surfaces as a confusing NotRelayer revert.
    ///
    /// keccak256 would not behave this way because it is an opcode rather than a precompile.
    /// This is a direct consequence of standardising the system on SHA-256, and precomputing
    /// every digest before a prank is the discipline that avoids it.
    bytes32 internal commitment;

    function setUp() public {
        roles = new AcreSyncRoles(
            ADMIN_ACC, RELAYER_ACC, PAUSER_ACC, TRUSTEE_ACC, 1 hours, true, 1
        );
        ballot = new AcreSyncBallot(roles, DELAY, WINDOW, MAX_ATTEMPTS);
        commitment = BallotEncoding.commitmentOf(SECRET);

        // Start well clear of genesis so blockhash is meaningful.
        vm.roll(1000);
    }

    /// @dev Fresh idempotency key per call. Reusing one is itself a tested behaviour, but
    /// most tests are not about that.
    function nextKey() internal returns (bytes32) {
        keyNonce++;
        return keccak256(abi.encodePacked("key", keyNonce));
    }

    function anchorBook() internal {
        vm.prank(RELAYER_ACC);
        ballot.anchorBidbook(BOOK_ROOT, BOOK_CID, 900, 900, 900, nextKey());
    }

    function commit() internal {
        vm.prank(RELAYER_ACC);
        ballot.commitSeed(commitment, nextKey());
    }

    // ---------------------------------------------------------------------
    // Ordering: the security property
    // ---------------------------------------------------------------------

    function test_HappyPath() public {
        anchorBook();
        assertEq(uint8(ballot.stage()), uint8(AcreSyncBallot.Stage.BidbookAnchored));
        assertEq(ballot.bidbookRoot(), BOOK_ROOT);

        commit();
        assertEq(uint8(ballot.stage()), uint8(AcreSyncBallot.Stage.SeedCommitted));
        assertEq(ballot.targetBlock(), 1000 + DELAY);
        assertEq(ballot.attempt(), 1);

        vm.roll(1000 + DELAY + 1);

        vm.prank(RELAYER_ACC);
        ballot.revealSeed(SECRET, nextKey());
        assertEq(uint8(ballot.stage()), uint8(AcreSyncBallot.Stage.SeedRevealed));

        bytes32 observed = ballot.targetBlockHash();
        bytes32 expected = BallotEncoding.finalSeed(SECRET, observed, BOOK_ROOT);
        assertEq(ballot.finalSeed(), expected, "final seed must be derived from all three inputs");
        assertTrue(ballot.finalSeed() != bytes32(0));

        vm.prank(RELAYER_ACC);
        ballot.anchorBallotResult(
            bytes32(uint256(0x8E5017)),
            bytes32(uint256(0xC1D2)),
            UNITS_ON_OFFER,
            475,
            UNITS_ON_OFFER,
            MIN_HOLDERS,
            1,
            nextKey()
        );
        assertEq(uint8(ballot.stage()), uint8(AcreSyncBallot.Stage.ResultAnchored));
    }

    /// @dev The bid book must be pinned before a commitment exists. Otherwise the operator
    /// could see the seed and then decide which bids to include.
    function test_CannotCommitBeforeAnchoringBook() public {
        vm.prank(RELAYER_ACC);
        vm.expectRevert(
            abi.encodeWithSelector(
                AcreSyncBallot.WrongStage.selector,
                AcreSyncBallot.Stage.BidbookAnchored,
                AcreSyncBallot.Stage.None
            )
        );
        ballot.commitSeed(commitment, nextKey());
    }

    function test_CannotRevealBeforeCommitting() public {
        anchorBook();
        vm.prank(RELAYER_ACC);
        vm.expectRevert(
            abi.encodeWithSelector(
                AcreSyncBallot.WrongStage.selector,
                AcreSyncBallot.Stage.SeedCommitted,
                AcreSyncBallot.Stage.BidbookAnchored
            )
        );
        ballot.revealSeed(SECRET, nextKey());
    }

    function test_BookCannotBeReanchored() public {
        anchorBook();
        vm.prank(RELAYER_ACC);
        vm.expectRevert();
        ballot.anchorBidbook(bytes32(uint256(0xDEAD)), BOOK_CID, 900, 900, 900, nextKey());
        assertEq(ballot.bidbookRoot(), BOOK_ROOT, "the frozen book must be immutable");
    }

    // ---------------------------------------------------------------------
    // The target block is the contract's choice
    // ---------------------------------------------------------------------

    /// @dev The caller supplies no block number anywhere in the ceremony. If it could, it
    /// would choose one whose hash it had reason to prefer.
    function test_ContractChoosesTargetBlock() public {
        anchorBook();
        uint256 atCommit = block.number;
        commit();
        assertEq(ballot.targetBlock(), atCommit + DELAY, "target must be commit block + delay");
    }

    function test_CannotRevealBeforeTargetBlock() public {
        anchorBook();
        commit();

        // At the target block itself the hash is not yet available.
        uint64 target = ballot.targetBlock();
        vm.roll(target);
        bytes32 key = nextKey();

        vm.expectRevert(
            abi.encodeWithSelector(
                AcreSyncBallot.TargetBlockNotReached.selector, target, uint64(block.number)
            )
        );
        vm.prank(RELAYER_ACC);
        ballot.revealSeed(SECRET, key);
    }

    function test_RevealAtFirstPermittedBlock() public {
        anchorBook();
        commit();
        vm.roll(ballot.targetBlock() + 1);

        vm.prank(RELAYER_ACC);
        ballot.revealSeed(SECRET, nextKey());
        assertEq(uint8(ballot.stage()), uint8(AcreSyncBallot.Stage.SeedRevealed));
    }

    function test_RevealAtLastPermittedBlock() public {
        anchorBook();
        commit();
        vm.roll(ballot.targetBlock() + WINDOW);

        vm.prank(RELAYER_ACC);
        ballot.revealSeed(SECRET, nextKey());
        assertEq(uint8(ballot.stage()), uint8(AcreSyncBallot.Stage.SeedRevealed));
    }

    function test_RevealAfterWindowReverts() public {
        anchorBook();
        commit();
        uint64 deadline = ballot.targetBlock() + WINDOW;
        vm.roll(deadline + 1);

        vm.prank(RELAYER_ACC);
        vm.expectRevert(
            abi.encodeWithSelector(
                AcreSyncBallot.RevealWindowExpired.selector, deadline, uint64(block.number)
            )
        );
        ballot.revealSeed(SECRET, nextKey());
    }

    function test_WrongSecretReverts() public {
        anchorBook();
        commit();
        vm.roll(ballot.targetBlock() + 1);

        bytes32 wrong = keccak256("not the committed secret");
        // Both digests computed before the prank; see the note on `commitment`.
        bytes32 wrongCommitment = BallotEncoding.commitmentOf(wrong);
        bytes32 key = nextKey();

        vm.expectRevert(
            abi.encodeWithSelector(
                AcreSyncBallot.CommitmentMismatch.selector, commitment, wrongCommitment
            )
        );
        vm.prank(RELAYER_ACC);
        ballot.revealSeed(wrong, key);
    }

    // ---------------------------------------------------------------------
    // The abandonment vector
    // ---------------------------------------------------------------------

    /// @dev The central anti-grinding property. recommitSeed takes no commitment argument,
    /// so the secret is fixed from the first commit and abandonment can reroll only the
    /// blockhash.
    function test_RecommitReusesTheStoredCommitment() public {
        anchorBook();
        commit();
        bytes32 original = ballot.seedCommitment();
        uint64 firstTarget = ballot.targetBlock();

        vm.roll(firstTarget + WINDOW + 1);

        vm.prank(RELAYER_ACC);
        ballot.recommitSeed(nextKey());

        assertEq(ballot.seedCommitment(), original, "the commitment must be immutable");
        assertEq(ballot.attempt(), 2);
        assertTrue(ballot.targetBlock() > firstTarget, "a fresh target block is required");

        // The original secret must still be the one that opens it.
        vm.roll(ballot.targetBlock() + 1);
        vm.prank(RELAYER_ACC);
        ballot.revealSeed(SECRET, nextKey());
        assertEq(uint8(ballot.stage()), uint8(AcreSyncBallot.Stage.SeedRevealed));
    }

    /// @dev Recommitting while a reveal is still possible would let an operator skip a draw
    /// they could already compute.
    function test_CannotRecommitWhileWindowOpen() public {
        anchorBook();
        commit();
        vm.roll(ballot.targetBlock() + 1);

        uint64 deadline = ballot.targetBlock() + WINDOW;
        vm.prank(RELAYER_ACC);
        vm.expectRevert(
            abi.encodeWithSelector(
                AcreSyncBallot.WindowStillOpen.selector, deadline, uint64(block.number)
            )
        );
        ballot.recommitSeed(nextKey());
    }

    function test_AttemptsAreCappedAtThree() public {
        anchorBook();
        commit();
        assertEq(ballot.attempt(), 1);

        for (uint8 i = 2; i <= MAX_ATTEMPTS; i++) {
            vm.roll(ballot.targetBlock() + WINDOW + 1);
            vm.prank(RELAYER_ACC);
            ballot.recommitSeed(nextKey());
            assertEq(ballot.attempt(), i);
        }

        // The fourth attempt is refused: the operator can no longer keep rolling.
        vm.roll(ballot.targetBlock() + WINDOW + 1);
        vm.prank(RELAYER_ACC);
        vm.expectRevert(
            abi.encodeWithSelector(AcreSyncBallot.MaxSeedAttemptsReached.selector, MAX_ATTEMPTS)
        );
        ballot.recommitSeed(nextKey());
    }

    /// @dev Each expiry is emitted, so the number of attempts is public rather than being
    /// something only the operator knows.
    function test_ExpiryIsPubliclyCounted() public {
        anchorBook();
        commit();
        uint64 firstTarget = ballot.targetBlock();
        vm.roll(firstTarget + WINDOW + 1);

        vm.expectEmit(false, false, false, true);
        emit AcreSyncBallot.SeedWindowExpired(firstTarget, 1);
        vm.prank(RELAYER_ACC);
        ballot.recommitSeed(nextKey());
    }

    function test_TrusteeEscalationAfterExhaustedAttempts() public {
        anchorBook();
        commit();
        for (uint8 i = 2; i <= MAX_ATTEMPTS; i++) {
            vm.roll(ballot.targetBlock() + WINDOW + 1);
            vm.prank(RELAYER_ACC);
            ballot.recommitSeed(nextKey());
        }
        vm.roll(ballot.targetBlock() + WINDOW + 1);

        // The relayer cannot escalate on its own behalf.
        vm.prank(RELAYER_ACC);
        vm.expectRevert(abi.encodeWithSelector(AcreSyncBallot.NotTrustee.selector, RELAYER_ACC));
        ballot.escalateBallot();

        vm.prank(TRUSTEE_ACC);
        ballot.escalateBallot();
        assertEq(uint8(ballot.stage()), uint8(AcreSyncBallot.Stage.Escalated));
    }

    function test_CannotEscalateBeforeAttemptsExhausted() public {
        anchorBook();
        commit();
        vm.roll(ballot.targetBlock() + WINDOW + 1);

        vm.prank(TRUSTEE_ACC);
        vm.expectRevert(
            abi.encodeWithSelector(AcreSyncBallot.MaxSeedAttemptsReached.selector, MAX_ATTEMPTS)
        );
        ballot.escalateBallot();
    }

    /// @dev An escalated ballot can still anchor a result, otherwise the offer would be
    /// permanently stranded after three unlucky windows.
    function test_EscalatedBallotCanStillAnchorAResult() public {
        anchorBook();
        commit();
        for (uint8 i = 2; i <= MAX_ATTEMPTS; i++) {
            vm.roll(ballot.targetBlock() + WINDOW + 1);
            vm.prank(RELAYER_ACC);
            ballot.recommitSeed(nextKey());
        }
        vm.roll(ballot.targetBlock() + WINDOW + 1);
        vm.prank(TRUSTEE_ACC);
        ballot.escalateBallot();

        vm.prank(RELAYER_ACC);
        ballot.anchorBallotResult(
            bytes32(uint256(0xAAAA)),
            bytes32(uint256(0xBBBB)),
            UNITS_ON_OFFER,
            475,
            UNITS_ON_OFFER,
            MIN_HOLDERS,
            1,
            nextKey()
        );
        assertEq(uint8(ballot.stage()), uint8(AcreSyncBallot.Stage.ResultAnchored));
    }

    // ---------------------------------------------------------------------
    // Result constraints
    // ---------------------------------------------------------------------

    function reachRevealed() internal {
        anchorBook();
        commit();
        vm.roll(ballot.targetBlock() + 1);
        vm.prank(RELAYER_ACC);
        ballot.revealSeed(SECRET, nextKey());
    }

    /// @dev The statutory floor, enforced at the anchor as well as at settlement. Catching
    /// it here is cheaper than discovering three transactions later that the scheme cannot
    /// list.
    function test_ResultBelowHolderFloorReverts() public {
        reachRevealed();

        vm.prank(RELAYER_ACC);
        vm.expectRevert(
            abi.encodeWithSelector(AcreSyncBallot.AllotteeFloorNotMet.selector, MIN_HOLDERS, 199)
        );
        ballot.anchorBallotResult(
            bytes32(uint256(0xAAAA)),
            bytes32(uint256(0xBBBB)),
            UNITS_ON_OFFER,
            199,
            UNITS_ON_OFFER,
            MIN_HOLDERS,
            1,
            nextKey()
        );
    }

    function test_ResultExceedingOfferReverts() public {
        reachRevealed();

        vm.prank(RELAYER_ACC);
        vm.expectRevert(
            abi.encodeWithSelector(
                AcreSyncBallot.UnitsExceedOffer.selector, UNITS_ON_OFFER + 1, UNITS_ON_OFFER
            )
        );
        ballot.anchorBallotResult(
            bytes32(uint256(0xAAAA)),
            bytes32(uint256(0xBBBB)),
            UNITS_ON_OFFER + 1,
            475,
            UNITS_ON_OFFER,
            MIN_HOLDERS,
            1,
            nextKey()
        );
    }

    /// @dev Units are indivisible, so more allottees than units is arithmetically impossible
    /// and signals a broken allocation engine.
    function test_MoreAllotteesThanUnitsReverts() public {
        reachRevealed();

        vm.prank(RELAYER_ACC);
        vm.expectRevert(AcreSyncBallot.InvalidCounts.selector);
        ballot.anchorBallotResult(
            bytes32(uint256(0xAAAA)),
            bytes32(uint256(0xBBBB)),
            300,
            301,
            UNITS_ON_OFFER,
            MIN_HOLDERS,
            1,
            nextKey()
        );
    }

    function test_MoreBiddersThanLeavesReverts() public {
        vm.prank(RELAYER_ACC);
        vm.expectRevert(AcreSyncBallot.InvalidCounts.selector);
        ballot.anchorBidbook(BOOK_ROOT, BOOK_CID, 100, 100, 101, nextKey());
    }

    // ---------------------------------------------------------------------
    // Access control, pause, idempotency
    // ---------------------------------------------------------------------

    function test_OnlyRelayerCanDriveTheCeremony() public {
        vm.prank(STRANGER);
        vm.expectRevert(abi.encodeWithSelector(AcreSyncBallot.NotRelayer.selector, STRANGER));
        ballot.anchorBidbook(BOOK_ROOT, BOOK_CID, 900, 900, 900, nextKey());

        anchorBook();

        vm.prank(TRUSTEE_ACC);
        vm.expectRevert(abi.encodeWithSelector(AcreSyncBallot.NotRelayer.selector, TRUSTEE_ACC));
        ballot.commitSeed(commitment, nextKey());
    }

    /// @dev A revoked relayer halts the ceremony immediately, which is the point of making
    /// revocation untimelocked.
    function test_RevokedRelayerCannotAct() public {
        anchorBook();

        vm.prank(ADMIN_ACC);
        roles.revokeRelayer();

        vm.prank(RELAYER_ACC);
        vm.expectRevert(abi.encodeWithSelector(AcreSyncBallot.NotRelayer.selector, RELAYER_ACC));
        ballot.commitSeed(commitment, nextKey());
    }

    function test_PauseBlocksTheCeremony() public {
        anchorBook();

        vm.prank(PAUSER_ACC);
        roles.pause(keccak256("investigation"));

        vm.prank(RELAYER_ACC);
        vm.expectRevert(AcreSyncBallot.ContractPaused.selector);
        ballot.commitSeed(commitment, nextKey());

        vm.prank(PAUSER_ACC);
        roles.unpause();

        commit();
        assertEq(uint8(ballot.stage()), uint8(AcreSyncBallot.Stage.SeedCommitted));
    }

    /// @dev The on-chain half of the two-layer replay defence. The database catches a retry
    /// before broadcast; this catches anything arriving by another path.
    function test_IdempotencyKeyCannotBeReused() public {
        bytes32 key = nextKey();

        vm.prank(RELAYER_ACC);
        ballot.anchorBidbook(BOOK_ROOT, BOOK_CID, 900, 900, 900, key);

        vm.expectRevert(abi.encodeWithSelector(AcreSyncBallot.KeyAlreadyUsed.selector, key));
        vm.prank(RELAYER_ACC);
        ballot.commitSeed(commitment, key);

        assertTrue(ballot.usedKey(key));
    }

    // ---------------------------------------------------------------------
    // Configuration
    // ---------------------------------------------------------------------

    /// @dev Beyond 256 blocks past the target, blockhash returns zero and no reveal can ever
    /// succeed. Rejecting the configuration beats accepting a window that is silently
    /// shorter than it claims.
    function test_ConstructorRejectsUnreachableWindow() public {
        vm.expectRevert(AcreSyncBallot.ZeroValue.selector);
        new AcreSyncBallot(roles, DELAY, 251, MAX_ATTEMPTS);

        vm.expectRevert(AcreSyncBallot.ZeroValue.selector);
        new AcreSyncBallot(roles, 0, WINDOW, MAX_ATTEMPTS);

        vm.expectRevert(AcreSyncBallot.ZeroValue.selector);
        new AcreSyncBallot(roles, DELAY, WINDOW, 0);
    }

    function test_RevealWindowView() public {
        anchorBook();
        commit();

        (bool open,,, ) = ballot.revealWindow();
        assertFalse(open, "not open before the target block");

        vm.roll(ballot.targetBlock() + 1);
        (open,,,) = ballot.revealWindow();
        assertTrue(open, "open just after the target block");

        vm.roll(ballot.targetBlock() + WINDOW + 1);
        (open,,,) = ballot.revealWindow();
        assertFalse(open, "closed past the deadline");
    }

    // ---------------------------------------------------------------------
    // Public verification
    // ---------------------------------------------------------------------

    /// @dev Without this the bid book root is a number in a log. With it, an investor who
    /// lost the ballot can still prove their bid was in the set the draw consumed.
    function test_VerifyBidInclusion() public {
        bytes32 leafA = BallotEncoding.bidLeaf(0, bytes16(uint128(0xA1)), keccak256("anchorA"), 3, 100_000_000);
        bytes32 leafB = BallotEncoding.bidLeaf(1, bytes16(uint128(0xB2)), keccak256("anchorB"), 5, 100_000_000);
        bytes32 root = MerkleLib.hashNode(leafA, leafB);

        vm.prank(RELAYER_ACC);
        ballot.anchorBidbook(root, BOOK_CID, 2, 8, 2, nextKey());

        bytes32[] memory proof = new bytes32[](1);
        proof[0] = leafB;

        assertTrue(
            ballot.verifyBidInclusion(0, bytes16(uint128(0xA1)), keccak256("anchorA"), 3, 100_000_000, proof),
            "an included bid must verify"
        );

        // A bid that was never in the book must not verify, even with a valid-looking proof.
        assertFalse(
            ballot.verifyBidInclusion(0, bytes16(uint128(0xA1)), keccak256("anchorA"), 4, 100_000_000, proof),
            "altering the unit count must break the proof"
        );
    }

    function test_VerifyReturnsFalseBeforeAnchoring() public view {
        bytes32[] memory proof = new bytes32[](0);
        assertFalse(ballot.verifyBidInclusion(0, bytes16(0), bytes32(0), 1, 1, proof));
        assertFalse(ballot.verifyAllotment(0, bytes32(0), 1, 0, 0, proof));
    }

    // ---------------------------------------------------------------------
    // Fuzz
    // ---------------------------------------------------------------------

    /// @dev Only the committed secret can open a commitment, for any other candidate.
    function testFuzz_OnlyCommittedSecretReveals(bytes32 candidate) public {
        vm.assume(candidate != SECRET);

        anchorBook();
        commit();
        vm.roll(ballot.targetBlock() + 1);

        vm.prank(RELAYER_ACC);
        vm.expectRevert();
        ballot.revealSeed(candidate, nextKey());
    }

    /// @dev A reveal must be impossible at every block outside the window, not merely at the
    /// boundaries the explicit tests probe.
    function testFuzz_RevealOnlyInsideWindow(uint32 offset) public {
        vm.assume(offset > WINDOW && offset < 100_000);

        anchorBook();
        commit();
        vm.roll(ballot.targetBlock() + offset);

        vm.prank(RELAYER_ACC);
        vm.expectRevert();
        ballot.revealSeed(SECRET, nextKey());
    }

    function testFuzz_NonRelayerNeverActs(address caller) public {
        vm.assume(caller != RELAYER_ACC && caller != address(0));

        vm.prank(caller);
        vm.expectRevert(abi.encodeWithSelector(AcreSyncBallot.NotRelayer.selector, caller));
        ballot.anchorBidbook(BOOK_ROOT, BOOK_CID, 900, 900, 900, nextKey());
    }
}
