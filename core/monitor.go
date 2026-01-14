package core

import (
	"bufio"
	"context"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"go.uber.org/zap"
	"golang.org/x/net/proxy"
)

const (
	// Transfer事件签名
	TransferEventSignature = "Transfer(address,address,uint256)"
	// 默认代币合约地址 (Base链)
	DefaultUSDCContract = "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913" // Base USDC (6位小数)
)

// TokenInfo 代币信息
type TokenInfo struct {
	Address  common.Address
	Symbol   string
	Decimals int
}

// MonitorConfig 监控器配置
type MonitorConfig struct {
	USDCContract string   // USDC合约地址 (Base链)
	WSUrls       []string // WebSocket节点列表 (支持故障转移)
	ProxyURL     *url.URL // 代理URL (可选)
}

// Monitor Base链上监控器
type Monitor struct {
	wsClient      *ethclient.Client
	rpcClient     *rpc.Client // 底层RPC客户端，用于原生订阅
	wsUrls        []string    // WS节点列表
	currentWsIdx  int         // 当前使用的节点索引
	queue         *MatchQueue // 内存队列 (替代Redis)
	filter        *Filter
	matcher       *Matcher
	dedup         *Deduplicator // 去重器
	logger        *zap.Logger
	stats         *Stats
	tokens        map[common.Address]TokenInfo // 代币地址 -> 代币信息
	transferTopic common.Hash
	proxyURL      *url.URL // 代理URL

	// 未匹配地址记录
	missedFile   *os.File
	missedWriter *bufio.Writer
	missedMu     sync.Mutex
}

// NewMonitor 创建监控器 (使用内存队列)
func NewMonitor(
	wsClient *ethclient.Client,
	queue *MatchQueue,
	filter *Filter,
	matcher *Matcher,
	logger *zap.Logger,
) *Monitor {
	return NewMonitorWithConfig(wsClient, queue, filter, matcher, logger, MonitorConfig{
		USDCContract: DefaultUSDCContract,
	})
}

// NewMonitorWithConfig 使用配置创建监控器
func NewMonitorWithConfig(
	wsClient *ethclient.Client,
	queue *MatchQueue,
	filter *Filter,
	matcher *Matcher,
	logger *zap.Logger,
	config MonitorConfig,
) *Monitor {
	return NewMonitorWithStats(wsClient, queue, filter, matcher, nil, logger, nil, config)
}

// NewMonitorWithStats 使用统计创建监控器
func NewMonitorWithStats(
	wsClient *ethclient.Client,
	queue *MatchQueue,
	filter *Filter,
	matcher *Matcher,
	dedup *Deduplicator,
	logger *zap.Logger,
	stats *Stats,
	config MonitorConfig,
) *Monitor {
	// 初始化代币列表 (Base链只监听USDC)
	tokens := make(map[common.Address]TokenInfo)

	// USDC (Base链 - 6位小数)
	usdcContract := config.USDCContract
	if usdcContract == "" {
		usdcContract = DefaultUSDCContract
	}
	usdcAddr := common.HexToAddress(usdcContract)
	tokens[usdcAddr] = TokenInfo{Address: usdcAddr, Symbol: "USDC", Decimals: 6}

	return &Monitor{
		wsClient:      wsClient,
		wsUrls:        config.WSUrls,
		currentWsIdx:  0,
		queue:         queue,
		filter:        filter,
		matcher:       matcher,
		dedup:         dedup,
		logger:        logger,
		stats:         stats,
		tokens:        tokens,
		transferTopic: crypto.Keccak256Hash([]byte(TransferEventSignature)),
		proxyURL:      config.ProxyURL,
	}
}

// EnableMissedLogging 启用未匹配地址记录
func (m *Monitor) EnableMissedLogging(filePath string) error {
	m.missedMu.Lock()
	defer m.missedMu.Unlock()

	file, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("打开未匹配记录文件失败: %w", err)
	}

	m.missedFile = file
	m.missedWriter = bufio.NewWriter(file)
	m.logger.Info("📝 未匹配地址记录已启用", zap.String("file", filePath))
	return nil
}

