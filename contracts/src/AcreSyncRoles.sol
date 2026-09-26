// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

/// @title AcreSyncRoles
/// @notice Shared access control and pause state for the AcreSync scheme contracts.
///
/// @dev Roles live in their own contract rather than being duplicated in the ballot and
/// the scheme for two reasons. Duplicated role storage can drift, so a key revoked in
/// one place would still be live in the other. And a single pause flag removes the
/// half-paused failure mode where one contract honours a pause and its sibling does not.
///
/// Immutable, with no proxy. An upgradeable audit trail is close to a contradiction in
/// terms: if the admin can change how the ledger computes, the ledger attests to nothing.
/// A bug means deploying v2 and anchoring a transparent migration.
///
/// # The asymmetry
///
/// Granting a role is timelocked; revoking the relayer is immediate. That is deliberate
/// and it is the most important thing in this file.
///
/// A timelock on grants means a compromised admin key cannot instantly install a hostile
/// relayer. But a timelock on revocation would hand an attacker who has taken the relayer
/// key a guaranteed window in which nobody can stop them. So revocation is immediate.
///
/// Immediate revocation halts the system, and halting is safe here precisely because
/// these contracts custody nothing. There is no ETH, no token with value, no withdrawal
/// path. The worst outcome of a halt is a shadow ledger that stops tracking the
/// depository, which the reconciliation gate already treats as a reason to block
/// distributions. Compare that with the worst outcome of a 48-hour revocation delay on a
/// live relayer key, and the choice makes itself.
contract AcreSyncRoles {
    // ---------------------------------------------------------------------
    // Roles
    // ---------------------------------------------------------------------

    /// @notice Sole caller of every business state transition. Holds no value.
    bytes32 public constant RELAYER = keccak256("acresync.role.relayer");

    /// @notice Governs role changes. Cannot touch holdings, periods or the ballot.
    bytes32 public constant ADMIN = keccak256("acresync.role.admin");

    /// @notice Can pause and unpause immediately.
    bytes32 public constant PAUSER = keccak256("acresync.role.pauser");

    /// @notice Approves period reversals and escalates an exhausted ballot. Cannot write
    /// business state alone.
    bytes32 public constant TRUSTEE = keccak256("acresync.role.trustee");

    // ---------------------------------------------------------------------
    // Immutables
    // ---------------------------------------------------------------------

    /// @notice Delay applied to role grants.
    ///
    /// @dev Runs on real block.timestamp, not the simulated business clock. Business time
    /// compresses a thirty-day offer into seconds; this does not compress at all. A
    /// production value of 48 hours is undemonstrable, so the Sepolia deployment uses a
    /// shorter delay and says so rather than leaving someone to discover it in the
    /// constructor arguments.
    uint32 public immutable roleTimelockSeconds;

    /// @notice True for any non-production deployment.
    /// @dev Immutable and emitted in every anchor, so a simulated record can never be
    /// mistaken for a live one.
    bool public immutable isSimulation;

    /// @notice Environment discriminator, mirroring the Postgres environment_tag enum.
    uint8 public immutable environmentTag;

    // ---------------------------------------------------------------------
    // State
    // ---------------------------------------------------------------------

    mapping(bytes32 role => address holder) public roleHolder;

    struct PendingChange {
        address account;
        uint64 eta;
    }

    mapping(bytes32 role => PendingChange) public pendingChange;

    bool public paused;
    bytes32 public pauseReasonHash;

    // ---------------------------------------------------------------------
    // Events
    // ---------------------------------------------------------------------

    event RoleChangeProposed(bytes32 indexed role, address account, uint64 eta);
    event RoleChanged(bytes32 indexed role, address previous, address current);
    event RoleChangeCancelled(bytes32 indexed role, address account);
    event RelayerRevoked(address previous, address by);
    event Paused(address by, bytes32 reasonHash);
    event Unpaused(address by);

    // ---------------------------------------------------------------------
    // Errors
    // ---------------------------------------------------------------------

    error NotAdmin(address caller);
    error NotPauser(address caller);
    error ZeroAddress();
    error RoleChangeNotReady(bytes32 role, uint64 eta, uint64 now_);
    error NoPendingRoleChange(bytes32 role);
    error UnknownRole(bytes32 role);
    error AlreadyPaused();
    error NotPaused();
    error SameAccount(bytes32 role, address account);

    // ---------------------------------------------------------------------
    // Construction
    // ---------------------------------------------------------------------

    /// @param initialAdmin Initial admin.
    /// @param initialRelayer Initial relayer.
    /// @param initialPauser Initial pauser.
    /// @param initialTrustee Initial trustee.
    /// @param timelockSeconds Delay on role grants.
    /// @param simulation True for the Sepolia demo.
    /// @param envTag 0 = LOCAL, 1 = SEPOLIA_SIM.
    constructor(
        address initialAdmin,
        address initialRelayer,
        address initialPauser,
        address initialTrustee,
        uint32 timelockSeconds,
        bool simulation,
        uint8 envTag
    ) {
        if (
            initialAdmin == address(0) || initialRelayer == address(0)
                || initialPauser == address(0) || initialTrustee == address(0)
        ) {
            revert ZeroAddress();
        }

        roleHolder[ADMIN] = initialAdmin;
        roleHolder[RELAYER] = initialRelayer;
        roleHolder[PAUSER] = initialPauser;
        roleHolder[TRUSTEE] = initialTrustee;

        roleTimelockSeconds = timelockSeconds;
        isSimulation = simulation;
        environmentTag = envTag;

        emit RoleChanged(ADMIN, address(0), initialAdmin);
        emit RoleChanged(RELAYER, address(0), initialRelayer);
        emit RoleChanged(PAUSER, address(0), initialPauser);
        emit RoleChanged(TRUSTEE, address(0), initialTrustee);
    }

    // ---------------------------------------------------------------------
    // Views used by the sibling contracts
    // ---------------------------------------------------------------------

    function relayer() external view returns (address) {
        return roleHolder[RELAYER];
    }

    function admin() external view returns (address) {
        return roleHolder[ADMIN];
    }

    function pauser() external view returns (address) {
        return roleHolder[PAUSER];
    }

    function trustee() external view returns (address) {
        return roleHolder[TRUSTEE];
    }

    function isRelayer(address account) external view returns (bool) {
        return account != address(0) && roleHolder[RELAYER] == account;
    }

    function isTrustee(address account) external view returns (bool) {
        return account != address(0) && roleHolder[TRUSTEE] == account;
    }

    // ---------------------------------------------------------------------
    // Role changes
    // ---------------------------------------------------------------------

    /// @notice Proposes a role grant, which becomes executable after the timelock.
    function proposeRoleChange(bytes32 role, address account) external {
        _requireAdmin();
        if (account == address(0)) revert ZeroAddress();
        if (!_isKnownRole(role)) revert UnknownRole(role);
        if (roleHolder[role] == account) revert SameAccount(role, account);

        // Casting to uint64 is safe: block.timestamp is seconds since 1970 and uint64
        // does not overflow until roughly the year 584 billion.
        // forge-lint: disable-next-line(unsafe-typecast)
        uint64 eta = uint64(block.timestamp) + roleTimelockSeconds;
        pendingChange[role] = PendingChange({account: account, eta: eta});
        emit RoleChangeProposed(role, account, eta);
    }

    /// @notice Executes a proposal whose timelock has elapsed.
    function executeRoleChange(bytes32 role) external {
        _requireAdmin();

        PendingChange memory p = pendingChange[role];
        if (p.account == address(0)) revert NoPendingRoleChange(role);
        // Validator timestamp manipulation is bounded at a handful of seconds, while the
        // role timelock is measured in hours. A validator who could shave twelve seconds
        // off a 48-hour delay has gained nothing, so block.timestamp is an appropriate
        // clock for this comparison.
        // forge-lint: disable-next-line(block-timestamp)
        if (block.timestamp < p.eta) {
            // forge-lint: disable-next-line(unsafe-typecast)
            revert RoleChangeNotReady(role, p.eta, uint64(block.timestamp));
        }

        address previous = roleHolder[role];
        roleHolder[role] = p.account;
        delete pendingChange[role];

        emit RoleChanged(role, previous, p.account);
    }

    /// @notice Cancels a pending proposal.
    function cancelRoleChange(bytes32 role) external {
        _requireAdmin();
        PendingChange memory p = pendingChange[role];
        if (p.account == address(0)) revert NoPendingRoleChange(role);
        delete pendingChange[role];
        emit RoleChangeCancelled(role, p.account);
    }

    /// @notice Revokes the relayer immediately, with no timelock.
    ///
    /// @dev The asymmetry explained in the contract docs. This halts every business
    /// transition until a new relayer is granted through the timelocked path, and halting
    /// is the safe state because nothing here custodies value.
    function revokeRelayer() external {
        _requireAdmin();
        address previous = roleHolder[RELAYER];
        roleHolder[RELAYER] = address(0);
        // A pending relayer grant is dropped too. Revoking in an emergency and leaving a
        // proposal that becomes executable minutes later would defeat the point.
        delete pendingChange[RELAYER];

        emit RelayerRevoked(previous, msg.sender);
        emit RoleChanged(RELAYER, previous, address(0));
    }

    // ---------------------------------------------------------------------
    // Pause
    // ---------------------------------------------------------------------

    /// @notice Pauses lifecycle, ballot and distribution operations immediately.
    ///
    /// @dev Reconciliation is deliberately NOT pausable. The depository is the legal
    /// register and the chain is a mirror of it; halting the sync during an emergency
    /// would let the mirror drift further from legal reality and turn a temporary problem
    /// into permanent state divergence. Distributions are already blocked while a
    /// divergence is unresolved, so the protective effect is achieved without blinding
    /// the ledger.
    function pause(bytes32 reasonHash) external {
        _requirePauser();
        if (paused) revert AlreadyPaused();
        paused = true;
        pauseReasonHash = reasonHash;
        emit Paused(msg.sender, reasonHash);
    }

    function unpause() external {
        _requirePauser();
        if (!paused) revert NotPaused();
        paused = false;
        pauseReasonHash = bytes32(0);
        emit Unpaused(msg.sender);
    }

    // ---------------------------------------------------------------------
    // Internals
    // ---------------------------------------------------------------------

    function _requireAdmin() internal view {
        if (roleHolder[ADMIN] != msg.sender) revert NotAdmin(msg.sender);
    }

    function _requirePauser() internal view {
        if (roleHolder[PAUSER] != msg.sender) revert NotPauser(msg.sender);
    }

    function _isKnownRole(bytes32 role) internal pure returns (bool) {
        return role == RELAYER || role == ADMIN || role == PAUSER || role == TRUSTEE;
    }
}
