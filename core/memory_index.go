package core

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// MemoryIndex 全内存索引 (支持15亿条数据)
// 内存占用估算: 15亿 * 8字节 ≈ 12GB (只存key的存在性)
// 查询延迟: <0.1ms (微秒级)
//
// 数据结构:
//
//	index44: map[prefix4+suffix4] -> bool (只标记存在)
//	index34: map[prefix3+suffix4] -> bool
//	index24: map[prefix2+suffix4] -> bool
//	index14: map[prefix1+suffix4] -> bool
//	index04: map[suffix4] -> bool (只匹配后4位)
//
// 查询流程:
//  1. 内存查找 prefix+suffix -> 是否存在
//  2. 如果存在，从数据库获取完整地址和私钥
type MemoryIndex struct {
	mu sync.RWMutex

	// 核心索引: 只存储key是否存在 (大幅减少内存)
	index44 map[string]bool // prefix4+suffix4 (8字符)
	index34 map[string]bool // prefix3+suffix4 (7字符)
	index24 map[string]bool // prefix2+suffix4 (6字符)
	index14 map[string]bool // prefix1+suffix4 (5字符)
	index04 map[string]bool // suffix4 (4字符)

	// 统计
	totalAddresses int64
	loadTime       time.Duration
	lastUpdate     time.Time

	// 数据库连接 (用于获取地址和私钥)
	db     *pgxpool.Pool
	logger *zap.Logger

	// 分表支持
	useSharding bool
	shardTables []string // vanity_00 ~ vanity_ff
}

// NewMemoryIndex 创建全内存索引
func NewMemoryIndex(db *pgxpool.Pool, logger *zap.Logger) *MemoryIndex {
	return &MemoryIndex{
		index44:     make(map[string]bool),
		index34:     make(map[string]bool),
		index24:     make(map[string]bool),
		index14:     make(map[string]bool),
		index04:     make(map[string]bool),
		db:          db,
		logger:      logger,
		useSharding: false,
		shardTables: generateShardTables(),
	}
}

// generateShardTables 生成256个分表名
func generateShardTables() []string {
	hexChars := []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "a", "b", "c", "d", "e", "f"}
	tables := make([]string, 0, 256)
	for _, c1 := range hexChars {
		for _, c2 := range hexChars {
			tables = append(tables, fmt.Sprintf("vanity_%s%s", c1, c2))
		}
	}
	return tables
}

// EnableSharding 启用分表模式
func (m *MemoryIndex) EnableSharding() {
	m.useSharding = true
	m.logger.Info("📊 内存索引启用分表模式", zap.Int("分表数", len(m.shardTables)))
}

// Load 加载全部数据到内存
// 对于15亿数据，预计耗时5-10分钟
func (m *MemoryIndex) Load(ctx context.Context) error {
	startTime := time.Now()
	m.logger.Info("🔄 开始加载全内存索引...")

	// 先查询总数据量
	var totalInDB int64
	if m.useSharding {
		// 统计分表总数
		for _, table := range m.shardTables {
			var cnt int64
			if err := m.db.QueryRow(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&cnt); err == nil {
				totalInDB += cnt
			}
		}
	} else {
		m.db.QueryRow(ctx, "SELECT COUNT(*) FROM vanity_addresses").Scan(&totalInDB)
	}
	m.logger.Info("📊 待加载数据量", zap.Int64("total", totalInDB))

	// 重置索引 (预分配容量，减少扩容)
	m.mu.Lock()
	m.index44 = make(map[string]bool, totalInDB/10) // 预分配，key会有重复
	m.index34 = make(map[string]bool, totalInDB/10)
	m.index24 = make(map[string]bool, totalInDB/10)
	m.index14 = make(map[string]bool, totalInDB/10)
	m.index04 = make(map[string]bool, totalInDB/100) // 后4位组合较少
	m.totalAddresses = 0
	m.mu.Unlock()

	var loadErr error
	if m.useSharding {
		loadErr = m.loadFromShards(ctx)
	} else {
		loadErr = m.loadFromSingleTable(ctx)
	}

	if loadErr != nil {
		return loadErr
	}

	m.loadTime = time.Since(startTime)
	m.lastUpdate = time.Now()

	// 打印内存使用情况
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	m.mu.RLock()
	m.logger.Info("✅ 全内存索引加载完成",
		zap.Int64("总地址数", m.totalAddresses),
		zap.Int("44索引条目", len(m.index44)),
		zap.Int("34索引条目", len(m.index34)),
		zap.Duration("耗时", m.loadTime),
		zap.Uint64("内存使用MB", memStats.Alloc/1024/1024),
		zap.Uint64("系统内存MB", memStats.Sys/1024/1024))
	m.mu.RUnlock()

	return nil
}

// loadFromSingleTable 从单表加载 (兼容模式)
func (m *MemoryIndex) loadFromSingleTable(ctx context.Context) error {
	return m.loadFromTable(ctx, "vanity_addresses")
}

// loadFromShards 从256个分表并行加载
func (m *MemoryIndex) loadFromShards(ctx context.Context) error {
	var wg sync.WaitGroup
	errChan := make(chan error, len(m.shardTables))

	// 限制并发数
	semaphore := make(chan struct{}, 8)

	for _, tableName := range m.shardTables {
		wg.Add(1)
		go func(table string) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			if err := m.loadFromTable(ctx, table); err != nil {
				m.logger.Warn("加载分表失败", zap.String("table", table), zap.Error(err))
				errChan <- err
			}
		}(tableName)
	}

	wg.Wait()
	close(errChan)

	// 检查是否有错误
	for err := range errChan {
		if err != nil {
			return err
		}
	}

	return nil
}