// CloseMissedLogging 关闭未匹配地址记录
func (m *Monitor) CloseMissedLogging() {
	m.missedMu.Lock()
	defer m.missedMu.Unlock()

	if m.missedWriter != nil {
		m.missedWriter.Flush()
	}
	if m.missedFile != nil {
		m.missedFile.Close()
	}
}

// logMissedCombo 记录未匹配的 prefix3+suffix 组合
func (m *Monitor) logMissedCombo(addr string) {
	if m.missedWriter == nil {
		return
	}

	// 提取 prefix3 和 suffix (去掉0x前缀)
	cleanAddr := strings.TrimPrefix(strings.ToLower(addr), "0x")
	if len(cleanAddr) < 40 {
		return
	}
	prefix3 := cleanAddr[:3]
	suffix := cleanAddr[36:]
	combo := prefix3 + suffix

	m.missedMu.Lock()
	defer m.missedMu.Unlock()

	m.missedWriter.WriteString(combo + "\n")
}

// connectWS 连接到WebSocket节点，支持自动故障转移和代理
// 返回 ethclient 和底层 rpc.Client
func (m *Monitor) connectWS() (*ethclient.Client, *rpc.Client, error) {
	if len(m.wsUrls) == 0 {
		return nil, nil, fmt.Errorf("no WebSocket URLs configured")
	}

	// 尝试所有节点
	for i := 0; i < len(m.wsUrls); i++ {
		idx := (m.currentWsIdx + i) % len(m.wsUrls)
		wsURL := m.wsUrls[idx]

		hasProxy := m.proxyURL != nil
		m.logger.Info("🔌 尝试连接WebSocket节点",
			zap.String("url", wsURL),
			zap.Int("index", idx+1),
			zap.Int("total", len(m.wsUrls)),
			zap.Bool("proxy", hasProxy))

		var client *ethclient.Client
		var rpcCli *rpc.Client
		var err error

		if m.proxyURL != nil {
			// 使用代理连接WebSocket
			client, rpcCli, err = m.dialWSWithProxy(wsURL)
		} else {
			// 直接连接
			rpcCli, err = rpc.Dial(wsURL)
			if err == nil {
				client = ethclient.NewClient(rpcCli)
			}
		}

		if err != nil {
			m.logger.Warn("❌ WebSocket连接失败",
				zap.String("url", wsURL),
				zap.Bool("proxy", hasProxy),
				zap.Error(err))
			continue
		}

		// 测试连接
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err = client.BlockNumber(ctx)
		cancel()
		if err != nil {
			m.logger.Warn("❌ WebSocket测试失败",
				zap.String("url", wsURL),
				zap.Error(err))
			client.Close()
			continue
		}

		m.currentWsIdx = idx
		m.logger.Info("✅ WebSocket连接成功",
			zap.String("url", wsURL),
			zap.Int("index", idx+1),
			zap.Bool("proxy", hasProxy))
		return client, rpcCli, nil
	}

	return nil, nil, fmt.Errorf("all %d WebSocket nodes failed", len(m.wsUrls))
}

