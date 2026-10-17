# 日程同步服务端

自托管的日程同步服务端。单个可执行文件，内置 SQLite、HTTPS 证书生成和可视化管理面板。不需要安装数据库、Web 服务器或 OpenSSL。

服务端只负责同步、分享和权限分发。日程内容保存在自己的机器上，不经过第三方。

## 功能

- 多端增量同步：全局序号、字段级合并、冲突记录、删除墓碑
- 账号体系：注册邀请码、管理员、封禁、批量建号、登录日志
- 共享与协作：日历成员、团队、邀请链接、实时笔记协作
- 内容审核：本地词库、举报处理、可选的大模型复核
- 客户端分发：版本更新、插件市场、设备列表
- 数据维护：孤儿扫描、重复日程清理、一致性备份、数据库恢复
- 管理面板：服务启停、配置、证书、账号、审核、分发、日志

## 运行环境

| 平台 | 说明 |
|---|---|
| Windows x64 | 直接运行 exe |
| Linux x64 / arm64 | 单个二进制 |
| macOS | 单个二进制 |

程序静态编译，客户机不需要 Go、SQLite 或任何运行库。SQLite 与 IANA 时区数据库已编译进去。

## 快速开始（Windows）

1. 把 `schedule-server.exe` 放进一个**可写目录**。不要放 `Program Files`，那里普通用户无法写数据。
2. 双击运行。程序会启动服务核心，并打开管理面板。
3. 浏览器地址是 `http://127.0.0.1:9800`。第一次打开时按提示创建管理员账号。
4. 在客户端里填服务器地址：`https://<这台机器的 IP 或域名>:8443`。

首次启动会自动生成一张本地 CA 和一张服务端证书，放在 `certs/` 目录：

```
certs/ca.crt        导入客户端设备，用于信任自签证书
certs/server.crt    服务端证书
certs/server.key    服务端私钥（权限 0600）
```

如果客户端不校验自签证书，可以跳过导入。生产环境建议在面板里上传正式证书。

## 命令行

```
schedule-server.exe                  启动管理器与管理面板
schedule-server.exe -run-server      只启动服务核心（由面板调用）
schedule-server.exe -init            写入默认配置后退出
schedule-server.exe -version         打印版本
schedule-server.exe -no-browser      启动时不打开浏览器
schedule-server.exe -config <路径>   指定配置文件
```

图形界面版本没有控制台窗口，日志写在 `data/logs/` 下：

```
data/logs/server.log    服务核心日志
data/logs/manager.log   管理器日志
data/logs/console.log   子进程的标准输出
```

## 配置文件

默认路径是 exe 同目录的 `config.json`。面板里的修改会写回这个文件。相对路径以 exe 所在目录为基准，不是当前工作目录。

| 字段 | 默认值 | 说明 |
|---|---|---|
| `addr` | `:8443` | HTTPS 监听地址，留空关闭 |
| `http_addr` | 空 | 额外的明文 HTTP 监听，适合放在反向代理后面 |
| `wordbook_addr` | 空 | 背词同步通道，留空关闭 |
| `internal_addr` | `127.0.0.1:0` | 面板调用的内部回环地址，`0` 表示随机端口 |
| `data_dir` | `data` | 数据库、日志、备份 |
| `cert_dir` | `certs` | 证书目录 |
| `server_name` | 空 | 生成证书时签入的域名 |
| `auto_cert` | `true` | 证书缺失时自动生成自签证书 |
| `invite_code` | 空 | 注册邀请码，空则首次启动自动生成 |
| `weather_place` / `weather_lat` / `weather_lon` | 北京市 | 下发给客户端的默认天气地点 |
| `live_secret` | 空 | 实时协作签名密钥，空则自动生成并落库 |
| `cos_base` | 空 | 更新包的对象存储前缀，空则从本机下载 |
| `market_url` | 空 | 网页版插件市场地址，空则客户端隐藏入口 |
| `panel_listen` | `127.0.0.1:9800` | 管理面板监听地址 |
| `panel_auto_open` | `true` | 启动时打开浏览器 |
| `auto_restart` | `true` | 服务核心异常退出时自动拉起 |

