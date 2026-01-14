#!/usr/bin/env python3
"""
查询CSV中地址的ETH和USDC余额 (Base链)
用法: python3 check_balances.py <csv文件路径>
"""

import sys
import csv
import time
from web3 import Web3
from concurrent.futures import ThreadPoolExecutor, as_completed

# ============ 配置 (Base链) ============
# Base RPC节点
RPC_URLS = [
    "https://base.drpc.org",
    "https://base-rpc.publicnode.com",
    "https://1rpc.io/base",
    "https://base.meowrpc.com",
]

# USDC合约地址 (Base链, 6位小数!)
USDC_CONTRACT = "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"

# ERC20 ABI (只需要balanceOf)
ERC20_ABI = [{"constant":True,"inputs":[{"name":"_owner","type":"address"}],"name":"balanceOf","outputs":[{"name":"balance","type":"uint256"}],"type":"function"}]

# 并发数
CONCURRENCY = 10
# ==============================


def get_web3():
    """获取Web3连接"""
    for url in RPC_URLS:
        try:
            w3 = Web3(Web3.HTTPProvider(url, request_kwargs={'timeout': 10}))
            if w3.is_connected():
                return w3
        except:
            continue
    raise Exception("无法连接到任何RPC节点")


def check_balance(w3, usdc_contract, address: str) -> dict:
    """查询单个地址的余额 (Base链)"""
    try:
        # 查询ETH余额
        eth_wei = w3.eth.get_balance(Web3.to_checksum_address(address))
        eth = float(w3.from_wei(eth_wei, 'ether'))

        # 查询USDC余额 (Base链USDC是6位小数!)
        usdc_wei = usdc_contract.functions.balanceOf(Web3.to_checksum_address(address)).call()
        usdc = float(usdc_wei) / 1e6  # USDC有6位小数

        return {
            'address': address,
            'eth': eth,
            'usdc': usdc,
            'error': None
        }
    except Exception as e:
        return {
            'address': address,
            'eth': 0,
            'usdc': 0,
            'error': str(e)
        }


def main():
    if len(sys.argv) < 2:
        print("用法: python3 check_balances.py <csv文件路径>")
        print("示例: python3 check_balances.py /root/bsc-test/exploit/2024-12-22/addresses_20241222_170000.csv")
        sys.exit(1)
    
    csv_path = sys.argv[1]
    
    # 读取CSV
    print(f"📂 读取文件: {csv_path}")
    addresses = []
    private_keys = {}
    
    with open(csv_path, 'r') as f:
        reader = csv.DictReader(f)
        for row in reader:
            addr = row['address'].strip()
            addresses.append(addr)
            private_keys[addr] = row.get('private_key', '')
    
    print(f"📋 共 {len(addresses)} 个地址")

    # 连接Base链
    print("🔗 连接Base网络...")
    w3 = get_web3()
    usdc_contract = w3.eth.contract(address=Web3.to_checksum_address(USDC_CONTRACT), abi=ERC20_ABI)
    print("✅ 连接成功")

    # 查询余额
    print(f"🔍 开始查询余额 (并发: {CONCURRENCY})...")
    results = []
    has_balance = []

    with ThreadPoolExecutor(max_workers=CONCURRENCY) as executor:
        futures = {executor.submit(check_balance, w3, usdc_contract, addr): addr for addr in addresses}

        for i, future in enumerate(as_completed(futures), 1):
            result = future.result()
            results.append(result)

            if result['eth'] > 0 or result['usdc'] > 0:
                has_balance.append(result)
                print(f"💰 [{i}/{len(addresses)}] {result['address']}: ETH={result['eth']:.8f}, USDC={result['usdc']:.6f}")
            elif i % 100 == 0:
                print(f"⏳ 已查询 {i}/{len(addresses)}...")
    
    # 输出有余额的地址
    print(f"\n{'='*60}")
    print(f"📊 查询完成! 共 {len(addresses)} 个地址")
    print(f"💰 有余额的地址: {len(has_balance)} 个")

    # 筛选 ETH > 0.0001 或 USDC > 1 的地址
    valuable = [r for r in has_balance if r['eth'] > 0.0001 or r['usdc'] > 1]

    if valuable:
        # 保存到ok.txt
        import os
        output_dir = os.path.dirname(csv_path)
        ok_path = os.path.join(output_dir, 'ok.txt')

        with open(ok_path, 'w') as f:
            f.write("# 有价值的地址 (ETH > 0.0001 或 USDC > 1) - Base链\n")
            f.write("# 格式: 地址,私钥,ETH余额,USDC余额\n")
            f.write("="*80 + "\n")
            for r in valuable:
                pk = private_keys.get(r['address'], '')
                f.write(f"{r['address']},{pk},{r['eth']:.8f},{r['usdc']:.6f}\n")

        print(f"\n🎉 发现 {len(valuable)} 个有价值地址!")
        print(f"📁 已保存到: {ok_path}")
        print(f"\n{'='*60}")
        print("💎 有价值地址详情 (ETH > 0.0001 或 USDC > 1):")
        total_eth = 0
        total_usdc = 0
        for r in valuable:
            print(f"  {r['address']}: ETH={r['eth']:.8f}, USDC={r['usdc']:.6f}")
            total_eth += r['eth']
            total_usdc += r['usdc']
        print(f"\n📈 总计: ETH={total_eth:.8f}, USDC={total_usdc:.6f}")
    else:
        print("\n😔 没有发现有价值的地址 (ETH > 0.0001 或 USDC > 1)")


if __name__ == "__main__":
    main()

