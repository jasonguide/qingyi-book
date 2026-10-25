# syntax=docker/dockerfile:1
# 「情谊有账」Web 版 —— 多阶段构建
# 第一阶段编译 Go 静态二进制，第二阶段只带二进制 + 前端静态资源。
# 注意：本项目只用标准库，无第三方依赖，go mod download 是空操作但保留以防后续加依赖。

FROM golang:1.21-alpine AS build

WORKDIR /src

# 先复制依赖清单，利用镜像层缓存
# 注意：本工程当前无第三方依赖，没有 go.sum 文件。
# 一旦引入第三方包并生成 go.sum，把下面这行改成 COPY go.mod go.sum ./
COPY go.mod ./
RUN go mod download

# 编译（只复制 .go 源码，避免把数据/二进制带进构建层）
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w" \
        -o /out/qingyi-web .

# ---------------------------------------------------------------- 运行阶段

FROM alpine:3.19

# wget 供 HEALTHCHECK 使用，tzdata 保证导出文件名里的日期是东八区
RUN apk add --no-cache ca-certificates tzdata wget \
    && addgroup -g 1000 -S app \
    && adduser -u 1000 -S -G app -h /app app

WORKDIR /app

COPY --from=build /out/qingyi-web /app/qingyi-web
COPY www /app/www

# /data 与 /exports 是默认挂载点，先建好并给运行用户权限
# （bind mount 会覆盖这两层，宿主机目录属主需与运行 UID 一致，见 docker-compose.yml 注释）
RUN mkdir -p /data /exports && chown -R app:app /app /data /exports

# 程序读取的环境变量。SOCKET / GATEWAY_PREFIX 必须留空，才会走 TCP 模式。
ENV PORT=6189 \
    WWW=/app/www \
    DATA_FILE=/data/data.json \
    TRIM_DATA_SHARE_PATHS=/exports \
    TZ=Asia/Shanghai

EXPOSE 6189

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:6189/api/storage > /dev/null || exit 1

USER app

ENTRYPOINT ["/app/qingyi-web"]