`diag` 打开 pprof，只在排障时使用。

## 管理面板

面板和服务核心是两个进程，所以停止服务不会把自己一起停掉。

| 页面 | 内容 |
|---|---|
| 概览 | 运行状态、账号/日历/日程数量、快捷操作、最近日志 |
| 服务 | 启动/停止/重启、监听与目录、证书生成与上传、日志 |
| 账号 | 搜索、封禁、管理员、删除、批量创建、邀请码 |
| 内容审核 | 违禁词增删改、待处理举报、AI 复核配置与测试 |
| 分发 | 客户端更新发布、插件审核、设备列表 |
| 维护 | 备份、下载、恢复、孤儿扫描、重复清理、登录日志、系统信息 |
| 设置 | 天气、对象存储、插件市场、实时协作、部署令牌 |

面板默认只监听 `127.0.0.1`。需要远程管理时，推荐用 SSH 隧道：

```bash
ssh -L 9800:127.0.0.1:9800 user@your-server
```

也可以把 `panel_listen` 改成 `0.0.0.0:9800`，但必须同时在防火墙里限制来源地址。

## 构建

需要 Go 1.23 或更高版本。`go.mod` 声明了 1.25，Go 会自动下载对应工具链。

Windows：

```powershell
./scripts/build-windows.ps1
```

Linux / macOS：

```bash
./scripts/build-unix.sh
```

手动构建：

```bash
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/schedule-server ./cmd/schedule-server
```

Windows 想隐藏控制台窗口，追加 `-H windowsgui`。

产物是静态二进制，可以直接复制到同架构的机器上运行。

## 目录结构

```
cmd/schedule-server/     入口：命令行、配置、管理器
internal/app/            服务核心：同步、账号、审核、分发、维护
internal/config/         配置结构与读写
internal/certgen/        自签 CA 与服务端证书生成
internal/panel/          管理面板后端与内嵌前端
  web/                   HTML / CSS / JS（无外部依赖）
scripts/                 构建脚本
```

运行时目录：

```
data/schedule.db         主数据库（SQLite WAL）
data/live.db             实时协作数据库
data/backups/            VACUUM INTO 生成的备份
data/logs/               日志
data/invite.txt          邀请码
data/internal.addr       内部管理地址与令牌（0600）
```

## 备份与恢复

面板里的“生成备份”使用 SQLite 的 `VACUUM INTO`，服务运行中也能得到一致性快照。

恢复会停掉服务、把当前数据库另存为 `schedule.db.bak-<时间>`，再替换并重启。恢复只接受 SQLite 文件头正确的 `.db`。

也可以直接复制 `data/schedule.db`，但要先停止服务，否则 WAL 里未落盘的部分会丢。

## 安全说明

- 第一个注册的账号自动成为管理员。部署完成后请立刻注册，不要留给别人。
- 密码使用 PBKDF2-HMAC-SHA256 存储，不保存原文。
- 管理面板的会话只存在内存里，重启管理器后需要重新登录。
- 自签证书的作用是让传输加密，不代表身份可信。客户端需要导入 `ca.crt`，或者换正式证书。
- 部署令牌保存在 exe 同目录的 `deploy-token.txt`，权限 0600。轮换后旧令牌立即失效。
- 服务端不保存客户端的大模型密钥。唯一例外是内容审核 AI 的密钥，它只用于服务端复核，接口返回时始终是掩码。

## 与客户端的协议兼容

插件清单的首选文件名是 `assets/schedule-plugin.json`（电脑端为包根 `schedule-plugin.json`）。历史包使用的 `assets/threees-plugin.json` 仍然兼容，不需要重新打包。

导航数据包与皮肤包的格式标识分别是 `schedule.navpack` 和 `schedule.skinpack`，旧标识同样兼容。

## 许可证

MIT，见 [LICENSE](LICENSE)。
