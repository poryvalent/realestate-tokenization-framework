// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {Test} from "forge-std/Test.sol";
import {stdJson} from "forge-std/StdJson.sol";

import {MerkleLib} from "../src/MerkleLib.sol";
import {SnapshotEncoding} from "../src/SnapshotEncoding.sol";

/// @title SnapshotParityTest
/// @notice Holds the snapshot entitlement leaf encoding to the Go implementation.
///
/// @dev The failure this exists to prevent is quiet and total. `verifyEntitlement` accepts whatever
/// leaf it is handed, so a one-byte disagreement between the two implementations does not error
/// anywhere: every proof an honest holder builds simply returns false, and the contract says nothing
/// about why. The contracts are immutable with no proxy, so that is not a defect you patch.
///
/// The ballot encodings already had this protection. The snapshot leaf did not, which made it the one
/// commitment in the system that no independent implementation was checked against.
contract SnapshotParityTest is Test {
    using stdJson for string;

    string internal vectors;

    function setUp() public {
        vectors = vm.readFile("test/testdata/vectors.json");
    }

    /// @dev The domain tag must match byte for byte. A differing tag changes every leaf, so this is
    /// checked before anything derived from it.
    function test_LeafDomainMatchesGo() public view {
        string memory goDomain = vectors.readString(".snapshot.leafDomain");

        assertEq(
            keccak256(bytes(goDomain)),
            keccak256(SnapshotEncoding.ENTITLEMENT_LEAF_DOMAIN),
            "snapshot leaf domain diverged from Go: every entitlement proof would fail"
        );
    }

    function test_AlgoVersionMatchesGo() public view {
        assertEq(vectors.readUint(".snapshot.algoVersion"), 1, "snapshot algo version drifted");
    }

    /// @dev Compares the preimage before the digest, so a divergence names the byte rather than only
    /// reporting that two hashes differ.
    function test_EntitlementPreimageMatchesGo() public view {
        uint32 leafIndex = uint32(vectors.readUint(".snapshot.entitlementLeaf.leafIndex"));
        address holder = vectors.readAddress(".snapshot.entitlementLeaf.holder");
        bytes32 anchor = vectors.readBytes32(".snapshot.entitlementLeaf.investorAnchor");
        uint32 units = uint32(vectors.readUint(".snapshot.entitlementLeaf.units"));
        bool excluded = vectors.readBool(".snapshot.entitlementLeaf.excluded");
        bytes memory expected = vectors.readBytes(".snapshot.entitlementLeaf.preimageHex");

        bytes memory actual =
            SnapshotEncoding.entitlementPreimage(leafIndex, holder, anchor, units, excluded);

        assertEq(actual.length, expected.length, "preimage length differs from Go");
        assertEq(keccak256(actual), keccak256(expected), "preimage bytes differ from Go");
    }

    function test_EntitlementLeafMatchesGo() public view {
        uint32 leafIndex = uint32(vectors.readUint(".snapshot.entitlementLeaf.leafIndex"));
        address holder = vectors.readAddress(".snapshot.entitlementLeaf.holder");
        bytes32 anchor = vectors.readBytes32(".snapshot.entitlementLeaf.investorAnchor");
        uint32 units = uint32(vectors.readUint(".snapshot.entitlementLeaf.units"));
        bool excluded = vectors.readBool(".snapshot.entitlementLeaf.excluded");
        bytes32 expected = vectors.readBytes32(".snapshot.entitlementLeaf.leaf");

        assertEq(
            SnapshotEncoding.entitlementLeaf(leafIndex, holder, anchor, units, excluded),
            expected,
            "entitlement leaf encoding diverged from Go: every holder proof would fail"
        );
    }

    /// @dev The excluded flag must be bound into the leaf. Were it not, a holder could be reclassified
    /// as countable after the root was anchored, which is exactly the figure the 200-unitholder
    /// minimum polices.
    function test_ExcludedFlagChangesTheLeaf() public view {
        uint32 leafIndex = uint32(vectors.readUint(".snapshot.entitlementLeaf.leafIndex"));
        address holder = vectors.readAddress(".snapshot.entitlementLeaf.holder");
        bytes32 anchor = vectors.readBytes32(".snapshot.entitlementLeaf.investorAnchor");
        uint32 units = uint32(vectors.readUint(".snapshot.entitlementLeaf.units"));
        bytes32 expectedFlipped = vectors.readBytes32(".snapshot.entitlementLeaf.excludedVariantLeaf");

        bytes32 asCountable = SnapshotEncoding.entitlementLeaf(leafIndex, holder, anchor, units, false);
        bytes32 asExcluded = SnapshotEncoding.entitlementLeaf(leafIndex, holder, anchor, units, true);

        assertTrue(asCountable != asExcluded, "the excluded flag is not bound into the leaf");
        assertEq(asExcluded, expectedFlipped, "excluded-variant leaf diverged from Go");
    }

    /// @dev Rebuilds the worked register from the published leaves and checks the root, which is what
    /// an external verifier does end to end.
    function test_RegisterRootMatchesGo() public view {
        uint256 count = vectors.readUint(".snapshot.register.leafCount");
        bytes32 expectedRoot = vectors.readBytes32(".snapshot.register.root");

        bytes32[] memory level = new bytes32[](count);
        for (uint256 i = 0; i < count; i++) {
            level[i] = vectors.readBytes32(
                string.concat(".snapshot.register.leaves[", vm.toString(i), "]")
            );
        }

        // The same construction MerkleLib.verify folds: sorted pairs, odd node promoted.
        while (level.length > 1) {
            uint256 next = (level.length + 1) / 2;
            bytes32[] memory up = new bytes32[](next);
            for (uint256 i = 0; i < level.length / 2; i++) {
                up[i] = MerkleLib.hashNode(level[2 * i], level[2 * i + 1]);
            }
            if (level.length % 2 == 1) {
                up[next - 1] = level[level.length - 1];
            }
            level = up;
        }

        assertEq(level[0], expectedRoot, "register root diverged from Go");
    }

    /// @dev The full holder journey: take the published leaf and proof, fold them, arrive at the
    /// anchored root. This is the operation `verifyEntitlement` performs on-chain.
    function test_ProofVerifiesAgainstRegisterRoot() public view {
        bytes32 root = vectors.readBytes32(".snapshot.register.root");
        bytes32 leaf = vectors.readBytes32(".snapshot.register.proofLeaf");
        bytes32[] memory proof = vectors.readBytes32Array(".snapshot.register.proof");

        assertTrue(
            MerkleLib.verify(root, leaf, proof),
            "a proof generated by Go did not verify under the Solidity verifier"
        );
    }

    /// @dev A leaf that was never in the register must not verify, or inclusion proves nothing.
    function test_ForeignLeafDoesNotVerify() public view {
        bytes32 root = vectors.readBytes32(".snapshot.register.root");
        bytes32[] memory proof = vectors.readBytes32Array(".snapshot.register.proof");

        bytes32 forged = SnapshotEncoding.entitlementLeaf(
            2, address(0xBEEF), bytes32(uint256(0xBEEF)), 9999, false
        );

        assertFalse(MerkleLib.verify(root, forged, proof), "a foreign leaf verified");
    }

    /// @dev Fixed-width encoding means two distinct lines can never share a preimage. A
    /// variable-length scheme would let leafIndex 1 with 23 units collide with leafIndex 12 with 3
    /// units, and a proof for one holder would then prove another's entitlement.
    function testFuzz_EntitlementLeafIsInjective(
        uint32 leafIndexA,
        uint32 leafIndexB,
        uint32 unitsA,
        uint32 unitsB,
        address holder,
        bytes32 anchor
    ) public pure {
        vm.assume(leafIndexA != leafIndexB || unitsA != unitsB);

        bytes32 a = SnapshotEncoding.entitlementLeaf(leafIndexA, holder, anchor, unitsA, false);
        bytes32 b = SnapshotEncoding.entitlementLeaf(leafIndexB, holder, anchor, unitsB, false);

        assertTrue(a != b, "two distinct snapshot lines produced the same leaf");
    }

    /// @dev The units field must be bound in, or a holding could be restated after anchoring.
    function testFuzz_UnitsChangeTheLeaf(uint32 unitsA, uint32 unitsB, address holder, bytes32 anchor)
        public
        pure
    {
        vm.assume(unitsA != unitsB);

        assertTrue(
            SnapshotEncoding.entitlementLeaf(0, holder, anchor, unitsA, false)
                != SnapshotEncoding.entitlementLeaf(0, holder, anchor, unitsB, false),
            "unit count is not bound into the leaf"
        );
    }
}
