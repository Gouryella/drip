<p align="center">
  <img src="assets/logo.png" alt="Drip Logo" width="200" />
</p>

<h1 align="center">Drip</h1>
<h3 align="center">Your Tunnel, Your Domain, Anywhere</h3>

<p align="center">
  A self-hosted tunnelling service that exposes local services on your own domain.
</p>

<p align="center">
  <a href="README_CN.md">中文文档</a>
</p>

<div align="center">

[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?style=flat&logo=go)](https://golang.org/)
[![License](https://img.shields.io/badge/License-BSD--3--Clause-blue.svg)](LICENSE)
[![TLS](https://img.shields.io/badge/TLS-1.3-green.svg)](https://tools.ietf.org/html/rfc8446)

</div>

> Drip is a quiet, disciplined tunnel.
> You light a small lamp on your network, and it carries that light outward—through your own infrastructure, on your own terms.

One Go binary is both the client and, through `drip server`, the server. Traffic
goes from your machine to your server and nowhere else.

**This is a fork of [Gouryella/drip](https://github.com/Gouryella/drip).** It adds
a control plane, permanent tunnel names and a Windows service; documentation
lives in this repository rather than on the upstream site.

---

## Contents

- [Why Drip?](#why-drip)
- [What this fork adds](#what-this-fork-adds)
- [Install](#install)
- [Quick start](#quick-start)
- [Tunnel types](#tunnel-types)
- [Configuration file](#configuration-file)
- [Run as a Windows service](#run-as-a-windows-service)
- [Running your own server](#running-your-own-server)
- [Control plane](#control-plane)
- [Admin panel](#admin-panel)
- [Command reference](#command-reference)
- [Build from source](#build-from-source)
- [License](#license)

---

## Why Drip?

- **Control your data** — no third-party servers; traffic stays between your client and your server
- **No limits** — unlimited tunnels, bandwidth and requests
- **Actually free** — use your own domain, no paid tiers, no feature gates
- **One binary** — client, server, control plane and admin panel in a single file
- **Open source** — BSD 3-Clause

## What this fork adds

| | |
|---|---|
| **Control plane** | A SQLite database of accounts and per-machine credentials (`drip_<id>_<secret>`), so every client is identified instead of sharing one token |
| **Reservations** | Pin a subdomain or a TCP port to a machine. It reconnects to the same URL forever, instead of a new random name per session |
| **Wildcard TLS** | The server obtains and renews `*.<domain>` itself over ACME DNS-01, so no reverse proxy and no certificate files are needed |
| **Admin panel** | An embedded web panel on its own port for credentials, reservations and live tunnel state |
| **Windows service** | `drip service install` runs configured tunnels as a Windows service that survives reboots and logoff |

Single-user self-hosted mode still works exactly as upstream: no database, one
shared token or none at all.

---

## Install

### Linux and macOS

```bash
bash <(curl -sL https://raw.githubusercontent.com/nickolasdeluca/drip-ex/main/scripts/install.sh)
```

The script asks whether you are installing the **client** or the **server**,
downloads the matching release and puts `drip` on your PATH.

### Windows

```powershell
irm https://raw.githubusercontent.com/nickolasdeluca/drip-ex/main/scripts/install-client.ps1 | iex
```

To pass options, download the script first:

```powershell
irm https://raw.githubusercontent.com/nickolasdeluca/drip-ex/main/scripts/install-client.ps1 -OutFile install-client.ps1
.\install-client.ps1 -InstallService -AllTunnels     # install and register the service
.\install-client.ps1 -Version v1.2.3 -InstallDir C:\tools\drip
.\install-client.ps1 -Uninstall
```

It verifies the release checksum, installs to `%ProgramFiles%\drip` when
elevated (`%LOCALAPPDATA%\Programs\drip` otherwise) and adds the directory to
PATH.

### Manual

Grab an archive from [Releases](https://github.com/nickolasdeluca/drip-ex/releases)
and put `drip` (or `drip.exe`) anywhere on your PATH. Or [build from
source](#build-from-source).

---

## Quick start

```bash
# 1. Point the client at your server (writes ~/.drip/config.yaml)
drip config init

# 2. Expose a local port
drip http 3000
# → https://swift-otter.your-domain.com

# 3. Or ask for a name
drip http 3000 --subdomain myapp
# → https://myapp.your-domain.com
```

Background tunnels:

```bash
drip http 3000 --daemon      # detach
drip list                    # what is running
drip attach http 3000        # watch its logs
drip stop http 3000          # or: drip stop all
```

On Windows, prefer [the service](#run-as-a-windows-service) over `--daemon`: a
daemon dies at logoff and does not come back after a reboot.

---

## Tunnel types

| Command | Exposes | Example |
|---|---|---|
| `drip http <port>` | A local HTTP server | `drip http 3000` |
| `drip https <port>` | A local HTTPS server | `drip https 8443 --skip-local-tls-verify` |
| `drip tcp <port>` | Any TCP service; the server allocates a public port | `drip tcp 5432` |

Flags shared by all three:

| Flag | Purpose |
|---|---|
| `-n, --subdomain <name>` | Ask for a specific name |
| `-a, --address <host>` | Forward somewhere other than `127.0.0.1` |
| `-d, --daemon` | Run in the background |
| `--allow-ip` / `--deny-ip` | Restrict callers by IP or CIDR (repeatable) |
| `--auth <password>` | Require proxy authentication with a password (`http`/`https` only) |
| `--auth-bearer <token>` | Require a bearer token (`http`/`https` only) |
| `--bandwidth <rate>` | Cap throughput: `500K`, `1M`, `1G` |
| `--transport <mode>` | `auto`, `tcp` (direct TLS 1.3) or `wss` (WebSocket, survives a CDN) |
| `--skip-local-tls-verify` | Do not verify the certificate of a local HTTPS backend |

Global flags: `-s/--server`, `-t/--token`, `-v/--verbose`, `-k/--insecure`
(testing only).

The effective bandwidth limit is the smallest of the server default, the
reservation override and what the client asked for — no side can raise a limit
another one set.

---

## Configuration file

`drip config init` writes `~/.drip/config.yaml` (`%USERPROFILE%\.drip\config.yaml`
on Windows). Set `DRIP_CONFIG` to read it from somewhere else.

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

Named tunnels are started by name:

```bash
drip start web        # one
drip start web api    # several
drip start --all      # everything in the file
drip start            # list what is configured
```

Other config commands: `drip config show`, `drip config set --server X --token Y`,
`drip config validate`, `drip config reset`.

---

## Run as a Windows service

Keeps configured tunnels connected across reboots with nobody logged in.

```powershell
# From an elevated PowerShell prompt
drip config init                    # once, as the user who owns the token
drip service install --all          # or: --tunnel web --tunnel api
drip service start
drip service status
```

`install-client.ps1 -InstallService -AllTunnels` does all of that during
installation, and `install-client.ps1 -Uninstall` reverses it.

| Command | |
|---|---|
| `drip service install` | Register the service. `--all` or `--tunnel <name>` (repeatable); `--start-type delayed\|auto\|manual`; `--username` / `--password` to run as a specific account; `--config`, `--log`, `--name` to override paths and the service name |
| `drip service start` / `stop` / `restart` | Control it |
| `drip service status` | State, PID, start type and the command line it runs |
| `drip service uninstall` | Stop and remove it; configuration and logs are kept |

**How it handles configuration.** A service runs as LocalSystem, whose home is
`C:\Windows\system32\config\systemprofile`, so it cannot read a config file from
your user profile. `service install` copies yours to
`%ProgramData%\drip\config.yaml` and restricts it to SYSTEM and Administrators —
it holds your token. Pass `--config` to point at a different file, which is then
left untouched.

**Reconnection.** The service retries forever with exponential backoff and
jitter, so a reboot before the network is up, or a server restart, is not fatal.
Windows restarts the service itself if it exits (after 5s, 30s, then 60s).

**Logs.** `%ProgramData%\drip\logs\service.log`, plus start, stop and failure
events in the Windows event log. To debug a service that will not stay up, run
the same supervisor in the foreground:

```powershell
drip service run --config C:\ProgramData\drip\config.yaml --all --verbose
```

On Linux and macOS, supervise `drip start --all` with systemd or launchd instead.

---

## Running your own server

The server needs a domain, a port and a way to obtain certificates. Pick one of
three TLS modes:

| `tls_mode` | Certificates come from | Use when |
|---|---|---|
| `acme` | The server itself, over ACME DNS-01 | You control the DNS zone and want `*.<domain>` handled for you |
| `manual` | `tls_cert` / `tls_key` on disk | You already have a certificate |
| `none` | A reverse proxy in front (Caddy, nginx) | Something else terminates TLS |

Only DNS-01 can issue a wildcard, and tunnel subdomains are unpredictable, so
`acme` mode requires DNS provider API credentials:

| `acme.dns_provider` | Credential |
|---|---|
| `cloudflare` | An API token with `Zone.DNS:Write` on the zone |
| `hostinger` | An API token generated in hPanel under **API** |

Hostinger tokens expire on a date chosen when they are created. Pick one well
past the certificate lifetime, or renewals start failing with HTTP 401 months
after setup. Adding another provider is one import plus one entry in
`internal/server/tls/dns.go`.

### Docker

No container image is published — Docker is unsupported for now — but the
Dockerfile and compose files are here and build locally:

```bash
cd deployments
cp config.acme.example.yaml config.yaml   # edit: domain, DNS token, email
docker compose up -d --build
```

`deployments/` holds the compose files and examples:

| File | |
|---|---|
| `docker-compose.yml` | Server with direct TLS |
| `docker-compose.caddy.yml` | Caddy terminating TLS in front of the server |
| `config.example.yaml` | Direct TLS with certificate files |
| `config.acme.example.yaml` | Automatic wildcard certificates |
| `config.caddy.example.yaml` | Behind a reverse proxy |
| `Caddyfile`, `nginx.example.conf` | Reverse proxy examples |

### Binary

```bash
drip server --config /etc/drip/config.yaml
```

Or entirely from flags and environment variables:

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

Every server flag has an environment variable (`DRIP_DOMAIN`, `DRIP_PORT`,
`DRIP_DB_PATH`, `DRIP_TLS_MODE`, …); run `drip server --help` for the full list.
`scripts/install-server.sh` installs the binary and a systemd unit for you.

---

## Control plane

Point the server at a database and every machine gets its own credential instead
of sharing one token:

```bash
drip server --db /var/lib/drip/drip.db --require-auth
```

| Server mode | Configuration |
|---|---|
| Per-client credentials | `db_path` set |
| One shared token | `token` set, no `db_path` |
| Anonymous | Neither (upstream default) |

Onboard a machine:

```bash
export DRIP_DB_PATH=/var/lib/drip/drip.db

drip admin account create acme
drip admin client create laptop-01 --account acme
# → drip_a1b2c3d4e5f60718_<secret>   shown once, never again

drip admin reservation create --account acme --subdomain myapp --client a1b2c3d4e5f60718
```

Put that token in the machine's `~/.drip/config.yaml` and it binds `myapp` on
every connection — with no `--subdomain` flag at all, since the first reservation
bound to a client is claimed automatically. That is the path the Windows service
is built for.

| | |
|---|---|
| `drip admin account create\|list` | Accounts |
| `drip admin client create\|list\|disable\|enable\|rotate\|delete` | Machine credentials |
| `drip admin reservation create\|list\|bind\|enable\|disable\|delete` | Names and ports |

Notes worth knowing:

- Only `sha256(secret)` is stored, and a token is displayed exactly once.
- Reservations survive credential deletion — the name stays with the account.
- `--reservations-only` turns the deployment into a closed fleet: registrations
  that do not bind a reservation are refused.
- `admin`, `api`, `www` and a handful of others are reserved and cannot be
  claimed by clients.

## Admin panel

```bash
drip server --db /var/lib/drip/drip.db --admin 127.0.0.1:8444
```

The panel is compiled into the binary and served on its own port, never on the
tunnel data path. The first visit creates the first operator account. English and
Brazilian Portuguese ship; the language follows the browser.

Reach it over a private network, a VPN or an SSH tunnel — exposing it publicly is
discouraged, especially before that first-run screen has been used.

---

## Command reference

| Command | |
|---|---|
| `drip http\|https\|tcp <port>` | Start a tunnel |
| `drip start [names…] [--all]` | Start tunnels defined in the config file |
| `drip list` | Show background tunnels (`-i` for an interactive picker) |
| `drip attach [type] [port]` | Follow a background tunnel's output |
| `drip stop <type> <port>` or `drip stop all` | Stop background tunnels |
| `drip config init\|show\|set\|validate\|reset` | Client configuration |
| `drip service …` | Windows service (see above) |
| `drip server` | Run the tunnel server |
| `drip server config` | Server configuration helpers |
| `drip admin …` | Control plane: accounts, credentials, reservations |
| `drip version` | Version, commit and build time |

---

## Build from source

Requires Go 1.26+.

```bash
make build            # bin/drip for this platform
make build-all        # linux, macOS and Windows, amd64 and arm64
make test             # go test -race -cover ./...
make e2e              # full end-to-end suite with a real server and client
make fmt lint         # gofmt and golangci-lint
make demo             # local server, admin panel and two clients
```

`make build-all VERSION=v1.2.3` stamps the version into the binaries. Releases
are produced by GoReleaser from `.goreleaser.yaml` when a `v*.*.*` tag is pushed.

---

## Recent upstream changes

### 2025-02-14

- **Bandwidth limiting (QoS)** — per-tunnel control with a token bucket; the
  server enforces `min(client, server)` as the effective limit
- **Transport protocol control** — independent configuration for the service
  domain and the tunnel domain

### 2025-01-29

- **Bearer token authentication** for tunnel access control
- **Code optimisation** — large modules split into focused components

---

## License

BSD 3-Clause License — see [LICENSE](LICENSE).
