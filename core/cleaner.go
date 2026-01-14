package core

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// CleanerConfig 清理器配置
// 注意: 地址现在可以重复使用，不再删除已使用地址
type CleanerConfig struct {
	Interval  time.Duration // 清理间隔
	MaxDays   int           // 地址最大保留天数 (0=不删除)
	BatchSize int           // 每批删除数量
}

// DefaultCleanerConfig 默认配置
func DefaultCleanerConfig() CleanerConfig {
	return CleanerConfig{
		Interval:  1 * time.Hour,
		MaxDays:   0, // 默认不删除地址 (地址可重复使用)
		BatchSize: 10000,
	}
}

// Cleaner 数据库清理器
type Cleaner struct {
	db     *pgxpool.Pool
	logger *zap.Logger
	config CleanerConfig
	stats  *Stats
	stopCh chan struct{}
}

// NewCleaner 创建清理器
func NewCleaner(db *pgxpool.Pool, logger *zap.Logger, config CleanerConfig, stats *Stats) *Cleaner {
	return &Cleaner{
		db:     db,
		logger: logger,
		config: config,
		stats:  stats,
		stopCh: make(chan struct{}),
	}
}

// Start 启动清理器
func (c *Cleaner) Start(ctx context.Context) {
	c.logger.Info("🧹 数据库清理器启动",
		zap.Duration("间隔", c.config.Interval),
		zap.Int("地址保留天数", c.config.MaxDays))

	// 启动时立即执行一次清理
	c.cleanup(ctx)

	ticker := time.NewTicker(c.config.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.cleanup(ctx)
		}
	}
}

// Stop 停止清理器
func (c *Cleaner) Stop() {
	close(c.stopCh)
}

// cleanup 执行清理
func (c *Cleaner) cleanup(ctx context.Context) {
	start := time.Now()
	c.logger.Info("🧹 开始清理旧数据...")

	var deleted int64
	var err error

	// 只有配置了保留天数才删除旧地址
	if c.config.MaxDays > 0 {
		deleted, err = c.deleteOldAddresses(ctx)
		if err != nil {
			c.logger.Error("删除旧地址失败", zap.Error(err))
		}
	}

	// 清理旧的分表 (如果存在)
	c.cleanupOldPartitions(ctx)

	elapsed := time.Since(start)
	c.logger.Info("🧹 清理完成",
		zap.Int64("删除地址", deleted),
		zap.Duration("耗时", elapsed))

	if c.stats != nil && deleted > 0 {
		c.stats.AddWebLog("INFO", "system",
			"🧹 数据库清理完成",
			fmt.Sprintf("删除: %d条, 耗时: %s", deleted, elapsed.String()))
	}
}

// deleteOldAddresses 删除创建时间过长的地址
func (c *Cleaner) deleteOldAddresses(ctx context.Context) (int64, error) {
	if c.config.MaxDays <= 0 {
		return 0, nil // 禁用
	}
	result, err := c.db.Exec(ctx,
		"DELETE FROM vanity_addresses WHERE created_at < NOW() - $1::interval",
		fmt.Sprintf("%d days", c.config.MaxDays))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected(), nil
}

// cleanupOldPartitions 清理旧的分表
func (c *Cleaner) cleanupOldPartitions(ctx context.Context) {
	// 查找并删除旧的分表 (vanity_addresses_YYYYMMDD)
	rows, err := c.db.Query(ctx, `
		SELECT tablename FROM pg_tables
		WHERE schemaname = 'public'
		AND tablename LIKE 'vanity_addresses_%'
		AND tablename ~ '^vanity_addresses_[0-9]{8}$'
	`)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var tableName string
		if err := rows.Scan(&tableName); err != nil {
			continue
		}
		// 删除旧分表
		_, err := c.db.Exec(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", tableName))
		if err != nil {
			c.logger.Warn("删除旧分表失败", zap.String("table", tableName), zap.Error(err))
		} else {
			c.logger.Info("✅ 删除旧分表", zap.String("table", tableName))
		}
	}
}

// GetTableStats 获取表统计 (地址可重复使用，不再区分已使用/未使用)
func (c *Cleaner) GetTableStats(ctx context.Context) (total int64, err error) {
	err = c.db.QueryRow(ctx, "SELECT COUNT(*) FROM vanity_addresses").Scan(&total)
	return
}