// dialWSWithProxy 通过代理连接WebSocket (支持SOCKS5和HTTP)
// 返回 ethclient 和底层 rpc.Client (用于原生订阅)
func (m *Monitor) dialWSWithProxy(wsURL string) (*ethclient.Client, *rpc.Client, error) {
	var proxyDialer func(ctx context.Context, network, addr string) (net.Conn, error)

	// 判断代理类型
	if m.proxyURL.Scheme == "socks5" {
		// SOCKS5 代理
		var auth *proxy.Auth
		if m.proxyURL.User != nil {
			password, _ := m.proxyURL.User.Password()
			auth = &proxy.Auth{
				User:     m.proxyURL.User.Username(),
				Password: password,
			}
		}

		socks5Dialer, err := proxy.SOCKS5("tcp", m.proxyURL.Host, auth, proxy.Direct)
		if err != nil {
			return nil, nil, fmt.Errorf("创建SOCKS5拨号器失败: %w", err)
		}

		proxyDialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return socks5Dialer.Dial(network, addr)
		}
		m.logger.Debug("使用SOCKS5代理", zap.String("host", m.proxyURL.Host))
	} else {
		// HTTP CONNECT 代理
		dialer := &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}

		proxyDialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
			proxyConn, err := dialer.DialContext(ctx, "tcp", m.proxyURL.Host)
			if err != nil {
				return nil, fmt.Errorf("连接代理失败: %w", err)
			}

			connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
			if m.proxyURL.User != nil {
				password, _ := m.proxyURL.User.Password()
				auth := m.proxyURL.User.Username() + ":" + password
				encoded := base64Encode(auth)
				connectReq += fmt.Sprintf("Proxy-Authorization: Basic %s\r\n", encoded)
			}
			connectReq += "\r\n"

			_, err = proxyConn.Write([]byte(connectReq))
			if err != nil {
				proxyConn.Close()
				return nil, fmt.Errorf("发送CONNECT请求失败: %w", err)
			}

			buf := make([]byte, 1024)
			n, err := proxyConn.Read(buf)
			if err != nil {
				proxyConn.Close()
				return nil, fmt.Errorf("读取代理响应失败: %w", err)
			}

			response := string(buf[:n])
			if !strings.Contains(response, "200") {
				proxyConn.Close()
				return nil, fmt.Errorf("代理连接失败: %s", response)
			}

			return proxyConn, nil
		}
		m.logger.Debug("使用HTTP代理", zap.String("host", m.proxyURL.Host))
	}

	// 使用自定义拨号器创建RPC客户端
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rpcClient, err := rpc.DialOptions(ctx, wsURL,
		rpc.WithHTTPClient(&http.Client{
			Transport: &http.Transport{
				DialContext: proxyDialer,
			},
			Timeout: 30 * time.Second,
		}),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("RPC连接失败: %w", err)
	}

	return ethclient.NewClient(rpcClient), rpcClient, nil
}

// base64Encode Base64编码
func base64Encode(s string) string {
	const base64Table = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	result := ""
	padding := 0
	data := []byte(s)

	for i := 0; i < len(data); i += 3 {
		n := int(data[i]) << 16
		if i+1 < len(data) {
			n |= int(data[i+1]) << 8
		} else {
			padding++
		}
		if i+2 < len(data) {
			n |= int(data[i+2])
		} else {
			padding++
		}

		result += string(base64Table[(n>>18)&0x3F])
		result += string(base64Table[(n>>12)&0x3F])
		if padding < 2 {
			result += string(base64Table[(n>>6)&0x3F])
		} else {
			result += "="
		}
		if padding < 1 {
			result += string(base64Table[n&0x3F])
		} else {
			result += "="
		}
	}
	return result
}

// Start 启动监控 (带自动重连)
func (m *Monitor) Start(ctx context.Context) error {
	// 列出所有监控的代币
	tokenList := make([]string, 0, len(m.tokens))
	for _, t := range m.tokens {
		tokenList = append(tokenList, t.Symbol)
	}
	m.logger.Info("Starting Base monitor",
		zap.Strings("tokens", tokenList),
		zap.Int("ws_nodes", len(m.wsUrls)))

	for {
		select {
		case <-ctx.Done():
			m.logger.Info("Monitor stopped")
			return ctx.Err()
		default:
		}

		// 连接WebSocket
		client, rpcCli, err := m.connectWS()
		if err != nil {
			m.logger.Error("所有WebSocket节点连接失败，10秒后重试", zap.Error(err))
			if m.stats != nil {
				m.stats.AddWebLog("ERROR", "monitor", "❌ 所有WS节点连接失败", err.Error())
			}
			time.Sleep(10 * time.Second)
			continue
		}
		m.wsClient = client
		m.rpcClient = rpcCli

		// 运行监控循环
		err = m.runMonitorLoop(ctx)
		if err != nil {
			m.logger.Warn("⚠️ 监控循环断开，尝试切换节点",
				zap.Error(err))
			if m.stats != nil {
				m.stats.AddWebLog("WARN", "monitor", "⚠️ WS连接断开，正在重连...", err.Error())
			}
			// 切换到下一个节点
			m.currentWsIdx = (m.currentWsIdx + 1) % len(m.wsUrls)
			client.Close()
			time.Sleep(2 * time.Second)
			continue
		}
	}
}

