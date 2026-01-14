package database

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// Partitioner 数据库表管理器 (单表模式)
// 注意: 已从分表模式改为单表模式
// 保留此结构用于:
// 1. 确保主表vanity_addresses存在
// 2. 清理旧的分表数据 (vanity_addresses_YYYYMMDD)
type Partitioner struct {
	db            *pgxpool.Pool
	logger        *zap.Logger
	retentionDays int
}

// NewPartitioner 创建表管理器
func NewPartitioner(db *pgxpool.Pool, logger *zap.Logger, retentionDays int) *Partitioner {
	if retentionDays <= 0 {
		retentionDays = 30 // 默认保留30天
	}
	return &Partitioner{
		db:            db,
		logger:        logger,
		retentionDays: retentionDays,
	}
}

// GetTableName 返回主表名 (单表模式)
func (p *Partitioner) GetTableName() string {
	return "vanity_addresses"
}

// GetCurrentTable 返回主表名 (单表模式)
func (p *Partitioner) GetCurrentTable() string {
	return "vanity_addresses"
}

// EnsureMainTable 确保主表存在 (单表模式，地址可重复使用)
func (p *Partitioner) EnsureMainTable(ctx context.Context) error {
	// 创建主表和索引 (如果不存在)
	// 注意: 移除了 used/used_at 字段，地址可重复使用
	createSQL := `
		CREATE TABLE IF NOT EXISTS vanity_addresses (
			id BIGSERIAL PRIMARY KEY,
			address CHAR(40) NOT NULL,
			prefix CHAR(4) NOT NULL,
			prefix3 CHAR(3) NOT NULL,
			suffix CHAR(4) NOT NULL,
			encrypted_private_key BYTEA NOT NULL,
			created_at TIMESTAMP DEFAULT NOW()
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_address_unique ON vanity_addresses(address);
		CREATE INDEX IF NOT EXISTS idx_prefix4_suffix4 ON vanity_addresses(prefix, suffix);
		CREATE INDEX IF NOT EXISTS idx_prefix3_suffix4 ON vanity_addresses(prefix3, suffix);
	`

	_, err := p.db.Exec(ctx, createSQL)
	if err != nil {
		return fmt.Errorf("create main table: %w", err)
	}

	// 尝试添加 prefix3 列 (如果表已存在但没有该列)
	_, _ = p.db.Exec(ctx, `
		ALTER TABLE vanity_addresses ADD COLUMN IF NOT EXISTS prefix3 CHAR(3);
		UPDATE vanity_addresses SET prefix3 = LEFT(prefix, 3) WHERE prefix3 IS NULL;
		ALTER TABLE vanity_addresses ALTER COLUMN prefix3 SET NOT NULL;
	`)

	// 创建已使用伪造地址表 (保存发送成功的地址)
	_, _ = p.db.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS used_fake_addresses (
			id BIGSERIAL PRIMARY KEY,
			address CHAR(40) NOT NULL,
			encrypted_private_key BYTEA NOT NULL,
			use_count INT NOT NULL DEFAULT 1,
			first_used_at TIMESTAMP NOT NULL DEFAULT NOW(),
			last_used_at TIMESTAMP NOT NULL DEFAULT NOW(),
			bnb_balance NUMERIC(36,18) NOT NULL DEFAULT 0,
			usdt_balance NUMERIC(36,18) NOT NULL DEFAULT 0,
			last_balance_check TIMESTAMP,
			has_value BOOLEAN NOT NULL DEFAULT FALSE
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_used_fake_address_unique ON used_fake_addresses(address);
	`)

	p.logger.Info("✅ 主表 vanity_addresses 已就绪 (地址可重复使用)")
	p.logger.Info("✅ 表 used_fake_addresses 已就绪")
	return nil
}

// EnsureTodayTable 兼容旧接口，实际调用 EnsureMainTable
func (p *Partitioner) EnsureTodayTable(ctx context.Context) error {
	return p.EnsureMainTable(ctx)
}

// GetRecentTables 获取最近N天的旧分表名列表 (用于清理)
func (p *Partitioner) GetRecentTables(days int) []string {
	tables := make([]string, 0, days)
	now := time.Now()
	for i := 0; i < days; i++ {
		date := now.AddDate(0, 0, -i)
		tables = append(tables, fmt.Sprintf("vanity_addresses_%s", date.Format("20060102")))
	}
	return tables
}

// CleanOldPartitionTables 清理旧的分表 (迁移到单表模式后的清理)
// 这会删除所有 vanity_addresses_YYYYMMDD 格式的旧分表
func (p *Partitioner) CleanOldPartitionTables(ctx context.Context) error {
	// 查找所有旧分表
	rows, err := p.db.Query(ctx, `
		SELECT table_name FROM information_schema.tables
		WHERE table_name LIKE 'vanity_addresses_%'
		AND table_name ~ '^vanity_addresses_[0-9]{8}$'
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var deletedCount int
	for rows.Next() {
		var tableName string
		if err := rows.Scan(&tableName); err != nil {
			continue
		}
		// 删除所有旧分表
		_, err := p.db.Exec(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", tableName))
		if err != nil {
			p.logger.Warn("删除旧分表失败", zap.String("table", tableName), zap.Error(err))
			continue
		}
		p.logger.Info("🗑️ 删除旧分表", zap.String("table", tableName))
		deletedCount++
	}

	if deletedCount > 0 {
		p.logger.Info("📊 旧分表清理完成", zap.Int("deleted", deletedCount))
	}
	return nil
}

// CleanOldTables 兼容旧接口
func (p *Partitioner) CleanOldTables(ctx context.Context) error {
	return p.CleanOldPartitionTables(ctx)
}

// Start 启动表管理器 (后台任务)
func (p *Partitioner) Start(ctx context.Context) {
	// 启动时确保主表存在
	if err := p.EnsureMainTable(ctx); err != nil {
		p.logger.Error("确保主表存在失败", zap.Error(err))
	}

	// 启动时清理旧分表 (一次性迁移)
	if err := p.CleanOldPartitionTables(ctx); err != nil {
		p.logger.Error("清理旧分表失败", zap.Error(err))
	}

	// 定时任务：每天检查一次
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// 每天清理一次旧分表 (以防有新的旧数据)
			if err := p.CleanOldPartitionTables(ctx); err != nil {
				p.logger.Error("清理旧分表失败", zap.Error(err))
			}
		}
	}
}
