<p align="center">
  <img src="assets/logo.png" alt="Drip Logo" width="200" />
</p>

<h1 align="center">Drip</h1>
<h3 align="center">你的隧道，你的域名，随处可用</h3>

<p align="center">
  自建隧道方案，让你的服务安全地暴露到公网。
</p>

<p align="center">
  <a href="https://driptunnel.app/docs">English</a>
  <span> | </span>
  <a href="https://driptunnel.app/docs">中文文档</a>
</p>

<div align="center">

[![Go](https://img.shields.io/badge/Go-1.26.8+-00ADD8?style=flat&logo=go)](https://golang.org/)
[![License](https://img.shields.io/badge/License-BSD--3--Clause-blue.svg)](LICENSE)
[![TLS](https://img.shields.io/badge/TLS-1.3-green.svg)](https://tools.ietf.org/html/rfc8446)

</div>

> Drip 是一条安静、自律的隧道。
> 你在自己的网络里点亮一盏小灯，它便把光带出去——经过你自己的基础设施，按你自己的方式。

## 为什么选择 Drip？

- **掌控数据** - 没有第三方服务器，流量只在你的客户端与服务器之间传输
- **没有限制** - 无限隧道、带宽和请求数
- **真的免费** - 用你自己的域名，没有付费档位或功能阉割
- **开源** - BSD 3-Clause 协议

## 最近更新

### 2026-10-25 — v0.8.0

- **吞吐 4×、p99 降低 6×** — 400 并发下 5.5k → 21k req/s，p99 145ms → 24ms。
- **自研 mux（dmux）** — 替换 yamux：零 RTT 开流、每流流控、批量写入；开/关流快约 4.5×。
- **HTTP/3（QUIC）** — 公网端口启用，支持 Alt-Svc 与 `--http3` / `DRIP_HTTP3` / `http3:` 配置。
- **大文件传输修复** — 1GiB 下载/上传 SHA-256 校验通过；慢速大文件不再被服务端超时中断。
- **默认不限制上传** — `max_request_body_bytes` 默认 0，S3 风格 PUT 与 chunked 上传开箱即用。
- **内存降低 35–45%** — 负载下 server 366 → 239MB，client 243 → 134MB。
- **Go 1.27.3** + 全新 CI 工具链（staticcheck 0.8.2、gosec 2.30.0、govulncheck 1.9.0）。

> 协议已变更：客户端与服务端需一起升级。

### 2025-02-14

- **带宽限速 (QoS)** - 支持按隧道粒度进行带宽控制，使用令牌桶算法，服务端按 `min(client, server)` 作为实际生效限速
- **传输协议控制** - 支持服务域名与隧道域名的独立配置

```bash
# Client: limit to 1MB/s
drip http 3000 --bandwidth 1M
```

```yaml
# Server: global limit (config.yaml)
bandwidth: 10M
burst_multiplier: 2.5
```

### 2025-01-29

- **Bearer Token 认证** - 新增 Bearer Token 认证支持，用于隧道访问控制
- **代码优化** - 将大型模块重构为更小、更专注的组件，提升可维护性

## 快速开始

### 安装

请选择固定 release 标签，并在执行任何脚本前先校验下载内容。安装脚本现在默认拒绝未固定版本的
release 下载（除非显式传入 `--allow-latest`），并会在解压前校验 release 归档的 SHA256。

```bash
VERSION=v0.8.0
SCRIPT=install-client.sh
SCRIPT_SHA256=<install-client.sh 的 sha256>
ARCHIVE_SHA256=<GitHub Release checksums 文件中的归档 sha256>

curl -fsSLO "https://raw.githubusercontent.com/Gouryella/drip/${VERSION}/scripts/${SCRIPT}"

# Linux:
printf '%s  %s\n' "$SCRIPT_SHA256" "$SCRIPT" | sha256sum -c -

# macOS:
printf '%s  %s\n' "$SCRIPT_SHA256" "$SCRIPT" | shasum -a 256 -c -

bash "$SCRIPT" --version "$VERSION" --checksum "$ARCHIVE_SHA256"
```

如果手动安装二进制，请先校验 release checksum 文件：

```bash
VERSION=v0.8.0
VERSION_NUMBER="${VERSION#v}"
ARCHIVE="drip_${VERSION_NUMBER}_linux_amd64.tar.gz"

curl -fsSLO "https://github.com/Gouryella/drip/releases/download/${VERSION}/${ARCHIVE}"
curl -fsSLO "https://github.com/Gouryella/drip/releases/download/${VERSION}/drip_${VERSION_NUMBER}_checksums.txt"
grep " ${ARCHIVE}$" "drip_${VERSION_NUMBER}_checksums.txt" | sha256sum -c -
tar -xzf "$ARCHIVE"
```

生产环境请避免使用 `curl | bash`。如果只是在一次性测试环境里有意使用管道，也应固定
`--version`、提供 `--checksum`，并先审阅脚本内容。

### 基本使用

```bash
# 配置（仅首次需要）
drip config init

# 暴露本地 HTTP 服务
drip http 3000

# 使用自定义子域名
drip http 3000 -n myapp
# → https://myapp.your-domain.com
```

## 文档

完整文档请访问 **[Docs](https://driptunnel.app/docs)**

- [安装指南](https://driptunnel.app/docs/installation)
- [基础使用](https://driptunnel.app/docs/basic-tunnels)
- [服务端部署](https://driptunnel.app/docs/direct-mode)
- [反向代理与客户端 IP 信任](https://driptunnel.app/docs/client-ip-trust)
- [命令参考](https://driptunnel.app/docs/commands)

## 协议

BSD 3-Clause License - 详见 [LICENSE](LICENSE)
