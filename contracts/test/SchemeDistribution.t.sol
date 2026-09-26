// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {SchemeHarness} from "./SchemeHarness.sol";
import {AcreSyncScheme} from "../src/AcreSyncScheme.sol";
import {MerkleLib} from "../src/MerkleLib.sol";

/// @notice Distribution: the 95% floor, entitlement accrual, and the two-transaction reversal.
contract SchemeDistributionTest is SchemeHarness {
    function setUp() public {
        setUpHarness();
        reachOperational(200);
    }

    // ---------------------------------------------------------------------
    // The 95% floor
    // ---------------------------------------------------------------------

    /// @dev The claim the whole product rests on: the statutory floor is enforced in code and
    /// publicly verifiable, not asserted in a report.
    function test_AnchorAtExactlyNinetyFivePercent() public {
        vm.prank(RELAYER_ACC);
        scheme.anchorPeriod(periodInput(1, NDCF, DISTRIBUTED), nextKey());

        AcreSyncScheme.Period memory p = scheme.getPeriod(1);
        assertEq(p.bps, 9500, "28.5 crore of 30 crore is exactly 9500 bps");
        assertEq(p.ndcfPaise, NDCF);
        assertEq(p.distributedPaise, DISTRIBUTED);
        assertEq(uint8(p.status), uint8(AcreSyncScheme.PeriodStatus.Anchored));
        assertTrue(scheme.distributionFloorMet(1));
    }

    /// @dev One paisa short must fail. Not "approximately 95%".
    function test_OnePaiseBelowFloorReverts() public {
        bytes32 key = nextKey();
        vm.expectRevert(
            abi.encodeWithSelector(AcreSyncScheme.DistributionFloorNotMet.selector, 9499, FLOOR_BPS)
        );
        vm.prank(RELAYER_ACC);
        scheme.anchorPeriod(periodInput(1, NDCF, DISTRIBUTED - 1), key);
    }

    function test_AboveFloorAccepted() public {
        vm.prank(RELAYER_ACC);
        scheme.anchorPeriod(periodInput(1, NDCF, NDCF), nextKey());

        AcreSyncScheme.Period memory p = scheme.getPeriod(1);
        assertEq(p.bps, 10_000, "distributing everything is 10000 bps");
    }

    function test_DistributingMoreThanNdcfReverts() public {
        bytes32 key = nextKey();
        vm.expectRevert(
            abi.encodeWithSelector(
                AcreSyncScheme.DistributedExceedsNdcf.selector, NDCF + 1, NDCF
            )
        );
        vm.prank(RELAYER_ACC);
        scheme.anchorPeriod(periodInput(1, NDCF, NDCF + 1), key);
    }

    function test_ZeroNdcfReverts() public {
        bytes32 key = nextKey();
        vm.expectRevert(AcreSyncScheme.ZeroNdcf.selector);
        vm.prank(RELAYER_ACC);
        scheme.anchorPeriod(periodInput(1, 0, 0), key);
    }

    /// @dev The reason the floor check cross-multiplies in uint256 rather than dividing first.
    /// At this magnitude `distributedPaise * 10000` exceeds uint64, and a narrower computation
    /// would either revert spuriously or, in an unchecked context, wrap to a small number that
    /// passes a floor it should fail.
    function test_FloorHoldsAtMagnitudesThatOverflowUint64() public {
        // ₹2,000 crore in paise, well past anything a single SM-REIT scheme would see, chosen
        // because 2e15 * 10000 = 2e19 is above uint64's 1.8e19 ceiling.
        uint64 bigNdcf = 2_000_000_000_000_000;
        uint64 bigPass = 1_900_000_000_000_000; // exactly 9500 bps
        uint64 bigFail = 1_899_999_999_999_999;

        vm.prank(RELAYER_ACC);
        scheme.anchorPeriod(periodInput(1, bigNdcf, bigPass), nextKey());
        assertEq(scheme.getPeriod(1).bps, 9500);

        bytes32 key = nextKey();
        vm.expectRevert();
        vm.prank(RELAYER_ACC);
        scheme.anchorPeriod(periodInput(2, bigNdcf, bigFail), key);
    }

    /// @dev Entitlement is computed on holdings as of a record date, so a snapshot that
    /// disagrees with the register it claims to describe is rejected. Accepting it would change
    /// every holder's per-unit rate.
    function test_SnapshotMustMatchTheRegister() public {
        AcreSyncScheme.PeriodAnchorInput memory in_ = periodInput(1, NDCF, DISTRIBUTED);
        in_.snapshotTotalUnits = TOTAL_UNITS - 1;

        bytes32 key = nextKey();
        vm.expectRevert(
            abi.encodeWithSelector(
                AcreSyncScheme.SnapshotUnitsMismatch.selector, TOTAL_UNITS, TOTAL_UNITS - 1
            )
        );
        vm.prank(RELAYER_ACC);
        scheme.anchorPeriod(in_, key);
    }

    function test_SnapshotBelowHolderFloorRejected() public {
        AcreSyncScheme.PeriodAnchorInput memory in_ = periodInput(1, NDCF, DISTRIBUTED);
        in_.snapshotHolders = 199;

        bytes32 key = nextKey();
        vm.expectRevert(
            abi.encodeWithSelector(
                AcreSyncScheme.SnapshotHolderFloorNotMet.selector, MIN_HOLDERS, 199
            )
        );
        vm.prank(RELAYER_ACC);
        scheme.anchorPeriod(in_, key);
    }

    function test_PeriodCannotBeReanchored() public {
        vm.prank(RELAYER_ACC);
        scheme.anchorPeriod(periodInput(1, NDCF, DISTRIBUTED), nextKey());

        bytes32 key = nextKey();
        vm.expectRevert(abi.encodeWithSelector(AcreSyncScheme.PeriodAlreadyAnchored.selector, 1));
        vm.prank(RELAYER_ACC);
        scheme.anchorPeriod(periodInput(1, NDCF, DISTRIBUTED), key);
    }

    function test_MissingDigestsRejected() public {
        AcreSyncScheme.PeriodAnchorInput memory in_ = periodInput(1, NDCF, DISTRIBUTED);
        in_.snapshotRoot = bytes32(0);

        bytes32 key = nextKey();
        vm.expectRevert(AcreSyncScheme.ZeroValue.selector);
        vm.prank(RELAYER_ACC);
        scheme.anchorPeriod(in_, key);
    }

    // ---------------------------------------------------------------------
    // Entitlements
    // ---------------------------------------------------------------------

    function anchorPeriodOne() internal {
        vm.prank(RELAYER_ACC);
        scheme.anchorPeriod(periodInput(1, NDCF, DISTRIBUTED), nextKey());
    }

    /// @dev Accrual must account for every unit in the snapshot before it can close. A missing
    /// holder would mean somebody's money was never attributed.
    function test_EntitlementsMustCoverEverySnapshotUnit() public {
        anchorPeriodOne();

        address[] memory hs = new address[](1);
        uint32[] memory us = new uint32[](1);
        hs[0] = IM_WALLET;
        us[0] = IM_UNITS;

        vm.prank(RELAYER_ACC);
        scheme.anchorEntitlementsBatch(1, hs, us, 0, nextKey());

        bytes32 key = nextKey();
        vm.expectRevert(
            abi.encodeWithSelector(
                AcreSyncScheme.EntitlementUnitsMismatch.selector, TOTAL_UNITS, IM_UNITS
            )
        );
        vm.prank(RELAYER_ACC);
        scheme.finaliseEntitlements(1, 1, key);
    }

    function test_EntitlementsCannotExceedSnapshot() public {
        anchorPeriodOne();

        address[] memory hs = new address[](1);
        uint32[] memory us = new uint32[](1);
        hs[0] = IM_WALLET;
        us[0] = TOTAL_UNITS + 1;

        bytes32 key = nextKey();
        vm.expectRevert(
            abi.encodeWithSelector(
                AcreSyncScheme.EntitlementUnitsMismatch.selector, TOTAL_UNITS, TOTAL_UNITS + 1
            )
        );
        vm.prank(RELAYER_ACC);
        scheme.anchorEntitlementsBatch(1, hs, us, 0, key);
    }

    /// @dev Accrues every holder in the register, in batches, then closes.
    function test_FullEntitlementCycle() public {
        anchorPeriodOne();

        uint256 count = scheme.holderCount();
        uint32 cursor = 0;

        while (cursor < count) {
            uint256 size = count - cursor;
            if (size > MAX_BATCH) size = MAX_BATCH;

            address[] memory hs = new address[](size);
            uint32[] memory us = new uint32[](size);
            for (uint256 i = 0; i < size; i++) {
                address h = scheme.holderAt(cursor + i);
                hs[i] = h;
                us[i] = uint32(scheme.balanceOf(h));
            }

            vm.prank(RELAYER_ACC);
            scheme.anchorEntitlementsBatch(1, hs, us, cursor, nextKey());
            cursor += uint32(size);
        }

        vm.prank(RELAYER_ACC);
        scheme.finaliseEntitlements(1, cursor, nextKey());

        AcreSyncScheme.Period memory p = scheme.getPeriod(1);
        assertEq(uint8(p.status), uint8(AcreSyncScheme.PeriodStatus.EntitlementsAnchored));
        assertEq(p.entitledUnitsAccrued, TOTAL_UNITS, "every unit accounted for");

        vm.prank(RELAYER_ACC);
        scheme.confirmPayouts(1, 201, 0, keccak256("payout report"), nextKey());
        assertEq(
            uint8(scheme.getPeriod(1).status),
            uint8(AcreSyncScheme.PeriodStatus.PayoutsConfirmed)
        );

        vm.prank(RELAYER_ACC);
        scheme.closePeriod(1, nextKey());
        assertEq(uint8(scheme.getPeriod(1).status), uint8(AcreSyncScheme.PeriodStatus.Closed));
    }

    function test_EntitlementCursorDiscipline() public {
        anchorPeriodOne();

        address[] memory hs = new address[](1);
        uint32[] memory us = new uint32[](1);
        hs[0] = IM_WALLET;
        us[0] = IM_UNITS;

        bytes32 key = nextKey();
        vm.expectRevert(abi.encodeWithSelector(AcreSyncScheme.CursorMismatch.selector, 0, 3));
        vm.prank(RELAYER_ACC);
        scheme.anchorEntitlementsBatch(1, hs, us, 3, key);
    }

    // ---------------------------------------------------------------------
    // Reversal: two roles, two transactions
    // ---------------------------------------------------------------------

    /// @dev Four-eyes enforced in the contract rather than in an admin UI. That is the version
    /// that survives diligence, because a UI control is only as good as the UI.
    function test_ReversalRequiresTrusteeApproval() public {
        anchorPeriodOne();

        bytes32 narrative = keccak256("rent injected twice");

        bytes32 key = nextKey();
        vm.expectRevert(abi.encodeWithSelector(AcreSyncScheme.ReversalNotApproved.selector, 1));
        vm.prank(RELAYER_ACC);
        scheme.reversePeriod(1, 1, narrative, key);

        vm.prank(TRUSTEE_ACC);
        scheme.approveReversal(1, 1, narrative);

        vm.prank(RELAYER_ACC);
        scheme.reversePeriod(1, 1, narrative, nextKey());

        assertEq(uint8(scheme.getPeriod(1).status), uint8(AcreSyncScheme.PeriodStatus.Reversed));
    }

    /// @dev The approval is bound to an exact reason and narrative. A trustee who approved a
    /// duplicate-injection reversal has not approved a valuation restatement.
    function test_ReversalMustMatchTheApproval() public {
        anchorPeriodOne();
        bytes32 narrative = keccak256("rent injected twice");

        vm.prank(TRUSTEE_ACC);
        scheme.approveReversal(1, 1, narrative);

        bytes32 k1 = nextKey();
        vm.expectRevert(AcreSyncScheme.ReversalApprovalMismatch.selector);
        vm.prank(RELAYER_ACC);
        scheme.reversePeriod(1, 2, narrative, k1);

        bytes32 k2 = nextKey();
        vm.expectRevert(AcreSyncScheme.ReversalApprovalMismatch.selector);
        vm.prank(RELAYER_ACC);
        scheme.reversePeriod(1, 1, keccak256("different story"), k2);
    }

    function test_RelayerCannotApproveItsOwnReversal() public {
        anchorPeriodOne();
        vm.prank(RELAYER_ACC);
        vm.expectRevert(abi.encodeWithSelector(AcreSyncScheme.NotTrustee.selector, RELAYER_ACC));
        scheme.approveReversal(1, 1, keccak256("x"));
    }

    /// @dev The Class 2 / Class 3 boundary. Before payouts confirm, a mistake is corrected with
    /// a compensating entry. After, money has left the escrow and only recovery is possible.
    function test_CannotReverseOncePayoutsConfirmed() public {
        anchorPeriodOne();

        // Accrue everything so the period can reach PayoutsConfirmed.
        uint256 count = scheme.holderCount();
        uint32 cursor = 0;
        while (cursor < count) {
            uint256 size = count - cursor;
            if (size > MAX_BATCH) size = MAX_BATCH;
            address[] memory hs = new address[](size);
            uint32[] memory us = new uint32[](size);
            for (uint256 i = 0; i < size; i++) {
                address h = scheme.holderAt(cursor + i);
                hs[i] = h;
                us[i] = uint32(scheme.balanceOf(h));
            }
            vm.prank(RELAYER_ACC);
            scheme.anchorEntitlementsBatch(1, hs, us, cursor, nextKey());
            cursor += uint32(size);
        }
        vm.prank(RELAYER_ACC);
        scheme.finaliseEntitlements(1, cursor, nextKey());

        // Approval taken while reversal was still available.
        bytes32 narrative = keccak256("too late");
        vm.prank(TRUSTEE_ACC);
        scheme.approveReversal(1, 1, narrative);

        vm.prank(RELAYER_ACC);
        scheme.confirmPayouts(1, 201, 0, keccak256("report"), nextKey());

        bytes32 key = nextKey();
        vm.expectRevert(
            abi.encodeWithSelector(AcreSyncScheme.PayoutsAlreadyConfirmed.selector, 1)
        );
        vm.prank(RELAYER_ACC);
        scheme.reversePeriod(1, 1, narrative, key);
    }

    /// @dev The reversed period stays visible and a corrected one names it. An audit trail that
    /// can hide its own corrections is worth nothing.
    function test_SupersedingPeriodPointsAtTheReversedOne() public {
        anchorPeriodOne();

        bytes32 narrative = keccak256("data entry error");
        vm.prank(TRUSTEE_ACC);
        scheme.approveReversal(1, 0, narrative);
        vm.prank(RELAYER_ACC);
        scheme.reversePeriod(1, 0, narrative, nextKey());

        AcreSyncScheme.PeriodAnchorInput memory in_ = periodInput(2, NDCF, DISTRIBUTED);
        in_.supersedes = 1;
        vm.prank(RELAYER_ACC);
        scheme.anchorPeriod(in_, nextKey());

        assertEq(scheme.getPeriod(2).supersedes, 1);
        assertEq(
            uint8(scheme.getPeriod(1).status),
            uint8(AcreSyncScheme.PeriodStatus.Reversed),
            "the original stays on chain forever"
        );
    }

    /// @dev A pause is often exactly when a reversal is needed, so reversal is not gated on it.
    function test_ReversalWorksWhilePaused() public {
        anchorPeriodOne();
        bytes32 narrative = keccak256("urgent");

        vm.prank(TRUSTEE_ACC);
        scheme.approveReversal(1, 1, narrative);

        vm.prank(PAUSER_ACC);
        roles.pause(keccak256("incident"));

        vm.prank(RELAYER_ACC);
        scheme.reversePeriod(1, 1, narrative, nextKey());
        assertEq(uint8(scheme.getPeriod(1).status), uint8(AcreSyncScheme.PeriodStatus.Reversed));
    }

    /// @dev Distribution is gated on the pause; reconciliation and reversal are not. Blinding
    /// the mirror to the legal register during an emergency compounds the problem.
    function test_PauseBlocksDistributionButNotReconciliation() public {
        vm.prank(PAUSER_ACC);
        roles.pause(keccak256("incident"));

        bytes32 key = nextKey();
        vm.expectRevert(AcreSyncScheme.ContractPaused.selector);
        vm.prank(RELAYER_ACC);
        scheme.anchorPeriod(periodInput(1, NDCF, DISTRIBUTED), key);

        // Reconciliation still works: the depository is the legal register and the mirror must
        // keep following it.
        address from = scheme.holderAt(1);
        uint32 units = uint32(scheme.balanceOf(from));
        vm.prank(RELAYER_ACC);
        scheme.reconcileTransfer(from, STRANGER, units, keccak256("DEP/1"), nextKey());

        assertEq(scheme.balanceOf(STRANGER), units);
    }

    // ---------------------------------------------------------------------
    // Public verification
    // ---------------------------------------------------------------------

    /// @dev Why the snapshot root is anchored at all. An investor can confirm from a block
    /// explorer that their record-date unit count is the one the distribution used, with no
    /// access to our systems.
    function test_VerifyEntitlementAgainstSnapshotRoot() public {
        bytes32 leafA = MerkleLib.hashLeaf(abi.encodePacked("holderA", uint32(3)));
        bytes32 leafB = MerkleLib.hashLeaf(abi.encodePacked("holderB", uint32(2)));
        bytes32 root = MerkleLib.hashNode(leafA, leafB);

        AcreSyncScheme.PeriodAnchorInput memory in_ = periodInput(1, NDCF, DISTRIBUTED);
        in_.snapshotRoot = root;
        vm.prank(RELAYER_ACC);
        scheme.anchorPeriod(in_, nextKey());

        bytes32[] memory proof = new bytes32[](1);
        proof[0] = leafB;
        assertTrue(scheme.verifyEntitlement(1, leafA, proof), "an included holder must verify");

        bytes32 forged = MerkleLib.hashLeaf(abi.encodePacked("holderA", uint32(30)));
        assertFalse(scheme.verifyEntitlement(1, forged, proof), "an inflated count must not verify");
    }

    function test_VerifyReturnsFalseForUnknownPeriod() public view {
        bytes32[] memory proof = new bytes32[](0);
        assertFalse(scheme.verifyEntitlement(99, bytes32(uint256(1)), proof));
        assertFalse(scheme.distributionFloorMet(99));
    }

    // ---------------------------------------------------------------------
    // Fuzz
    // ---------------------------------------------------------------------

    /// @dev The floor holds for every ratio, not only the ones the explicit cases probe.
    function testFuzz_FloorEnforcedForAnyRatio(uint64 ndcf, uint64 distributed) public {
        ndcf = uint64(bound(ndcf, 1, 1e18));
        distributed = uint64(bound(distributed, 0, ndcf));

        bool shouldPass = uint256(distributed) * 10_000 >= uint256(ndcf) * FLOOR_BPS;

        bytes32 key = nextKey();
        if (!shouldPass) {
            vm.expectRevert();
        }
        vm.prank(RELAYER_ACC);
        scheme.anchorPeriod(periodInput(1, ndcf, distributed), key);

        if (shouldPass) {
            assertGe(scheme.getPeriod(1).bps, FLOOR_BPS);
        }
    }
}
