# 情谊有账（qingyi-book）

家庭人情往来账本。记录婚礼、满月、生日、乔迁、升学、白事等收支，按账本分开记，按对象看往来结余。
Go 单二进制 + 单个 JSON 数据文件，零第三方依赖，数据只存自己服务器。

## 功能

- 礼尚往来：收支、对象、事由、关系、金额、备注、日期
- 多账本：自家／爸妈家／个人分开记
- 对象汇总：按亲友看往来历史，自动算出该回多少
- 统计：收支构成、分类分布、月度趋势
- Excel 导入导出、JSON 备份
- 访问密码：进入需输密码，连续 5 次错误锁 15 分钟

移动端与桌面端自适应。

## 部署

改 `docker-compose.yml` 里两处：访问密码、数据目录。

```yaml
services:
  qingyi-book:
    build: .
    image: qingyi-book:latest
    container_name: qingyi-book
    restart: unless-stopped
    ports:
      - "6189:6189"
    environment:
      QY_PASSWORD: "你的密码"
      TZ: Asia/Shanghai
      QY_COOKIE_SECURE: "false"
    volumes:
      - /你的路径/qingyi-book-data:/data
      - /你的路径/qingyi-book-exports:/exports
```

启动：

```bash
docker compose up -d --build
```

打开 http://服务器IP:6189 ，输入密码进入。

**数据目录务必填宿主机绝对路径**，不要放在源码目录下——删源码不影响数据。

## 运行环境变量

| 变量 | 说明 |
|---|---|
| `QY_PASSWORD` | 访问密码，必填。留空则不启用门禁 |
| `QY_SESSION_SECRET` | 会话签名密钥。不设则由密码派生 |
| `QY_COOKIE_SECURE` | 默认 false。挂 HTTPS 反代后设 true |
| `PORT` | 监听端口，默认 6189 |
| `SOCKET` | Unix Socket 路径，设置后忽略 PORT |
| `GATEWAY_PREFIX` | 网关挂载前缀，如 `/app/qingyi-book` |
| `WWW` | 前端目录，默认 `www` |
| `DATA_FILE` | 数据文件路径，默认 `data.json` |
| `TRIM_DATA_SHARE_PATHS` | 允许导出到共享目录的路径，冒号或逗号分隔 |

## 裸机运行

需要 Go 1.25 或更高。

```bash
export QY_PASSWORD='你的访问密码'
go build -o qingyi-web .
./run.sh
```

`run.sh` 额外认 `QY_PORT` / `QY_WWW` / `QY_DATA` / `QY_SHARES` / `QY_LOG`。

## 数据与备份

```
data.json          主数据
data.json.bak.1    上一版（每次写入前轮转）
data.json.bak.2
data.json.bak.3
```

- 写入原子：先写 `.tmp` 再 `rename`，中断不会留下残缺文件。
- 每次写入前自动轮转，保留最近 3 个版本。
- 备份＝拷走数据目录。恢复＝放回去重启。

## 访问门禁

- 未登录时页面请求 302 到 `/login`，`/api/*` 返回 401。
- 登录页服务端渲染，不引外部资源，离线内网可用。
- 口令 PBKDF2-SHA256（21 万次迭代、16 字节随机盐），常量时间比对。
- 会话 Cookie：`HttpOnly` + `SameSite=Lax`，30 天有效，HMAC-SHA256 签名。
- 同一 IP 连续 5 次密码错误锁 15 分钟。

### 接口

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/login` | 登录页 |
| POST | `/api/login` | 提交密码，支持表单与 JSON |
| POST | `/api/logout` | 清除会话 |
| GET | `/api/session` | 登录态 |
| GET | `/api/health` | 健康检查，无需登录 |
| GET/PUT | `/api/state` | 读取／保存全量数据 |
| GET | `/api/export/xlsx` | 导出 Excel |
| POST | `/api/import/xlsx` | 导入 Excel |
| POST | `/api/export/to-nas` | 导出到共享目录 |
| GET | `/api/backups` | 备份清单 |
| GET | `/api/storage` | 数据文件与可写目录 |

除 `/login`、`/api/login`、`/api/session`、`/api/health` 外均需登录。

## HTTPS 反向代理

公网访问请走 HTTPS，并把 `QY_COOKIE_SECURE` 改成 `true`。

```
ledger.example.com {
    reverse_proxy 127.0.0.1:6189
}
```

Nginx：

```nginx
location / {
    proxy_pass http://127.0.0.1:6189;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

## 结构

```
main.go                 监听、路由、静态资源、账本读写与导出
auth.go                 口令派生、会话签名、登录限流、门禁中间件、登录页
handlers_auth.go        登录／登出／会话状态／健康检查
state.go                数据模型、校验与规范化、ID 生成、上限约束
store.go                落盘、原子写、备份轮转、共享目录探测
xlsx.go                 OOXML 读写（导出／导入）
shim.go                 工具垫片
state_test.go           规范化与解析单测
auth_ratelimit_test.go  限流／门禁／会话签名单测
www/index.html          前端（单文件，无外部资源）
data/data.sample.json   示例数据
```

## 数据约束

- 账本上限 200 本，记录上限 20 万条，单笔金额上限 1e9。
- 读取时规范化：孤儿记录丢弃、方向非 out 归 in、非法日期置空、金额保留两位。
- Excel 导入按列位读取，合计行中断导入，最多 5 万行。
