#!/usr/bin/env python3
"""
后台数据迁移脚本: 从单表 vanity_addresses 迁移到 256个分表 vanity_xx
特点:
- 后台运行 (nohup)
- SQL批量迁移 (超快)
- 断点续传
- 低CPU占用
- 进度日志

用法:
    # 前台运行
    python3 migrate_to_shards.py

    # 后台运行
    nohup python3 migrate_to_shards.py > migrate.log 2>&1 &

    # 查看进度
    tail -f migrate.log
"""

import os
import sys
import time
import signal
import psycopg2

# 数据库配置
DB_HOST = os.getenv("POSTGRES_HOST", "localhost")
DB_PORT = os.getenv("POSTGRES_PORT", "5432")
DB_NAME = os.getenv("POSTGRES_DB", "poison_db")
DB_USER = os.getenv("POSTGRES_USER", "poison_db")
DB_PASS = os.getenv("POSTGRES_PASSWORD", "D07dZedJebQH1VXDPu8db8wM2aN523jy9v")

# 迁移配置
SLEEP_BETWEEN_TABLES = 0.5  # 每个分表迁移后休眠秒数，降低负载

running = True

def signal_handler(sig, frame):
    global running
    print("\n⚠️ 收到停止信号，等待当前批次完成...")
    running = False

signal.signal(signal.SIGINT, signal_handler)
signal.signal(signal.SIGTERM, signal_handler)

def get_connection():
    return psycopg2.connect(
        host=DB_HOST, port=DB_PORT, dbname=DB_NAME, user=DB_USER, password=DB_PASS
    )

def log(msg):
    """带时间戳的日志"""
    timestamp = time.strftime("%Y-%m-%d %H:%M:%S")
    print(f"[{timestamp}] {msg}", flush=True)

def create_shard_tables(conn):
    """创建256个分表"""
    cur = conn.cursor()
    hex_chars = "0123456789abcdef"

    for c1 in hex_chars:
        for c2 in hex_chars:
            table_name = f"vanity_{c1}{c2}"
            cur.execute(f"""
                CREATE TABLE IF NOT EXISTS {table_name} (
                    id BIGSERIAL PRIMARY KEY,
                    address CHAR(40) NOT NULL,
                    prefix CHAR(4) NOT NULL,
                    prefix3 CHAR(3) NOT NULL,
                    suffix CHAR(4) NOT NULL,
                    encrypted_private_key BYTEA NOT NULL,
                    created_at TIMESTAMP DEFAULT NOW()
                )
            """)
            cur.execute(f"CREATE UNIQUE INDEX IF NOT EXISTS idx_{c1}{c2}_addr ON {table_name}(address)")
            cur.execute(f"CREATE INDEX IF NOT EXISTS idx_{c1}{c2}_p4s4 ON {table_name}(prefix, suffix)")
            cur.execute(f"CREATE INDEX IF NOT EXISTS idx_{c1}{c2}_p3s4 ON {table_name}(prefix3, suffix)")

    conn.commit()
    log("✅ 256个分表已创建")

def get_migration_progress(conn) -> set:
    """获取已迁移的分表列表"""
    cur = conn.cursor()
    try:
        cur.execute("SELECT table_name FROM migration_shard_progress")
        return set(row[0] for row in cur.fetchall())
    except:
        conn.rollback()  # 回滚失败的事务
        cur.execute("""
            CREATE TABLE IF NOT EXISTS migration_shard_progress (
                table_name VARCHAR(20) PRIMARY KEY,
                migrated_count BIGINT,
                migrated_at TIMESTAMP DEFAULT NOW()
            )
        """)
        conn.commit()
        return set()

def save_shard_progress(conn, table_name: str, count: int):
    """保存分表迁移进度"""
    cur = conn.cursor()
    cur.execute("""
        INSERT INTO migration_shard_progress (table_name, migrated_count, migrated_at)
        VALUES (%s, %s, NOW())
        ON CONFLICT (table_name) DO UPDATE SET migrated_count = %s, migrated_at = NOW()
    """, (table_name, count, count))
    conn.commit()

def migrate_shard(conn, shard_key: str) -> int:
    """使用SQL批量迁移单个分表 (超快)"""
    table_name = f"vanity_{shard_key}"
    cur = conn.cursor()

    # 单条SQL批量迁移整个分表
    cur.execute(f"""
        INSERT INTO {table_name} (address, prefix, prefix3, suffix, encrypted_private_key, created_at)
        SELECT
            address,
            prefix,
            COALESCE(prefix3, LEFT(prefix, 3)),
            suffix,
            encrypted_private_key,
            COALESCE(created_at, NOW())
        FROM vanity_addresses
        WHERE LEFT(LOWER(prefix), 2) = %s
        ON CONFLICT (address) DO NOTHING
    """, (shard_key,))

    count = cur.rowcount
    conn.commit()
    return count

def main():
    global running

    log("🚀 后台迁移脚本启动")
    log(f"📡 数据库: {DB_HOST}:{DB_PORT}/{DB_NAME}")

    conn = get_connection()

    # 创建分表
    create_shard_tables(conn)

    # 获取总数
    cur = conn.cursor()
    cur.execute("SELECT COUNT(*) FROM vanity_addresses")
    total_count = cur.fetchone()[0]
    log(f"📊 源表总数据量: {total_count:,}")

    # 获取已迁移的分表
    done_shards = get_migration_progress(conn)
    log(f"📋 已迁移分表数: {len(done_shards)}/256")

    # 生成所有分表key
    hex_chars = "0123456789abcdef"
    all_shards = [c1 + c2 for c1 in hex_chars for c2 in hex_chars]
    pending_shards = [s for s in all_shards if f"vanity_{s}" not in done_shards]

    if not pending_shards:
        log("✅ 所有分表已迁移完成!")
        return

    log(f"⏳ 待迁移分表数: {len(pending_shards)}")

    # 开始迁移
    start_time = time.time()
    total_migrated = 0

    for i, shard_key in enumerate(pending_shards):
        if not running:
            log("⚠️ 迁移已暂停，下次启动将继续")
            break

        table_name = f"vanity_{shard_key}"

        try:
            count = migrate_shard(conn, shard_key)
            total_migrated += count
            save_shard_progress(conn, table_name, count)

            elapsed = time.time() - start_time
            progress = (i + 1) / len(pending_shards) * 100
            eta = elapsed / (i + 1) * (len(pending_shards) - i - 1) if i > 0 else 0

            log(f"✅ {table_name}: {count:,} 条 | 进度: {progress:.1f}% | ETA: {eta/60:.1f}分钟")

            # 休眠降低负载
            time.sleep(SLEEP_BETWEEN_TABLES)

        except Exception as e:
            log(f"❌ {table_name} 迁移失败: {e}")
            continue

    elapsed = time.time() - start_time
    log(f"🎉 迁移完成! 共迁移: {total_migrated:,} 条, 耗时: {elapsed/60:.1f}分钟")
    conn.close()

if __name__ == "__main__":
    main()

