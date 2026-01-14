package core

import (
	"context"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"go.uber.org/zap"
)

// USDC合约地址 (Base链, 6位小数)
const usdcContractBase = "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"

// BalanceChecker 余额检查器 (Base链)
type BalanceChecker struct {
	ethClient     *ethclient.Client
	usedAddrStore *UsedAddressStore
	logger        *zap.Logger
	interval      time.Duration
	batchSize     int
	concurrency   int
	usdcContract  common.Address // Base USDC (6位小数)
	stats         *Stats
}

// BalanceCheckerConfig 配置
type BalanceCheckerConfig struct {
	Interval    time.Duration // 检查间隔
	BatchSize   int           // 每批检查数量
	Concurrency int           // 并发数
}

// NewBalanceChecker 创建余额检查器
func NewBalanceChecker(
	ethClient *ethclient.Client,
	usedAddrStore *UsedAddressStore,
	logger *zap.Logger,
	stats *Stats,
	config BalanceCheckerConfig,
) *BalanceChecker {
	if config.Interval <= 0 {
		config.Interval = 5 * time.Minute
	}
	if config.BatchSize <= 0 {
		config.BatchSize = 100
	}
	if config.Concurrency <= 0 {
		config.Concurrency = 10
	}

	return &BalanceChecker{
		ethClient:     ethClient,
		usedAddrStore: usedAddrStore,
		logger:        logger,
		stats:         stats,
		interval:      config.Interval,
		batchSize:     config.BatchSize,
		concurrency:   config.Concurrency,
		usdcContract:  common.HexToAddress(usdcContractBase),
	}
}

// Start 启动余额检查器 (后台运行)
func (bc *BalanceChecker) Start(ctx context.Context) {
	bc.logger.Info("💰 余额检查器启动",
		zap.Duration("interval", bc.interval),
		zap.Int("batchSize", bc.batchSize),
		zap.Int("concurrency", bc.concurrency))

	// 启动时先检查一次
	bc.checkBatch(ctx)

	ticker := time.NewTicker(bc.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			bc.logger.Info("💰 余额检查器停止")
			return
		case <-ticker.C:
			bc.checkBatch(ctx)
		}
	}
}

// checkBatch 检查一批地址的余额
func (bc *BalanceChecker) checkBatch(ctx context.Context) {
	// 获取需要检查的地址
	addresses, err := bc.usedAddrStore.GetAddressesNeedCheck(ctx, bc.batchSize)
	if err != nil {
		bc.logger.Warn("获取待检查地址失败", zap.Error(err))
		return
	}

	if len(addresses) == 0 {
		return
	}

	bc.logger.Info("🔍 开始检查余额", zap.Int("count", len(addresses)))

	var wg sync.WaitGroup
	sem := make(chan struct{}, bc.concurrency)
	var valuableCount int64

	for _, addr := range addresses {
		wg.Add(1)
		sem <- struct{}{}
		go func(a UsedFakeAddress) {
			defer wg.Done()
			defer func() { <-sem }()

			ethBalance, usdcBalance, err := bc.getBalances(ctx, a.Address)
			if err != nil {
				bc.logger.Debug("查询余额失败",
					zap.String("address", a.Address[:8]+"..."),
					zap.Error(err))
				return
			}

			// 更新数据库 (ETH余额存储在原bnb_balance字段, USDC存储在原usdt_balance字段)
			if updateErr := bc.usedAddrStore.UpdateBalance(ctx, a.Address, ethBalance, usdcBalance); updateErr != nil {
				bc.logger.Warn("更新余额失败", zap.Error(updateErr))
				return
			}

			// 如果有价值，记录日志 (ETH>0.001 或 USDC>1)
			if ethBalance > 0.001 || usdcBalance > 1 {
				valuableCount++
				bc.logger.Info("💎 发现有价值地址",
					zap.String("address", "0x"+a.Address),
					zap.Float64("ETH", ethBalance),
					zap.Float64("USDC", usdcBalance))
				if bc.stats != nil {
					bc.stats.AddWebLog("INFO", "balance",
						"💎 发现有价值地址",
						"地址: 0x"+a.Address[:10]+"..., ETH: "+formatFloat(ethBalance)+", USDC: "+formatFloat(usdcBalance))
				}
			}
		}(addr)
	}

	wg.Wait()

	if valuableCount > 0 {
		bc.logger.Info("✅ 余额检查完成", zap.Int64("valuable", valuableCount))
	}
}

