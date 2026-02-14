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

[![Go](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://golang.org/)
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

## 快速开始

### 安装

```bash
bash <(curl -sL https://driptunnel.app/install.sh)
```

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
- [命令参考](https://driptunnel.app/docs/commands)

## 最近更新

**2025-02-14**
- **带宽限速 (QoS)** - 基于令牌桶算法的隧道级带宽控制，服务端取 `min(客户端, 服务端)` 作为生效带宽
- **传输协议控制** - 支持服务域名与隧道域名独立配置

```bash
# 客户端：限速 1MB/s
drip http 3000 --bandwidth 1M
```

```yaml
# 服务端：全局限速 (config.yaml)
bandwidth: 10M
burst_multiplier: 2.5
```

## 协议

BSD 3-Clause License - 详见 [LICENSE](LICENSE)
