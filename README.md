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

[![Go](https://img.shields.io/badge/Go-1.26.8+-00ADD8?style=flat&logo=go)](https://golang.org/)
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

## Recent Changes

### 2026-10-25 — v0.8.0

- **4× throughput, 6× lower p99** — 5.5k → 21k req/s at 400 connections; p99 145 ms → 24 ms on the same hardware.
- **In-house mux (dmux)** — replaces yamux with zero-RTT stream open, per-stream flow control, and batched writes; stream open/close is ~4.5× faster.
- **HTTP/3 (QUIC)** — enabled on the public port with Alt-Svc and `--http3` / `DRIP_HTTP3` / `http3:` switches.
- **Large transfers fixed** — 1 GiB downloads/uploads verified by SHA-256; slow transfers are no longer cut by server timeouts.
- **Unlimited uploads by default** — `max_request_body_bytes` defaults to 0, so S3-style PUT and chunked uploads work out of the box.
- **35–45% less memory** — server 366 → 239 MB, client 243 → 134 MB under load.
- **Go 1.27.3** with refreshed CI tooling (staticcheck 0.8.2, gosec 2.30.0, govulncheck 1.9.0).

> The wire protocol changed: upgrade the client and server together.

### 2025-02-14

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

### 2025-01-29

- **Bearer Token Authentication** - Added bearer token authentication support for tunnel access control
- **Code Optimization** - Refactored large modules into smaller, focused components for better maintainability

## Quick Start

### Install

Choose a release tag and verify downloads before executing anything. The
installers now refuse unpinned release downloads unless `--allow-latest` is set,
and they verify the release archive SHA256 before extraction.

```bash
VERSION=v0.8.0
SCRIPT=install-client.sh
SCRIPT_SHA256=<sha256-of-install-client.sh>
ARCHIVE_SHA256=<sha256-from-the-GitHub-Release-checksums-file>

curl -fsSLO "https://raw.githubusercontent.com/Gouryella/drip/${VERSION}/scripts/${SCRIPT}"

# Linux:
printf '%s  %s\n' "$SCRIPT_SHA256" "$SCRIPT" | sha256sum -c -

# macOS:
printf '%s  %s\n' "$SCRIPT_SHA256" "$SCRIPT" | shasum -a 256 -c -

bash "$SCRIPT" --version "$VERSION" --checksum "$ARCHIVE_SHA256"
```

For a manual binary install, verify the release checksum file first:

```bash
VERSION=v0.8.0
VERSION_NUMBER="${VERSION#v}"
ARCHIVE="drip_${VERSION_NUMBER}_linux_amd64.tar.gz"

curl -fsSLO "https://github.com/Gouryella/drip/releases/download/${VERSION}/${ARCHIVE}"
curl -fsSLO "https://github.com/Gouryella/drip/releases/download/${VERSION}/drip_${VERSION_NUMBER}_checksums.txt"
grep " ${ARCHIVE}$" "drip_${VERSION_NUMBER}_checksums.txt" | sha256sum -c -
tar -xzf "$ARCHIVE"
```

Avoid `curl | bash` for production installs. If you intentionally use a pipe for
a disposable environment, pin `--version`, provide `--checksum`, and review the
script first.

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
- [Reverse Proxy & Client IP Trust](https://driptunnel.app/docs/client-ip-trust)
- [Command Reference](https://driptunnel.app/docs/commands)

## License

BSD 3-Clause License - see [LICENSE](LICENSE) for details
