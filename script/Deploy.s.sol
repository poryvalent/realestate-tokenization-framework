// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import "forge-std/Script.sol";
import "../src/IdentityRegistry.sol";
import "../src/TitleDeedNFT.sol";
import "../src/DeltaRegistry.sol";
import "../src/FractionalToken.sol";

contract Deploy is Script {
    function run() external {
        vm.startBroadcast();
        IdentityRegistry id = new IdentityRegistry();
        TitleDeedNFT t = new TitleDeedNFT(msg.sender);
        DeltaRegistry r = new DeltaRegistry(address(t));
        vm.stopBroadcast();
        console.log("Identity:", address(id));
        console.log("TitleNFT:", address(t));
        console.log("Registry:", address(r));
    }
}
