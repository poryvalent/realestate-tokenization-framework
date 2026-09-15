// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import "forge-std/Test.sol";
import "../src/IdentityRegistry.sol";
import "../src/TitleDeedNFT.sol";
import "../src/FractionalToken.sol";
import "../src/DeltaRegistry.sol";
import "../src/PayoutDistributor.sol";

contract DeltaTest is Test {
    IdentityRegistry id;
    TitleDeedNFT title;
    DeltaRegistry registry;
    FractionalToken frac;
    PayoutDistributor pay;

    address admin = address(this);
    address alice = address(0xA11CE);
    address bob = address(0xB0B);
    address outsider = address(0xBAD);

    bytes32 propId = keccak256("MUMBAI-BANDRA-1234");

    function setUp() public {
        id = new IdentityRegistry();
        id.addVerifier(admin);
        id.authorize(alice, 356, uint64(block.timestamp + 365 days));
        id.authorize(bob, 356, uint64(block.timestamp + 365 days));
        // outsider not authorized

        title = new TitleDeedNFT(admin);
        registry = new DeltaRegistry(address(title));
        title.setDeltaRegistry(address(registry));

        uint256 tid = title.mint(admin, propId, "123/4", "Mumbai Suburban", 500, keccak256("7/12-bundle"));
        registry.register(propId, tid);

        frac = new FractionalToken("Bandra 123 Frac", "BND123", address(id), address(title), tid, 0, 356);
        registry.markTokenized(propId, address(frac));
        pay = new PayoutDistributor(address(frac));

        frac.mint(alice, 600 ether);
        frac.mint(bob, 400 ether);
    }

    function testTransferBlockedForUnverified() public {
        vm.prank(alice);
        vm.expectRevert("recipient blocked");
        frac.transfer(outsider, 1 ether);
    }

    function testTransferOkBetweenVerified() public {
        vm.prank(alice);
        frac.transfer(bob, 100 ether);
        assertEq(frac.balanceOf(bob), 500 ether);
    }

    function testPayoutSplit() public {
        pay.distribute{value: 10 ether}();
        assertEq(pay.owed(alice), 6 ether);
        assertEq(pay.owed(bob), 4 ether);
        vm.prank(alice);
        pay.claim();
        assertEq(alice.balance, 6 ether);
    }
}