// loadFromTable 从单个表加载数据
func (m *MemoryIndex) loadFromTable(ctx context.Context, tableName string) error {
	// 使用无超时的context，因为大表查询需要很长时间
	queryCtx := context.Background()

	// 使用游标分批读取，避免一次性加载太多数据
	query := fmt.Sprintf(`SELECT address, prefix, COALESCE(prefix3, LEFT(prefix, 3)), suffix FROM %s`, tableName)

	rows, err := m.db.Query(queryCtx, query)
	if err != nil {
		return fmt.Errorf("query %s: %w", tableName, err)
	}
	defer rows.Close()

	var localCount int64
	startTime := time.Now()
	lastLog := time.Now()

	// 批量写入，减少锁操作 (每10000条写入一次)
	const batchSize = 10000
	type batchKeys struct {
		key44, key34, key24, key14, key04 string
	}
	batch := make([]batchKeys, 0, batchSize)

	flushBatch := func() {
		if len(batch) == 0 {
			return
		}
		m.mu.Lock()
		for _, k := range batch {
			m.index44[k.key44] = true
			m.index34[k.key34] = true
			m.index24[k.key24] = true
			m.index14[k.key14] = true
			m.index04[k.key04] = true
		}
		m.mu.Unlock()
		batch = batch[:0]
	}

	for rows.Next() {
		var address, prefix, prefix3, suffix string
		if err := rows.Scan(&address, &prefix, &prefix3, &suffix); err != nil {
			continue
		}

		prefix = strings.ToLower(strings.TrimSpace(prefix))
		prefix3 = strings.ToLower(strings.TrimSpace(prefix3))
		suffix = strings.ToLower(strings.TrimSpace(suffix))

		// 构建所有前缀长度的key
		keys := batchKeys{
			key44: prefix + suffix,     // 前4后4 (8字符)
			key34: prefix3 + suffix,    // 前3后4 (7字符)
			key24: prefix[:2] + suffix, // 前2后4 (6字符)
			key14: prefix[:1] + suffix, // 前1后4 (5字符)
			key04: suffix,              // 只后4 (4字符)
		}

		batch = append(batch, keys)
		localCount++

		// 批量写入
		if len(batch) >= batchSize {
			flushBatch()
		}

		// 每10秒输出一次进度
		if time.Since(lastLog) > 10*time.Second {
			m.logger.Info("⏳ 加载中...",
				zap.String("table", tableName),
				zap.Int64("已加载", localCount),
				zap.Duration("耗时", time.Since(startTime)))
			lastLog = time.Now()
		}
	}

	// 写入剩余数据
	flushBatch()

	m.mu.Lock()
	m.totalAddresses += localCount
	m.mu.Unlock()

	m.logger.Info("📦 表加载完成",
		zap.String("table", tableName),
		zap.Int64("count", localCount),
		zap.Duration("耗时", time.Since(startTime)))
	return nil
}

// LookupByPrefixLen 根据前缀长度查找
func (m *MemoryIndex) LookupByPrefixLen(prefixLen int, prefix, suffix string) bool {
	var key string
	if prefixLen == 0 {
		key = strings.ToLower(suffix)
	} else {
		key = strings.ToLower(prefix + suffix)
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	switch prefixLen {
	case 4:
		return m.index44[key]
	case 3:
		return m.index34[key]
	case 2:
		return m.index24[key]
	case 1:
		return m.index14[key]
	case 0:
		return m.index04[key]
	}
	return false
}

// Lookup44 检查 prefix4+suffix4 是否存在
func (m *MemoryIndex) Lookup44(prefix4, suffix string) bool {
	return m.LookupByPrefixLen(4, prefix4, suffix)
}

// Lookup34 检查 prefix3+suffix4 是否存在
func (m *MemoryIndex) Lookup34(prefix3, suffix string) bool {
	return m.LookupByPrefixLen(3, prefix3, suffix)
}

// Has44 检查 prefix4+suffix4 是否存在
func (m *MemoryIndex) Has44(prefix4, suffix string) bool {
	return m.LookupByPrefixLen(4, prefix4, suffix)
}

// Has34 检查 prefix3+suffix4 是否存在
func (m *MemoryIndex) Has34(prefix3, suffix string) bool {
	return m.LookupByPrefixLen(3, prefix3, suffix)
}

// GetStats 获取统计信息
func (m *MemoryIndex) GetStats() (total int64, index44Size, index34Size int, loadTime time.Duration) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.totalAddresses, len(m.index44), len(m.index34), m.loadTime
}

// GetAddressWithPrivateKey 根据地址获取私钥 (从数据库)
func (m *MemoryIndex) GetAddressWithPrivateKey(ctx context.Context, address string) (*VanityAddress, error) {
	addr := strings.ToLower(strings.TrimPrefix(address, "0x"))

	var tableName string
	if m.useSharding && len(addr) >= 2 {
		tableName = fmt.Sprintf("vanity_%s", addr[:2])
	} else {
		tableName = "vanity_addresses"
	}

	query := fmt.Sprintf(`
		SELECT id, address, prefix, COALESCE(prefix3, LEFT(prefix, 3)), suffix, encrypted_private_key
		FROM %s WHERE address = $1 LIMIT 1
	`, tableName)

	var va VanityAddress
	err := m.db.QueryRow(ctx, query, addr).Scan(
		&va.ID, &va.Address, &va.Prefix, &va.Prefix3, &va.Suffix, &va.EncryptedPrivateKey,
	)
	if err != nil {
		return nil, err
	}
	return &va, nil
}
