# Build stage
FROM golang:1.21-alpine AS builder

WORKDIR /app

# 安装依赖
RUN apk add --no-cache git ca-certificates

# 复制go.mod和go.sum
COPY go.mod go.sum ./
RUN go mod download

# 复制源代码
COPY . .

# 编译
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o poison-core .

# Runtime stage
FROM alpine:3.18

WORKDIR /app

# 安装ca证书
RUN apk --no-cache add ca-certificates tzdata

# 设置时区
ENV TZ=Asia/Shanghai

# 从builder复制二进制文件
COPY --from=builder /app/poison-core .

# 创建配置目录
RUN mkdir -p /app/config

# 暴露端口
EXPOSE 8080 9090

# 运行
CMD ["./poison-core"]

