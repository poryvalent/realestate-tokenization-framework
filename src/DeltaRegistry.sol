// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import "./TitleDeedNFT.sol";

/// @title DeltaRegistry - links off-chain propertyId to on-chain title + fractions
contract DeltaRegistry {
    address public admin;

    enum AssetStatus {
        None,
        Pending,
        Verified,
        Tokenized,
        Frozen,
        Sold
    }

    struct Asset {
        bytes32 propertyId;
        uint256 titleTokenId;
        address fractional;
        AssetStatus status;
    }

    mapping(bytes32 => Asset) public assets;
    TitleDeedNFT public titleNft;

    event Registered(bytes32 indexed propertyId, uint256 titleTokenId);
    event Tokenized(bytes32 indexed propertyId, address fractional);
    event StatusChanged(bytes32 indexed propertyId, AssetStatus status);

    modifier onlyAdmin() {
        require(msg.sender == admin, "not admin");
        _;
    }

    constructor(address _titleNft) {
        admin = msg.sender;
        titleNft = TitleDeedNFT(_titleNft);
    }

    function register(bytes32 propertyId, uint256 titleTokenId) external onlyAdmin {
        require(assets[propertyId].propertyId == bytes32(0), "exists");
        assets[propertyId] = Asset(propertyId, titleTokenId, address(0), AssetStatus.Verified);
        emit Registered(propertyId, titleTokenId);
    }

    function markTokenized(bytes32 propertyId, address fractional) external onlyAdmin {
        Asset storage a = assets[propertyId];
        require(a.propertyId != bytes32(0), "no asset");
        a.fractional = fractional;
        a.status = AssetStatus.Tokenized;
        titleNft.setStatus(a.titleTokenId, TitleDeedNFT.TitleStatus.Tokenized);
        emit Tokenized(propertyId, fractional);
        emit StatusChanged(propertyId, AssetStatus.Tokenized);
    }

    function freeze(bytes32 propertyId) external onlyAdmin {
        Asset storage a = assets[propertyId];
        a.status = AssetStatus.Frozen;
        titleNft.setStatus(a.titleTokenId, TitleDeedNFT.TitleStatus.Frozen);
        emit StatusChanged(propertyId, AssetStatus.Frozen);
    }
}
