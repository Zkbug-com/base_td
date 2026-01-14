-- =====================================================
-- 快速迁移脚本 (停机模式，10-30分钟完成1.58亿条)
-- =====================================================

-- 性能优化设置
SET work_mem = '1GB';
SET maintenance_work_mem = '2GB';
SET max_parallel_workers_per_gather = 4;

-- 禁用触发器和约束检查加速
SET session_replication_role = replica;

\timing on
\echo '🚀 开始快速迁移...'

-- 使用存储过程批量迁移
DO $$
DECLARE
    hex_chars TEXT[] := ARRAY['0','1','2','3','4','5','6','7','8','9','a','b','c','d','e','f'];
    c1 TEXT;
    c2 TEXT;
    table_name TEXT;
    prefix_val TEXT;
    cnt BIGINT;
    total BIGINT := 0;
    start_ts TIMESTAMP := clock_timestamp();
BEGIN
    RAISE NOTICE '开始时间: %', start_ts;
    
    FOREACH c1 IN ARRAY hex_chars LOOP
        FOREACH c2 IN ARRAY hex_chars LOOP
            table_name := 'vanity_' || c1 || c2;
            prefix_val := c1 || c2;
            
            -- 直接INSERT...SELECT，超快
            EXECUTE format('
                INSERT INTO %I (address, prefix, prefix3, suffix, encrypted_private_key, created_at)
                SELECT address, prefix, 
                       COALESCE(prefix3, LEFT(prefix, 3)), 
                       suffix, encrypted_private_key, 
                       COALESCE(created_at, NOW())
                FROM vanity_addresses
                WHERE LEFT(LOWER(prefix), 2) = %L
                ON CONFLICT (address) DO NOTHING
            ', table_name, prefix_val);
            
            GET DIAGNOSTICS cnt = ROW_COUNT;
            total := total + cnt;
            
            IF cnt > 0 THEN
                RAISE NOTICE '✅ % : % 条 | 累计: % | 耗时: %', 
                    table_name, cnt, total, clock_timestamp() - start_ts;
            END IF;
        END LOOP;
        
        -- 每完成一行(16个表)显示进度
        RAISE NOTICE '--- 完成 %x 系列 (累计: %) ---', c1, total;
    END LOOP;
    
    RAISE NOTICE '';
    RAISE NOTICE '🎉🎉🎉 迁移完成! 🎉🎉🎉';
    RAISE NOTICE '总计: % 条', total;
    RAISE NOTICE '总耗时: %', clock_timestamp() - start_ts;
END $$;

-- 恢复正常模式
SET session_replication_role = DEFAULT;

-- 验证迁移结果
\echo ''
\echo '📊 验证迁移结果...'

SELECT 
    'vanity_addresses (原表)' as table_name,
    COUNT(*) as count
FROM vanity_addresses
UNION ALL
SELECT 
    '分表总计' as table_name,
    (
        SELECT SUM(cnt) FROM (
            SELECT COUNT(*) as cnt FROM vanity_00
            UNION ALL SELECT COUNT(*) FROM vanity_01
            UNION ALL SELECT COUNT(*) FROM vanity_02
            UNION ALL SELECT COUNT(*) FROM vanity_03
            UNION ALL SELECT COUNT(*) FROM vanity_0a
            UNION ALL SELECT COUNT(*) FROM vanity_0f
            UNION ALL SELECT COUNT(*) FROM vanity_ff
            -- 只查几个代表性的
        ) t
    ) as count;

\echo ''
\echo '✅ 迁移完成! 现在可以:'
\echo '1. 设置 USE_SHARDING=true'
\echo '2. 重启投毒程序'
\echo '3. (可选) 稍后删除旧表: DROP TABLE vanity_addresses;'

