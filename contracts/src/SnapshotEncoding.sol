// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {MerkleLib} from "./MerkleLib.sol";

/// @title SnapshotEncoding
/// @notice Reference implementation of the record-date snapshot entitlement leaf.
///
/// @dev # This library is not part of the deployed contract set
///
/// Nothing in AcreSyncScheme, AcreSyncBallot or AcreSyncRoles links against it, and adding it does not
/// change their bytecode or the addresses already deployed on Sepolia. It exists for two readers: the
/// parity test, which holds it against the Go implementation, and anyone building independent
/// verification tooling who would otherwise have to reconstruct the encoding from prose.
///
/// # Why the encoding lives outside the scheme contract
///
/// `AcreSyncScheme.verifyEntitlement` takes the leaf as a calldata parameter rather than computing it,
/// which keeps the contract generic and makes the pinned snapshot document the authority on how a leaf
/// is built. The cost of that choice is that no on-chain function rejects a wrongly encoded leaf. It
/// simply fails to verify, and the holder is told their entitlement does not match with nothing said
/// about why.
///
/// The ballot leaves are protected from that class of mistake by BallotEncoding plus shared golden
/// vectors plus a parity test. This library gives the snapshot leaf the same protection.
library SnapshotEncoding {
    /// @notice Domain tag separating snapshot leaves from every other hash in the system.
    ///
    /// @dev Versioned. The encoding cannot change without making every already-anchored snapshot root
    /// unreproducible, so a revision would be a new domain with both kept alive while older periods
    /// remain verifiable.
    bytes internal constant ENTITLEMENT_LEAF_DOMAIN = "acresync.snapshot.entitlement.v1";

    /// @notice Computes the Merkle leaf for one snapshot line.
    ///
    /// @dev Layout, all fields fixed width and big-endian:
    ///
    ///   sha256( 0x00 || "acresync.snapshot.entitlement.v1"
    ///                || leafIndex       uint32,  4 bytes
    ///                || holder          address, 20 bytes
    ///                || investorAnchor  bytes32, 32 bytes
    ///                || units           uint32,  4 bytes
    ///                || excluded        bool,     1 byte )
    ///
    /// The leading 0x00 is MerkleLib's leaf prefix, which is what stops an internal node being
    /// replayed as a leaf.
    ///
    /// Fixed widths make the encoding injective. A variable-length scheme with no separators would let
    /// a line with leafIndex 1 and 23 units encode identically to one with leafIndex 12 and 3 units,
    /// so two distinct holders could share a leaf and a proof for one would prove the other.
    ///
    /// `abi.encodePacked` emits a sized integer as exactly that many big-endian bytes, an address as
    /// its 20 raw bytes, and a bool as a single 0x00 or 0x01, which is what Go's
    /// `binary.BigEndian.AppendUint32` and explicit flag byte produce.
    ///
    /// @param leafIndex Position of the line in the ordered register.
    /// @param holder The unitholder's wallet address, public and known to its owner.
    /// @param investorAnchor HMAC anchor identifying the investor across periods.
    /// @param units Units held at the record date.
    /// @param excluded Whether the line is excluded from the statutory holder count. Note this is
    /// excluded from the *count*, not from the distribution: the entitlement denominator is every
    /// issued unit, including units held by excluded holders.
    function entitlementLeaf(
        uint32 leafIndex,
        address holder,
        bytes32 investorAnchor,
        uint32 units,
        bool excluded
    ) internal pure returns (bytes32) {
        return MerkleLib.hashLeaf(
            abi.encodePacked(
                ENTITLEMENT_LEAF_DOMAIN, leafIndex, holder, investorAnchor, units, excluded
            )
        );
    }

    /// @notice Returns the bytes hashed by entitlementLeaf.
    ///
    /// @dev Exposed so the parity test can compare preimages, not only digests. Two differing digests
    /// say nothing about where the implementations diverged; two preimages show the offending byte.
    function entitlementPreimage(
        uint32 leafIndex,
        address holder,
        bytes32 investorAnchor,
        uint32 units,
        bool excluded
    ) internal pure returns (bytes memory) {
        return abi.encodePacked(
            ENTITLEMENT_LEAF_DOMAIN, leafIndex, holder, investorAnchor, units, excluded
        );
    }
}
