# 3e 日程服务

一个**自托管的多人共享日历服务端**：Go 单二进制 + 纯 Go SQLite（无 CGO，交叉编译直接出静态文件）。

面向的场景是「几个人（同学 / 同事 / 家人）共用一本日程，但**提醒各管各的**」——
日程本身是共享的，提醒和强提醒是**按人**的：同一场会议，你可以提前 30 分钟提醒、
我可以不提醒、他还可以开「强提醒」。

> **本仓库只包含服务端。** 桌面端 / 移动端 / 手表端的客户端不在这里。

---

## 特性

**日程与日历**

- 多日历，每个日历可设颜色、可见性；支持重复日程（完整 RFC 5545 `RRULE`，含 `RDATE` / `EXDATE`）
- 单次修改（override）：只改某一次重复，不动整个系列
- 待办项（`is_task` + `done_at`）、稍后提醒（snooze）
- **字段级冲突中心**：两边同时改了**同一个字段**才产生冲突记录（`conflicts` 表），
  并支持按 `keep_mine` / `keep_theirs` 等方式解决。时间重叠的判断留给客户端

**权限：四档角色**

`owner ⊃ admin ⊃ editor ⊃ viewer`，判定收在一处（`acl.go`）。

- **`admin ≠ owner`**：admin 能管成员，但不能删日历、不能转移归属
- **加成员不会改动日历本身的共享状态** —— 「给别人权限」和「把日历变共享」是两件独立的事
- 权限**一次性批量提交**：先全量校验再开事务，全有或全无，不会留下改了一半的中间状态
- 角色解析的默认值一律往**收紧**方向偏（缺字段 = `viewer`，不是 `editor`）

**提醒是按人的（本项目的核心区别）**

- `reminders(event_id, user_id, minutes_before, channel)`：每条日程、每个人各自一组提前量
- **空列表是一个有意义的状态**，表示"这条不提醒我"。为此另立 `reminder_prefs` 区分
  「明确要 0 条」和「从没表过态」——否则"把提醒全部取消"这件事会被服务端的默认值悄悄撤销
- JSON 下发时 `reminders` **不带 `omitempty`**：空数组必须照样出现，
  否则客户端读到 `undefined`，分不清"服务端说没有"和"这个字段没下发"

**强提醒（per-event / per-user）**

把「这条日程要强提醒」这个**意图**存到服务端，让所有端看到同一个设置。
服务端**只存意图、不管实现**：安卓写进系统日历交给系统去提醒、桌面弹置顶提示栏、
手表响铃震动 —— 各端能力不同，这个判断不属于服务端。

**同步：增量 + 字段级合并**

- 客户端带游标（`since`）拉增量，服务端用 `sync_log` 记变更
- **字段级合并**：`field_seqs` 记录每个字段各自的版本。
  "A 改了标题、B 同时改了时间"能自动合并；只有**同一个字段**被两边都改过才产生冲突记录
- 客户端按 id upsert，重发天然幂等
- 支持离线队列（`POST /api/sync/push`）：断网时的改动攒着，恢复后带幂等键补交

**其他**

- 天气（met.no 为主，Open-Meteo 兜底；遵守 `Expires` 头 + 两级缓存，不轮询）
- 课程表节次配置（`course.go`）
- 机器接口（`/api/ext/*`）：给 AI / 脚本用的精简 API，配 OpenAPI 文档，用 `fsk_` 前缀的 API Key 鉴权
- 应用内更新分发通道（`/api/updates`：按平台发布 / 比对版本 / 下载，带 sha256）
- 单文件 SQLite 存储，启动时自动迁移（`CREATE TABLE IF NOT EXISTS` + `ensureColumn` 做加法式演进）

---

## 快速开始

需要 **Go 1.25+**。

```bash
git clone https://github.com/3ekbbbs/3e-schedule-server.git
cd 3e-schedule-server

go build -o 3e-schedule-server .

# 先起明文 HTTP 验证（-addr "" 关掉 HTTPS）
./3e-schedule-server -data ./data -addr "" -http 127.0.0.1:8080
```

首次启动会自动生成一个**邀请码**并打印在日志里：

```
invite code: a1b2c3d4e5      ← 抄下来，注册第二个及以后的账号要它
```

**第一个注册的账号自动成为管理员**，之后注册必须带邀请码。用 curl 建号：

```bash
# 第一个账号（免邀请码，自动成为 admin）
curl -X POST http://127.0.0.1:8080/api/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"name":"alice","pass":"your-password"}'

# 之后的账号（要邀请码）
curl -X POST http://127.0.0.1:8080/api/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"name":"bob","pass":"another-password","invite_code":"a1b2c3d4e5"}'
```

返回里的 `token` 就是会话令牌，后续请求带 `Authorization: Bearer <token>`。

---

