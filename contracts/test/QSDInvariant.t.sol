// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {Test} from "forge-std/Test.sol";

import {QSD} from "../src/QSD.sol";
import {MockERC20} from "./mocks/MockERC20.sol";
import {MockPriceOracle} from "./mocks/MockPriceOracle.sol";

/// @notice Stateful handler that the foundry invariant runner calls
///         with random sequences. Each method bounds inputs to
///         reasonable ranges and silently swallows reverts so that
///         random "bad" calls don't break the test run.
contract QSDHandler is Test {
    QSD public qsd;
    MockERC20 public iqrl;
    MockPriceOracle public oracle;

    address[] public actors;
    uint256 public depositCount;
    uint256 public redeemCount;
    uint256 public swapCount;
    uint256 public priceMoveCount;

    constructor(QSD _qsd, MockERC20 _iqrl, MockPriceOracle _oracle) {
        qsd = _qsd;
        iqrl = _iqrl;
        oracle = _oracle;

        actors.push(address(0xA1));
        actors.push(address(0xA2));
        actors.push(address(0xA3));

        // Deep native + ERC-20 balances and an iQRL approval for QSD.
        for (uint256 i = 0; i < actors.length; i++) {
            vm.deal(actors[i], 1e36);
            iqrl.mint(actors[i], 1e30);
            vm.prank(actors[i]);
            iqrl.approve(address(qsd), type(uint256).max);
        }
    }

    function _pickActor(uint256 seed) internal view returns (address) {
        return actors[bound(seed, 0, actors.length - 1)];
    }

    function deposit(uint256 actorSeed, uint256 qrlAmount, uint256 iqrlAmount) external {
        address actor = _pickActor(actorSeed);
        qrlAmount = bound(qrlAmount, 1, 1e24);
        // iqrlAmount serves as maxIqrlIn (slippage cap on iQRL leg);
        // pad it generously so the pool-ratio match almost always
        // succeeds on the symmetric-deposit branch.
        iqrlAmount = bound(iqrlAmount, qrlAmount, 1e26);

        vm.prank(actor);
        try qsd.deposit{value: qrlAmount}(iqrlAmount, 0) returns (uint256, uint256) {
            depositCount++;
        } catch {}
    }

    function redeem(uint256 actorSeed, uint256 amount) external {
        address actor = _pickActor(actorSeed);
        uint256 bal = qsd.balanceOf(actor);
        if (bal == 0) return;
        amount = bound(amount, 1, bal);

        vm.prank(actor);
        try qsd.redeem(amount) returns (uint256, uint256) {
            redeemCount++;
        } catch {}
    }

    function swap(uint256 actorSeed, uint256 amountIn, bool qrlIn) external {
        address actor = _pickActor(actorSeed);
        amountIn = bound(amountIn, 1, 1e22);

        uint256 reserveIn = qrlIn ? qsd.poolQRL() : qsd.poolIQRL();
        if (reserveIn == 0) return;

        amountIn = bound(amountIn, 1, reserveIn / 2);
        if (amountIn == 0) return;

        vm.prank(actor);
        if (qrlIn) {
            try qsd.swapQrlForIqrl{value: amountIn}(0) returns (uint256) {
                swapCount++;
            } catch {}
        } else {
            try qsd.swapIqrlForQrl(amountIn, 0) returns (uint256) {
                swapCount++;
            } catch {}
        }
    }

    function moveOraclePrice(uint256 newPrice) external {
        newPrice = bound(newPrice, 1e15, 1e21);
        oracle.setPrice(newPrice);
        priceMoveCount++;
    }
}

/// @notice Foundry invariant test: drives QSDHandler with random
///         sequences and verifies invariants after each call.
contract QSDInvariantTest is Test {
    QSD internal qsd;
    MockERC20 internal iqrl;
    MockPriceOracle internal oracle;
    QSDHandler internal handler;

    function setUp() public {
        iqrl = new MockERC20("iQRL", "iQRL");
        oracle = new MockPriceOracle(1e18);
        qsd = new QSD(iqrl, oracle);

        handler = new QSDHandler(qsd, iqrl, oracle);

        targetContract(address(handler));

        bytes4[] memory selectors = new bytes4[](4);
        selectors[0] = QSDHandler.deposit.selector;
        selectors[1] = QSDHandler.redeem.selector;
        selectors[2] = QSDHandler.swap.selector;
        selectors[3] = QSDHandler.moveOraclePrice.selector;
        targetSelector(FuzzSelector({addr: address(handler), selectors: selectors}));
    }

    /// @notice Core solvency: 2*sqrt(k) >= totalSupply at all times.
    function invariant_solvent() public view {
        assertTrue(qsd.checkInvariants(), "solvency invariant violated");
    }

    /// @notice Pool reserves match the contract's actual holdings of
    ///         each asset. Native QRL: poolQRL == address(qsd).balance.
    ///         iQRL: poolIQRL == iqrl.balanceOf(qsd). Strict equality
    ///         under handler operations; force-crediting (SELFDESTRUCT
    ///         victim, surprise ERC-20 transfer) could make balances
    ///         exceed reserves, but never the reverse.
    function invariant_reservesMatchBalances() public view {
        assertEq(qsd.poolQRL(), address(qsd).balance, "QRL reserve mismatch");
        assertEq(qsd.poolIQRL(), iqrl.balanceOf(address(qsd)), "iQRL reserve mismatch");
    }

    /// @notice Pool composition: if supply > 0, both reserves > 0.
    function invariant_bothReservesPositiveIfSupplyPositive() public view {
        if (qsd.totalSupply() > 0) {
            assertGt(qsd.poolQRL(), 0);
            assertGt(qsd.poolIQRL(), 0);
        }
    }

    /// @notice Sanity stats — call counts visible in the foundry
    ///         invariant report.
    function invariant_callsVisible() public view {}
}
