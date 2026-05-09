// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {ERC20} from "@openzeppelin/contracts/token/ERC20/ERC20.sol";
import {ERC20Burnable} from "@openzeppelin/contracts/token/ERC20/extensions/ERC20Burnable.sol";

/// @notice Minimal ERC20 with public mint/burn for tests.
/// Inherits ERC20Burnable so callers (e.g. PayWithIQRL) can invoke
/// the standard `burn(uint256)` from msg.sender's balance.
contract MockERC20 is ERC20Burnable {
    constructor(string memory name_, string memory symbol_) ERC20(name_, symbol_) {}

    function mint(address to, uint256 amount) external {
        _mint(to, amount);
    }

    /// Test-only escape hatch to burn from an arbitrary address
    /// without going through allowance. Distinct selector from the
    /// inherited burn(uint256) / burnFrom(address,uint256).
    function debugBurn(address from, uint256 amount) external {
        _burn(from, amount);
    }
}
