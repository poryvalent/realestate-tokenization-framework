// SPDX-License-Identifier: UNLICENSED
pragma solidity 0.8.28;

import {SchemeHarness} from "./SchemeHarness.sol";
import {AcreSyncScheme} from "../src/AcreSyncScheme.sol";

/// @title SchemeWalletCollisionTest
/// @notice Establishes what the contract does NOT catch, so the off-chain guard has a reason to exist.
///
/// @dev The orchestrator refuses to build a settlement batch in which two allottees share a register
/// address. That guard is not a mirror of a contract rule, because there is no contract rule to mirror,
/// and a guard with no stated justification is the kind of thing a later change removes as redundant.
///
/// settleBatch credits additively: _setBalance(holder, unitsOf[holder] + amount). Two entries naming one
/// address therefore both land on it. The cursor still advances twice because it counts array entries,
/// the units still sum correctly, and totalUnitsIssued still reaches the scheme total. Only the holder
/// array gains one member instead of two.
///
/// These tests pin down exactly when that is caught and when it is not.
contract SchemeWalletCollisionTest is SchemeHarness {
    function setUp() public {
        setUpHarness();
    }

    /// @dev Settles `count` public holders, optionally pointing entry 1 at entry 0's address.
    ///
    /// Unit shape: the first 175 entries take two units and the rest take one, which sums to 475 across
    /// 300 entries. Mirrors a real allotment at this scale, where a 200-holder floor over 475 units
    /// leaves room for very few large holdings.
    function settleWithCollision(uint32 count, bool collide) internal {
        vm.prank(RELAYER_ACC);
        scheme.beginSettlement(count, PUBLIC_UNITS, keccak256("allotment file"), nextKey());

        uint32 cursor = 0;
        while (cursor < count) {
            uint32 size = count - cursor;
            if (size > MAX_BATCH) size = MAX_BATCH;

            address[] memory hs = new address[](size);
            uint32[] memory us = new uint32[](size);

            for (uint32 i = 0; i < size; i++) {
                uint32 idx = cursor + i;

                // The collision: entry 1 is credited to entry 0's address.
                if (collide && idx == 1) {
                    hs[i] = holderAddr(0);
                } else {
                    hs[i] = holderAddr(idx);
                }

                us[i] = idx < 175 ? 2 : 1;
            }

            vm.prank(RELAYER_ACC);
            scheme.settleBatch(hs, us, cursor, nextKey());
            cursor += size;
        }
    }

    /// @notice A clean 300-holder settlement finalises, and the counts are what they should be.
    ///
    /// @dev The control case. Without it, the collision test below could pass because the fixture was
    /// broken rather than because the contract accepted a defective register.
    function test_CleanSettlementFinalises() public {
        reachAllocated();
        settleWithCollision(300, false);

        assertEq(scheme.totalUnitsIssued(), TOTAL_UNITS, "475 public plus the manager's 25");
        assertEq(scheme.distinctHolderCount(), 300, "300 countable holders, manager excluded");
        assertEq(scheme.balanceOf(holderAddr(0)), 2, "holder 0 holds its own two units");

        vm.prank(RELAYER_ACC);
        scheme.finaliseSettlement(nextKey());

        assertEq(uint8(scheme.status()), uint8(AcreSyncScheme.Status.Settled), "settled");
    }

    /// @notice A duplicated wallet passes all five finalisation checks. This is the hole.
    ///
    /// @dev Every check holds: the issued total is exactly right, the holder count is merely one lower
    /// and still far above the floor, the manager's holding is untouched, and credited units and holders
    /// both equal what was declared. The scheme reaches Settled with a register that has 299 holders
    /// where the allotment file says 300, and with two investors' units commingled in one address where
    /// neither can be individually credited or paid.
    ///
    /// Nothing here is a contract bug. The contract cannot know that two array entries were meant to be
    /// two different people, because an address is all it is given. That is precisely why the check has
    /// to live where the investor identities still exist, which is off-chain.
    function test_DuplicateWalletStillFinalises() public {
        reachAllocated();
        settleWithCollision(300, true);

        // The register lost a holder.
        assertEq(scheme.distinctHolderCount(), 299, "two entries collapsed into one address");

        // And the collided address holds both allotments.
        assertEq(scheme.balanceOf(holderAddr(0)), 4, "holder 0 holds its own two units and holder 1's");
        assertEq(scheme.balanceOf(holderAddr(1)), 0, "holder 1 was never credited anything");

        // Yet every finalisation invariant is satisfied.
        assertEq(scheme.totalUnitsIssued(), TOTAL_UNITS, "check 1: the issued total is still correct");
        assertTrue(scheme.distinctHolderCount() >= MIN_HOLDERS, "check 2: still above the floor");
        assertEq(scheme.balanceOf(IM_WALLET), IM_UNITS, "check 3: the manager is untouched");

        vm.prank(RELAYER_ACC);
        scheme.finaliseSettlement(nextKey());

        assertEq(
            uint8(scheme.status()),
            uint8(AcreSyncScheme.Status.Settled),
            "the scheme settles on a register that silently lost a holder"
        );
    }

    /// @notice At exactly the holder floor, the same collision IS caught.
    ///
    /// @dev The refinement that matters. Losing one holder from 300 leaves 299, comfortably above the
    /// floor. Losing one from exactly 200 leaves 199 and the second check fails. So the contract catches
    /// a collision only when the allotment happens to sit on the floor, which is the one case an
    /// operator cannot rely on. The off-chain guard is what covers the rest.
    function test_DuplicateWalletIsCaughtOnlyAtTheFloor() public {
        reachAllocated();

        vm.prank(RELAYER_ACC);
        scheme.beginSettlement(200, PUBLIC_UNITS, keccak256("allotment file"), nextKey());

        // 200 entries summing to 475: the first 75 take three units, the rest take two.
        // 75*3 + 125*2 = 225 + 250 = 475.
        uint32 cursor = 0;
        while (cursor < 200) {
            uint32 size = 200 - cursor;
            if (size > MAX_BATCH) size = MAX_BATCH;

            address[] memory hs = new address[](size);
            uint32[] memory us = new uint32[](size);

            for (uint32 i = 0; i < size; i++) {
                uint32 idx = cursor + i;
                hs[i] = idx == 1 ? holderAddr(0) : holderAddr(idx);
                us[i] = idx < 75 ? 3 : 2;
            }

            vm.prank(RELAYER_ACC);
            scheme.settleBatch(hs, us, cursor, nextKey());
            cursor += size;
        }

        assertEq(scheme.totalUnitsIssued(), TOTAL_UNITS, "the units still reconcile exactly");
        assertEq(scheme.distinctHolderCount(), 199, "but the register is one holder short");

        // Here, and only here, the contract refuses.
        bytes32 key = nextKey();
        vm.expectRevert(
            abi.encodeWithSelector(AcreSyncScheme.HolderFloorNotMet.selector, MIN_HOLDERS, 199)
        );
        vm.prank(RELAYER_ACC);
        scheme.finaliseSettlement(key);
    }
}
