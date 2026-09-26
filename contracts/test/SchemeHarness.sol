// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {Test} from "forge-std/Test.sol";
import {AcreSyncRoles} from "../src/AcreSyncRoles.sol";
import {AcreSyncBallot} from "../src/AcreSyncBallot.sol";
import {AcreSyncScheme} from "../src/AcreSyncScheme.sol";
import {BallotEncoding} from "../src/BallotEncoding.sol";

/// @title SchemeHarness
/// @notice Shared fixture for the AcreSyncScheme suites.
///
/// @dev Deploys the three contracts wired together and provides helpers to drive a scheme to
/// each interesting state. Kept separate because the scheme has enough distinct concerns
/// (lifecycle, register, settlement, distribution, reversal) that one test file would be
/// unreadable, and they all need the same setup.
abstract contract SchemeHarness is Test {
    AcreSyncRoles internal roles;
    AcreSyncBallot internal ballot;
    AcreSyncScheme internal scheme;

    address internal constant ADMIN_ACC = address(0xA1);
    address internal constant RELAYER_ACC = address(0xB2);
    address internal constant PAUSER_ACC = address(0xC3);
    address internal constant TRUSTEE_ACC = address(0xD4);
    address internal constant STRANGER = address(0xE5);
    address internal constant IM_WALLET = address(0x1111);

    // The v1 scheme: 500 units at ten lakh rupees is fifty crore, the band floor.
    bytes32 internal constant SCHEME_REF = bytes32("SEBI/SM-REIT/2026/001");
    uint64 internal constant UNIT_PRICE = 100_000_000; // ₹10 lakh in paise
    uint32 internal constant TOTAL_UNITS = 500;
    uint32 internal constant IM_UNITS = 25; // 5% for an unleveraged scheme
    uint32 internal constant PUBLIC_UNITS = 475;
    uint16 internal constant MIN_HOLDERS = 200;
    uint16 internal constant FLOOR_BPS = 9500;
    uint32 internal constant MAX_BATCH = 100;

    // NDCF ₹30 crore, 95% of which is ₹28.5 crore.
    uint64 internal constant NDCF = 30_000_000_000;
    uint64 internal constant DISTRIBUTED = 28_500_000_000;

    bytes32 internal constant SECRET = keccak256("ballot secret");
    bytes32 internal constant BOOK_ROOT = bytes32(uint256(0xB00C));
    bytes32 internal constant RESULT_ROOT = bytes32(uint256(0x8E5017));
    bytes32 internal constant SNAPSHOT_ROOT = bytes32(uint256(0x5A0A));

    uint256 internal keyNonce;

    /// @dev Cached register totals, refreshed explicitly by refreshSnapshot.
    ///
    /// periodInput must not make external calls. `vm.prank` applies to the next call, and a
    /// view call like `scheme.totalUnitsIssued()` evaluated while building a function argument
    /// would spend the prank before the real call happens, so the operation would run as the
    /// test contract and revert NotRelayer. Reading cached state variables is an SLOAD rather
    /// than a call and leaves the prank intact.
    uint32 internal snapUnits;
    uint16 internal snapHolders;

    function setUpHarness() internal {
        roles = new AcreSyncRoles(
            ADMIN_ACC, RELAYER_ACC, PAUSER_ACC, TRUSTEE_ACC, 1 hours, true, 1
        );
        ballot = new AcreSyncBallot(roles, 10, 200, 3);
        scheme = new AcreSyncScheme(
            roles, ballot, SCHEME_REF, UNIT_PRICE, TOTAL_UNITS, IM_UNITS,
            MIN_HOLDERS, FLOOR_BPS, MAX_BATCH
        );
        vm.roll(1000);
    }

    function nextKey() internal returns (bytes32) {
        keyNonce++;
        return keccak256(abi.encodePacked("harness", keyNonce));
    }

    /// @dev A distinct address per index, avoiding precompiles and the zero address.
    function holderAddr(uint256 i) internal pure returns (address) {
        return address(uint160(0x10000 + i));
    }

    function anchorDoc(AcreSyncScheme.DocType d) internal {
        vm.prank(RELAYER_ACC);
        scheme.setDocAnchor(uint8(d), keccak256(abi.encodePacked("doc", uint8(d))), nextKey());
    }

    function advance(AcreSyncScheme.Status target) internal {
        vm.prank(RELAYER_ACC);
        scheme.advanceStatus(target, nextKey());
    }

    /// @dev Drives the scheme to Allocated with the ballot result anchored, which is the
    /// precondition for settlement.
    function reachAllocated() internal {
        advance(AcreSyncScheme.Status.SpvFormed);
        anchorDoc(AcreSyncScheme.DocType.Valuation);
        advance(AcreSyncScheme.Status.Valued);
        advance(AcreSyncScheme.Status.Filed);
        advance(AcreSyncScheme.Status.OfferApproved);

        anchorDoc(AcreSyncScheme.DocType.TrusteeEscrow);
        anchorDoc(AcreSyncScheme.DocType.OfferDocument);

        // The manager subscribes before the public ballot so the allocation engine starts
        // from a clean 475-unit pool.
        vm.prank(RELAYER_ACC);
        scheme.recordImSubscription(IM_WALLET, IM_UNITS, nextKey());

        advance(AcreSyncScheme.Status.OfferOpen);
        advance(AcreSyncScheme.Status.OfferClosed);

        runBallot();

        advance(AcreSyncScheme.Status.Allocated);
    }

    /// @dev Full commit-reveal ceremony. Digests are computed before any prank because sha256
    /// is a precompile and a staticcall to it would consume the prank.
    function runBallot() internal {
        bytes32 commitment = BallotEncoding.commitmentOf(SECRET);

        vm.prank(RELAYER_ACC);
        ballot.anchorBidbook(BOOK_ROOT, bytes32(uint256(0xC1D)), 900, 900, 900, nextKey());

        vm.prank(RELAYER_ACC);
        ballot.commitSeed(commitment, nextKey());

        vm.roll(ballot.targetBlock() + 1);

        vm.prank(RELAYER_ACC);
        ballot.revealSeed(SECRET, nextKey());

        vm.prank(RELAYER_ACC);
        ballot.anchorBallotResult(
            RESULT_ROOT, bytes32(uint256(0xC1D2)), PUBLIC_UNITS, 200,
            PUBLIC_UNITS, MIN_HOLDERS, 1, nextKey()
        );
    }

    /// @dev Credits `count` holders with units summing to PUBLIC_UNITS, in batches.
    ///
    /// Shape mirrors a real allotment at this scale: with 475 units and a 200-holder floor
    /// most allottees receive one or two units, because there is no room for more.
    function settleHolders(uint32 count) internal {
        vm.prank(RELAYER_ACC);
        scheme.beginSettlement(count, PUBLIC_UNITS, keccak256("allotment file"), nextKey());

        uint32 remaining = PUBLIC_UNITS;
        uint32 cursor = 0;

        while (cursor < count) {
            uint32 size = count - cursor;
            if (size > MAX_BATCH) size = MAX_BATCH;

            address[] memory hs = new address[](size);
            uint32[] memory us = new uint32[](size);

            for (uint32 i = 0; i < size; i++) {
                uint32 idx = cursor + i;
                hs[i] = holderAddr(idx);
                // The last holder absorbs whatever is left so the total lands exactly.
                if (idx == count - 1) {
                    us[i] = remaining;
                } else {
                    uint32 give = remaining - (count - 1 - idx);
                    us[i] = give > 2 ? 2 : 1;
                }
                remaining -= us[i];
            }

            vm.prank(RELAYER_ACC);
            scheme.settleBatch(hs, us, cursor, nextKey());
            cursor += size;
        }
    }

    /// @dev Drives the scheme all the way to Operational with `count` public holders.
    function reachOperational(uint32 count) internal {
        reachAllocated();
        settleHolders(count);

        vm.prank(RELAYER_ACC);
        scheme.finaliseSettlement(nextKey());

        // The manager's two-year lock-in runs from listing, so the expiry can only be written
        // now rather than at subscription.
        vm.prank(RELAYER_ACC);
        scheme.setLockIn(IM_WALLET, uint64(block.timestamp + 730 days), nextKey());

        advance(AcreSyncScheme.Status.Listed);
        advance(AcreSyncScheme.Status.Operational);

        refreshSnapshot();
    }

    /// @dev Re-reads the register totals into the cache. Call after any change to holdings.
    function refreshSnapshot() internal {
        snapUnits = scheme.totalUnitsIssued();
        snapHolders = scheme.distinctHolderCount();
    }

    function periodInput(uint32 periodId, uint64 ndcf, uint64 distributed)
        internal
        view
        returns (AcreSyncScheme.PeriodAnchorInput memory)
    {
        return AcreSyncScheme.PeriodAnchorInput({
            periodId: periodId,
            recordDate: uint64(block.timestamp),
            ndcfPaise: ndcf,
            distributedPaise: distributed,
            snapshotTotalUnits: snapUnits,
            snapshotHolders: snapHolders,
            supersedes: 0,
            statementHash: keccak256("ndcf statement"),
            snapshotRoot: SNAPSHOT_ROOT,
            ndcfCidDigest: keccak256("ndcf cid")
        });
    }
}