// getBalances 获取地址的 ETH 和 USDC 余额 (Base链)
func (bc *BalanceChecker) getBalances(ctx context.Context, address string) (eth, usdc float64, err error) {
	addr := common.HexToAddress("0x" + strings.TrimPrefix(address, "0x"))

	// 获取 ETH 余额 (Base原生币)
	ethWei, err := bc.ethClient.BalanceAt(ctx, addr, nil)
	if err != nil {
		return 0, 0, err
	}
	eth = weiToFloat18(ethWei) // ETH 18位小数

	// 获取 USDC 余额 (ERC20, 6位小数)
	usdcWei, err := bc.getERC20Balance(ctx, bc.usdcContract, addr)
	if err != nil {
		return eth, 0, nil // USDC查询失败不影响ETH
	}
	usdc = weiToFloat6(usdcWei) // USDC 6位小数

	return eth, usdc, nil
}

// getERC20Balance 获取 ERC20 代币余额
func (bc *BalanceChecker) getERC20Balance(ctx context.Context, tokenAddr, walletAddr common.Address) (*big.Int, error) {
	// balanceOf(address) 函数签名: 0x70a08231
	data := append([]byte{0x70, 0xa0, 0x82, 0x31}, common.LeftPadBytes(walletAddr.Bytes(), 32)...)

	msg := ethereum.CallMsg{
		To:   &tokenAddr,
		Data: data,
	}
	result, err := bc.ethClient.CallContract(ctx, msg, nil)
	if err != nil {
		return nil, err
	}

	if len(result) < 32 {
		return big.NewInt(0), nil
	}

	return new(big.Int).SetBytes(result), nil
}

// weiToFloat18 将 wei 转换为浮点数 (18位小数, 用于ETH)
func weiToFloat18(wei *big.Int) float64 {
	if wei == nil {
		return 0
	}
	f := new(big.Float).SetInt(wei)
	divisor := new(big.Float).SetFloat64(1e18)
	f.Quo(f, divisor)
	result, _ := f.Float64()
	return result
}

// weiToFloat6 将最小单位转换为浮点数 (6位小数, 用于USDC)
func weiToFloat6(wei *big.Int) float64 {
	if wei == nil {
		return 0
	}
	f := new(big.Float).SetInt(wei)
	divisor := new(big.Float).SetFloat64(1e6)
	f.Quo(f, divisor)
	result, _ := f.Float64()
	return result
}

// weiToFloat 将 wei 转换为浮点数 (18位小数) - 兼容旧代码
func weiToFloat(wei *big.Int) float64 {
	return weiToFloat18(wei)
}

// formatFloat 格式化浮点数
func formatFloat(f float64) string {
	if f == 0 {
		return "0"
	}
	if f < 0.0001 {
		return "<0.0001"
	}
	// 使用简单的字符串格式化
	s := strings.TrimRight(strings.TrimRight(
		string(append([]byte{}, []byte(floatToString(f, 4))...)),
		"0"), ".")
	if s == "" {
		return "0"
	}
	return s
}

// floatToString 将浮点数转换为字符串
func floatToString(f float64, decimals int) string {
	// 简单实现：整数部分 + 小数部分
	intPart := int64(f)
	fracPart := f - float64(intPart)

	result := strconv.FormatInt(intPart, 10)
	if decimals > 0 && fracPart > 0 {
		result += "."
		for i := 0; i < decimals; i++ {
			fracPart *= 10
			digit := int(fracPart) % 10
			result += string(rune('0' + digit))
		}
	}
	return result
}
