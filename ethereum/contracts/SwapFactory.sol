// SPDX-License-Identifier: LGPLv3
pragma solidity ^0.8.5;

import "./Secp256k1.sol";

contract SwapFactory {
    Secp256k1 immutable secp256k1;

    uint256 nextID;

    struct Swap {
        // contract creator, Alice
        address payable owner;

        // address allowed to claim the ether in this contract
        address payable claimer;

        // the keccak256 hash of the expected public key derived from the secret `s_b`.
        // this public key is a point on the secp256k1 curve
        bytes32 pubKeyClaim;

        // the keccak256 hash of the expected public key derived from the secret `s_a`.
        // this public key is a point on the secp256k1 curve
        bytes32 pubKeyRefund;

        // timestamp (set at contract creation)
        // before which Alice can call either set_ready or refund
        uint256 timeout_0;

        // timestamp after which Bob cannot claim, only Alice can refund.
        uint256 timeout_1;

        // Alice sets ready to true when she sees the funds locked on the other chain.
        // this prevents Bob from withdrawing funds without locking funds on the other chain first
        bool isReady;  

        // set to true upon the swap value being claimed or refunded.
        bool completed;

        // the value of this swap.
        uint256 value;      
    }

    mapping(uint256 => Swap) public swaps;

    event New(uint256 swapID, bytes32 claimKey, bytes32 refundKey);
    event Ready(uint256 swapID);
    event Claimed(uint256 swapID, bytes32 s);
    event Refunded(uint256 swapID, bytes32 s);

    constructor() {
        secp256k1 = new Secp256k1();
    }

    // new_swap creates a new Swap instance with the given parameters.
    // it returns the swap's ID.
    function new_swap(bytes32 _pubKeyClaim, 
        bytes32 _pubKeyRefund, 
        address payable _claimer, 
        uint256 _timeoutDuration
    ) public payable returns (uint256) {
        uint256 id = nextID;

        Swap memory swap;
        swap.owner = payable(msg.sender);
        swap.claimer = _claimer;
        swap.pubKeyClaim = _pubKeyClaim;
        swap.pubKeyRefund = _pubKeyRefund;
        swap.timeout_0 = block.timestamp + _timeoutDuration;
        swap.timeout_1 = block.timestamp + (_timeoutDuration * 2);
        swap.value = msg.value;

        emit New(id, _pubKeyClaim, _pubKeyRefund);
        nextID += 1;
        swaps[id] = swap;
        return id;
    }

    // Alice must call set_ready() within t_0 once she verifies the XMR has been locked
    function set_ready(uint256 id) public {
        require(!swaps[id].completed, "swap is already completed");
        require(swaps[id].owner == msg.sender, "only the swap owner can call set_ready");
        require(!swaps[id].isReady, "swap was already set to ready");
        swaps[id].isReady = true;
        emit Ready(id);
    }

    // is_ready returns whether a swap has been set to "ready" or not.
    // note: it will return false, not revert, if the swap does not exist.
    function is_ready(uint256 id) public view returns (bool) {
        return swaps[id].isReady;
    }

    // Bob can claim if:
    // - Alice doesn't call set_ready or refund within t_0, or
    // - Alice calls ready within t_0, in which case Bob can call claim until t_1
    function claim(uint256 id, bytes32 _s) public {
        Swap memory swap = swaps[id];
        require(!swap.completed, "swap is already completed");
        require(msg.sender == swap.claimer, "only claimer can claim!");
        require((block.timestamp >= swap.timeout_0 || swap.isReady), "too early to claim!");
        require(block.timestamp < swap.timeout_1, "too late to claim!");

        verifySecret(_s, swap.pubKeyClaim);
        emit Claimed(id, _s);

        // send eth to caller (Bob)
        swap.claimer.transfer(swap.value);
        swaps[id].completed = true;
    }

    // Alice can claim a refund:
    // - Until t_0 unless she calls set_ready
    // - After t_1, if she called set_ready
    function refund(uint256 id, bytes32 _s) public {
        Swap memory swap = swaps[id];
        require(!swap.completed, "swap is already completed");
        require(msg.sender == swap.owner, "refund must be called by the swap owner");
        require(
            block.timestamp >= swap.timeout_1 ||
            (block.timestamp < swap.timeout_0 && !swap.isReady),
            "it's the counterparty's turn, unable to refund, try again later"
        );

        verifySecret(_s, swap.pubKeyRefund);
        emit Refunded(id, _s);

        // send eth back to owner==caller (Alice)
        swap.owner.transfer(swap.value);
        swaps[id].completed = true;
    }

    function verifySecret(bytes32 _s, bytes32 pubKey) internal view {
        require(
            secp256k1.mulVerify(uint256(_s), uint256(pubKey)),
            "provided secret does not match the expected public key"
        );
    }
}
