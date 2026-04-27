// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {Script, console2} from "forge-std/Script.sol";

import {ValidatorOracle} from "../src/ValidatorOracle.sol";
import {InverseQRL, IQsdPool} from "../src/InverseQRL.sol";
import {QSD} from "../src/QSD.sol";
import {PayWithIQRL} from "../src/PayWithIQRL.sol";
import {IPriceOracle} from "../src/interfaces/IPriceOracle.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";

/// @title  PredeployScript
/// @notice Builds the QSD stability layer (ValidatorOracle, InverseQRL,
///         QSD) deployed at the reserved addresses from go-qrl's
///         core/qsd_predeploy.go, then dumps the resulting genesis
///         state to JSON for go-qrl to embed.
///
/// @dev    Approach
///         --------
///         These addresses (0x010000-0x010002) cannot be derived from
///         any deployer-nonce pair, so we cannot land contracts there
///         via standard `new`. Instead we deploy normally to a temp
///         address, then `vm.etch` the deployed bytecode and
///         `vm.store`-replicate every non-zero storage slot at the
///         reserved address. The reserved address now has identical
///         code+storage; any subsequent `new`s of dependent contracts
///         pass the reserved address as a constructor arg so that
///         downstream `immutable`s reference the on-chain instance.
///
///         Voting params (voteStalenessBlocks, minQuorum*) are
///         immutable and therefore baked into the deployed bytecode;
///         they are NOT in storage and cannot be patched by the loader.
///         Change them by re-running this script.
///
///         Usage
///         -----
///             forge script script/Predeploy.s.sol --tc PredeployScript -vv
///
///         The dump lands at out/qsd-genesis-state.json.
contract PredeployScript is Script {
    address internal constant ORACLE_ADDR    = 0x0000000000000000000000000000000000010000;
    address internal constant IQRL_ADDR      = 0x0000000000000000000000000000000000010001;
    address internal constant QSD_ADDR       = 0x0000000000000000000000000000000000010002;
    address internal constant PAYMASTER_ADDR = 0x0000000000000000000000000000000000010003;

    // Voting params — must match DefaultQSDPredeployParams() in
    // go-qrl/core/qsd_predeploy.go. These bake into the deployed
    // bytecode (immutable) so the go-qrl side cannot adjust them.
    uint256 internal constant VOTE_STALENESS_BLOCKS = 10;
    uint256 internal constant MIN_QUORUM_NUMERATOR  = 2;
    uint256 internal constant MIN_QUORUM_DENOMINATOR = 3;

    // Sentinel slot count: we copy slots 0..N-1. With current contract
    // layouts the relevant slots all sit well below this bound; raise
    // if storage layout grows.
    uint256 internal constant SLOTS_TO_COPY = 32;

    function run() external {
        // 1. ValidatorOracle. The validator set is consensus-driven —
        //    the chain's beacon engine system-calls setValidatorSet
        //    on every block — so there's no owner to bake in here.
        ValidatorOracle oracleSrc = new ValidatorOracle(
            VOTE_STALENESS_BLOCKS,
            MIN_QUORUM_NUMERATOR,
            MIN_QUORUM_DENOMINATOR
        );
        _relocate(address(oracleSrc), ORACLE_ADDR);

        // 2. InverseQRL — constructor takes the oracle address AND
        //    the QSD pool address (which iQRL.mint reads to compute
        //    the TWAP-side mint price). Both bake into immutables.
        //    Pass the RESERVED addresses so the immutables point at
        //    the predeploy slots even though QSD is relocated next.
        InverseQRL iqrlSrc = new InverseQRL(IPriceOracle(ORACLE_ADDR), IQsdPool(QSD_ADDR));
        _relocate(address(iqrlSrc), IQRL_ADDR);

        // 3. QSD — constructor takes iqrl + oracle. Same reasoning.
        QSD qsdSrc = new QSD(IERC20(IQRL_ADDR), IPriceOracle(ORACLE_ADDR));
        _relocate(address(qsdSrc), QSD_ADDR);

        // 4. PayWithIQRL — paymaster that lets txs settle fees in
        //    iQRL. Constructor takes the iQRL token reference, which
        //    is now the reserved iQRL address.
        PayWithIQRL pmSrc = new PayWithIQRL(IERC20(IQRL_ADDR));
        _relocate(address(pmSrc), PAYMASTER_ADDR);

        // 5. Sanity-check the placed contracts respond at the reserved
        //    addresses. Any failure here is louder than a silent dump
        //    of broken state.
        require(
            ValidatorOracle(ORACLE_ADDR).voteStalenessBlocks() == VOTE_STALENESS_BLOCKS,
            "oracle staleness mismatch at reserved addr"
        );
        require(
            address(InverseQRL(IQRL_ADDR).oracle()) == ORACLE_ADDR,
            "iqrl oracle immutable mismatch"
        );
        require(
            address(QSD(QSD_ADDR).iqrl()) == IQRL_ADDR,
            "qsd iqrl immutable mismatch"
        );
        require(
            address(QSD(QSD_ADDR).oracle()) == ORACLE_ADDR,
            "qsd oracle immutable mismatch"
        );
        require(
            address(PayWithIQRL(PAYMASTER_ADDR).iqrl()) == IQRL_ADDR,
            "paymaster iqrl immutable mismatch"
        );

        // 6. Wipe the temp deployer-derived contracts. Without this
        //    they'd clutter vm.dumpState with duplicate code + state.
        _wipe(address(oracleSrc));
        _wipe(address(iqrlSrc));
        _wipe(address(qsdSrc));
        _wipe(address(pmSrc));

        // 7. Dump.
        string memory outPath = "./out/qsd-genesis-state.json";
        vm.dumpState(outPath);
        console2.log("dumped predeploy state to:");
        console2.log(outPath);
        console2.log("oracle    =", ORACLE_ADDR);
        console2.log("iqrl      =", IQRL_ADDR);
        console2.log("qsd       =", QSD_ADDR);
        console2.log("paymaster =", PAYMASTER_ADDR);
    }

    /// @dev Copy bytecode + the first SLOTS_TO_COPY storage slots from
    ///      `src` to `dst`. Used to lift a normally-deployed contract
    ///      to the reserved predeploy address.
    function _relocate(address src, address dst) internal {
        vm.etch(dst, src.code);
        for (uint256 i = 0; i < SLOTS_TO_COPY; i++) {
            bytes32 slot = bytes32(i);
            bytes32 v = vm.load(src, slot);
            if (v != bytes32(0)) {
                vm.store(dst, slot, v);
            }
        }
    }

    /// @dev Erase `target`: empty bytecode, zero out the same slot
    ///      window we copied. Keeps the dumped state focused on the
    ///      reserved addresses.
    function _wipe(address target) internal {
        vm.etch(target, "");
        for (uint256 i = 0; i < SLOTS_TO_COPY; i++) {
            vm.store(target, bytes32(i), bytes32(0));
        }
    }
}
