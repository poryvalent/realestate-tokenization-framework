// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

/// @title PayoutDistributor - pro-rata ETH (rent/sale) distribution to fraction holders
/// @notice Uses cumulative-per-token accounting. Expects fractional token balanceOf().
interface IFraction {
    function totalSupply() external view returns (uint256);
    function balanceOf(address) external view returns (uint256);
}

contract PayoutDistributor {
    IFraction public fraction;
    address public admin;
    uint256 public cumulativePerToken; // scaled 1e18
    mapping(address => uint256) public claimedPerToken; // per-user checkpoint
    mapping(address => uint256) public claimable;

    event Distributed(uint256 amount);
    event Claimed(address indexed to, uint256 amount);

    constructor(address _fraction) {
        fraction = IFraction(_fraction);
        admin = msg.sender;
    }

    receive() external payable {
        distribute();
    }

    function distribute() public payable {
        uint256 supply = fraction.totalSupply();
        require(supply > 0, "no supply");
        cumulativePerToken += (msg.value * 1e18) / supply;
        emit Distributed(msg.value);
    }

    function _owed(address u) internal view returns (uint256) {
        uint256 bal = fraction.balanceOf(u);
        uint256 owed = (bal * (cumulativePerToken - claimedPerToken[u])) / 1e18;
        return owed + claimable[u];
    }

    function claim() external {
        uint256 owed = _owed(msg.sender);
        require(owed > 0, "nothing");
        claimable[msg.sender] = 0;
        claimedPerToken[msg.sender] = cumulativePerToken;
        (bool ok, ) = msg.sender.call{value: owed}("");
        require(ok, "send fail");
        emit Claimed(msg.sender, owed);
    }

    function owed(address u) external view returns (uint256) {
        return _owed(u);
    }
}
