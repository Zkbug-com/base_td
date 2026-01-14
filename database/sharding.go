package database

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// ShardRouter 分表路由器
// 按地址prefix前2位分成256个表: vanity_00 ~ vanity_ff
type ShardRouter struct {
	db     *pgxpool.Pool
	logger *zap.Logger
	mu     sync.RWMutex
	ready  map[string]bool // 已就绪的分表
}

// NewShardRouter 创建分表路由器
func NewShardRouter(db *pgxpool.Pool, logger *zap.Logger) *ShardRouter {
	return &ShardRouter{
		db:     db,
		logger: logger,
		ready:  make(map[string]bool),
	}
}

// GetTableName 根据地址获取分表名
// address: 40位hex地址(不含0x)
func (r *ShardRouter) GetTableName(address string) string {
	addr := strings.ToLower(address)
	if len(addr) < 2 {
		return "vanity_00"
	}
	return fmt.Sprintf("vanity_%s", addr[:2])
}

// GetTableByPrefix 根据prefix获取分表名
func (r *ShardRouter) GetTableByPrefix(prefix string) string {
	p := strings.ToLower(prefix)
	if len(p) < 2 {
		return "vanity_00"
	}
	return fmt.Sprintf("vanity_%s", p[:2])
}

// GetAllTableNames 获取所有256个分表名
func (r *ShardRouter) GetAllTableNames() []string {
	hexChars := []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "a", "b", "c", "d", "e", "f"}
	tables := make([]string, 0, 256)
	for _, c1 := range hexChars {
		for _, c2 := range hexChars {
			tables = append(tables, fmt.Sprintf("vanity_%s%s", c1, c2))
		}
	}
	return tables
}

// EnsureAllTables 确保所有256个分表存在
func (r *ShardRouter) EnsureAllTables(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	hexChars := []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "a", "b", "c", "d", "e", "f"}
	
	for _, c1 := range hexChars {
		for _, c2 := range hexChars {
			shardKey := c1 + c2
			tableName := fmt.Sprintf("vanity_%s", shardKey)
			
			if r.ready[tableName] {
				continue
			}

			// 创建分表
			createSQL := fmt.Sprintf(`
				CREATE TABLE IF NOT EXISTS %s (
					id BIGSERIAL PRIMARY KEY,
					address CHAR(40) NOT NULL,
					prefix CHAR(4) NOT NULL,
					prefix3 CHAR(3) NOT NULL,
					suffix CHAR(4) NOT NULL,
					encrypted_private_key BYTEA NOT NULL,
					created_at TIMESTAMP DEFAULT NOW()
				)`, tableName)
			
			if _, err := r.db.Exec(ctx, createSQL); err != nil {
				return fmt.Errorf("create table %s: %w", tableName, err)
			}

			// 创建索引
			indexSQL := fmt.Sprintf(`
				CREATE UNIQUE INDEX IF NOT EXISTS idx_%s_address ON %s(address);
				CREATE INDEX IF NOT EXISTS idx_%s_p4s4 ON %s(prefix, suffix);
				CREATE INDEX IF NOT EXISTS idx_%s_p3s4 ON %s(prefix3, suffix);
			`, shardKey, tableName, shardKey, tableName, shardKey, tableName)
			
			if _, err := r.db.Exec(ctx, indexSQL); err != nil {
				r.logger.Warn("创建索引失败", zap.String("table", tableName), zap.Error(err))
			}

			r.ready[tableName] = true
		}
	}

	r.logger.Info("✅ 256个分表已就绪")
	return nil
}

// GetTableStats 获取各分表统计
func (r *ShardRouter) GetTableStats(ctx context.Context) (map[string]int64, error) {
	stats := make(map[string]int64)
	
	for _, tableName := range r.GetAllTableNames() {
		var count int64
		query := fmt.Sprintf("SELECT COUNT(*) FROM %s", tableName)
		if err := r.db.QueryRow(ctx, query).Scan(&count); err != nil {
			continue
		}
		if count > 0 {
			stats[tableName] = count
		}
	}
	
	return stats, nil
}

// GetTotalCount 获取总数据量
func (r *ShardRouter) GetTotalCount(ctx context.Context) (int64, error) {
	var total int64
	for _, tableName := range r.GetAllTableNames() {
		var count int64
		query := fmt.Sprintf("SELECT COUNT(*) FROM %s", tableName)
		if err := r.db.QueryRow(ctx, query).Scan(&count); err != nil {
			continue
		}
		total += count
	}
	return total, nil
}

