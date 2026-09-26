// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {Script} from "forge-std/Script.sol";
import {console2} from "forge-std/console2.sol";

import {AcreSyncRoles} from "../src/AcreSyncRoles.sol";
import {AcreSyncBallot} from "../src/AcreSyncBallot.sol";
import {AcreSyncScheme} from "../src/AcreSyncScheme.sol";

/// @title Deploy
/// @notice Deploys the three AcreSync contracts in dependency order.
///
/// @dev Order is forced by the one-directional references. Roles first, because both siblings
/// read it. Ballot second. Scheme last, because it reads the ballot's anchored result when
/// settlement opens. Nothing points backwards, so there is no initialisation step after
/// deployment and no window in which a contract is deployed but not yet wired.
///
/// # No proxy
///
/// These are deployed as immutable contracts. That is a decision, not an omission. If the
/// admin could change how the ledger computes, the ledger attests to nothing, and an
/// upgradeable audit trail is close to a contradiction in terms. A bug means deploying v2 and
/// anchoring a transparent migration, which is a worse day operationally and a much better
/// story for anyone relying on the record.
///
/// # The environment tag
///
/// `environmentTag` and `isSimulation` are immutable and stamped into every anchor. A Sepolia
/// deployment tagged LOCAL would permanently misdescribe itself, and there is no way to
/// correct it afterwards. The script therefore refuses to deploy unless the tag matches the
/// chain it is pointed at.
contract Deploy is Script {
    uint8 internal constant ENV_LOCAL = 0;
    uint8 internal constant ENV_SEPOLIA_SIM = 1;

    uint256 internal constant SEPOLIA_CHAIN_ID = 11155111;

    // ----- Scheme parameters: the locked v1 numbers -----
    //
    // 500 units at ten lakh rupees is fifty crore, the SM-REIT band floor. 25 units is the 5%
    // an unleveraged scheme's investment manager must hold, leaving 475 public. The scheme
    // needs at least 200 distinct unitholders and must distribute at least 95% of NDCF.
    uint64 internal constant UNIT_PRICE_PAISE = 100_000_000;
    uint32 internal constant TOTAL_UNITS = 500;
    uint32 internal constant IM_UNITS = 25;
    uint16 internal constant MIN_PUBLIC_HOLDERS = 200;
    uint16 internal constant DISTRIBUTION_FLOOR_BPS = 9500;

    // A batch of 100 credits costs roughly 6 to 9 million gas, comfortably inside a 30 million
    // block. The cap exists so an oversized batch cannot run out of gas and wedge the cursor.
    uint32 internal constant MAX_BATCH_SIZE = 100;

    // ----- Ballot parameters -----
    //
    // Ten blocks is about two minutes on Sepolia: long enough that a proposer cannot casually
    // line up the target block, short enough to demonstrate live.
    uint32 internal constant SEED_DELAY_BLOCKS = 10;

    // Hard-bounded by the EVM at 256 blocks, past which blockhash returns zero. 200 leaves
    // margin without pretending the window is longer than the chain allows.
    uint32 internal constant SEED_REVEAL_WINDOW_BLOCKS = 200;

    uint8 internal constant MAX_SEED_ATTEMPTS = 3;

    function run() external {
        uint256 deployerKey = vm.envUint("ACRESYNC_RELAYER_PRIVATE_KEY");
        address deployer = vm.addr(deployerKey);

        string memory envName = vm.envString("ACRESYNC_ENVIRONMENT");
        uint8 envTag = _resolveEnvironmentTag(envName);
        bool simulation = envTag != ENV_LOCAL;

        // The role timelock runs on real block.timestamp and cannot be compressed by the
        // simulated business clock. A production value of 48 hours is undemonstrable, so the
        // demo uses one hour and says so rather than leaving it to be discovered in the
        // constructor arguments.
        uint32 timelock = uint32(vm.envOr("ACRESYNC_ROLE_TIMELOCK_SECONDS", uint256(3600)));

        bytes32 schemeRef = _schemeRef();

        // All four roles default to the deployer for the demo. In production these are four
        // separate keys, and the asymmetry in AcreSyncRoles only means something when they
        // are: a single key holding both admin and relayer can grant itself anything, given
        // the timelock.
        address adminAcc = vm.envOr("ACRESYNC_ADMIN_ADDRESS", deployer);
        address relayerAcc = vm.envOr("ACRESYNC_RELAYER_ADDRESS", deployer);
        address pauserAcc = vm.envOr("ACRESYNC_PAUSER_ADDRESS", deployer);
        address trusteeAcc = vm.envOr("ACRESYNC_TRUSTEE_ADDRESS", deployer);

        _logPlan(deployer, envName, envTag, timelock, adminAcc, relayerAcc, pauserAcc, trusteeAcc);

        vm.startBroadcast(deployerKey);

        AcreSyncRoles roles = new AcreSyncRoles(
            adminAcc, relayerAcc, pauserAcc, trusteeAcc, timelock, simulation, envTag
        );

        AcreSyncBallot ballot = new AcreSyncBallot(
            roles, SEED_DELAY_BLOCKS, SEED_REVEAL_WINDOW_BLOCKS, MAX_SEED_ATTEMPTS
        );

        AcreSyncScheme scheme = new AcreSyncScheme(
            roles,
            ballot,
            schemeRef,
            UNIT_PRICE_PAISE,
            TOTAL_UNITS,
            IM_UNITS,
            MIN_PUBLIC_HOLDERS,
            DISTRIBUTION_FLOOR_BPS,
            MAX_BATCH_SIZE
        );

        vm.stopBroadcast();

        _verifyDeployment(roles, ballot, scheme, envTag, simulation);
        _logResult(roles, ballot, scheme);
    }

    /// @dev Refuses a tag that disagrees with the chain.
    ///
    /// Both directions matter. Deploying to Sepolia while tagged LOCAL produces anchors that
    /// claim to be local forever. Deploying to Anvil while tagged SEPOLIA_SIM produces test
    /// artifacts that claim to be from the demo chain. Neither is correctable after the fact,
    /// because the tag is immutable.
    function _resolveEnvironmentTag(string memory envName) internal view returns (uint8) {
        bytes32 h = keccak256(bytes(envName));

        if (h == keccak256("SEPOLIA_SIM")) {
            require(
                block.chainid == SEPOLIA_CHAIN_ID,
                "ACRESYNC_ENVIRONMENT=SEPOLIA_SIM but the RPC is not Sepolia; the tag is immutable"
            );
            return ENV_SEPOLIA_SIM;
        }
        if (h == keccak256("LOCAL")) {
            require(
                block.chainid != SEPOLIA_CHAIN_ID,
                "ACRESYNC_ENVIRONMENT=LOCAL but the RPC is Sepolia; set SEPOLIA_SIM before deploying"
            );
            return ENV_LOCAL;
        }
        revert("ACRESYNC_ENVIRONMENT must be LOCAL or SEPOLIA_SIM");
    }

    /// @dev The SEBI scheme reference, right-padded into bytes32.
    function _schemeRef() internal view returns (bytes32) {
        string memory s = vm.envOr("ACRESYNC_SEBI_SCHEME_REF", string("SEBI/SM-REIT/2026/001"));
        bytes memory b = bytes(s);
        require(b.length > 0 && b.length <= 32, "scheme ref must be 1 to 32 bytes");

        bytes32 out;
        for (uint256 i = 0; i < b.length; i++) {
            out |= bytes32(b[i]) >> (i * 8);
        }
        return out;
    }

    /// @dev Reads back what was actually deployed rather than trusting the constructor
    /// arguments. Cheap, and it catches a wrong-contract or wrong-arguments deploy before
    /// anyone builds on it.
    function _verifyDeployment(
        AcreSyncRoles roles,
        AcreSyncBallot ballot,
        AcreSyncScheme scheme,
        uint8 expectedTag,
        bool expectedSimulation
    ) internal view {
        require(roles.environmentTag() == expectedTag, "roles: environment tag mismatch");
        require(roles.isSimulation() == expectedSimulation, "roles: simulation flag mismatch");
        require(roles.relayer() != address(0), "roles: relayer unset");

        require(address(ballot.roles()) == address(roles), "ballot: not wired to roles");
        require(ballot.seedDelayBlocks() == SEED_DELAY_BLOCKS, "ballot: seed delay mismatch");
        require(
            ballot.seedRevealWindowBlocks() == SEED_REVEAL_WINDOW_BLOCKS,
            "ballot: reveal window mismatch"
        );

        require(address(scheme.roles()) == address(roles), "scheme: not wired to roles");
        require(address(scheme.ballot()) == address(ballot), "scheme: not wired to ballot");
        require(scheme.totalUnits() == TOTAL_UNITS, "scheme: total units mismatch");
        require(scheme.imUnits() == IM_UNITS, "scheme: IM units mismatch");
        require(scheme.publicUnits() == TOTAL_UNITS - IM_UNITS, "scheme: public units mismatch");
        require(
            scheme.minPublicHolders() == MIN_PUBLIC_HOLDERS, "scheme: holder floor mismatch"
        );
        require(
            scheme.distributionFloorBps() == DISTRIBUTION_FLOOR_BPS,
            "scheme: distribution floor mismatch"
        );
        require(scheme.decimals() == 0, "scheme: decimals must be zero, units are indivisible");

        // The scheme's asset value is units times price, and it must land on the SM-REIT band
        // floor of fifty crore rupees. Getting this wrong means a scheme that cannot legally
        // exist, deployed immutably.
        uint256 assetValuePaise = uint256(TOTAL_UNITS) * uint256(UNIT_PRICE_PAISE);
        require(assetValuePaise == 50_000_000_000, "scheme: asset value is not fifty crore");
    }

    function _logPlan(
        address deployer,
        string memory envName,
        uint8 envTag,
        uint32 timelock,
        address adminAcc,
        address relayerAcc,
        address pauserAcc,
        address trusteeAcc
    ) internal view {
        console2.log("=== AcreSync deployment plan ===");
        console2.log("chain id        ", block.chainid);
        console2.log("environment     ", envName);
        console2.log("environment tag ", envTag);
        console2.log("deployer        ", deployer);
        console2.log("role timelock   ", timelock, "seconds");
        console2.log("admin           ", adminAcc);
        console2.log("relayer         ", relayerAcc);
        console2.log("pauser          ", pauserAcc);
        console2.log("trustee         ", trusteeAcc);

        if (adminAcc == relayerAcc) {
            console2.log("");
            console2.log("NOTE: admin and relayer are the same key.");
            console2.log("  Acceptable for a demo. In production they must differ, because the");
            console2.log("  asymmetry between timelocked grants and immediate revocation only");
            console2.log("  protects anything when the two keys are held separately.");
        }
        console2.log("");
    }

    function _logResult(AcreSyncRoles roles, AcreSyncBallot ballot, AcreSyncScheme scheme)
        internal
        pure
    {
        console2.log("=== deployed ===");
        console2.log("ACRESYNC_ROLES_ADDRESS= ", address(roles));
        console2.log("ACRESYNC_BALLOT_ADDRESS=", address(ballot));
        console2.log("ACRESYNC_SCHEME_ADDRESS=", address(scheme));
        console2.log("");
        console2.log("Copy those three lines into .env.");
    }
}
