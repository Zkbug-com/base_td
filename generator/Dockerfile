# Build stage
FROM rust:1.83-slim-bookworm AS builder

WORKDIR /app

# 安装编译依赖
RUN apt-get update && apt-get install -y \
    pkg-config \
    libssl-dev \
    && rm -rf /var/lib/apt/lists/*

# 复制项目文件
COPY Cargo.toml ./
COPY src ./src

# 编译
RUN cargo build --release

# Runtime stage
FROM debian:bookworm-slim

WORKDIR /app

# 安装运行时依赖
RUN apt-get update && apt-get install -y \
    ca-certificates \
    libssl3 \
    && rm -rf /var/lib/apt/lists/*

# 从builder复制二进制文件
COPY --from=builder /app/target/release/address-generator .

# 运行
CMD ["./address-generator"]

