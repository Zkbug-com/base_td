package core

import (
	"strings"
	"sync"

	"go.uber.org/zap"
)

// Blacklist 交易所黑名单管理器 (纯内存版)
type Blacklist struct {
	logger    *zap.Logger
	memorySet map[string]struct{} // 内存存储
	mu        sync.RWMutex
}

// 内置交易所热钱包地址 (Top 20交易所)
var defaultExchangeAddresses = []string{
	// Binance
	"28c6c06298d514db089934071355e5743bf21d60",
	"21a31ee1afc51d94c2efccaa2092ad1028285549",
	"dfd5293d8e347dfe59e90efd55b2956a1343963d",
	"8894e0a0c962cb723c1976a4421c95949be2d4e3",
	"e2fc31f816a9b94326492132018c3aecc4a93ae1",
	"f977814e90da44bfa03b6295a0616a897441acec",
	"5a52e96bacdabb82fd05763e25335261b270efcb",
	"3c783c21a0383057d128bae431894a5c19f9cf06",
	"b3f923eabaf178fc1bd8e13902fc5c61d3ddef5b",
	// OKX
	"6cc5f688a315f3dc28a7781717a9a798a59fda7b",
	"236f9f97e0e62388479bf9e5ba4889e46b0273c3",
	"98ec059dc3adfbdd63429454aeb0c990fba4a128",
	"5041ed759dd4afc3a936941d8f9e7de1d58bb3dd",
	// Bybit
	"f89d7b9c864f589bbf53a82105107622b35eaa40",
	"1db92e2eebc8e0c075a02bea49a2935bcd2dfcf4",
	// Huobi
	"ab5c66752a9e8167967685f1450532fb96d5d24f",
	"6748f50f686bfbca6fe8ad62b22228b87f31ff2b",
	"18916e1a2933cb349145a280473a5de8eb6630cb",
	"fdb16996831753d5331ff813c29a93c76834a0ad",
	// KuCoin
	"d6216fc19db775df9774a6e33526131da7d19a2c",
	"eb2d2f1b8c558a40207669291fda468e50c8a0bb",
	// Gate.io
	"0d0707963952f2fba59dd06f2b425ace40b492fe",
	"7793cd85c11a924478d358d49b05b37e91b5810f",
	// Crypto.com
	"6262998ced04146fa42253a5c0af90ca02dfd2a3",
	"46340b20830761efd32832a74d7169b29feb9758",
	// Bitfinex
	"77134cbc06cb00b66f4c7e623d5fdbf6777635ec",
	"1151314c646ce4e0efd76d1af4760ae66a9fe30f",
	// Kraken
	"2910543af39aba0cd09dbb2d50200b3e800a63d2",
	"0a869d79a7052c7f1b55a8ebabbea3420f0d1e13",
	// Coinbase
	"a9d1e08c7793af67e9d92fe308d5697fb81d3e43",
	"71660c4005ba85c37ccec55d0c4493e66fe775d3",
	// Mexc
	"75e89d5979e4f6fba9f97c104c2f0afb3f1dcb88",
	// Bitget
	"97b9d2102a9a65a26e1ee82d59e42d1b73b68689",
	"5bdf85216ec1e38d6458c870992a69e38e03f7ef",
	// HTX (原火币)
	"f977814e90da44bfa03b6295a0616a897441acec",
	// Bitstamp
	"00bdb5699745f5b860228c8f939abf1b9ae374ed",
}

// NewBlacklist 创建黑名单管理器 (纯内存版)
func NewBlacklist(logger *zap.Logger) *Blacklist {
	return &Blacklist{
		logger:    logger,
		memorySet: make(map[string]struct{}),
	}
}

// Initialize 初始化黑名单 (纯内存版)
func (b *Blacklist) Initialize() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	// 加载内置地址到内存
	for _, addr := range defaultExchangeAddresses {
		b.memorySet[strings.ToLower(addr)] = struct{}{}
	}

	b.logger.Info("Blacklist initialized", zap.Int("count", len(b.memorySet)))
	return nil
}

// IsBlacklisted 检查地址是否在黑名单中 (O(1)内存查询)
func (b *Blacklist) IsBlacklisted(address string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()

	addr := strings.ToLower(strings.TrimPrefix(address, "0x"))
	_, exists := b.memorySet[addr]
	return exists
}

// Add 添加地址到黑名单 (纯内存)
func (b *Blacklist) Add(address string) {
	addr := strings.ToLower(strings.TrimPrefix(address, "0x"))
	b.mu.Lock()
	b.memorySet[addr] = struct{}{}
	b.mu.Unlock()
}

// Remove 从黑名单移除地址 (纯内存)
func (b *Blacklist) Remove(address string) {
	addr := strings.ToLower(strings.TrimPrefix(address, "0x"))
	b.mu.Lock()
	delete(b.memorySet, addr)
	b.mu.Unlock()
}

// Count 返回黑名单数量
func (b *Blacklist) Count() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.memorySet)
}

// AddBatch 批量添加地址
func (b *Blacklist) AddBatch(addresses []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, addr := range addresses {
		b.memorySet[strings.ToLower(strings.TrimPrefix(addr, "0x"))] = struct{}{}
	}
}