## 命令行参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-addr` | `:8443` | HTTPS 监听地址。传空字符串关闭 HTTPS |
| `-http` | 空 | 明文 HTTP 监听地址，例如 `:8080`（建议只在反代后面用） |
| `-data` | `./data` | 数据目录（SQLite 文件、天气缓存都放这里） |
| `-certs` | `./certs` | 证书目录，里面放 `server.crt` / `server.key`。**两个都在才启用 HTTPS**，缺了就跳过 |
| `-invite` | 空 | 邀请码。不传则复用已存的，没有就自动生成 |
| `-place` / `-lat` / `-lon` | 北京市 / 39.9042 / 116.4074 | 天气地点。**默认值只是占位，部署时请改成你自己的位置** |
| `-ua` | `3e-schedule-server/<ver> (…; contact: you@example.com)` | 发给 met.no 的 User-Agent。**请把 contact 换成你的邮箱** —— met.no 不接受匿名调用 |
| `-pprof` | `false` | 打开 `/debug/pprof`（仅诊断用，别在生产长期开着） |

---

## 启用 HTTPS

```bash
mkdir -p certs
# 仓库里带了一个小工具，用标准库生成自签证书（不需要 openssl）
go run ./cmd/mkcert -out ./certs -names localhost,calendar.example.com -ips 203.0.113.10

./3e-schedule-server -data ./data -certs ./certs -addr :8443
```

`cmd/mkcert` 会同时生成 CA 与服务端证书（默认 CA 20 年 / 服务端 5 年）。
**把 CA 证书装到客户端**（手机/桌面）才能免警告；
自签证书的 SAN 里必须包含客户端实际使用的主机名或 IP，否则会校验失败。
之后只换服务端证书时用 `-renew`，别重建 CA —— 重建 CA 会让所有已装 CA 的客户端全部断连。

> 说明：`-certs` 目录下的证书是**启动时**读取的。
> 换证书后必须重启进程才会生效。

---

## 部署建议

- **别把 8443 直接暴露成明文**：本服务只讲 HTTPS，明文请走 `-http` 并用反代终结 TLS
- **备份 `data/` 整个目录**，不是只拷 `schedule.db`。SQLite 处于 WAL 模式，
  `schedule.db-wal` 里可能有还没 checkpoint 的数据（表结构变更尤其容易只出现在 WAL 里）
- **`certs/server.key` 与 `data/` 绝对不能进版本库**（`.gitignore` 已经挡了）
- 需要常驻就用系统的服务管理器（systemd / 计划任务 / NSSM），别指望手动双击的窗口一直活着

---

## 机器接口（给 AI / 脚本用）

除了给客户端用的 `/api/*`，还有一套面向自动化的 `/api/ext/*`：时间字段既接受
epoch 秒也接受 `"2026-09-15 14:30"` 这种字符串，`/api/ext/agenda` 还会直接返回
**排好版的纯文本**，方便喂给不会解析 JSON 的调用方。

```bash
# 先在客户端里生成一个 API Key（形如 fsk_...），然后：
curl -H "Authorization: Bearer fsk_xxxxxxxx" https://YOUR-HOST:8443/api/ext/whoami
curl -H "Authorization: Bearer fsk_xxxxxxxx" https://YOUR-HOST:8443/api/ext/agenda?date=today
```

完整说明在服务端自带的 `GET /api/ext/guide`（Markdown），机器可读的定义在
`GET /api/ext/openapi.json`。

---

## 安全说明

- 密码用 **PBKDF2-HMAC-SHA256** 加盐哈希存储（标准库实现，无额外依赖）
- 会话令牌存 `tokens` 表；改密码会**作废其它设备的会话**，保留当前设备
- API Key 只存哈希（`key_hash`），明文只在创建时返回一次
- 邀请码防止陌生人注册；第一个账号是管理员
- 所有查询都走参数化 SQL

**发现安全问题请通过 GitHub 的私密漏洞报告（Security → Report a vulnerability）反馈，不要开公开 issue。**

---

## 已知限制

- **天气地点是全局唯一的**（一个服务实例一个地点），设计上是给"一个小圈子共用"用的
- **没有 Web 管理后台**：账号、日历、共享都要通过 API 操作
- **没有全文搜索 / 导入导出（iCal）**
- 时间统一按 UTC 存储，展示时由客户端按时区渲染；服务端不处理"跨时区会议"这类复杂语义

---

## 目录结构

```
main.go        启动、路由表、配置
db.go          SQLite 打开与迁移（schema 常量 + ensureColumn）
models.go      对外的数据结构（JSON tag 是客户端契约 —— 改之前先想清楚）
acl.go         角色模型、日历成员、按人提醒 / 强提醒
auth.go        注册、登录、改密码、邀请码
cal.go         日历 CRUD
event.go       日程 CRUD、override、稍后提醒
recur.go       RRULE 展开、实例计算
sync.go        增量同步、字段级合并、冲突
updates.go     应用内更新分发通道
extapi.go      机器接口（/api/ext/*）+ OpenAPI + 使用指南
weather.go     天气抓取与缓存
course.go      课程表节次
util.go        ID/令牌生成、密码哈希、各种编解码
cmd/mkcert    自签证书工具
cmd/rrulecheck 重复规则自检工具
```

---

## 许可证

[MIT](LICENSE)