// runMonitorLoop 运行监控循环
func (m *Monitor) runMonitorLoop(ctx context.Context) error {
	// 构建所有代币地址列表
	tokenSymbols := make([]string, 0, len(m.tokens))
	addressStrings := make([]string, 0, len(m.tokens))
	for addr, info := range m.tokens {
		tokenSymbols = append(tokenSymbols, info.Symbol)
		addressStrings = append(addressStrings, addr.Hex())
	}

	// 使用原生 eth_subscribe 订阅日志
	// 避免 go-ethereum 的 SubscribeFilterLogs 可能触发 eth_getLogs 历史查询
	logs := make(chan types.Log, 1000)

	// 构建订阅参数 - 只订阅实时日志，不查询历史
	filterArgs := map[string]interface{}{
		"address": addressStrings,
		"topics":  []interface{}{m.transferTopic.Hex()},
	}

	sub, err := m.rpcClient.EthSubscribe(ctx, logs, "logs", filterArgs)
	if err != nil {
		return fmt.Errorf("subscribe failed: %w", err)
	}
	defer sub.Unsubscribe()

	m.logger.Info("✅ 已订阅多代币 Transfer 事件", zap.Strings("tokens", tokenSymbols))
	if m.stats != nil {
		m.stats.AddWebLog("INFO", "monitor", fmt.Sprintf("✅ Base监控已订阅 (%s)", strings.Join(tokenSymbols, "/")), m.wsUrls[m.currentWsIdx])
	}

	// 超时检测: Base每2秒出块，2分钟没收到数据说明连接已断开
	const heartbeatTimeout = 2 * time.Minute
	heartbeatTimer := time.NewTimer(heartbeatTimeout)
	defer heartbeatTimer.Stop()

	// 定期检测连接: 每30秒ping一次 (保持代理连接活跃)
	pingTicker := time.NewTicker(30 * time.Second)
	defer pingTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case err := <-sub.Err():
			return fmt.Errorf("subscription error: %w", err)

		case <-heartbeatTimer.C:
			// 超时没收到数据，认为连接已断开
			m.logger.Warn("⚠️ WebSocket心跳超时，5分钟未收到数据")
			return fmt.Errorf("heartbeat timeout: no data for %v", heartbeatTimeout)

		case <-pingTicker.C:
			// 定期ping检测连接是否存活
			pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, err := m.wsClient.BlockNumber(pingCtx)
			cancel()
			if err != nil {
				m.logger.Warn("⚠️ WebSocket ping失败", zap.Error(err))
				return fmt.Errorf("ping failed: %w", err)
			}
			m.logger.Debug("💓 WebSocket心跳正常")

		case vLog := <-logs:
			// 收到数据，重置心跳计时器
			heartbeatTimer.Reset(heartbeatTimeout)
			go m.handleLog(ctx, vLog)
		}
	}
}

