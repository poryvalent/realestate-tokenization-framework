// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {SchemeHarness} from "./SchemeHarness.sol";
import {AcreSyncScheme} from "../src/AcreSyncScheme.sol";

/// @notice Settlement: the chunked cursor and the permissionless finalisation gate.
contract SchemeSettlementTest is SchemeHarness {
    function setUp() public {
        setUpHarness();
    }

    // ---------------------------------------------------------------------
    // The happy path at real scale
    // ---------------------------------------------------------------------

    /// @dev 200 holders sharing 475 units across three batches, which is the shape a real
    /// allotment takes when the holder floor and the unit count are this close together.
    function test_SettlesTwoHundredHolders() public {
        reachAllocated();
        settleHolders(200);

        assertEq(scheme.totalUnitsIssued(), TOTAL_UNITS, "IM 25 plus public 475");
        assertEq(scheme.distinctHolderCount(), 200, "the manager is excluded from the count");

        vm.prank(RELAYER_ACC);
        scheme.finaliseSettlement(nextKey());

        assertEq(uint8(scheme.status()), uint8(AcreSyncScheme.Status.Settled));
        (AcreSyncScheme.SettlementStage stage,,,,,,) = scheme.settlement();
        assertEq(uint8(stage), uint8(AcreSyncScheme.SettlementStage.Finalised));
    }

    /// @dev The manager holds units and earns on them, but does not count toward the 200.
    /// Conflating the two would let a scheme list with 199 real investors.
    function test_ManagerIsExcludedFromTheHolderCount() public {
        reachAllocated();
        assertEq(scheme.unitsOf(IM_WALLET), IM_UNITS);
        assertTrue(scheme.excludedFromCount(IM_WALLET));
        assertEq(scheme.distinctHolderCount(), 0, "only the manager holds, and it does not count");
        assertEq(scheme.holderCount(), 1, "but it is in the holder array");
    }

    // ---------------------------------------------------------------------
    // Cursor discipline
    // ---------------------------------------------------------------------

    /// @dev Strict equality is the whole safety property: batches cannot overlap, skip, or
    /// be reordered.
    function test_CursorMustMatchExactly() public {
        reachAllocated();
        vm.prank(RELAYER_ACC);
        scheme.beginSettlement(3, PUBLIC_UNITS, keccak256("file"), nextKey());

        address[] memory hs = new address[](1);
        uint32[] memory us = new uint32[](1);
        hs[0] = holderAddr(0);
        us[0] = 1;

        // Skipping ahead is refused.
        bytes32 k1 = nextKey();
        vm.expectRevert(abi.encodeWithSelector(AcreSyncScheme.CursorMismatch.selector, 0, 1));
        vm.prank(RELAYER_ACC);
        scheme.settleBatch(hs, us, 1, k1);

        // The correct cursor is accepted.
        vm.prank(RELAYER_ACC);
        scheme.settleBatch(hs, us, 0, nextKey());

        // Replaying the same cursor is refused even with a fresh key.
        hs[0] = holderAddr(1);
        bytes32 k2 = nextKey();
        vm.expectRevert(abi.encodeWithSelector(AcreSyncScheme.CursorMismatch.selector, 1, 0));
        vm.prank(RELAYER_ACC);
        scheme.settleBatch(hs, us, 0, k2);
    }

    /// @dev A batch that ran out of gas did not advance the cursor, so it is retryable
    /// verbatim. A batch that succeeded is rejected twice over: once by the idempotency key,
    /// once by the cursor.
    function test_SucceededBatchCannotBeReplayed() public {
        reachAllocated();
        vm.prank(RELAYER_ACC);
        scheme.beginSettlement(2, PUBLIC_UNITS, keccak256("file"), nextKey());

        address[] memory hs = new address[](1);
        uint32[] memory us = new uint32[](1);
        hs[0] = holderAddr(0);
        us[0] = 1;

        bytes32 key = nextKey();
        vm.prank(RELAYER_ACC);
        scheme.settleBatch(hs, us, 0, key);

        vm.expectRevert(abi.encodeWithSelector(AcreSyncScheme.KeyAlreadyUsed.selector, key));
        vm.prank(RELAYER_ACC);
        scheme.settleBatch(hs, us, 0, key);
    }

    function test_BatchSizeCapEnforced() public {
        reachAllocated();
        vm.prank(RELAYER_ACC);
        scheme.beginSettlement(200, PUBLIC_UNITS, keccak256("file"), nextKey());

        uint32 over = MAX_BATCH + 1;
        address[] memory hs = new address[](over);
        uint32[] memory us = new uint32[](over);
        for (uint32 i = 0; i < over; i++) {
            hs[i] = holderAddr(i);
            us[i] = 1;
        }

        bytes32 key = nextKey();
        vm.expectRevert(abi.encodeWithSelector(AcreSyncScheme.BatchTooLarge.selector, MAX_BATCH));
        vm.prank(RELAYER_ACC);
        scheme.settleBatch(hs, us, 0, key);
    }

    function test_MismatchedArraysRejected() public {
        reachAllocated();
        vm.prank(RELAYER_ACC);
        scheme.beginSettlement(2, PUBLIC_UNITS, keccak256("file"), nextKey());

        address[] memory hs = new address[](2);
        uint32[] memory us = new uint32[](1);
        hs[0] = holderAddr(0);
        hs[1] = holderAddr(1);
        us[0] = 1;

        bytes32 key = nextKey();
        vm.expectRevert(AcreSyncScheme.ArrayLengthMismatch.selector);
        vm.prank(RELAYER_ACC);
        scheme.settleBatch(hs, us, 0, key);
    }

    function test_EmptyBatchRejected() public {
        reachAllocated();
        vm.prank(RELAYER_ACC);
        scheme.beginSettlement(2, PUBLIC_UNITS, keccak256("file"), nextKey());

        address[] memory hs = new address[](0);
        uint32[] memory us = new uint32[](0);

        bytes32 key = nextKey();
        vm.expectRevert(AcreSyncScheme.EmptyBatch.selector);
        vm.prank(RELAYER_ACC);
        scheme.settleBatch(hs, us, 0, key);
    }

    /// @dev Over-issuing must abort the whole batch rather than partially apply and advance
    /// the cursor past the breach.
    function test_CannotIssueBeyondTotalUnits() public {
        reachAllocated();
        vm.prank(RELAYER_ACC);
        scheme.beginSettlement(1, PUBLIC_UNITS, keccak256("file"), nextKey());

        address[] memory hs = new address[](1);
        uint32[] memory us = new uint32[](1);
        hs[0] = holderAddr(0);
        us[0] = PUBLIC_UNITS + 1;

        bytes32 key = nextKey();
        vm.expectRevert();
        vm.prank(RELAYER_ACC);
        scheme.settleBatch(hs, us, 0, key);

        assertEq(scheme.totalUnitsIssued(), IM_UNITS, "no units credited by a failed batch");
    }

    // ---------------------------------------------------------------------
    // The finalisation gate
    // ---------------------------------------------------------------------

    /// @dev The answer to "so you control the final cap table". Anyone can call this; the
    /// contract accepts it only when all five invariants hold, so the cap table is governed by
    /// arithmetic rather than by whoever holds a key.
    function test_FinalisationIsPermissionless() public {
        reachAllocated();
        settleHolders(200);

        vm.prank(STRANGER);
        scheme.finaliseSettlement(nextKey());

        assertEq(uint8(scheme.status()), uint8(AcreSyncScheme.Status.Settled));
    }

    function test_FinalisationRefusedBelowHolderFloor() public {
        reachAllocated();
        settleHolders(199);

        bytes32 key = nextKey();
        vm.expectRevert(
            abi.encodeWithSelector(AcreSyncScheme.HolderFloorNotMet.selector, MIN_HOLDERS, 199)
        );
        vm.prank(STRANGER);
        scheme.finaliseSettlement(key);

        assertEq(uint8(scheme.status()), uint8(AcreSyncScheme.Status.Allocated), "status unchanged");
    }

    /// @dev A failed finalisation leaves the stage InProgress so the operator can correct and
    /// retry. There is no abort path and none is needed: the contract custodies nothing.
    function test_FailedFinalisationIsRetryable() public {
        reachAllocated();
        settleHolders(199);

        bytes32 k1 = nextKey();
        vm.expectRevert();
        vm.prank(RELAYER_ACC);
        scheme.finaliseSettlement(k1);

        (AcreSyncScheme.SettlementStage stage,,,,,,) = scheme.settlement();
        assertEq(uint8(stage), uint8(AcreSyncScheme.SettlementStage.InProgress));

        // Correcting the shortfall: move one unit to a 200th holder.
        vm.prank(RELAYER_ACC);
        scheme.correctHolding(holderAddr(0), 1, 0, keccak256("reduce"), nextKey());
        vm.prank(RELAYER_ACC);
        scheme.correctHolding(holderAddr(999), 1, 0, keccak256("add 200th"), nextKey());

        assertEq(scheme.distinctHolderCount(), 200);
    }

    function test_FinalisationRefusedIfUnitsShort() public {
        reachAllocated();
        vm.prank(RELAYER_ACC);
        scheme.beginSettlement(1, PUBLIC_UNITS, keccak256("file"), nextKey());

        address[] memory hs = new address[](1);
        uint32[] memory us = new uint32[](1);
        hs[0] = holderAddr(0);
        us[0] = 100; // far short of 475

        vm.prank(RELAYER_ACC);
        scheme.settleBatch(hs, us, 0, nextKey());

        bytes32 key = nextKey();
        vm.expectRevert(
            abi.encodeWithSelector(
                AcreSyncScheme.TotalUnitsMismatch.selector, TOTAL_UNITS, IM_UNITS + 100
            )
        );
        vm.prank(RELAYER_ACC);
        scheme.finaliseSettlement(key);
    }

    function test_SettlementRequiresAnchoredBallot() public {
        // Reach Allocated without running the ballot.
        advance(AcreSyncScheme.Status.SpvFormed);
        anchorDoc(AcreSyncScheme.DocType.Valuation);
        advance(AcreSyncScheme.Status.Valued);
        advance(AcreSyncScheme.Status.Filed);
        advance(AcreSyncScheme.Status.OfferApproved);
        anchorDoc(AcreSyncScheme.DocType.TrusteeEscrow);
        anchorDoc(AcreSyncScheme.DocType.OfferDocument);
        vm.prank(RELAYER_ACC);
        scheme.recordImSubscription(IM_WALLET, IM_UNITS, nextKey());
        advance(AcreSyncScheme.Status.OfferOpen);
        advance(AcreSyncScheme.Status.OfferClosed);
        advance(AcreSyncScheme.Status.Allocated);

        bytes32 key = nextKey();
        vm.expectRevert(AcreSyncScheme.BallotNotAnchored.selector);
        vm.prank(RELAYER_ACC);
        scheme.beginSettlement(200, PUBLIC_UNITS, keccak256("file"), key);
    }

    /// @dev Binding the settlement to the draw means this cap table is permanently associated
    /// with one specific ballot outcome.
    function test_SettlementRecordsTheBallotResultRoot() public {
        reachAllocated();
        vm.prank(RELAYER_ACC);
        scheme.beginSettlement(200, PUBLIC_UNITS, keccak256("file"), nextKey());

        (,,,,, , bytes32 recordedRoot) = scheme.settlement();
        assertEq(recordedRoot, RESULT_ROOT);
    }

    function test_SettlementCannotBeRestarted() public {
        reachAllocated();
        vm.prank(RELAYER_ACC);
        scheme.beginSettlement(200, PUBLIC_UNITS, keccak256("file"), nextKey());

        bytes32 key = nextKey();
        vm.expectRevert(AcreSyncScheme.SettlementAlreadyFinalised.selector);
        vm.prank(RELAYER_ACC);
        scheme.beginSettlement(200, PUBLIC_UNITS, keccak256("file2"), key);
    }

    // ---------------------------------------------------------------------
    // Register bookkeeping
    // ---------------------------------------------------------------------

    /// @dev The holder array, the distinct count and the issued total all derive from one
    /// place. If they could drift, finaliseSettlement would be checking them against each
    /// other and passing on inconsistent data.
    function test_HolderArrayStaysConsistent() public {
        reachAllocated();
        settleHolders(200);

        assertEq(scheme.holderCount(), 201, "200 public plus the manager");

        uint32 sum = 0;
        for (uint256 i = 0; i < scheme.holderCount(); i++) {
            sum += uint32(scheme.balanceOf(scheme.holderAt(i)));
        }
        assertEq(sum, scheme.totalUnitsIssued(), "array must account for every issued unit");
    }

    /// @dev Reducing a holder to zero removes them from the array and the count, and the
    /// swap-and-pop must not corrupt another holder's position.
    function test_ZeroingAHolderRemovesThemCleanly() public {
        reachAllocated();
        settleHolders(200);

        uint256 before = scheme.holderCount();
        address victim = holderAddr(5);
        uint32 victimUnits = uint32(scheme.balanceOf(victim));

        vm.prank(RELAYER_ACC);
        scheme.correctHolding(victim, 0, 0, keccak256("depository correction"), nextKey());

        assertEq(scheme.holderCount(), before - 1);
        assertEq(scheme.distinctHolderCount(), 199);
        assertEq(scheme.balanceOf(victim), 0);

        uint32 sum = 0;
        for (uint256 i = 0; i < scheme.holderCount(); i++) {
            sum += uint32(scheme.balanceOf(scheme.holderAt(i)));
        }
        assertEq(sum, scheme.totalUnitsIssued(), "swap and pop must not lose a holder");
        assertEq(scheme.totalUnitsIssued(), TOTAL_UNITS - victimUnits);
    }

    // ---------------------------------------------------------------------
    // The deliberately absent ERC20 surface
    // ---------------------------------------------------------------------

    /// @dev Units are indivisible. A Demat account cannot hold a fraction, so a non-zero
    /// decimals value would let the ledger represent a state that cannot legally exist.
    function test_DecimalsIsZero() public view {
        assertEq(scheme.decimals(), 0);
    }

    /// @dev transfer, approve, transferFrom and allowance are absent rather than reverting.
    /// Omission means there is no selector for an aggregator to call, so a user-initiated
    /// movement is impossible by construction rather than by guard.
    function test_NoTransferFunctionExists() public {
        reachAllocated();

        (bool ok,) = address(scheme).call(
            abi.encodeWithSignature("transfer(address,uint256)", STRANGER, 1)
        );
        assertFalse(ok, "transfer must not exist");

        (ok,) = address(scheme).call(
            abi.encodeWithSignature("approve(address,uint256)", STRANGER, 1)
        );
        assertFalse(ok, "approve must not exist");

        (ok,) = address(scheme).call(
            abi.encodeWithSignature("transferFrom(address,address,uint256)", IM_WALLET, STRANGER, 1)
        );
        assertFalse(ok, "transferFrom must not exist");

        (ok,) = address(scheme).call(abi.encodeWithSignature("allowance(address,address)", IM_WALLET, STRANGER));
        assertFalse(ok, "allowance must not exist");
    }

    function test_ReadSurfaceWorks() public {
        reachAllocated();
        assertEq(scheme.totalSupply(), IM_UNITS);
        assertEq(scheme.balanceOf(IM_WALLET), IM_UNITS);
        assertEq(scheme.name(), "AcreSync Scheme Unit");
        assertEq(scheme.symbol(), "ACRE-U");
    }

    // ---------------------------------------------------------------------
    // Fuzz
    // ---------------------------------------------------------------------

    function testFuzz_CursorMustBeExact(uint32 wrongCursor) public {
        vm.assume(wrongCursor != 0 && wrongCursor < 100_000);

        reachAllocated();
        vm.prank(RELAYER_ACC);
        scheme.beginSettlement(5, PUBLIC_UNITS, keccak256("file"), nextKey());

        address[] memory hs = new address[](1);
        uint32[] memory us = new uint32[](1);
        hs[0] = holderAddr(0);
        us[0] = 1;

        bytes32 key = nextKey();
        vm.expectRevert(
            abi.encodeWithSelector(AcreSyncScheme.CursorMismatch.selector, 0, wrongCursor)
        );
        vm.prank(RELAYER_ACC);
        scheme.settleBatch(hs, us, wrongCursor, key);
    }
}
