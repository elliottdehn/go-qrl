// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {SafeERC20} from "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";
import {ReentrancyGuard} from "@openzeppelin/contracts/utils/ReentrancyGuard.sol";

/// @title  YieldQSDDesk — Quote board for yQSD ⇄ QRL
/// @notice Permissionless OTC desk where makers post firm-priced
///         quotes (Sell yQSD / Buy yQSD) and takers fill them
///         atomically. yQSD is an ERC-20 (this protocol's yield-claim
///         token); QRL is the native asset (transferred via msg.value).
///         No admin, no fees, no rent.
///
/// @dev    Properties
///         ----------
///         - Maker locks their offered tokens at post time. yQSD via
///           safeTransferFrom; QRL via msg.value.
///         - Takes are atomic: the taker delivers the counter-asset
///           and receives the maker's locked asset in a single tx.
///         - Partial fills are opt-in by the maker (allowPartial). When
///           enabled, takers may fill any fraction of yqsdRemaining;
///           the QRL leg scales pro-rata with ceiling rounding so the
///           maker's price is preserved or improved. Final fills always
///           sweep qrlRemaining exactly to avoid dust accumulation.
///         - Cancel: the maker can cancel any open quote anytime.
///           Anyone may cancel an expired quote (auto-cleanup pattern).
///         - Pagination: openQuoteIds is maintained as an indexed array
///           with swap-and-pop on close, so listOpenQuotes(offset,
///           limit) returns a stable contiguous page.
///         - Followed CEI: state is mutated before any external call.
///           ReentrancyGuard backstops cross-function reentrancy.
contract YieldQSDDesk is ReentrancyGuard {
    using SafeERC20 for IERC20;

    enum Side { SellYQSD, BuyYQSD }

    struct Quote {
        address maker;
        Side    side;
        uint256 yqsdInitial;     // total yQSD on maker's side at post (immutable)
        uint256 qrlInitial;      // total QRL on maker's side at post (immutable)
        uint256 yqsdRemaining;   // yQSD remaining to fill
        uint256 qrlRemaining;    // QRL remaining to fill
        uint64  expiresAt;
        bool    allowPartial;
        bool    open;
    }

    /// @notice The yQSD token traded on this desk.
    IERC20 public immutable yqsd;

    /// @notice quoteId => Quote. Includes closed quotes (open=false)
    ///         for historical lookup.
    mapping(uint256 => Quote) public quotes;

    /// @notice Next quote id; monotone increasing, never zero.
    uint256 public nextQuoteId = 1;

    /// @dev Ordered list of currently-open quote ids. Mutated by
    ///      _addToOpen / _removeFromOpen. Used for pagination.
    uint256[] internal openQuoteIds;

    /// @dev Inverse mapping: quoteId => index in openQuoteIds.
    ///      Required for swap-and-pop on close.
    mapping(uint256 => uint256) internal openQuoteIndex;

    error ZeroAmount();
    error InvalidExpiry();
    error QuoteNotOpen();
    error QuoteExpired();
    error NotMaker();
    error PartialFillsDisabled();
    error InsufficientLiquidity(uint256 requested, uint256 available);
    error InvalidValue(uint256 expected, uint256 supplied);
    error TransferFailed();

    event QuotePosted(
        uint256 indexed id,
        address indexed maker,
        Side    side,
        uint256 yqsdAmount,
        uint256 qrlAmount,
        uint64  expiresAt,
        bool    allowPartial
    );

    event QuoteFilled(
        uint256 indexed id,
        address indexed taker,
        uint256 yqsdFilled,
        uint256 qrlFilled,
        bool    fullyFilled
    );

    event QuoteCancelled(uint256 indexed id, address indexed by);

    constructor(IERC20 _yqsd) {
        yqsd = _yqsd;
    }

    // ------------------------------------------------------------------
    // Post
    // ------------------------------------------------------------------

    /// @notice Post a SellYQSD quote: maker offers `yqsdAmount` yQSD
    ///         in exchange for `qrlAmount` QRL total. Caller must have
    ///         approved this contract for at least `yqsdAmount` yQSD.
    function postSell(
        uint256 yqsdAmount,
        uint256 qrlAmount,
        uint64  expiresAt,
        bool    allowPartial
    ) external nonReentrant returns (uint256 id) {
        if (yqsdAmount == 0 || qrlAmount == 0) revert ZeroAmount();
        if (expiresAt <= block.timestamp) revert InvalidExpiry();

        yqsd.safeTransferFrom(msg.sender, address(this), yqsdAmount);

        id = nextQuoteId++;
        quotes[id] = Quote({
            maker:         msg.sender,
            side:          Side.SellYQSD,
            yqsdInitial:   yqsdAmount,
            qrlInitial:    qrlAmount,
            yqsdRemaining: yqsdAmount,
            qrlRemaining:  qrlAmount,
            expiresAt:     expiresAt,
            allowPartial:  allowPartial,
            open:          true
        });
        _addToOpen(id);

        emit QuotePosted(id, msg.sender, Side.SellYQSD, yqsdAmount, qrlAmount, expiresAt, allowPartial);
    }

    /// @notice Post a BuyYQSD quote: maker offers `msg.value` QRL in
    ///         exchange for `yqsdAmount` yQSD total.
    function postBuy(
        uint256 yqsdAmount,
        uint64  expiresAt,
        bool    allowPartial
    ) external payable nonReentrant returns (uint256 id) {
        if (yqsdAmount == 0 || msg.value == 0) revert ZeroAmount();
        if (expiresAt <= block.timestamp) revert InvalidExpiry();

        id = nextQuoteId++;
        quotes[id] = Quote({
            maker:         msg.sender,
            side:          Side.BuyYQSD,
            yqsdInitial:   yqsdAmount,
            qrlInitial:    msg.value,
            yqsdRemaining: yqsdAmount,
            qrlRemaining:  msg.value,
            expiresAt:     expiresAt,
            allowPartial:  allowPartial,
            open:          true
        });
        _addToOpen(id);

        emit QuotePosted(id, msg.sender, Side.BuyYQSD, yqsdAmount, msg.value, expiresAt, allowPartial);
    }

    // ------------------------------------------------------------------
    // Take
    // ------------------------------------------------------------------

    /// @notice Fill `yqsdToFill` of an open quote.
    ///         For SellYQSD: taker buys yQSD, sends QRL via msg.value.
    ///         For BuyYQSD:  taker sells yQSD, receives QRL.
    ///         Partial fills require maker's allowPartial; otherwise
    ///         yqsdToFill must equal yqsdRemaining.
    function take(uint256 id, uint256 yqsdToFill)
        external
        payable
        nonReentrant
    {
        Quote storage q = quotes[id];
        if (!q.open) revert QuoteNotOpen();
        if (block.timestamp > q.expiresAt) revert QuoteExpired();
        if (yqsdToFill == 0) revert ZeroAmount();
        if (yqsdToFill > q.yqsdRemaining) revert InsufficientLiquidity(yqsdToFill, q.yqsdRemaining);

        bool full = (yqsdToFill == q.yqsdRemaining);
        if (!full && !q.allowPartial) revert PartialFillsDisabled();

        // QRL leg pro-rata. Final/full fills sweep qrlRemaining exactly
        // to avoid dust; partials use ceiling division so the maker's
        // posted price is preserved or improved per fill.
        uint256 qrlPortion;
        if (full) {
            qrlPortion = q.qrlRemaining;
        } else {
            qrlPortion = (yqsdToFill * q.qrlInitial + q.yqsdInitial - 1) / q.yqsdInitial;
            if (qrlPortion > q.qrlRemaining) qrlPortion = q.qrlRemaining;
        }

        // Validate msg.value against side.
        if (q.side == Side.SellYQSD) {
            if (msg.value != qrlPortion) revert InvalidValue(qrlPortion, msg.value);
        } else {
            if (msg.value != 0) revert InvalidValue(0, msg.value);
        }

        // Snapshot before state mutation (CEI).
        address maker = q.maker;
        Side    side  = q.side;

        q.yqsdRemaining -= yqsdToFill;
        q.qrlRemaining  -= qrlPortion;
        bool fullyFilled = (q.yqsdRemaining == 0 || q.qrlRemaining == 0);
        if (fullyFilled) {
            q.open = false;
            _removeFromOpen(id);
        }

        // External calls (after state update).
        if (side == Side.SellYQSD) {
            yqsd.safeTransfer(msg.sender, yqsdToFill);
            (bool ok, ) = maker.call{value: qrlPortion}("");
            if (!ok) revert TransferFailed();
        } else {
            yqsd.safeTransferFrom(msg.sender, maker, yqsdToFill);
            (bool ok, ) = msg.sender.call{value: qrlPortion}("");
            if (!ok) revert TransferFailed();
        }

        emit QuoteFilled(id, msg.sender, yqsdToFill, qrlPortion, fullyFilled);
    }

    // ------------------------------------------------------------------
    // Cancel
    // ------------------------------------------------------------------

    /// @notice Cancel an open quote and return locked tokens to the
    ///         maker. The maker may cancel anytime; anyone may cancel
    ///         an expired quote (auto-cleanup).
    function cancel(uint256 id) external nonReentrant {
        Quote storage q = quotes[id];
        if (!q.open) revert QuoteNotOpen();

        bool isMaker = (msg.sender == q.maker);
        bool expired = (block.timestamp > q.expiresAt);
        if (!isMaker && !expired) revert NotMaker();

        Side    side    = q.side;
        address maker   = q.maker;
        uint256 yqsdRem = q.yqsdRemaining;
        uint256 qrlRem  = q.qrlRemaining;

        q.open = false;
        _removeFromOpen(id);

        // Refund locked tokens to maker.
        if (side == Side.SellYQSD) {
            if (yqsdRem > 0) yqsd.safeTransfer(maker, yqsdRem);
        } else {
            if (qrlRem > 0) {
                (bool ok, ) = maker.call{value: qrlRem}("");
                if (!ok) revert TransferFailed();
            }
        }

        emit QuoteCancelled(id, msg.sender);
    }

    // ------------------------------------------------------------------
    // Pagination views
    // ------------------------------------------------------------------

    /// @notice Number of currently-open quotes.
    function openQuoteCount() external view returns (uint256) {
        return openQuoteIds.length;
    }

    /// @notice Page through open quotes. Returns up to `limit` quotes
    ///         starting at `offset`. Caller filters by side / expiry as
    ///         desired (Quote.expiresAt is included in the struct).
    function listOpenQuotes(uint256 offset, uint256 limit)
        external
        view
        returns (uint256[] memory ids, Quote[] memory page, uint256 totalOpen)
    {
        totalOpen = openQuoteIds.length;
        if (offset >= totalOpen || limit == 0) {
            return (new uint256[](0), new Quote[](0), totalOpen);
        }
        uint256 count = totalOpen - offset;
        if (count > limit) count = limit;
        ids = new uint256[](count);
        page = new Quote[](count);
        for (uint256 i = 0; i < count; i++) {
            uint256 qid = openQuoteIds[offset + i];
            ids[i] = qid;
            page[i] = quotes[qid];
        }
    }

    /// @notice Fetch a single quote by id (open or closed).
    function getQuote(uint256 id) external view returns (Quote memory) {
        return quotes[id];
    }

    /// @notice Quote price the maker is offering, in QRL per yQSD,
    ///         1e18-scaled. Useful for callers that want a single
    ///         comparable number rather than the (yqsdAmount, qrlAmount)
    ///         pair.
    function quotePriceQrlPerYqsd(uint256 id) external view returns (uint256) {
        Quote storage q = quotes[id];
        if (q.yqsdInitial == 0) return 0;
        return (q.qrlInitial * 1e18) / q.yqsdInitial;
    }

    // ------------------------------------------------------------------
    // Internal: openQuoteIds bookkeeping
    // ------------------------------------------------------------------

    function _addToOpen(uint256 id) internal {
        openQuoteIndex[id] = openQuoteIds.length;
        openQuoteIds.push(id);
    }

    function _removeFromOpen(uint256 id) internal {
        uint256 idx = openQuoteIndex[id];
        uint256 lastIdx = openQuoteIds.length - 1;
        if (idx != lastIdx) {
            uint256 lastId = openQuoteIds[lastIdx];
            openQuoteIds[idx] = lastId;
            openQuoteIndex[lastId] = idx;
        }
        openQuoteIds.pop();
        delete openQuoteIndex[id];
    }
}