// handleLog 处理单个Transfer事件
func (m *Monitor) handleLog(ctx context.Context, vLog types.Log) {
	start := time.Now()

	// 统计: 检测到转账
	if m.stats != nil {
		m.stats.IncrDetected()
		m.stats.UpdateActivity()
	}

	// 解析Transfer事件
	// Transfer(address indexed from, address indexed to, uint256 value)
	if len(vLog.Topics) < 3 {
		return
	}

	// 识别代币类型
	tokenInfo, ok := m.tokens[vLog.Address]
	if !ok {
		// 未知代币，跳过
		return
	}

	from := common.HexToAddress(vLog.Topics[1].Hex())
	to := common.HexToAddress(vLog.Topics[2].Hex())
	amount := new(big.Int).SetBytes(vLog.Data)

	// 计算USD金额 (根据代币小数位数)
	amountUSD := new(big.Float).SetInt(amount)
	// Base USDC 是6位小数
	divisor := new(big.Float).SetInt(big.NewInt(1e6))
	amountUSD.Quo(amountUSD, divisor)
	usdFloat, _ := amountUSD.Float64()

	// 新逻辑: A→B转账，MatchAddr=B(接收方)用于匹配，PoisonTo=A(发送方)
	fromAddr := strings.ToLower(from.Hex()[2:])
	toAddr := strings.ToLower(to.Hex()[2:])

	target := Target{
		Address:   toAddr,   // 兼容旧逻辑
		MatchAddr: toAddr,   // B: 用于匹配伪造地址的前后4位
		PoisonTo:  fromAddr, // A: 投毒目标(发送方)
		Amount:    amount,
		AmountUSD: usdFloat,
		TxHash:    vLog.TxHash.Hex()[2:],
		From:      fromAddr,
		BlockNum:  vLog.BlockNumber,
		TokenType: tokenInfo.Symbol, // 代币类型: USDT, USDC, WBNB
	}

	// 过滤检查
	if !m.filter.ShouldPoison(ctx, target) {
		return
	}

	// 统计: 过滤后
	if m.stats != nil {
		m.stats.IncrFiltered()
	}

	// 匹配伪造地址
	matched, err := m.matcher.Match(ctx, target)
	if err != nil {
		m.logger.Warn("Match error", zap.Error(err))
		return
	}
	if matched == nil {
		// 没有匹配的伪造地址，记录用于后续分析
		m.logMissedCombo(target.MatchAddr)
		return
	}

	// 显示信息
	// MatchAddr = B (接收方，用于匹配伪造地址)
	// PoisonTo = A (发送方，投毒目标)
	matchAddrShort := target.MatchAddr[:8] + "..." + target.MatchAddr[36:]
	poisonToShort := target.PoisonTo[:8] + "..." + target.PoisonTo[36:]
	fakeShort := matched.FakeAddress.Address[:8] + "..." + matched.FakeAddress.Address[36:]

	// 去重检查: 2天内是否已对该投毒目标(发送方A)发送过
	if m.dedup != nil {
		inCooldown, lastRecord, err := m.dedup.CheckCooldown(ctx, target.PoisonTo)
		if err != nil {
			m.logger.Warn("去重检查失败", zap.Error(err))
			// 检查失败继续处理
		} else if inCooldown && lastRecord != nil {
			// 在冷却期内，跳过
			cooldownHours := time.Since(lastRecord.SentAt).Hours()
			m.logger.Debug("⏭️ 投毒目标在冷却期内，跳过",
				zap.String("poisonTo", poisonToShort),
				zap.Float64("已过小时", cooldownHours),
				zap.String("上次TxHash", lastRecord.TxHash[:16]+"..."))
			if m.stats != nil {
				m.stats.AddWebLog("DEBUG", "dedup",
					fmt.Sprintf("⏭️ 跳过: 投毒目标 %s 在2天内已投毒", poisonToShort),
					fmt.Sprintf("上次发送: %.1f小时前", cooldownHours))
			}
			return
		}
	}

	// 统计: 匹配成功
	if m.stats != nil {
		m.stats.IncrMatch()
		m.stats.MatchesPending.Add(1)
	}

	// 推送到内存队列 (替代Redis)
	if !m.queue.Push(*matched) {
		m.logger.Error("队列已满，匹配丢弃")
		return
	}

	elapsed := time.Since(start)
	totalMatches := m.stats.MatchesFound.Load()
	pending := m.stats.MatchesPending.Load()

	// 日志: 匹配B的前后4位，给A发送投毒
	m.logger.Info("🎯 匹配成功 (B→A投毒)",
		zap.String("匹配地址(B)", matchAddrShort),
		zap.String("投毒目标(A)", poisonToShort),
		zap.String("伪造地址", fakeShort),
		zap.Float64("金额USD", usdFloat),
		zap.Duration("延迟", elapsed),
		zap.Int64("总匹配", totalMatches))

	// 添加Web日志
	if m.stats != nil {
		m.stats.AddWebLog("INFO", "match",
			fmt.Sprintf("🎯 匹配成功 #%d: 匹配%s → 投毒%s (伪造%s)",
				totalMatches, matchAddrShort, poisonToShort, fakeShort),
			fmt.Sprintf("金额: $%.2f, 延迟: %v, 待处理: %d", usdFloat, elapsed.Round(time.Microsecond), pending))
	}
}
