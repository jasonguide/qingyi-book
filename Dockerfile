# 多阶段构建：Go 编译静态二进制 -> alpine 运行
FROM golang:1.25-alpine AS build

WORKDIR /src
COPY go.mod ./
RUN go mod download

COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w" \
        -o /out/qingyi-web .

# ---------------------------------------------------------------- 运行阶段

FROM alpine:3.21

# wget 供 HEALTHCHECK 使用，tzdata 供时区设置
RUN apk add --no-cache ca-certificates tzdata wget

WORKDIR /app

COPY --from=build /out/qingyi-web /app/qingyi-web
COPY www /app/www

RUN mkdir -p /data /exports

ENV PORT=6189 \
    WWW=/app/www \
    DATA_FILE=/data/data.json \
    TRIM_DATA_SHARE_PATHS=/exports \
    TZ=Asia/Shanghai

EXPOSE 6189

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:6189/api/health > /dev/null || exit 1

ENTRYPOINT ["/app/qingyi-web"]
