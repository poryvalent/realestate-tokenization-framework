// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {Test} from "forge-std/Test.sol";
import {AcreSyncRoles} from "../src/AcreSyncRoles.sol";

contract AcreSyncRolesTest is Test {
    AcreSyncRoles internal roles;

    address internal constant ADMIN_ACC = address(0xA1);
    address internal constant RELAYER_ACC = address(0xB2);
    address internal constant PAUSER_ACC = address(0xC3);
    address internal constant TRUSTEE_ACC = address(0xD4);
    address internal constant STRANGER = address(0xE5);
    address internal constant NEW_RELAYER = address(0xF6);

    uint32 internal constant TIMELOCK = 1 hours;

    // Role identifiers are cached rather than read through `RELAYER_ROLE` at each use.
    //
    // vm.prank applies to exactly the next call, and a public constant getter is a call.
    // Reading the identifier inside a pranked sequence therefore consumes the prank and
    // the operation that follows runs as the test contract, which shows up as a confusing
    // NotAdmin revert rather than as the mistake it is.
    bytes32 internal RELAYER_ROLE;
    bytes32 internal ADMIN_ROLE;
    bytes32 internal PAUSER_ROLE;
    bytes32 internal TRUSTEE_ROLE;

    function setUp() public {
        roles = new AcreSyncRoles(
            ADMIN_ACC, RELAYER_ACC, PAUSER_ACC, TRUSTEE_ACC, TIMELOCK, true, 1
        );

        AcreSyncRoles r = roles;
        RELAYER_ROLE = r.RELAYER();
        ADMIN_ROLE = r.ADMIN();
        PAUSER_ROLE = r.PAUSER();
        TRUSTEE_ROLE = r.TRUSTEE();
    }

    // ---------------------------------------------------------------------
    // Construction
    // ---------------------------------------------------------------------

    function test_InitialRoles() public view {
        assertEq(roles.admin(), ADMIN_ACC);
        assertEq(roles.relayer(), RELAYER_ACC);
        assertEq(roles.pauser(), PAUSER_ACC);
        assertEq(roles.trustee(), TRUSTEE_ACC);
        assertTrue(roles.isRelayer(RELAYER_ACC));
        assertTrue(roles.isTrustee(TRUSTEE_ACC));
        assertFalse(roles.isRelayer(STRANGER));
    }

    function test_SimulationFlagsAreImmutable() public view {
        assertTrue(roles.isSimulation(), "the demo deployment must be marked as simulation");
        assertEq(roles.environmentTag(), 1);
        assertEq(roles.roleTimelockSeconds(), TIMELOCK);
    }

    function test_ConstructorRejectsZeroAddress() public {
        vm.expectRevert(AcreSyncRoles.ZeroAddress.selector);
        new AcreSyncRoles(address(0), RELAYER_ACC, PAUSER_ACC, TRUSTEE_ACC, TIMELOCK, true, 1);

        vm.expectRevert(AcreSyncRoles.ZeroAddress.selector);
        new AcreSyncRoles(ADMIN_ACC, address(0), PAUSER_ACC, TRUSTEE_ACC, TIMELOCK, true, 1);
    }

    /// @dev The zero address must never satisfy a role check. After revokeRelayer the
    /// stored relayer is zero, and a call from address(0) is not reachable in practice,
    /// but a role check that returned true for it would be a latent hole.
    function test_ZeroAddressNeverHoldsARole() public view {
        assertFalse(roles.isRelayer(address(0)));
        assertFalse(roles.isTrustee(address(0)));
    }

    // ---------------------------------------------------------------------
    // Timelocked grants
    // ---------------------------------------------------------------------

    function test_GrantRequiresTimelock() public {
        vm.prank(ADMIN_ACC);
        roles.proposeRoleChange(RELAYER_ROLE, NEW_RELAYER);

        // Still the old relayer until the delay elapses. This is what stops a compromised
        // admin key from instantly installing a hostile relayer.
        assertEq(roles.relayer(), RELAYER_ACC);

        vm.prank(ADMIN_ACC);
        vm.expectRevert(
            abi.encodeWithSelector(
                AcreSyncRoles.RoleChangeNotReady.selector,
                RELAYER_ROLE,
                uint64(block.timestamp + TIMELOCK),
                uint64(block.timestamp)
            )
        );
        roles.executeRoleChange(RELAYER_ROLE);

        vm.warp(block.timestamp + TIMELOCK);

        vm.prank(ADMIN_ACC);
        roles.executeRoleChange(RELAYER_ROLE);
        assertEq(roles.relayer(), NEW_RELAYER);
    }

    function test_ExecuteAtExactEtaSucceeds() public {
        vm.prank(ADMIN_ACC);
        roles.proposeRoleChange(TRUSTEE_ROLE, STRANGER);

        (, uint64 eta) = roles.pendingChange(TRUSTEE_ROLE);
        vm.warp(eta);

        vm.prank(ADMIN_ACC);
        roles.executeRoleChange(TRUSTEE_ROLE);
        assertEq(roles.trustee(), STRANGER);
    }

    function test_CancelProposal() public {
        vm.prank(ADMIN_ACC);
        roles.proposeRoleChange(RELAYER_ROLE, NEW_RELAYER);

        vm.prank(ADMIN_ACC);
        roles.cancelRoleChange(RELAYER_ROLE);

        vm.warp(block.timestamp + TIMELOCK + 1);
        vm.prank(ADMIN_ACC);
        vm.expectRevert(
            abi.encodeWithSelector(AcreSyncRoles.NoPendingRoleChange.selector, RELAYER_ROLE)
        );
        roles.executeRoleChange(RELAYER_ROLE);

        assertEq(roles.relayer(), RELAYER_ACC);
    }

    function test_ProposalWithNoPendingChangeReverts() public {
        vm.prank(ADMIN_ACC);
        vm.expectRevert(
            abi.encodeWithSelector(AcreSyncRoles.NoPendingRoleChange.selector, PAUSER_ROLE)
        );
        roles.executeRoleChange(PAUSER_ROLE);
    }

    function test_UnknownRoleRejected() public {
        bytes32 bogus = keccak256("acresync.role.treasurer");
        vm.prank(ADMIN_ACC);
        vm.expectRevert(abi.encodeWithSelector(AcreSyncRoles.UnknownRole.selector, bogus));
        roles.proposeRoleChange(bogus, STRANGER);
    }

    function test_ProposingTheIncumbentReverts() public {
        vm.prank(ADMIN_ACC);
        vm.expectRevert(
            abi.encodeWithSelector(
                AcreSyncRoles.SameAccount.selector, RELAYER_ROLE, RELAYER_ACC
            )
        );
        roles.proposeRoleChange(RELAYER_ROLE, RELAYER_ACC);
    }

    // ---------------------------------------------------------------------
    // The asymmetry
    // ---------------------------------------------------------------------

    /// @dev The central security property. A timelock on revocation would guarantee an
    /// attacker holding the relayer key a window in which nobody can stop them.
    function test_RevokeRelayerIsImmediate() public {
        vm.prank(ADMIN_ACC);
        roles.revokeRelayer();

        assertEq(roles.relayer(), address(0));
        assertFalse(roles.isRelayer(RELAYER_ACC));
    }

    /// @dev Revoking in an emergency while leaving a grant that becomes executable minutes
    /// later would defeat the purpose entirely.
    function test_RevokeDropsAPendingGrant() public {
        vm.prank(ADMIN_ACC);
        roles.proposeRoleChange(RELAYER_ROLE, NEW_RELAYER);

        vm.prank(ADMIN_ACC);
        roles.revokeRelayer();

        vm.warp(block.timestamp + TIMELOCK + 1);
        vm.prank(ADMIN_ACC);
        vm.expectRevert(
            abi.encodeWithSelector(AcreSyncRoles.NoPendingRoleChange.selector, RELAYER_ROLE)
        );
        roles.executeRoleChange(RELAYER_ROLE);

        assertEq(roles.relayer(), address(0), "relayer should still be revoked");
    }

    /// @dev Restoring service after a revocation must go through the timelock, so an
    /// attacker who also holds the admin key cannot simply reinstate themselves.
    function test_ReinstatingARelayerIsStillTimelocked() public {
        vm.prank(ADMIN_ACC);
        roles.revokeRelayer();

        vm.prank(ADMIN_ACC);
        roles.proposeRoleChange(RELAYER_ROLE, NEW_RELAYER);

        vm.prank(ADMIN_ACC);
        vm.expectRevert();
        roles.executeRoleChange(RELAYER_ROLE);

        vm.warp(block.timestamp + TIMELOCK);
        vm.prank(ADMIN_ACC);
        roles.executeRoleChange(RELAYER_ROLE);
        assertEq(roles.relayer(), NEW_RELAYER);
    }

    // ---------------------------------------------------------------------
    // Access control
    // ---------------------------------------------------------------------

    function test_OnlyAdminCanManageRoles() public {
        vm.prank(STRANGER);
        vm.expectRevert(abi.encodeWithSelector(AcreSyncRoles.NotAdmin.selector, STRANGER));
        roles.proposeRoleChange(RELAYER_ROLE, NEW_RELAYER);

        vm.prank(STRANGER);
        vm.expectRevert(abi.encodeWithSelector(AcreSyncRoles.NotAdmin.selector, STRANGER));
        roles.revokeRelayer();

        vm.prank(STRANGER);
        vm.expectRevert(abi.encodeWithSelector(AcreSyncRoles.NotAdmin.selector, STRANGER));
        roles.cancelRoleChange(RELAYER_ROLE);
    }

    /// @dev The relayer signs every anchor but must have no authority over who holds
    /// which role. Otherwise a compromised relayer key escalates to full control.
    function test_RelayerCannotManageRoles() public {
        vm.prank(RELAYER_ACC);
        vm.expectRevert(abi.encodeWithSelector(AcreSyncRoles.NotAdmin.selector, RELAYER_ACC));
        roles.proposeRoleChange(ADMIN_ROLE, RELAYER_ACC);
    }

    // ---------------------------------------------------------------------
    // Pause
    // ---------------------------------------------------------------------

    function test_PauseAndUnpause() public {
        bytes32 reason = keccak256("depository divergence under investigation");

        vm.prank(PAUSER_ACC);
        roles.pause(reason);
        assertTrue(roles.paused());
        assertEq(roles.pauseReasonHash(), reason);

        vm.prank(PAUSER_ACC);
        roles.unpause();
        assertFalse(roles.paused());
        assertEq(roles.pauseReasonHash(), bytes32(0));
    }

    /// @dev An emergency pause that waits 48 hours is not an emergency control.
    function test_PauseIsImmediate() public {
        uint256 before = block.timestamp;
        vm.prank(PAUSER_ACC);
        roles.pause(bytes32(0));
        assertTrue(roles.paused());
        assertEq(block.timestamp, before, "pause must not require any delay");
    }

    function test_OnlyPauserCanPause() public {
        vm.prank(ADMIN_ACC);
        vm.expectRevert(abi.encodeWithSelector(AcreSyncRoles.NotPauser.selector, ADMIN_ACC));
        roles.pause(bytes32(0));

        vm.prank(RELAYER_ACC);
        vm.expectRevert(abi.encodeWithSelector(AcreSyncRoles.NotPauser.selector, RELAYER_ACC));
        roles.pause(bytes32(0));
    }

    function test_DoublePauseReverts() public {
        vm.prank(PAUSER_ACC);
        roles.pause(bytes32(0));

        vm.prank(PAUSER_ACC);
        vm.expectRevert(AcreSyncRoles.AlreadyPaused.selector);
        roles.pause(bytes32(0));
    }

    function test_UnpauseWhenNotPausedReverts() public {
        vm.prank(PAUSER_ACC);
        vm.expectRevert(AcreSyncRoles.NotPaused.selector);
        roles.unpause();
    }

    /// @dev Changing the pauser is timelocked even though pausing itself is not, so a
    /// compromised admin cannot instantly take away the ability to stop them.
    function test_ChangingThePauserIsTimelocked() public {
        vm.prank(ADMIN_ACC);
        roles.proposeRoleChange(PAUSER_ROLE, STRANGER);

        assertEq(roles.pauser(), PAUSER_ACC, "the incumbent pauser must remain able to act");

        vm.prank(PAUSER_ACC);
        roles.pause(bytes32(0));
        assertTrue(roles.paused());
    }

    // ---------------------------------------------------------------------
    // Events
    // ---------------------------------------------------------------------

    function test_EventsEmitted() public {
        vm.expectEmit(true, false, false, true);
        emit AcreSyncRoles.RoleChangeProposed(
            RELAYER_ROLE, NEW_RELAYER, uint64(block.timestamp + TIMELOCK)
        );
        vm.prank(ADMIN_ACC);
        roles.proposeRoleChange(RELAYER_ROLE, NEW_RELAYER);

        vm.warp(block.timestamp + TIMELOCK);

        vm.expectEmit(true, false, false, true);
        emit AcreSyncRoles.RoleChanged(RELAYER_ROLE, RELAYER_ACC, NEW_RELAYER);
        vm.prank(ADMIN_ACC);
        roles.executeRoleChange(RELAYER_ROLE);
    }

    // ---------------------------------------------------------------------
    // Fuzz
    // ---------------------------------------------------------------------

    function testFuzz_OnlyAdminEverSucceeds(address caller) public {
        vm.assume(caller != ADMIN_ACC);
        vm.assume(caller != address(0));

        vm.prank(caller);
        vm.expectRevert(abi.encodeWithSelector(AcreSyncRoles.NotAdmin.selector, caller));
        roles.proposeRoleChange(RELAYER_ROLE, NEW_RELAYER);
    }

    function testFuzz_GrantNeverEarlyThanEta(uint32 warpBy) public {
        vm.assume(warpBy < TIMELOCK);

        vm.prank(ADMIN_ACC);
        roles.proposeRoleChange(RELAYER_ROLE, NEW_RELAYER);

        vm.warp(block.timestamp + warpBy);
        vm.prank(ADMIN_ACC);
        vm.expectRevert();
        roles.executeRoleChange(RELAYER_ROLE);

        assertEq(roles.relayer(), RELAYER_ACC);
    }
}
