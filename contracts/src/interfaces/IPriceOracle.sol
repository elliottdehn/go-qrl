// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @title Price Oracle Interface
/// @notice Returns the price of the base token (e.g. QRL) in USD,
///         scaled by 1e18. A return value of 1e18 means $1.00 per
///         token.
/// @dev    Implementations are expected to aggregate prices from
///         validator-signed votes and expose a median (or similar
///         manipulation-resistant) reference price.
interface IPriceOracle {
    /// @notice Current oracle price of the base token in USD, scaled
    ///         by 1e18.
    /// @return p USD price of one base-token unit, scaled by 1e18.
    function price() external view returns (uint256 p);

    /// @notice Whether the oracle is currently in a healthy state.
    ///         Implementations should return false when quorum is not
    ///         reached, when prices are stale beyond a configured
    ///         threshold, or under any other condition that should
    ///         cause downstream price-dependent operations to halt.
    function healthy() external view returns (bool);
}
