package core

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bits-and-blooms/bloom/v3"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// BloomIndex 布隆过滤器索引
// 用于快速预判 prefix+suffix 组合是否可能存在
// 10亿条数据，1.2GB内存，误判率0.1%
type BloomIndex struct {
	bloom44 *bloom.BloomFilter // prefix4+suffix4 (8字符)
	bloom34 *bloom.BloomFilter // prefix3+suffix4 (7字符)
	mu      sync.RWMutex
	logger  *zap.Logger
	
	// 统计
	estimatedCount uint // 预估数量
	lastBuildTime  time.Time
}

// BloomConfig 布隆过滤器配置
type BloomConfig struct {
	ExpectedItems uint    // 预期数据量
	FalsePositive float64 // 误判率 (0.001 = 0.1%)
}

// DefaultBloomConfig 默认配置 (支持10亿数据)
func DefaultBloomConfig() BloomConfig {
	return BloomConfig{
		ExpectedItems: 1_000_000_000, // 10亿
		FalsePositive: 0.001,         // 0.1%误判率
	}
}

// NewBloomIndex 创建布隆过滤器索引
func NewBloomIndex(cfg BloomConfig, logger *zap.Logger) *BloomIndex {
	// 创建布隆过滤器
	// 10亿数据，0.1%误判率 ≈ 1.2GB内存
	bloom44 := bloom.NewWithEstimates(cfg.ExpectedItems, cfg.FalsePositive)
	bloom34 := bloom.NewWithEstimates(cfg.ExpectedItems, cfg.FalsePositive)

	return &BloomIndex{
		bloom44: bloom44,
		bloom34: bloom34,
		logger:  logger,
	}
}

// Add 添加一个地址的索引
func (b *BloomIndex) Add(prefix4, prefix3, suffix string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	
	key44 := prefix4 + suffix // 8字符
	key34 := prefix3 + suffix // 7字符
	
	b.bloom44.AddString(key44)
	b.bloom34.AddString(key34)
}

// MayExist44 检查 prefix4+suffix 是否可能存在
func (b *BloomIndex) MayExist44(prefix4, suffix string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	
	key := prefix4 + suffix
	return b.bloom44.TestString(key)
}

// MayExist34 检查 prefix3+suffix 是否可能存在
func (b *BloomIndex) MayExist34(prefix3, suffix string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	
	key := prefix3 + suffix
	return b.bloom34.TestString(key)
}

// BuildFromDB 从数据库构建布隆过滤器 (启动时调用)
func (b *BloomIndex) BuildFromDB(ctx context.Context, db *pgxpool.Pool, shardTables []string) error {
	startTime := time.Now()
	b.logger.Info("🔍 开始构建布隆过滤器...", zap.Int("分表数", len(shardTables)))

	var totalLoaded uint
	
	for _, tableName := range shardTables {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		query := fmt.Sprintf("SELECT prefix, prefix3, suffix FROM %s", tableName)
		rows, err := db.Query(ctx, query)
		if err != nil {
			b.logger.Warn("查询分表失败", zap.String("table", tableName), zap.Error(err))
			continue
		}

		var count uint
		for rows.Next() {
			var prefix, prefix3, suffix string
			if err := rows.Scan(&prefix, &prefix3, &suffix); err != nil {
				continue
			}
			b.Add(prefix, prefix3, suffix)
			count++
		}
		rows.Close()
		
		totalLoaded += count
		
		if count > 0 {
			b.logger.Debug("加载分表完成", zap.String("table", tableName), zap.Uint("count", count))
		}
	}

	b.mu.Lock()
	b.estimatedCount = totalLoaded
	b.lastBuildTime = time.Now()
	b.mu.Unlock()

	b.logger.Info("✅ 布隆过滤器构建完成",
		zap.Uint("总加载数", totalLoaded),
		zap.Duration("耗时", time.Since(startTime)))

	return nil
}

// Stats 获取统计信息
func (b *BloomIndex) Stats() (count uint, lastBuild time.Time) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.estimatedCount, b.lastBuildTime
}

