#!/usr/bin/env python3
"""
高性能以太坊地址生成器 - 多线程版本
与 Rust 生成器完全兼容的加密格式

直接读取 .env 文件配置
"""

import os
import sys
import time
import secrets
import threading
from pathlib import Path
from queue import Queue
from typing import List, Tuple

# 加载 .env 文件
def load_dotenv(env_file: str = ".env"):
    """手动加载 .env 文件"""
    env_path = Path(env_file)
    if not env_path.exists():
        # 尝试脚本同目录
        env_path = Path(__file__).parent / ".env"
    if env_path.exists():
        with open(env_path, 'r') as f:
            for line in f:
                line = line.strip()
                if line and not line.startswith('#') and '=' in line:
                    key, _, value = line.partition('=')
                    key = key.strip()
                    value = value.strip()
                    # 移除引号
                    if value and value[0] in ('"', "'") and value[-1] == value[0]:
                        value = value[1:-1]
                    os.environ.setdefault(key, value)
        print(f"✅ 已加载配置: {env_path}")
    else:
        print(f"⚠️  未找到 .env 文件，使用环境变量")

load_dotenv()

# 第三方库
try:
    import psycopg2
    from psycopg2 import pool
    from eth_keys import keys
    from cryptography.hazmat.primitives.ciphers.aead import AESGCM
    from cryptography.hazmat.primitives.kdf.pbkdf2 import PBKDF2HMAC
    from cryptography.hazmat.primitives import hashes
except ImportError as e:
    print(f"❌ 缺少依赖: {e}")
    print("请安装: pip install psycopg2-binary eth-keys cryptography")
    sys.exit(1)

# ================== 从 .env 读取配置 ==================
DB_HOST = os.getenv("POSTGRES_HOST", "localhost")
DB_PORT = int(os.getenv("POSTGRES_PORT", "5432"))
DB_NAME = os.getenv("POSTGRES_DB", "poison_db")
DB_USER = os.getenv("POSTGRES_USER", "poison_db")
DB_PASS = os.getenv("POSTGRES_PASSWORD", "")
MASTER_KEY = os.getenv("GENERATOR_MASTER_KEY", "")
TABLE_NAME = os.getenv("TABLE_NAME", "vanity_addresses")

# 性能参数 (可通过环境变量覆盖)
THREADS = int(os.getenv("GEN_THREADS", str(os.cpu_count() or 4)))
BATCH_SIZE = int(os.getenv("GEN_BATCH_SIZE", "1000"))
QUEUE_SIZE = int(os.getenv("GEN_QUEUE_SIZE", "10"))
# ======================================================


class AddressGenerator:
    """地址生成器"""
    
    def __init__(self, master_key: bytes):
        self.derived_key = self._derive_key(master_key)
    
    def _derive_key(self, master_key: bytes) -> bytes:
        """派生加密密钥 (与Rust完全相同)"""
        kdf = PBKDF2HMAC(
            algorithm=hashes.SHA256(),
            length=32,
            salt=b"address-generator-salt",
            iterations=10000,
        )
        return kdf.derive(master_key)
    
    def _encrypt_private_key(self, private_key: bytes) -> bytes:
        """AES-256-GCM加密私钥 (与Rust完全相同)"""
        nonce = secrets.token_bytes(12)
        aesgcm = AESGCM(self.derived_key)
        ciphertext = aesgcm.encrypt(nonce, private_key, None)
        return nonce + ciphertext  # 12 + 48 = 60 bytes
    
    def generate_one(self) -> Tuple[str, str, str, str, bytes]:
        """生成单个地址, 返回 (address, prefix, prefix3, suffix, encrypted_pk)"""
        # 生成随机私钥 (32字节)
        private_key_bytes = secrets.token_bytes(32)
        
        # 从私钥派生公钥和地址
        pk = keys.PrivateKey(private_key_bytes)
        address = pk.public_key.to_checksum_address()[2:].lower()  # 去掉0x, 小写
        
        prefix = address[:4]
        prefix3 = address[:3]
        suffix = address[-4:]
        
        # 加密私钥
        encrypted_pk = self._encrypt_private_key(private_key_bytes)
        
        return address, prefix, prefix3, suffix, encrypted_pk
    
    def generate_batch(self, count: int) -> List[Tuple[str, str, str, str, bytes]]:
        """批量生成地址"""
        return [self.generate_one() for _ in range(count)]


class DatabaseWriter:
    """数据库写入器"""
    
    def __init__(self, conn_pool, table_name: str):
        self.pool = conn_pool
        self.table_name = table_name
        self.total_inserted = 0
        self.lock = threading.Lock()
    
    def insert_batch(self, addresses: List[Tuple[str, str, str, str, bytes]]) -> int:
        """批量插入地址, 返回插入数量"""
        conn = self.pool.getconn()
        try:
            cur = conn.cursor()
            
            # 使用 executemany 批量插入
            sql = f"""
                INSERT INTO {self.table_name} 
                (address, prefix, prefix3, suffix, encrypted_private_key)
                VALUES (%s, %s, %s, %s, %s)
                ON CONFLICT (address) DO NOTHING
            """
            cur.executemany(sql, addresses)
            inserted = cur.rowcount
            conn.commit()
            
            with self.lock:
                self.total_inserted += inserted
            
            return inserted
        except Exception as e:
            conn.rollback()
            print(f"❌ 数据库错误: {e}")
            return 0
        finally:
            self.pool.putconn(conn)


