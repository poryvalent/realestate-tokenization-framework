// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import "./IdentityRegistry.sol";

/// @title FractionalToken - permissioned ERC20 backed by one TitleDeedNFT
contract FractionalToken {
    string public name;
    string public symbol;
    uint8 public constant decimals = 18;

    uint256 public totalSupply;
    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;

    IdentityRegistry public identity;
    address public admin;
    address public titleNft;
    uint256 public titleTokenId;
    bool public paused;

    // compliance knobs (DELTA-pluggable)
    uint256 public maxHolding; // 0 = no cap; else max tokens per wallet
    uint16 public allowedCountry; // 0 = any; else ISO numeric required

    event Transfer(address indexed from, address indexed to, uint256 value);
    event Approval(address indexed owner, address indexed spender, uint256 value);

    modifier onlyAdmin() {
        require(msg.sender == admin, "not admin");
        _;
    }

    constructor(
        string memory _name,
        string memory _symbol,
        address _identity,
        address _titleNft,
        uint256 _titleTokenId,
        uint256 _maxHolding,
        uint16 _allowedCountry
    ) {
        admin = msg.sender;
        name = _name;
        symbol = _symbol;
        identity = IdentityRegistry(_identity);
        titleNft = _titleNft;
        titleTokenId = _titleTokenId;
        maxHolding = _maxHolding;
        allowedCountry = _allowedCountry;
    }

    function setPaused(bool p) external onlyAdmin {
        paused = p;
    }

    function mint(address to, uint256 amount) external onlyAdmin {
        require(_canHold(to, balanceOf[to] + amount), "compliance: recipient");
        totalSupply += amount;
        balanceOf[to] += amount;
        emit Transfer(address(0), to, amount);
    }

    function _canHold(address w, uint256 newBal) internal view returns (bool) {
        if (!identity.isVerified(w)) return false;
        if (maxHolding != 0 && newBal > maxHolding) return false;
        if (allowedCountry != 0) {
            IdentityRegistry.Identity memory id = identity.identityOf(w);
            if (id.countryCode != allowedCountry) return false;
        }
        return true;
    }

    function _transfer(address from, address to, uint256 amount) internal {
        require(!paused, "paused");
        require(balanceOf[from] >= amount, "bal");
        // allow burn to zero address; otherwise enforce allowlist on both sides
        if (to != address(0)) {
            require(identity.isVerified(from), "sender not verified");
            require(_canHold(to, balanceOf[to] + amount), "recipient blocked");
        }
        balanceOf[from] -= amount;
        balanceOf[to] += amount;
        emit Transfer(from, to, amount);
    }

    function transfer(address to, uint256 amount) external returns (bool) {
        _transfer(msg.sender, to, amount);
        return true;
    }

    function approve(address s, uint256 a) external returns (bool) {
        allowance[msg.sender][s] = a;
        emit Approval(msg.sender, s, a);
        return true;
    }

    function transferFrom(address f, address t, uint256 a) external returns (bool) {
        uint256 al = allowance[f][msg.sender];
        require(al >= a, "allow");
        allowance[f][msg.sender] = al - a;
        _transfer(f, t, a);
        return true;
    }
}
