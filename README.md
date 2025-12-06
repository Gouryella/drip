<p align="center">
  <img src="images/logo.png" alt="Drip Logo" width="200" />
</p>

<h1 align="center">Drip</h1>
<h3 align="center">Your Tunnel, Your Domain, Anywhere</h3>

<p align="center">
  A self-hosted tunneling solution to securely expose your services to the internet.
</p>

<p align="center ">
  <a href="README.md">English</a>
  <span> | </span>
  <a href="README_CN.md">中文文档</a>
</p>

<p align="center">
  <a href="https://golang.org/">
    <img src="https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go" alt="Go Version" />
  </a>
  <a href="LICENSE">
    <img src="https://img.shields.io/badge/License-BSD--3--Clause-blue.svg" alt="License" />
  </a>
  <a href="https://tools.ietf.org/html/rfc8446">
    <img src="https://img.shields.io/badge/TLS-1.3-green.svg" alt="TLS" />
  </a>
</p>

> Drip is a quiet, disciplined tunnel.  
> You light a small lamp on your network, and it carries that light outward—through your own infrastructure, on your own terms.


## Why?

**Control your data.** No third-party servers means your traffic stays between your client and your server.

**No limits.** Run as many tunnels as you need, use as much bandwidth as your server can handle.

**Actually free.** Use your own domain, no paid tiers or feature restrictions.

| Feature | Drip | ngrok Free |
|---------|------|------------|
| Privacy | Your infrastructure | Third-party servers |
| Domain | Your domain | 1 static subdomain |
| Bandwidth | Unlimited | 1 GB/month |
| Active Endpoints | Unlimited | 1 endpoint |
| Tunnels per Agent | Unlimited | Up to 3 |
| Requests | Unlimited | 20,000/month |
| Interstitial Page | None | Yes (removable with header) |
| Open Source | ✓ | ✗ |

## Quick Install

### Client (macOS/Linux)

```bash
bash <(curl -sL https://raw.githubusercontent.com/Gouryella/drip/main/scripts/install.sh)
```

### Server (Linux)

```bash
bash <(curl -sL https://raw.githubusercontent.com/Gouryella/drip/main/scripts/install-server.sh)
```

### Uninstall
```bash
bash <(curl -sL https://raw.githubusercontent.com/Gouryella/drip/main/scripts/uninstall.sh)
```

## Usage

### First Time Setup

```bash
# Configure server and token (only needed once)
drip config init
```

### Basic Tunnels

```bash
# Expose local HTTP server
drip http 3000

# Expose local HTTPS server
drip https 443

# Pick your subdomain
drip http 3000 -n myapp
# → https://myapp.your-domain.com

# Expose TCP service (database, SSH, etc.)
drip tcp 5432
```

### Forward to Any Address

Not just localhost - forward to any device on your network:

```bash
# Forward to another machine on LAN
drip http 8080 -a 192.168.1.100

# Forward to Docker container
drip http 3000 -a 172.17.0.2

# Forward to specific interface
drip http 3000 -a 10.0.0.5
```

### Background Mode

Run tunnels in the background with `-d`:

```bash
# Start tunnel in background
drip http 3000 -d
drip https 8443 -n api -d

# List running tunnels
drip list

# View tunnel logs
drip attach http 3000

# Stop tunnels
drip stop http 3000
drip stop all
```

## Server Deployment

### Prerequisites

- A domain with DNS pointing to your server (A record)
- Wildcard DNS for subdomains: `*.tunnel.example.com -> YOUR_IP`
- SSL certificate (wildcard recommended)

### Option 1: Direct (Recommended)

Drip server handles TLS directly on port 443:

```bash
# Get wildcard certificate
sudo certbot certonly --manual --preferred-challenges dns \
  -d "*.tunnel.example.com" -d "tunnel.example.com"

# Start server
drip-server \
  --port 443 \
  --domain tunnel.example.com \
  --tls-cert /etc/letsencrypt/live/tunnel.example.com/fullchain.pem \
  --tls-key /etc/letsencrypt/live/tunnel.example.com/privkey.pem \
  --token YOUR_SECRET_TOKEN
```

### Option 2: Behind Nginx

Run Drip on port 8443, let Nginx handle SSL termination:

```nginx
server {
    listen 443 ssl http2;
    server_name *.tunnel.example.com;

    ssl_certificate /etc/letsencrypt/live/tunnel.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/tunnel.example.com/privkey.pem;

    location / {
        proxy_pass https://127.0.0.1:8443;
        proxy_ssl_protocols TLSv1.3;
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

### Systemd Service

The install script creates `/etc/systemd/system/drip-server.service` automatically. Manage with:

```bash
sudo systemctl start drip-server
sudo systemctl enable drip-server
sudo journalctl -u drip-server -f
```

## Features

**Security**
- TLS 1.3 encryption for all connections
- Token-based authentication
- No legacy protocol support

**Flexibility**
- HTTP, HTTPS, and TCP tunnels
- Forward to localhost or any LAN address
- Custom subdomains or auto-generated
- Daemon mode for persistent tunnels

**Performance**
- Binary protocol with msgpack encoding
- Connection pooling and reuse
- Minimal overhead between client and server

**Simplicity**
- One-line installation
- Save config once, use everywhere
- Real-time connection stats

## Architecture

```
┌─────────────┐         ┌──────────────┐         ┌─────────────┐
│   Internet  │ ──────> │    Server    │ <────── │   Client    │
│   User      │  HTTPS  │    (Drip)    │ TLS 1.3 │  localhost  │
└─────────────┘         └──────────────┘         └─────────────┘
```

## Common Use Cases

**Development & Testing**
```bash
# Show local dev site to client
drip http 3000

# Test webhooks from services like Stripe
drip http 8000 -n webhooks
```

**Home Server Access**
```bash
# Access home NAS remotely
drip http 5000 -a 192.168.1.50

# Remote into home network via SSH
drip tcp 22
```

**Docker & Containers**
```bash
# Expose containerized app
drip http 8080 -a 172.17.0.3

# Database access for debugging
drip tcp 5432 -a db-container
```

## Command Reference

```bash
# HTTP tunnel
drip http <port> [flags]
  -n, --subdomain    Custom subdomain
  -a, --address      Target address (default: 127.0.0.1)
  -d, --daemon       Run in background
  -s, --server       Server address
  -t, --token        Auth token

# HTTPS tunnel (same flags as http)
drip https <port> [flags]

# TCP tunnel (same flags as http)
drip tcp <port> [flags]

# Background tunnel management
drip list              List running tunnels
drip list -i           Interactive mode
drip attach [type] [port]   View logs
drip stop <type> <port>     Stop tunnel
drip stop all               Stop all tunnels

# Configuration
drip config init       Set up server and token
drip config show       Show current config
drip config set <key> <value>
```

## License

BSD 3-Clause License - see [LICENSE](LICENSE) for details
