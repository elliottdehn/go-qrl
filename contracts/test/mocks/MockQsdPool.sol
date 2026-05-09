// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {IQsdPool} from "../../src/InverseQRL.sol";

/// @notice Settable QSD pool stub for InverseQRL tests. Returns
///         whatever (cumulative, timestamp) the test set, plus any
///         block-elapsed contribution at a fixed spot price the
///         test specifies.
contract MockQsdPool is IQsdPool {
    uint256 public cumulative;
    uint64 public timestampAt;
    uint256 public spotPrice; // 1e18 scale; advances cumulative when block.timestamp moves

    function setSnapshot(uint256 c, uint64 t) external {
        cumulative = c;
        timestampAt = t;
    }

    function setSpotPrice(uint256 p) external {
        // Roll the cumulative forward at the previous spot price first
        // so the change in spot price has no retroactive effect on the
        // cumulative integral.
        _rollForward();
        spotPrice = p;
    }

    function priceCumulativeNow() external view returns (uint256 c, uint64 ts) {
        ts = uint64(block.timestamp);
        c = cumulative;
        if (timestampAt < ts && spotPrice > 0) {
            uint256 elapsed = uint256(ts - timestampAt);
            c += spotPrice * elapsed;
        }
    }

    function _rollForward() internal {
        uint64 nowTs = uint64(block.timestamp);
        if (timestampAt < nowTs && spotPrice > 0) {
            uint256 elapsed = uint256(nowTs - timestampAt);
            cumulative += spotPrice * elapsed;
        }
        timestampAt = nowTs;
    }
}
