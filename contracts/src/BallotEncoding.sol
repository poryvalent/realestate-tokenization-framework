// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {MerkleLib} from "./MerkleLib.sol";

/// @title BallotEncoding
/// @notice Preimage encodings shared with `orchestrator/internal/ballot`.
///
/// @dev Every encoding here has an exact counterpart in Go and the two are held
/// together by the golden vectors in test/testdata/vectors.json. If they diverge, the
/// contract will reject valid proofs and accept nothing, so the parity test is not a
/// nicety.
///
/// All fields are fixed width and big-endian. `abi.encodePacked` on a sized integer
/// emits exactly that many big-endian bytes, which matches Go's
/// `binary.BigEndian.AppendUintN`. Fixed widths matter because a variable-length
/// encoding with no separators would let two different bids produce the same preimage:
/// a bid with leafIndex 1 and units 23 could encode identically to one with leafIndex
/// 12 and units 3.
///
/// Note that the seed and rank derivations reuse the 0x00 leaf prefix from MerkleLib
/// rather than introducing a third prefix. They are not tree leaves, so that is a
/// slight abuse of the name, but it is safe because each derivation carries a distinct
/// domain string, and it keeps a single hashing helper in use across the system.
library BallotEncoding {
    /// @dev Domain strings. Changing any of these invalidates every root and seed
    /// AcreSync has anchored, so they are versioned rather than edited.
    bytes internal constant RANK_DOMAIN = "acresync.ballot.rank.v1";
    bytes internal constant SEED_DOMAIN = "acresync.ballot.finalseed.v1";
    bytes internal constant BID_LEAF_DOMAIN = "acresync.ballot.bidleaf.v1";
    bytes internal constant ALLOT_LEAF_DOMAIN = "acresync.ballot.allotleaf.v1";

    /// @notice Outcome codes, ordered to match the Go Outcome constants and the
    /// Postgres allocation_outcome enum.
    /// @dev The numeric value is part of the allotment leaf preimage, so these cannot
    /// be reordered without invalidating anchored roots.
    uint8 internal constant OUTCOME_FULL = 0;
    uint8 internal constant OUTCOME_PARTIAL = 1;
    uint8 internal constant OUTCOME_NIL_BALLOT = 2;
    uint8 internal constant OUTCOME_NIL_TECHNICAL = 3;

    /// @notice Computes the bid book Merkle leaf for one bid.
    /// @param leafIndex Position of the bid in the frozen book.
    /// @param bidRef Opaque 16-byte bid reference.
    /// @param investorAnchor HMAC anchor identifying the investor.
    /// @param unitsBid Units applied for.
    /// @param pricePerUnitPaise Bid price in paise.
    function bidLeaf(
        uint32 leafIndex,
        bytes16 bidRef,
        bytes32 investorAnchor,
        uint32 unitsBid,
        uint64 pricePerUnitPaise
    ) internal pure returns (bytes32) {
        return MerkleLib.hashLeaf(
            abi.encodePacked(
                BID_LEAF_DOMAIN, leafIndex, bidRef, investorAnchor, unitsBid, pricePerUnitPaise
            )
        );
    }

    /// @notice Computes the allotment file Merkle leaf for one allocation.
    function allotmentLeaf(
        uint32 leafIndex,
        bytes32 investorAnchor,
        uint32 unitsAllotted,
        uint8 outcomeCode,
        uint32 ballotRank
    ) internal pure returns (bytes32) {
        return MerkleLib.hashLeaf(
            abi.encodePacked(
                ALLOT_LEAF_DOMAIN, leafIndex, investorAnchor, unitsAllotted, outcomeCode, ballotRank
            )
        );
    }

    /// @notice Derives the seed the draw consumes.
    ///
    /// @dev Each input contributes something the others cannot. The committed secret
    /// proves the operator's intent predated the draw. The target block hash is unknown
    /// to everyone at commit time, so the operator cannot choose the outcome. The bid
    /// book root binds the seed to one exact set of bids, so swapping a bid invalidates
    /// the draw rather than quietly changing it.
    function finalSeed(bytes32 secret, bytes32 targetBlockHash, bytes32 bidbookRoot)
        internal
        pure
        returns (bytes32)
    {
        return MerkleLib.hashLeaf(
            abi.encodePacked(SEED_DOMAIN, secret, targetBlockHash, bidbookRoot)
        );
    }

    /// @notice Derives a bid's ballot ranking key.
    ///
    /// @dev Sorting on this key produces the draw order. The contract does not sort, and
    /// does not need to: it anchors the result root computed off-chain, and this function
    /// exists so that anyone can recompute a single bid's key on-chain and check its
    /// published rank. A shuffle-based scheme would have required replaying the entire
    /// permutation to verify one position.
    function rankKey(bytes32 seed, bytes32 investorAnchor, uint32 leafIndex)
        internal
        pure
        returns (bytes32)
    {
        return MerkleLib.hashLeaf(abi.encodePacked(RANK_DOMAIN, seed, investorAnchor, leafIndex));
    }

    /// @notice Verifies the revealed secret against its commitment.
    /// @dev sha256, not keccak256, for consistency with every other digest in the
    /// system. See MerkleLib for the reasoning.
    function commitmentOf(bytes32 secret) internal pure returns (bytes32) {
        return sha256(abi.encodePacked(secret));
    }
}
