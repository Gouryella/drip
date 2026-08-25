<p align="center">
  <img src="assets/logo.png" alt="Drip Logo" width="200" />
</p>

<h1 align="center">Drip</h1>
<h3 align="center">你的隧道，你的域名，随处可用</h3>

<p align="center">
  自建隧道方案，把本地服务暴露在你自己的域名下。
</p>

<p align="center">
  <a href="README.md">English</a>
</p>

<div align="center">

[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?style=flat&logo=go)](https://golang.org/)
[![License](https://img.shields.io/badge/License-BSD--3--Clause-blue.svg)](LICENSE)
[![TLS](https://img.shields.io/badge/TLS-1.3-green.svg)](https://tools.ietf.org/html/rfc8446)

</div>

> Drip 是一条安静、自律的隧道。
> 你在自己的网络里点亮一盏小灯，它便把光带出去——经过你自己的基础设施，按你自己的方式。

一个 Go 二进制文件既是客户端，也通过 `drip server` 充当服务端。流量只在你的机器
和你的服务器之间流动。

**本仓库是 [Gouryella/drip](https://github.com/Gouryella/drip) 的 fork**，新增了
控制平面、固定隧道名称和 Windows 服务。文档保存在本仓库中，不再依赖上游站点。

---

## 目录

- [为什么选择 Drip？](#为什么选择-drip)
- [本 fork 新增的功能](#本-fork-新增的功能)
- [安装](#安装)
- [快速开始](#快速开始)
- [隧道类型](#隧道类型)
- [配置文件](#配置文件)
- [作为 Windows 服务运行](#作为-windows-服务运行)
- [部署自己的服务端](#部署自己的服务端)
- [控制平面](#控制平面)
- [管理面板](#管理面板)
- [命令速查](#命令速查)
- [从源码构建](#从源码构建)
- [许可证](#许可证)

---

## 为什么选择 Drip？

- **数据自主可控** —— 没有第三方服务器，流量只经过你自己的客户端与服务端
- **没有限制** —— 隧道数量、带宽、请求数均不设限
- **真正免费** —— 使用你自己的域名，没有付费档位，没有功能阉割
- **单一二进制** —— 客户端、服务端、控制平面和管理面板都在一个文件里
- **开源** —— BSD 3-Clause

## 本 fork 新增的功能

| | |
|---|---|
| **控制平面** | 用 SQLite 保存账户和每台机器的凭据（`drip_<id>_<secret>`），每个客户端都有独立身份，不再共用一个 token |
| **预留（Reservations）** | 把子域名或 TCP 端口固定给某台机器，重连后永远是同一个 URL，而不是每次随机分配 |
| **通配符 TLS** | 服务端通过 ACME DNS-01 自行签发并续期 `*.<domain>`，无需反向代理，也不需要证书文件 |
| **管理面板** | 内嵌的 Web 面板，运行在独立端口上，用于管理凭据、预留和实时隧道状态 |
| **Windows 服务** | `drip service install` 把配置好的隧道注册为 Windows 服务，重启和注销后依然运行 |

上游的单用户自建模式完全保留：可以不用数据库，使用单一共享 token，或完全匿名。

---

## 安装

### Linux 和 macOS

```bash
bash <(curl -sL https://raw.githubusercontent.com/nickolasdeluca/drip-ex/main/scripts/install.sh)
```

脚本会先询问安装**客户端**还是**服务端**，然后下载对应的发行版并把 `drip` 加入
PATH。

### Windows

```powershell
irm https://raw.githubusercontent.com/nickolasdeluca/drip-ex/main/scripts/install-client.ps1 | iex
```

需要传参数时，先下载脚本：

```powershell
irm https://raw.githubusercontent.com/nickolasdeluca/drip-ex/main/scripts/install-client.ps1 -OutFile install-client.ps1
.\install-client.ps1 -InstallService -AllTunnels     # 安装并注册服务
.\install-client.ps1 -Version v1.2.3 -InstallDir C:\tools\drip
.\install-client.ps1 -Uninstall
```

脚本会校验发行版的 checksum；以管理员身份运行时安装到 `%ProgramFiles%\drip`，
否则安装到 `%LOCALAPPDATA%\Programs\drip`，并把该目录加入 PATH。

### 手动安装

从 [Releases](https://github.com/nickolasdeluca/drip-ex/releases) 下载压缩包，把
`drip`（或 `drip.exe`）放到 PATH 中的任意目录，或者[从源码构建](#从源码构建)。

---

## 快速开始

```bash
# 1. 配置服务器地址（写入 ~/.drip/config.yaml）
drip config init

# 2. 暴露本地端口
drip http 3000
# → https://swift-otter.your-domain.com

# 3. 或者指定子域名
drip http 3000 --subdomain myapp
# → https://myapp.your-domain.com
```

后台隧道：

```bash
drip http 3000 --daemon      # 后台运行
drip list                    # 查看运行中的隧道
drip attach http 3000        # 查看实时日志
drip stop http 3000          # 或：drip stop all
```

在 Windows 上建议使用[服务](#作为-windows-服务运行)而不是 `--daemon`：daemon 会
在注销时终止，重启后也不会自动恢复。

---

## 隧道类型

| 命令 | 暴露对象 | 示例 |
|---|---|---|
| `drip http <port>` | 本地 HTTP 服务 | `drip http 3000` |
| `drip https <port>` | 本地 HTTPS 服务 | `drip https 8443 --skip-local-tls-verify` |
| `drip tcp <port>` | 任意 TCP 服务，由服务端分配公网端口 | `drip tcp 5432` |

三者共用的参数：

| 参数 | 作用 |
|---|---|
| `-n, --subdomain <name>` | 申请指定的子域名 |
| `-a, --address <host>` | 转发到 `127.0.0.1` 以外的地址 |
| `-d, --daemon` | 后台运行 |
| `--allow-ip` / `--deny-ip` | 按 IP 或 CIDR 限制来源（可重复） |
| `--auth <password>` | 启用密码认证（仅 `http`/`https`） |
| `--auth-bearer <token>` | 启用 Bearer Token 认证（仅 `http`/`https`） |
| `--bandwidth <rate>` | 限速：`500K`、`1M`、`1G` |
| `--transport <mode>` | `auto`、`tcp`（直连 TLS 1.3）或 `wss`（WebSocket，可穿透 CDN） |
| `--skip-local-tls-verify` | 不校验本地 HTTPS 后端的证书 |

全局参数：`-s/--server`、`-t/--token`、`-v/--verbose`、`-k/--insecure`（仅用于
测试）。

实际生效的带宽限制是服务端默认值、预留覆盖值和客户端请求值三者中的最小值——任何
一方都无法抬高另一方设置的上限。

---

## 配置文件

`drip config init` 会写入 `~/.drip/config.yaml`（Windows 上为
`%USERPROFILE%\.drip\config.yaml`）。设置 `DRIP_CONFIG` 可以指定其他路径。

```yaml
server: tunnel.example.com:443
token: drip_a1b2c3d4e5f60718_YOUR_SECRET
tls: true

tunnels:
  - name: web
    type: http
    port: 3000
    subdomain: myapp

  - name: api
    type: http
    port: 8080
    subdomain: api
    transport: wss
    auth_bearer: sk-secret
    bandwidth: 5M

  - name: db
    type: tcp
    port: 5432
    subdomain: postgres
    allow_ips:
      - 10.0.0.0/8
```

按名字启动：

```bash
drip start web        # 单个
drip start web api    # 多个
drip start --all      # 配置文件中的全部隧道
drip start            # 列出已配置的隧道
```

其他配置命令：`drip config show`、`drip config set --server X --token Y`、
`drip config validate`、`drip config reset`。

---

## 作为 Windows 服务运行

即使没有用户登录，重启后依然保持隧道连接。

```powershell
# 在管理员 PowerShell 中执行
drip config init                    # 只需一次，用拥有 token 的用户执行
drip service install --all          # 或：--tunnel web --tunnel api
drip service start
drip service status
```

`install-client.ps1 -InstallService -AllTunnels` 会在安装时一并完成这些步骤，
`install-client.ps1 -Uninstall` 则全部撤销。

| 命令 | |
|---|---|
| `drip service install` | 注册服务。`--all` 或 `--tunnel <name>`（可重复）；`--start-type delayed\|auto\|manual`；`--username` / `--password` 指定运行账户；`--config`、`--log`、`--name` 覆盖路径和服务名 |
| `drip service start` / `stop` / `restart` | 控制服务 |
| `drip service status` | 状态、PID、启动类型和实际运行的命令行 |
| `drip service uninstall` | 停止并删除服务，配置和日志保留 |

**配置文件的处理方式。** 服务以 LocalSystem 身份运行，其主目录是
`C:\Windows\system32\config\systemprofile`，因此读不到用户目录下的配置文件。
`service install` 会把你的配置复制到 `%ProgramData%\drip\config.yaml`，并把权限
限制为 SYSTEM 和 Administrators——里面存着你的 token。用 `--config` 指定其他文件
时，该文件的权限不会被改动。

**重连策略。** 服务会以指数退避加抖动的方式无限重试，因此开机时网络尚未就绪或
服务端重启都不致命。若进程退出，Windows 会自行重启服务（间隔 5 秒、30 秒、60 秒）。

**日志。** 写入 `%ProgramData%\drip\logs\service.log`，启动、停止和失败事件同时
写入 Windows 事件日志。排查无法保持运行的服务时，可以在前台运行同一套逻辑：

```powershell
drip service run --config C:\ProgramData\drip\config.yaml --all --verbose
```

在 Linux 和 macOS 上，请改用 systemd 或 launchd 托管 `drip start --all`。

---

## 部署自己的服务端

服务端需要一个域名、一个端口，以及获取证书的方式。TLS 有三种模式：

| `tls_mode` | 证书来源 | 适用场景 |
|---|---|---|
| `acme` | 服务端自己通过 ACME DNS-01 签发 | 你掌握 DNS 解析权，希望自动获得 `*.<domain>` |
| `manual` | 磁盘上的 `tls_cert` / `tls_key` | 你已经有证书 |
| `none` | 前面的反向代理（Caddy、nginx） | 由其他组件终止 TLS |

只有 DNS-01 能签发通配符证书，而隧道子域名是不可预测的，所以 `acme` 模式必须提供
DNS 服务商的 API 凭据：

| `acme.dns_provider` | 凭据 |
|---|---|
| `cloudflare` | 对该 zone 具有 `Zone.DNS:Write` 权限的 API token |
| `hostinger` | 在 hPanel 的 **API** 页面生成的 API token |

Hostinger 的 token 在创建时会指定过期日期。请选择远晚于证书有效期的日期，否则几个月后
续期会以 HTTP 401 失败。新增其他服务商只需一个 import 加上
`internal/server/tls/dns.go` 中的一条注册项。

### Docker

目前不发布容器镜像，Docker 暂不受支持，但 Dockerfile 和 compose 文件都在仓库里，
可以在本地构建：

```bash
cd deployments
cp config.acme.example.yaml config.yaml   # 修改域名、DNS token、邮箱
docker compose up -d --build
```

`deployments/` 目录下提供了 compose 文件和示例：

| 文件 | |
|---|---|
| `docker-compose.yml` | 服务端直连 TLS |
| `docker-compose.caddy.yml` | Caddy 在前面终止 TLS |
| `config.example.yaml` | 使用证书文件的直连 TLS |
| `config.acme.example.yaml` | 自动通配符证书 |
| `config.caddy.example.yaml` | 位于反向代理之后 |
| `Caddyfile`、`nginx.example.conf` | 反向代理示例 |

### 二进制

```bash
drip server --config /etc/drip/config.yaml
```

也可以完全用参数和环境变量：

```bash
drip server \
  --domain tunnel.example.com \
  --port 443 \
  --db /var/lib/drip/drip.db \
  --tls-mode acme \
  --acme-dns-provider cloudflare \
  --acme-dns-token "$CF_API_TOKEN" \
  --acme-email ops@example.com \
  --admin 127.0.0.1:8444
```

每个服务端参数都有对应的环境变量（`DRIP_DOMAIN`、`DRIP_PORT`、`DRIP_DB_PATH`、
`DRIP_TLS_MODE` 等），完整列表见 `drip server --help`。
`scripts/install-server.sh` 会安装二进制并生成 systemd unit。

---

## 控制平面

让服务端使用数据库后，每台机器都拥有独立凭据，不再共用一个 token：

```bash
drip server --db /var/lib/drip/drip.db --require-auth
```

| 服务端模式 | 配置 |
|---|---|
| 每客户端独立凭据 | 设置了 `db_path` |
| 单一共享 token | 设置了 `token`，未设置 `db_path` |
| 匿名 | 两者都未设置（上游默认） |

接入一台机器：

```bash
export DRIP_DB_PATH=/var/lib/drip/drip.db

drip admin account create acme
drip admin client create laptop-01 --account acme
# → drip_a1b2c3d4e5f60718_<secret>   仅显示一次，之后无法找回

drip admin reservation create --account acme --subdomain myapp --client a1b2c3d4e5f60718
```

把该 token 写进那台机器的 `~/.drip/config.yaml`，之后每次连接都会绑定到
`myapp`——甚至不需要 `--subdomain` 参数，因为绑定到该客户端的第一个预留会被自动
认领。这正是 Windows 服务所依赖的路径。

| | |
|---|---|
| `drip admin account create\|list` | 账户 |
| `drip admin client create\|list\|disable\|enable\|rotate\|delete` | 机器凭据 |
| `drip admin reservation create\|list\|bind\|enable\|disable\|delete` | 名称与端口 |

需要注意的几点：

- 数据库中只保存 `sha256(secret)`，token 仅显示一次。
- 删除凭据不会删除预留——名称仍归该账户所有。
- `--reservations-only` 会把部署变成封闭集群：未绑定预留的注册一律拒绝。
- `admin`、`api`、`www` 等若干名称是保留子域名，客户端无法占用。

## 管理面板

```bash
drip server --db /var/lib/drip/drip.db --admin 127.0.0.1:8444
```

面板编译进二进制，运行在独立端口，绝不在隧道数据路径上。首次访问时创建第一个
运维账户。内置英文和巴西葡萄牙文，语言跟随浏览器设置。

请通过内网、VPN 或 SSH 隧道访问——不建议公开暴露，尤其是在首次初始化之前。

---

## 命令速查

| 命令 | |
|---|---|
| `drip http\|https\|tcp <port>` | 启动隧道 |
| `drip start [names…] [--all]` | 启动配置文件中定义的隧道 |
| `drip list` | 查看后台隧道（`-i` 进入交互模式） |
| `drip attach [type] [port]` | 查看后台隧道的实时输出 |
| `drip stop <type> <port>` 或 `drip stop all` | 停止后台隧道 |
| `drip config init\|show\|set\|validate\|reset` | 客户端配置 |
| `drip service …` | Windows 服务（见上文） |
| `drip server` | 运行隧道服务端 |
| `drip server config` | 服务端配置辅助命令 |
| `drip admin …` | 控制平面：账户、凭据、预留 |
| `drip version` | 版本、commit 和构建时间 |

---

## 从源码构建

需要 Go 1.26 及以上。

```bash
make build            # 构建当前平台的 bin/drip
make build-all        # linux、macOS、Windows 的 amd64 与 arm64
make test             # go test -race -cover ./...
make e2e              # 启动真实服务端与客户端的端到端测试
make fmt lint         # gofmt 与 golangci-lint
make demo             # 本地服务端、管理面板和两个客户端
```

`make build-all VERSION=v1.2.3` 会把版本号写进二进制。推送 `v*.*.*` tag 时，由
GoReleaser 依据 `.goreleaser.yaml` 产出发行版。

---

## 上游更新记录

### 2025-02-14

- **带宽限制（QoS）** —— 基于令牌桶的按隧道限速，服务端以 `min(客户端, 服务端)`
  作为实际限制
- **传输协议控制** —— 服务域名与隧道域名可独立配置

### 2025-01-29

- **Bearer Token 认证** —— 用于隧道访问控制
- **代码优化** —— 将大型模块拆分为更小、更专注的组件

---

## 许可证

BSD 3-Clause License，详见 [LICENSE](LICENSE)。
