// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

/// @title IdentityRegistry - DELTA allowlist for KYC'd investors
/// @notice Minimal ERC-3643-style identity registry. Only verified wallets can hold land tokens.
contract IdentityRegistry {
    address public admin;
    mapping(address => bool) public verifiers;

    struct Identity {
        bool verified;
        uint16 countryCode; // ISO numeric, e.g. 356 = India
        uint64 kycExpiry;
    }

    mapping(address => Identity) private _identities;

    event VerifierAdded(address indexed v);
    event Authorized(address indexed wallet, uint16 countryCode, uint64 expiry);
    event Revoked(address indexed wallet);

    modifier onlyAdmin() {
        require(msg.sender == admin, "not admin");
        _;
    }

    modifier onlyVerifier() {
        require(msg.sender == admin || verifiers[msg.sender], "not verifier");
        _;
    }

    constructor() {
        admin = msg.sender;
    }

    function addVerifier(address v) external onlyAdmin {
        verifiers[v] = true;
        emit VerifierAdded(v);
    }

    function authorize(address wallet, uint16 countryCode, uint64 expiry) external onlyVerifier {
        _identities[wallet] = Identity(true, countryCode, expiry);
        emit Authorized(wallet, countryCode, expiry);
    }

    function revoke(address wallet) external onlyVerifier {
        _identities[wallet].verified = false;
        emit Revoked(wallet);
    }

    function isVerified(address wallet) public view returns (bool) {
        Identity memory id = _identities[wallet];
        return id.verified && block.timestamp < id.kycExpiry;
    }

    function identityOf(address wallet) external view returns (Identity memory) {
        return _identities[wallet];
    }
}
