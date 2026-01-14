#!/bin/bash
# Base 链地址投毒系统 - 一键部署脚本

set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

echo -e "${GREEN}========================================${NC}"
echo -e "${GREEN}  Base Chain Address Poisoning System  ${NC}"
echo -e "${GREEN}========================================${NC}"
echo ""

# 检查 Docker
if ! command -v docker &> /dev/null; then
    echo -e "${RED}[错误] Docker 未安装${NC}"
    echo "安装 Docker: curl -fsSL https://get.docker.com | sh"
    exit 1
fi

if ! command -v docker compose &> /dev/null; then
    echo -e "${RED}[错误] Docker Compose 未安装${NC}"
    exit 1
fi

echo -e "${GREEN}[✓] Docker 环境检查通过${NC}"

# 检查配置文件
if [ ! -f ".env" ]; then
    echo -e "${YELLOW}[!] .env 文件不存在，正在从模板创建...${NC}"
    cp .env.example .env
    echo -e "${RED}[重要] 请编辑 .env 文件，填写以下必要配置:${NC}"
    echo "  - POSTGRES_PASSWORD"
    echo "  - GENERATOR_MASTER_KEY"
    echo "  - POISONER_CONTRACT"
    echo "  - OWNER_PRIVATE_KEY"
    echo "  - WEB_PASSWORD"
    echo ""
    echo "编辑完成后重新运行此脚本"
    exit 1
fi

# 简单检查关键配置
source .env
if [ "$POSTGRES_PASSWORD" == "CHANGE_ME_TO_STRONG_PASSWORD" ]; then
    echo -e "${RED}[错误] 请修改 .env 中的 POSTGRES_PASSWORD${NC}"
    exit 1
fi

if [ "$OWNER_PRIVATE_KEY" == "YOUR_PRIVATE_KEY_HERE" ]; then
    echo -e "${RED}[错误] 请修改 .env 中的 OWNER_PRIVATE_KEY${NC}"
    exit 1
fi

echo -e "${GREEN}[✓] 配置文件检查通过${NC}"

# 选择操作
echo ""
echo "请选择操作:"
echo "  1) 启动全部服务 (监控+执行)"
echo "  2) 启动地址生成器"
echo "  3) 查看日志"
echo "  4) 停止所有服务"
echo "  5) 查看状态"
echo ""
read -p "输入选项 [1-5]: " choice

case $choice in
    1)
        echo -e "${GREEN}[*] 正在启动服务...${NC}"
        docker compose -f docker-compose.prod.yml up -d postgres
        echo "等待数据库启动..."
        sleep 10
        docker compose -f docker-compose.prod.yml up -d core
        echo ""
        echo -e "${GREEN}[✓] 服务已启动${NC}"
        echo -e "Web 控制台: http://localhost:${WEB_PORT:-8083}/${WEB_SECRET_PATH:-admin}"
        ;;
    2)
        echo -e "${YELLOW}[!] 地址生成器会持续运行，Ctrl+C 停止${NC}"
        echo -e "${GREEN}[*] 正在启动地址生成器...${NC}"
        docker compose -f docker-compose.prod.yml run generator
        ;;
    3)
        docker logs -f poison_core
        ;;
    4)
        echo -e "${YELLOW}[*] 正在停止服务...${NC}"
        docker compose -f docker-compose.prod.yml down
        echo -e "${GREEN}[✓] 服务已停止${NC}"
        ;;
    5)
        docker compose -f docker-compose.prod.yml ps
        ;;
    *)
        echo -e "${RED}无效选项${NC}"
        exit 1
        ;;
esac