def worker(generator: AddressGenerator, batch_queue: Queue, batch_size: int, stop_event: threading.Event, thread_id: int):
    """生成线程: 生成地址并放入队列"""
    try:
        while not stop_event.is_set():
            batch = generator.generate_batch(batch_size)
            batch_queue.put(batch)
    except Exception as e:
        print(f"❌ 生成线程 {thread_id} 出错: {e}")
        import traceback
        traceback.print_exc()


def writer_worker(db_writer: DatabaseWriter, batch_queue: Queue, stop_event: threading.Event):
    """写入线程: 从队列取出并写入数据库"""
    while not stop_event.is_set() or not batch_queue.empty():
        try:
            batch = batch_queue.get(timeout=1)
            db_writer.insert_batch(batch)
            batch_queue.task_done()
        except:
            pass


def main():
    # 验证配置
    if not MASTER_KEY:
        print("❌ 请设置 MASTER_KEY 环境变量")
        sys.exit(1)
    if not DB_PASS:
        print("❌ 请设置 DB_PASS 环境变量")
        sys.exit(1)
    if len(MASTER_KEY) < 32:
        print("❌ MASTER_KEY 长度必须 >= 32 字节")
        sys.exit(1)

    print("=" * 60)
    print("🚀 以太坊地址生成器 (Python多线程版)")
    print("=" * 60)
    print(f"📊 线程数: {THREADS}")
    print(f"📦 批次大小: {BATCH_SIZE}")
    print(f"🗄️  目标表: {TABLE_NAME}")
    print(f"🔌 数据库: {DB_HOST}:{DB_PORT}/{DB_NAME}")
    print("=" * 60)

    # 创建数据库连接池
    print("🔗 连接数据库...")
    try:
        conn_pool = psycopg2.pool.ThreadedConnectionPool(
            minconn=2,
            maxconn=THREADS + 2,
            host=DB_HOST,
            port=DB_PORT,
            dbname=DB_NAME,
            user=DB_USER,
            password=DB_PASS
        )
    except Exception as e:
        print(f"❌ 数据库连接失败: {e}")
        sys.exit(1)
    print("✅ 数据库连接成功")

    # 初始化组件
    generator = AddressGenerator(MASTER_KEY.encode('utf-8'))

    # 测试生成一个地址
    print("🧪 测试生成地址...")
    try:
        test_addr = generator.generate_one()
        print(f"✅ 测试成功: 0x{test_addr[0][:8]}... (加密长度: {len(test_addr[4])})")
    except Exception as e:
        print(f"❌ 测试失败: {e}")
        import traceback
        traceback.print_exc()
        sys.exit(1)

    db_writer = DatabaseWriter(conn_pool, TABLE_NAME)
    batch_queue = Queue(maxsize=QUEUE_SIZE)
    stop_event = threading.Event()

    # 启动生成线程
    print(f"🏃 启动 {THREADS} 个生成线程...")
    gen_threads = []
    for i in range(THREADS):
        t = threading.Thread(target=worker, args=(generator, batch_queue, BATCH_SIZE, stop_event, i))
        t.daemon = True
        t.start()
        gen_threads.append(t)

    # 启动写入线程 (根据生成线程数调整)
    num_writers = max(4, THREADS // 10)
    print(f"🏃 启动 {num_writers} 个写入线程...")
    write_threads = []
    for _ in range(num_writers):
        t = threading.Thread(target=writer_worker, args=(db_writer, batch_queue, stop_event))
        t.daemon = True
        t.start()
        write_threads.append(t)

    print("✅ 开始生成地址...")
    print("-" * 60)

    start_time = time.time()
    last_count = 0
    last_time = start_time

    try:
        while True:
            time.sleep(5)

            elapsed = time.time() - start_time
            current = db_writer.total_inserted

            # 计算速率
            interval = time.time() - last_time
            rate = (current - last_count) / interval if interval > 0 else 0
            avg_rate = current / elapsed if elapsed > 0 else 0

            print(f"📈 已插入: {current:,} | 速率: {rate:.0f}/s | 平均: {avg_rate:.0f}/s | 队列: {batch_queue.qsize()}")

            last_count = current
            last_time = time.time()

    except KeyboardInterrupt:
        print("\n⏹️  正在停止...")
        stop_event.set()

        # 等待队列清空
        batch_queue.join()

        elapsed = time.time() - start_time
        total = db_writer.total_inserted
        print("=" * 60)
        print(f"✅ 生成完成!")
        print(f"📊 总计插入: {total:,} 条")
        print(f"⏱️  总耗时: {elapsed:.1f} 秒")
        print(f"📈 平均速率: {total/elapsed:.0f} 条/秒")
        print("=" * 60)

        conn_pool.closeall()


if __name__ == "__main__":
    main()

