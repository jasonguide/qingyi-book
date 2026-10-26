# 情谊有账（qingyi-book）

家庭人情往来账本。记录婚礼、满月、生日、乔迁、升学、白事等收支，按账本分开记，按对象看往来结余。
Go 单二进制 + 单个 JSON 数据文件，无第三方依赖，数据只存自己服务器。

## 功能

- **礼尚往来**：收支方向、对象、事由、关系、金额、备注、日期
- **多账本**：按自家／爸妈家／个人分开记，互不干扰
- **对象汇总**：按亲友查看往来历史，自动算出「该回多少」
- **统计**：收支构成、分类分布、月度趋势
- **Excel**：导出 .xlsx，也可导入合并
- **备份**：导出／导入 JSON，方便换设备
- **访问密码**：进入需输密码，连续 5 次错误锁 15 分钟

移动端与桌面端自适应，同一套页面。

## 结构

```
main.go                 监听、路由、静态资源、账本读写与导出入口
auth.go                 口令派生、会话签名、登录限流、门禁中间件、登录页
handlers_auth.go        登录／登出／会话状态／健康检查
state.go                数据模型、校验与规范化、ID 生成、上限约束
store.go                落盘、原子写、备份轮转（保留 3 份）、共享目录探测
xlsx.go                 OOXML 手写读写（导出／导入）
shim.go                 极小工具垫片
state_test.go           规范化与解析单测
auth_ratelimit_test.go  限流／门禁／会话签名单测
www/index.html          前端（单文件，含样式与脚本，无外部资源）
data/data.sample.json   示例数据（真实数据不入库）
exports/                「导出到 NAS」落地目录
run.sh                  裸机启动脚本
```

## 运行

需要 Go 1.25 或更高。

```bash
export QY_PASSWORD='你的访问密码'
go build -o qingyi-web .
./run.sh
```

打开 http://<服务器IP>:6189 ，输入密码进入。

开发调试可直接运行：

```bash
QY_PASSWORD=dev PORT=6189 DATA_FILE=./data/data.json go run .
```

## Docker 部署

```bash
cp .env.example .env        # 改掉 QY_PASSWORD
docker compose up -d --build
docker compose logs -f
```

首页 http://<服务器IP>:6189 。

三个常见问题：

1. **QY_PASSWORD 必须设置**。compose 里用 `${QY_PASSWORD:?...}` 强制校验，.env 未填会直接拒绝启动。
2. **目录属主**。容器以非 root 运行（默认 1000:1000），宿主机 `./data`、`./exports` 需可写；属主不同就在 .env 里设 `QY_UID` / `QY_GID`，或 `chown -R 1000:1000 data exports`。
3. **不要设 SOCKET**。为空才走 TCP 监听 6189，设了会改去监听 Unix socket，端口无法映射。

## 环境变量

| 变量 | 说明 |
|---|---|
| `QY_PASSWORD` | 访问密码，必填。留空则不启用门禁 |
| `QY_SESSION_SECRET` | 会话签名密钥。不设则由密码派生，改密码即让所有已登录设备下线 |
| `QY_COOKIE_SECURE` | 默认 false。挂 HTTPS 反代后设为 true |
| `PORT` | 监听端口，默认 6189 |
| `SOCKET` | Unix Socket 路径。设置后忽略 PORT，走 socket 模式 |
| `GATEWAY_PREFIX` | 网关挂载前缀，如 `/app/qingyi-book`，分发前剥掉 |
| `WWW` | 前端目录，默认 `www` |
| `DATA_FILE` | 数据文件路径，默认 `data.json` |
| `TRIM_DATA_SHARE_PATHS` | 允许「导出到 NAS」的目录，冒号或逗号分隔 |

`run.sh` 额外认 `QY_PORT` / `QY_WWW` / `QY_DATA` / `QY_SHARES` / `QY_LOG`，再映射到上面的变量。

## 数据与备份

```
data/data.json          主数据
data/data.json.bak.1    上一版（每次写入前轮转）
data/data.json.bak.2
data/data.json.bak.3
```

- 写入是原子的：先写 `.tmp` 再 `rename`，中断不会留下残缺文件。
- 每次写入前自动轮转，保留最近 3 个版本。
- 备份＝拷走整个 `data/` 目录。恢复＝放回去重启。
- 升级或重建容器不影响数据，数据在挂载目录里。

## 访问门禁

- 未登录时页面请求 302 到 `/login`，`/api/*` 返回 401。
- 登录页由服务端渲染，不引外部资源，离线内网可用。
- 口令用 PBKDF2-SHA256（21 万次迭代、16 字节随机盐）校验，比对用常量时间。
- 会话 Cookie：`HttpOnly` + `SameSite=Lax`，30 天有效，HMAC-SHA256 签名。
- 同一 IP 连续 5 次密码错误锁 15 分钟；IP 优先取 `X-Forwarded-For` 首段，反代下仍能识别真实来源。
- `next` 参数只接受站内路径，防开放重定向。

### 门禁相关端点

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/login` | 登录页，已登录则跳回首页 |
| POST | `/api/login` | 提交密码，支持表单与 JSON。成功下发 Cookie |
| POST | `/api/logout` | 清除会话 |
| GET | `/api/session` | `{"authEnabled":bool,"loggedIn":bool}` |
| GET | `/api/health` | 健康检查，无需登录 |

其余接口：`GET/PUT /api/state`、`GET /api/export/xlsx`、`POST /api/import/xlsx`、`POST /api/export/to-nas`、`GET /api/backups`、`GET /api/storage`，均需登录。

## HTTPS 反向代理

公网访问请只走 HTTPS，并设 `QY_COOKIE_SECURE=true`。

Caddy：

```
ledger.example.com {
    reverse_proxy 127.0.0.1:6189
}
```

Nginx：

```nginx
server {
    listen 443 ssl http2;
    server_name ledger.example.com;
    ssl_certificate     /etc/letsencrypt/live/ledger.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/ledger.example.com/privkey.pem;
    location / {
        proxy_pass http://127.0.0.1:6189;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

## 数据约束

- 账本上限 200 本，记录上限 20 万条。
- 单笔金额上限 1e9，超出会被截断。
- 读取时做规范化：孤儿记录丢弃、方向非 out 归 in、非法日期置空、金额四舍五入到两位。
- Excel 导入按列位读取，合计行中断导入，最多 5 万行。
