// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {IPriceOracle} from "../../src/interfaces/IPriceOracle.sol";

/// @notice Settable price oracle for tests.
contract MockPriceOracle is IPriceOracle {
    uint256 internal _price;
    bool internal _healthy;

    constructor(uint256 initialPrice) {
        _price = initialPrice;
        _healthy = true;
    }

    function price() external view returns (uint256) {
        return _price;
    }

    function healthy() external view returns (bool) {
        return _healthy;
    }

    function setPrice(uint256 p) external {
        _price = p;
    }

    function setHealthy(bool h) external {
        _healthy = h;
    }
}
