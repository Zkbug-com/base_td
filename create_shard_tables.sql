-- =====================================================
-- 创建256个分表 (vanity_00 ~ vanity_ff)
-- 按 prefix 前2位分表，支持10亿级数据
-- =====================================================

-- 创建分表的函数
DO $$
DECLARE
    hex_chars TEXT[] := ARRAY['0','1','2','3','4','5','6','7','8','9','a','b','c','d','e','f'];
    c1 TEXT;
    c2 TEXT;
    table_name TEXT;
BEGIN
    FOREACH c1 IN ARRAY hex_chars LOOP
        FOREACH c2 IN ARRAY hex_chars LOOP
            table_name := 'vanity_' || c1 || c2;
            
            -- 创建分表
            EXECUTE format('
                CREATE TABLE IF NOT EXISTS %I (
                    id BIGSERIAL PRIMARY KEY,
                    address CHAR(40) NOT NULL,
                    prefix CHAR(4) NOT NULL,
                    prefix3 CHAR(3) NOT NULL,
                    suffix CHAR(4) NOT NULL,
                    encrypted_private_key BYTEA NOT NULL,
                    created_at TIMESTAMP DEFAULT NOW()
                )', table_name);
            
            -- 创建索引
            EXECUTE format('CREATE UNIQUE INDEX IF NOT EXISTS idx_%s_addr ON %I(address)', c1 || c2, table_name);
            EXECUTE format('CREATE INDEX IF NOT EXISTS idx_%s_p4s4 ON %I(prefix, suffix)', c1 || c2, table_name);
            EXECUTE format('CREATE INDEX IF NOT EXISTS idx_%s_p3s4 ON %I(prefix3, suffix)', c1 || c2, table_name);
            
        END LOOP;
    END LOOP;
    
    RAISE NOTICE '✅ 256个分表已创建完成';
END $$;

-- 创建迁移进度表
CREATE TABLE IF NOT EXISTS migration_progress (
    migrated_id BIGINT DEFAULT 0,
    updated_at TIMESTAMP DEFAULT NOW()
);
INSERT INTO migration_progress (migrated_id) SELECT 0 WHERE NOT EXISTS (SELECT 1 FROM migration_progress);

-- 查看分表统计
SELECT 
    'vanity_' || c1 || c2 as table_name,
    (SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'vanity_' || c1 || c2) as exists
FROM 
    unnest(ARRAY['0','1','2','3','4','5','6','7','8','9','a','b','c','d','e','f']) c1,
    unnest(ARRAY['0','1','2','3','4','5','6','7','8','9','a','b','c','d','e','f']) c2
LIMIT 10;

