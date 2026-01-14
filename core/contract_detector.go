package core

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	lru "github.com/hashicorp/golang-lru/v2"
	"go.uber.org/zap"
)

const (
	// LRU缓存大小 (纯内存，无Redis)
	lruCacheSize = 200000
)

// ContractDetector 合约地址检测器 (支持多RPC节点+代理)
type ContractDetector struct {
	rpcUrls    []string
	clients    []*ethclient.Client
	currentIdx uint32 // 当前使用的节点索引
	logger     *zap.Logger
	lruCache   *lru.Cache[string, bool]
	httpClient *http.Client // 代理HTTP客户端
	mu         sync.RWMutex
}

// NewContractDetector 创建合约检测器 (单节点兼容)
func NewContractDetector(ethClient *ethclient.Client, logger *zap.Logger) (*ContractDetector, error) {
	cache, err := lru.New[string, bool](lruCacheSize)
	if err != nil {
		return nil, err
	}

	return &ContractDetector{
		rpcUrls:  []string{},
		clients:  []*ethclient.Client{ethClient},
		logger:   logger,
		lruCache: cache,
	}, nil
}

// NewContractDetectorMulti 创建合约检测器 (多节点)
func NewContractDetectorMulti(rpcUrls []string, logger *zap.Logger) (*ContractDetector, error) {
	return NewContractDetectorWithProxy(rpcUrls, nil, logger)
}

// NewContractDetectorWithProxy 创建合约检测器 (多节点+代理)
func NewContractDetectorWithProxy(rpcUrls []string, httpClient *http.Client, logger *zap.Logger) (*ContractDetector, error) {
	cache, err := lru.New[string, bool](lruCacheSize)
	if err != nil {
		return nil, err
	}

	return &ContractDetector{
		rpcUrls:    rpcUrls,
		clients:    make([]*ethclient.Client, 0),
		logger:     logger,
		lruCache:   cache,
		httpClient: httpClient,
	}, nil
}

// getClient 获取一个可用的RPC客户端 (轮换)
func (d *ContractDetector) getClient(ctx context.Context) (*ethclient.Client, error) {
	// 如果有预连接的客户端，轮换使用
	d.mu.RLock()
	if len(d.clients) > 0 {
		idx := atomic.AddUint32(&d.currentIdx, 1) % uint32(len(d.clients))
		client := d.clients[idx]
		d.mu.RUnlock()
		return client, nil
	}
	d.mu.RUnlock()

	// 动态连接节点
	if len(d.rpcUrls) == 0 {
		return nil, nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// 再次检查 (避免重复连接)
	if len(d.clients) > 0 {
		idx := atomic.AddUint32(&d.currentIdx, 1) % uint32(len(d.clients))
		return d.clients[idx], nil
	}

	// 连接所有节点 (支持代理)
	for _, url := range d.rpcUrls {
		url = strings.TrimSpace(url)
		var client *ethclient.Client
		var err error

		if d.httpClient != nil {
			// 使用代理HTTP客户端
			rpcClient, rpcErr := rpc.DialHTTPWithClient(url, d.httpClient)
			if rpcErr != nil {
				d.logger.Warn("RPC连接失败(代理)", zap.String("url", url), zap.Error(rpcErr))
				continue
			}
			client = ethclient.NewClient(rpcClient)
		} else {
			// 直接连接
			client, err = ethclient.Dial(url)
			if err != nil {
				d.logger.Warn("RPC连接失败", zap.String("url", url), zap.Error(err))
				continue
			}
		}
		d.clients = append(d.clients, client)
		d.logger.Info("✅ RPC节点连接成功", zap.String("url", url), zap.Bool("proxy", d.httpClient != nil))
	}

	if len(d.clients) == 0 {
		return nil, nil
	}

	return d.clients[0], nil
}

// IsContract 检查地址是否为合约
// 优先级: 内存LRU -> RPC (多节点轮换)
func (d *ContractDetector) IsContract(ctx context.Context, address string) (bool, error) {
	addr := strings.ToLower(strings.TrimPrefix(address, "0x"))

	// 1. 检查LRU缓存
	if isContract, ok := d.lruCache.Get(addr); ok {
		return isContract, nil
	}

	// 2. 调用RPC检查 (尝试多个节点)
	isContract, err := d.checkViaRPCWithRetry(ctx, addr)
	if err != nil {
		return false, err
	}

	// 写入缓存
	d.lruCache.Add(addr, isContract)
	return isContract, nil
}

// checkViaRPCWithRetry 通过RPC检查，失败时尝试其他节点
func (d *ContractDetector) checkViaRPCWithRetry(ctx context.Context, address string) (bool, error) {
	addr := common.HexToAddress(address)

	d.mu.RLock()
	clientCount := len(d.clients)
	d.mu.RUnlock()

	// 尝试所有节点
	maxRetries := clientCount
	if maxRetries == 0 {
		maxRetries = len(d.rpcUrls)
	}
	if maxRetries == 0 {
		maxRetries = 1
	}

	var lastErr error
	for i := 0; i < maxRetries; i++ {
		client, err := d.getClient(ctx)
		if err != nil || client == nil {
			continue
		}

		code, err := client.CodeAt(ctx, addr, nil)
		if err != nil {
			lastErr = err
			continue
		}

		return len(code) > 0, nil
	}

	return false, lastErr
}

// BatchCheck 批量检查地址（纯内存版）
func (d *ContractDetector) BatchCheck(ctx context.Context, addresses []string) (map[string]bool, error) {
	results := make(map[string]bool)
	needRPC := make([]string, 0)

	// 先检查LRU缓存
	for _, address := range addresses {
		addr := strings.ToLower(strings.TrimPrefix(address, "0x"))

		if isContract, ok := d.lruCache.Get(addr); ok {
			results[addr] = isContract
			continue
		}

		needRPC = append(needRPC, addr)
	}

	// RPC检查 (使用多节点重试)
	for _, addr := range needRPC {
		isContract, err := d.checkViaRPCWithRetry(ctx, addr)
		if err != nil {
			d.logger.Warn("RPC check failed", zap.String("address", addr), zap.Error(err))
			results[addr] = false
			continue
		}

		results[addr] = isContract
		d.lruCache.Add(addr, isContract)
	}

	return results, nil
}

// CacheStats 返回缓存统计
func (d *ContractDetector) CacheStats() int {
	return d.lruCache.Len()
}
