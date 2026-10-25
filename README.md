# 情谊有账 — Web 版重写工程

原应用：`/vol3/@appcenter/qingyi-youzhang`（第三方 fpk，v1.0.1，Go 1.21.4 静态编译、已 strip）
本工程：按原版实测契约重写，纯 Web 服务，不打包 fpk。

## 目录

```
www/index.html   前端原件，与官方 fpk 内 www/index.html 字节一致（md5 0e3afbea...）
main.go          路由 / 配置 / 8 个 HTTP handler / 静态文件服务
state.go         数据模型、校验与规范化、ID 生成、上限约束
store.go         落盘、原子写、.bak 轮转（3 份）、storage 探测
xlsx.go          OOXML 手写读写（导出 / 导入 / 写回 NAS）
state_test.go    规范化与解析单测
run.sh           TCP 模式启动脚本
data/data.json   运行数据（当前是正式应用数据的只读副本）
exports/         导出到 NAS 的落地目录
```

## 构建与启动

```bash
export HOME=/vol3/@apphome/hermes-agent/data
go build -o qingyi-web .
./run.sh
```

访问 http://<NAS_IP>:6189

`run.sh` 只认 `QY_` 前缀变量（`QY_PORT` / `QY_WWW` / `QY_DATA` / `QY_SHARES` / `QY_LOG`），
再由它映射成程序读取的 `PORT` / `WWW` / `DATA_FILE` / `TRIM_DATA_SHARE_PATHS`。
这样可避免继承 Hermes 运行时同名的 `TRIM_DATA_SHARE_PATHS=/vol3/@appshare/hermes-agent`
——早期版本直接 `${VAR:-默认}` 就被污染，导致「导出到 NAS」写进 Hermes 自己的共享目录。

## 程序读取的环境变量

| 变量 | 说明 |
|---|---|
| PORT | TCP 端口，默认 6189 |
| SOCKET | Unix Socket 路径；设置后走网关模式，忽略 PORT |
| GATEWAY_PREFIX | 网关挂载前缀，如 /app/qingyi-youzhang |
| WWW | 前端目录 |
| DATA_FILE | data.json 路径 |
| TRIM_DATA_SHARE_PATHS | 允许「导出到 NAS」的目录，冒号分隔 |

## API 契约（与原版实测一致）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | /api/state | 读全量状态 |
| PUT | /api/state | 写全量状态，返回 `{"ok":true}` |
| GET | /api/export/xlsx | 下载 xlsx（8 列 + 合计行） |
| POST | /api/import/xlsx | multipart 字段 `file`，返回 rows/skipped/truncated |
| POST | /api/export/to-nas | 写入 share 目录，返回 name/path |
| GET | /api/backups | 备份清单 |
| GET | /api/storage | 数据文件与可写目录 |

## 与原版的行为对齐要点

- 校验为「规范化」而非拒绝：脏记录丢弃、direction 非 out 归 in、日期非法置空、
  金额 clamp 至 1e9 后 round2。
- 上限：200 本账、20 万条记录。
- 备份轮转 3 份。
- Excel 导入按列位读取（不看表头文字），合计行中断导入，最多 5 万行。

## Docker Compose 部署（文件已就绪，尚未构建）

| 文件 | 作用 |
|---|---|
| `Dockerfile` | 多阶段：`golang:1.21-alpine` 编译静态二进制 → `alpine:3.19` 运行 |
| `docker-compose.yml` | 端口映射、数据卷、健康检查、日志轮转 |
| `.dockerignore` | 排除 data/、exports/、本地二进制、日志 |
| `.env.example` | 端口与运行 UID 的默认值 |

```bash
cp .env.example .env          # 按需改 QY_PORT / QY_UID / QY_GID
sudo docker compose up -d --build
sudo docker compose logs -f
```

两个必须注意的点：

1. **目录属主**。容器非 root 运行（默认 1000:1000），宿主机 `./data`、`./exports`
   属主必须一致。本工程目录属主是 hermes-agent(987)，沿用同目录时在 `.env` 里设
   `QY_UID=987`、`QY_GID=987`，或 `sudo chown -R 1000:1000 data exports`。
   否则启动后写 data.json 会 permission denied。
2. **不要设 SOCKET**。容器里 `SOCKET` 为空才走 TCP 模式监听 `:6189`；
   设了会去监听 Unix socket，端口就映射不出来。

