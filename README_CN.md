<p align="center">
  <img src="assets/logo.png" alt="Drip Logo" width="200" />
</p>

<h1 align="center">Drip</h1>
<h3 align="center">你的隧道，你的域名，随处可用</h3>

<p align="center">
  自建隧道方案，让你的服务安全地暴露到公网。
</p>

<p align="center ">
  <a href="README.md">English</a>
  <span> | </span>
  <a href="README_CN.md">中文文档</a>
</p>

<div align="center">

[![Go](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://golang.org/)
[![License](https://img.shields.io/badge/License-BSD--3--Clause-blue.svg)](LICENSE)
[![TLS](https://img.shields.io/badge/TLS-1.3-green.svg)](https://tools.ietf.org/html/rfc8446)

</div>

> Drip 是一条安静、自律的隧道。  
> 你在自己的网络里点亮一盏小灯，它便把光带出去——经过你自己的基础设施，按你自己的方式。


## 为什么？

**掌控数据。** 没有第三方服务器，流量只在你的客户端与服务器之间传输。

**没有限制。** 想开多少隧道就开多少，带宽只受你的服务器性能限制。

**真的免费。** 用你自己的域名，没有付费档位或功能阉割。

| 特性 | Drip | ngrok 免费 |
|------|------|-----------|
| 隐私 | 自己的基础设施 | 第三方服务器 |
| 域名 | 你的域名 | 1 个固定子域名 |
| 带宽 | 无限制 | 1 GB/月 |
| 活跃端点 | 无限制 | 1 个端点 |
| 每个 Agent 的隧道数 | 无限制 | 最多 3 条 |
| 请求数 | 无限制 | 20,000 次/月 |
| 中间页 | 无 | 有（加请求头可移除） |
| 开源 | ✓ | ✗ |

## v0.5.0 更新内容

### 🔄 切换到 Yamux 协议

自研的多路复用协议存在较多边界情况的 bug，难以复现和修复。我们决定暂时放弃自研协议，改用 HashiCorp 久经考验的 [yamux](https://github.com/hashicorp/yamux) 库。

**为什么选择 Yamux？**
- 生产环境验证，被 Consul、Nomad 等关键基础设施广泛使用
- 内置流控和心跳保活
- 活跃的社区维护

**变更内容：**
- 移除：自研 HPACK 压缩、流控机制、二进制帧协议、HTTP 编解码器
- 新增：基于 Yamux 的连接池和会话管理
- 结果：协议层代码减少约 60%，稳定性显著提升

### ⚡ 性能提升

| 指标 | 提升幅度 |
|------|----------|
| 连接建立 | 快 3 倍（会话复用） |
| 单隧道内存占用 | -50%（状态简化） |
| 延迟 (p99) | -40%（减少编码层） |
| 吞吐量 | +80%（高效多路复用） |

> ⚠️ **破坏性变更**：协议与 v0.4.x 不兼容，客户端和服务端需同时升级。

## 快速安装

```bash
bash <(curl -sL https://raw.githubusercontent.com/Gouryella/drip/main/scripts/install.sh)
```

- 先选择语言，再选择安装 **客户端**（macOS/Linux）或 **服务器**（Linux）。
- 非交互示例：
  - 客户端：`bash <(curl -sL https://raw.githubusercontent.com/Gouryella/drip/main/scripts/install.sh) --client`
  - 服务器：`bash <(curl -sL https://raw.githubusercontent.com/Gouryella/drip/main/scripts/install.sh) --server`

### 卸载
```bash
bash <(curl -sL https://raw.githubusercontent.com/Gouryella/drip/main/scripts/uninstall.sh)
```

## 使用

### 首次配置

```bash
# 配置服务器地址和 token（只需一次）
drip config init
```

### 基础隧道

```bash
# 暴露本地 HTTP 服务
drip http 3000

# 暴露本地 HTTPS 服务
drip https 443

# 选择你的子域名
drip http 3000 -n myapp
# → https://myapp.your-domain.com

# 暴露 TCP 服务（数据库、SSH 等）
drip tcp 5432
```

### 转发到任意地址

不只是 localhost，可以转发到网络里的任何设备：

```bash
# 转发到局域网其他机器
drip http 8080 -a 192.168.1.100

# 转发到 Docker 容器
drip http 3000 -a 172.17.0.2

# 转发到特定网卡
drip http 3000 -a 10.0.0.5
```

### 后台模式

使用 `-d` 让隧道在后台运行：

```bash
# 后台启动隧道
drip http 3000 -d
drip https 8443 -n api -d

# 列出运行中的隧道
drip list

# 查看隧道日志
drip attach http 3000

# 停止隧道
drip stop http 3000
drip stop all
```

## 服务端部署

### 前置条件

- 域名 A 记录已指向服务器
- 子域名的泛解析：`*.tunnel.example.com -> 你的 IP`
- SSL 证书（推荐通配符）

### 方案一：直接部署（推荐）

Drip 服务端直接在 443 端口处理 TLS：

```bash
# 获取通配符证书
sudo certbot certonly --manual --preferred-challenges dns \
  -d "*.tunnel.example.com" -d "tunnel.example.com"

# 启动服务
drip-server \
  --port 443 \
  --domain tunnel.example.com \
  --tls-cert /etc/letsencrypt/live/tunnel.example.com/fullchain.pem \
  --tls-key /etc/letsencrypt/live/tunnel.example.com/privkey.pem \
  --token 你的密钥
```

### 方案二：Nginx 反向代理

Drip 监听 8443 端口，由 Nginx 负责 SSL 终止：

```nginx
server {
    listen 443 ssl http2;
    server_name *.tunnel.example.com;

    ssl_certificate /etc/letsencrypt/live/tunnel.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/tunnel.example.com/privkey.pem;

    location / {
        proxy_pass https://127.0.0.1:8443;
        proxy_ssl_verify off;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_buffering off;
    }
}
```

### Systemd 服务

安装脚本会自动创建 `/etc/systemd/system/drip-server.service`。管理方式：

```bash
sudo systemctl start drip-server
sudo systemctl enable drip-server
sudo journalctl -u drip-server -f
```

## 特性

**安全性**
- 所有连接使用 TLS 1.3 加密
- 基于 Token 的身份验证
- 不支持任何遗留协议

**灵活性**
- 支持 HTTP、HTTPS 和 TCP 隧道
- 可以转发到 localhost 或任何局域网地址
- 自定义子域名或自动生成
- 守护模式保持隧道持久运行

**性能**
- 二进制协议 + msgpack 编码
- 连接池复用
- 客户端与服务器之间的额外开销极小

**简单**
- 一行命令完成安装
- 配置一次，到处可用
- 实时查看连接统计

## 架构

```
┌─────────────┐         ┌──────────────┐         ┌─────────────┐
│  互联网用户  │ ──────> │     服务器    │ <────── │    客户端    │
│             │  HTTPS  │    (Drip)    │ TLS 1.3 │  localhost  │
└─────────────┘         └──────────────┘         └─────────────┘
```

## 常见场景

**开发与测试**
```bash
# 把本地开发站点给客户预览
drip http 3000

# 测试第三方 webhook（如 Stripe）
drip http 8000 -n webhooks
```

**家庭服务器访问**
```bash
# 远程访问家里的 NAS
drip http 5000 -a 192.168.1.50

# 通过 SSH 远程进入家庭网络
drip tcp 22
```

**Docker 与容器**
```bash
# 暴露容器化应用
drip http 8080 -a 172.17.0.3

# 数据库调试
drip tcp 5432 -a db-container
```

## 命令参考

```bash
# HTTP 隧道
drip http <端口> [参数]
  -n, --subdomain    自定义子域名
  -a, --address      目标地址（默认：127.0.0.1）
  -d, --daemon       后台运行
  -s, --server       服务器地址
  -t, --token        认证 token

# HTTPS 隧道（参数同 http）
drip https <端口> [参数]

# TCP 隧道（参数同 http）
drip tcp <端口> [参数]

# 后台隧道管理
drip list              列出运行中的隧道
drip list -i           交互模式
drip attach [类型] [端口]   查看日志
drip stop <类型> <端口>     停止隧道
drip stop all               停止所有隧道

# 配置
drip config init       设置服务器和 token
drip config show       显示当前配置
drip config set <键> <值>
```

## 协议

BSD 3-Clause License - 详见 [LICENSE](LICENSE)
