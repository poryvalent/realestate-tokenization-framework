// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

/// @title MerkleLib
/// @notice Verifies Merkle proofs against roots anchored by the AcreSync orchestrator.
///
/// @dev This library is the on-chain half of a specification shared with
/// `orchestrator/internal/merkle`. The two implementations must agree byte for byte,
/// because a verifier who can prove membership off-chain but not on-chain has nothing
/// useful. The Go golden vectors are replayed against this code in MerkleLib.t.sol.
///
/// Three construction choices, each load-bearing.
///
/// SHA-256 rather than keccak256. This deviates from Ethereum convention on purpose.
/// AcreSync already uses SHA-256 for idempotency keys, IPFS content digests and
/// investor anchors, and asking an independent verifier to implement one hash function
/// instead of two materially lowers the barrier to checking our work. Solidity exposes
/// sha256 as a precompile, so a proof of depth ten costs a few hundred gas more than
/// the keccak equivalent inside a view function, which is immaterial.
///
/// Domain-separated prefixes. Leaves are sha256(0x00 || data) and internal nodes are
/// sha256(0x01 || left || right). Without the distinct prefixes an internal node hash
/// is a perfectly valid hash of sixty-four bytes of "leaf data", and an attacker can
/// prove membership of something that was never a leaf. The one-byte prefix closes
/// that second-preimage attack.
///
/// Sorted pairs. Siblings are concatenated in ascending byte order, so a proof needs
/// no direction bits and verification is a fold. The cost is that a tree containing
/// duplicate leaves is ambiguous, which the Go builder refuses to produce.
library MerkleLib {
    /// @dev Domain separator for leaf hashes.
    bytes1 internal constant LEAF_PREFIX = 0x00;

    /// @dev Domain separator for internal node hashes.
    bytes1 internal constant NODE_PREFIX = 0x01;

    /// @notice Hashes leaf data with the leaf domain prefix.
    /// @dev Callers pass the already-encoded preimage. The encoding of each leaf type
    /// lives with the type that owns it, so that a change to one leaf layout cannot
    /// silently alter another.
    function hashLeaf(bytes memory data) internal pure returns (bytes32) {
        return sha256(abi.encodePacked(LEAF_PREFIX, data));
    }

    /// @notice Hashes two sibling nodes in canonical (ascending) order.
    function hashNode(bytes32 a, bytes32 b) internal pure returns (bytes32) {
        return a <= b
            ? sha256(abi.encodePacked(NODE_PREFIX, a, b))
            : sha256(abi.encodePacked(NODE_PREFIX, b, a));
    }

    /// @notice Recomputes a root from a leaf and its proof.
    /// @param root The anchored root.
    /// @param leaf The leaf hash, already domain-prefixed by hashLeaf.
    /// @param proof Sibling hashes ordered from the leaf level upward.
    /// @return True when the fold reproduces the root.
    ///
    /// @dev Proof length varies between leaves in a tree whose leaf count is not a
    /// power of two, because the Go builder promotes an unpaired node rather than
    /// hashing it with itself. Promotion contributes no sibling, so folding the proof
    /// without assuming a fixed depth is not merely convenient, it is required for
    /// correctness. Duplicating an unpaired node instead would let a tree of n leaves
    /// collide with a particular tree of n+1 leaves, which is a forgery surface.
    function verify(bytes32 root, bytes32 leaf, bytes32[] memory proof)
        internal
        pure
        returns (bool)
    {
        bytes32 computed = leaf;
        for (uint256 i = 0; i < proof.length; i++) {
            computed = hashNode(computed, proof[i]);
        }
        return computed == root;
    }

    /// @notice Computes the root a leaf and proof imply, without comparing it.
    /// @dev Useful in tests and for surfacing a mismatch in an error message.
    function processProof(bytes32 leaf, bytes32[] memory proof)
        internal
        pure
        returns (bytes32)
    {
        bytes32 computed = leaf;
        for (uint256 i = 0; i < proof.length; i++) {
            computed = hashNode(computed, proof[i]);
        }
        return computed;
    }
}
