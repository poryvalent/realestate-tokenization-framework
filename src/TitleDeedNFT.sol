// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

/// @title TitleDeedNFT - 1:1 soulbound-ish title mirror for MahaBhulekh records
/// @notice Each token mirrors one off-chain land record. Only hashes stored on-chain.
contract TitleDeedNFT {
    address public registrar;
    address public deltaRegistry;
    uint256 public nextId = 1;

    enum TitleStatus {
        Pending,
        Verified,
        Tokenized,
        Frozen,
        Retired
    }

    struct Title {
        string surveyNumber;
        string district;
        uint256 areaSqM;
        bytes32 recordHash; // keccak256 of digitized 7/12 utara + property card bundle (off-chain)
        TitleStatus status;
    }

    mapping(uint256 => Title) public titles;
    mapping(uint256 => address) private _owners;
    mapping(address => uint256) private _balances;

    event TitleMinted(uint256 indexed tokenId, bytes32 indexed propertyId, bytes32 recordHash);
    event TitleStatusChanged(uint256 indexed tokenId, TitleStatus status);

    modifier onlyRegistrar() {
        require(msg.sender == registrar, "not registrar");
        _;
    }

    modifier onlyRegistrarOrRegistry() {
        require(msg.sender == registrar || msg.sender == deltaRegistry, "not registrar");
        _;
    }

    constructor(address _registrar) {
        registrar = _registrar == address(0) ? msg.sender : _registrar;
    }

    function setDeltaRegistry(address r) external onlyRegistrar {
        deltaRegistry = r;
    }

    function mint(
        address custodian,
        bytes32 propertyId,
        string calldata surveyNumber,
        string calldata district,
        uint256 areaSqM,
        bytes32 recordHash
    ) external onlyRegistrar returns (uint256 tokenId) {
        tokenId = nextId++;
        titles[tokenId] = Title(surveyNumber, district, areaSqM, recordHash, TitleStatus.Verified);
        _owners[tokenId] = custodian;
        _balances[custodian] += 1;
        emit TitleMinted(tokenId, propertyId, recordHash);
    }

    function setStatus(uint256 tokenId, TitleStatus s) external onlyRegistrarOrRegistry {
        titles[tokenId].status = s;
        emit TitleStatusChanged(tokenId, s);
    }

    // --- minimal ERC721 reads ---
    function ownerOf(uint256 tokenId) external view returns (address) {
        address o = _owners[tokenId];
        require(o != address(0), "no token");
        return o;
    }

    function balanceOf(address a) external view returns (uint256) {
        return _balances[a];
    }
}
