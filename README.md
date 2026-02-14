<p align="center">
  <img src="assets/logo.png" alt="Drip Logo" width="200" />
</p>

<h1 align="center">Drip</h1>
<h3 align="center">Your Tunnel, Your Domain, Anywhere</h3>

<p align="center">
  A self-hosted tunneling solution to securely expose your services to the internet.
</p>

<p align="center">
  <a href="https://driptunnel.app/docs">Documentation</a>
  <span> | </span>
  <a href="https://driptunnel.app/docs">中文文档</a>
</p>

<div align="center">

[![Go](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://golang.org/)
[![License](https://img.shields.io/badge/License-BSD--3--Clause-blue.svg)](LICENSE)
[![TLS](https://img.shields.io/badge/TLS-1.3-green.svg)](https://tools.ietf.org/html/rfc8446)

</div>

> Drip is a quiet, disciplined tunnel.
> You light a small lamp on your network, and it carries that light outward—through your own infrastructure, on your own terms.

## Why Drip?

- **Control your data** - No third-party servers, traffic stays between your client and server
- **No limits** - Unlimited tunnels, bandwidth, and requests
- **Actually free** - Use your own domain, no paid tiers or feature restrictions
- **Open source** - BSD 3-Clause License

## Quick Start

### Install

```bash
bash <(curl -sL https://driptunnel.app/install.sh)
```

### Basic Usage

```bash
# Configure (first time only)
drip config init

# Expose local HTTP server
drip http 3000

# With custom subdomain
drip http 3000 -n myapp
# → https://myapp.your-domain.com
```

## Documentation

For complete documentation, visit **[Docs](https://driptunnel.app/docs)**

- [Installation Guide](https://driptunnel.app/docs/installation)
- [Basic Usage](https://driptunnel.app/docs/basic-tunnels)
- [Server Deployment](https://driptunnel.app/docs/direct-mode)
- [Command Reference](https://driptunnel.app/docs/commands)

## Recent Changes

**2025-02-14**
- **Bandwidth Limiting (QoS)** - Per-tunnel bandwidth control with token bucket algorithm, server enforces `min(client, server)` as effective limit
- **Transport Protocol Control** - Support independent configuration for service domain and tunnel domain

```bash
# Client: limit to 1MB/s
drip http 3000 --bandwidth 1M
```

```yaml
# Server: global limit (config.yaml)
bandwidth: 10M
burst_multiplier: 2.5
```

## License

BSD 3-Clause License - see [LICENSE](LICENSE) for details
