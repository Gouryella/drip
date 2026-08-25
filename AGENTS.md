# AGENTS.md

Repo-specific instructions for coding agents. Read this before your first commit
in a session.

## What this project is

Drip is a self-hosted tunnel service (an ngrok-alike) written in Go: one binary,
`drip`, that is both the client and — via `drip server` — the server.

This fork is being turned into a **managed multi-tenant tunnel service** in the
shape of pinggy: pre-allocated tunnels bound to a client identity, an admin
panel to manage them, and a Windows service that connects and binds to its
reserved tunnel forever. The upstream single-user self-hosted mode must keep
working throughout.

## Roadmap

Six phases. Keep this list current as phases land.

1. **Control plane foundation** — SQLite store, client credentials, identity
   threaded through registration. *Done.*
2. **Wildcard TLS via certmagic** — embed ACME DNS-01 so the server obtains
   `*.<domain>` itself, with manual certs and reverse-proxy modes kept. *Done.*
3. **Reservations** — pin a subdomain or TCP port to an account/client; portal
   creates the reservation, client binds it automatically at registration. *Done.*
4. **Admin API + embedded UI** — on its own port, separate from tunnel traffic.
5. **Claim flow** — `active_sessions` rows, "pin this running tunnel" endpoint,
   rename-on-pin.
6. **Windows service** — `golang.org/x/sys/windows/svc` wrapper around the
   existing tunnel runner.

### Design decisions already made

- **SQLite** (`modernc.org/sqlite`, CGO-free) so the single-binary story holds.
  The store is behind a struct, not an interface; introduce an interface only if
  Postgres actually arrives.
- **Admin panel on a separate port**, never on the tunnel data path.
- **certmagic, not `x/crypto/acme/autocert`** — autocert cannot do DNS-01 and so
  cannot issue wildcards. The old `autocert.go` was dead code whose host policy
  was broken anyway (`HostWhitelist` does exact matching, so the literal
  `*.example.com` never matched a real subdomain); it is gone.
- **Both reservation paths ship**: portal-first (create the reservation, then the
  client binds it) is primary; claiming a running ephemeral tunnel is secondary.
  Both write the same `tunnel_reservations` row.

## Layout

```
cmd/drip/                  entry point; both client and server
internal/client/cli/       cobra commands: http, tcp, server, admin, daemon, ...
internal/client/tcp/       client-side dialer, connection pool, yamux sessions
internal/server/tcp/       listener, per-connection handler, registration, proxy
internal/server/tunnel/    Manager: the in-memory registry of live tunnels
internal/server/proxy/     HTTP handler; subdomain routing, /stats, /metrics
internal/server/store/     SQLite control plane (accounts, clients, ...)
internal/server/auth/      credentials, Argon2id passwords, Authenticator
internal/server/reservations/  which subdomain or port a client may bind
internal/server/tls/       TLS modes: none, manual certs, ACME DNS-01 wildcard
internal/shared/protocol/  5-byte framing, register/data-connect messages
internal/shared/           pools, qos, netutil, httputil, ui, ...
pkg/config/                server and client YAML config
deployments/               Dockerfiles, Caddyfile, example configs
```

## Commands

```bash
make build          # build bin/drip
make test           # go test -v -race -cover ./...
make e2e            # scripts/test/e2e-full.sh; starts real server + client
make fmt lint       # gofmt + golangci-lint
go test -race ./... # what CI effectively runs; keep it green
```

Always run `go vet ./...` and `go test -race ./...` before committing. The race
detector matters here: this codebase is concurrency-heavy and has had real races.

## Conventions

- **Never add yourself as a commit co-author.** No `Co-Authored-By` trailer, no
  "generated with" footer, in commit messages or PR bodies.
- Conventional commit subjects: `feat:`, `fix:`, `refactor:`, `test:`, `chore:`,
  `docs:`, optionally scoped (`fix(tcp):`). Imperative mood, lowercase.
- Errors are wrapped with `fmt.Errorf("...: %w", err)`; the message says what
  failed, not what the function is.
- Logging is `go.uber.org/zap` with typed fields, never `Sprintf` into a message.
- Comments explain *why*, not *what*. Match the existing density: exported
  identifiers get doc comments, non-obvious invariants get a sentence, ordinary
  code gets nothing.
- Table-driven tests where the cases vary; plain tests otherwise. Test names
  state the behavior (`TestAuthenticatedClientsBypassPerIPLimits`).
- No new dependencies without a reason worth stating in the commit message.
- **Admin panel copy lives in `admin/ui/i18n.js`, never as a literal in
  `app.js` or `index.html`.** Views call `t('key')`; static markup carries
  `data-i18n`. English and `pt-BR` ship, the language is picked from
  `localStorage` then the browser, and both dictionaries must define the same
  keys. Server messages pass through untranslated unless `serverError` has an
  `err.<exact message>` entry for them.

## Control plane

`internal/server/store` owns the SQLite database; `internal/server/auth` turns a
registration token into an identity.

Client credentials are `drip_<id>_<secret>`:

- `<id>` is 16 hex chars, stored in the clear as the `clients` primary key.
- `<secret>` is 24 random bytes, base64url — **it can contain `_`**, so parsing
  splits into exactly three fields (`strings.SplitN(token, "_", 3)`).
- Only `sha256(secret)` is stored. A fast hash is deliberate: the secret is 192
  bits of `crypto/rand`, so KDF cost buys nothing. Admin **passwords** are
  low-entropy human input and use Argon2id (`auth/password.go`). Do not
  "upgrade" credential hashing to Argon2id; do not downgrade passwords to SHA-256.

Server modes, selected by config:

| `db_path` | `token` | `require_auth` | Behavior |
|---|---|---|---|
| set | — | any | Per-client credentials from the database |
| — | set | any | Legacy single shared token |
| — | — | false | Anonymous: anyone may register (upstream default) |
| — | — | true | Rejected at config validation |

Migrations live in `store/schema.go` as an ordered slice; the index is the
version. **Never edit an applied migration — append a new one.**

## Reservations

`internal/server/reservations` decides what a registering client may bind.
`RegistrationHandler.Register` calls it before allocating anything, then hands
the resolved name to `tunnel.Manager`.

Resolution order:

1. **Client asked for a name.** If a reservation owns it: same account, enabled,
   and either unbound or bound to this client, it binds. Otherwise the
   registration is refused - never silently downgraded to a random name.
   If nobody owns it, it becomes an ephemeral tunnel.
2. **Client asked for nothing.** The first enabled reservation bound to this
   client that no live tunnel already holds is bound automatically. This is the
   Windows-service path: install the credential, connect, always land on the
   same URL.
3. **Neither.** A random subdomain, exactly as upstream Drip behaves.

`reservations_only: true` removes step 3 and turns the deployment into a closed
fleet where every tunnel is pre-allocated. It requires `db_path`.

Two rules that are easy to get wrong:

- A client whose reservations are *all* live gets `ErrReservationInUse`, not a
  random subdomain. Handing out a random name there would look like the
  reservation had been lost.
- Unauthenticated and legacy-token registrations have no account, so they can
  never take a reserved name by asking for it. `checkOwnership` treats an empty
  `AccountID` as a mismatch; do not "simplify" that check.

`http` and `https` share one reservation family (`NormalizeTunnelType`): a
reserved subdomain is a name, and the same name serves both. `tcp` reservations
pin a port instead, and the manager keys them by the derived `tcp-<port>`.

Reservations survive client deletion: the `clients` foreign key is
`ON DELETE SET NULL`, so the name stays with the account and only the binding
drops. Deleting the *account* cascades the reservations away with it.

## TLS

`tls_mode` picks one of three paths, and `ResolveTLSMode` infers it when unset so
pre-existing config files keep working:

| Mode | Certificate source | Inferred when |
|---|---|---|
| `none` | none; a reverse proxy terminates TLS | nothing else is configured |
| `manual` | `tls_cert` / `tls_key` on disk | a cert pair or `tls_enabled` is set |
| `acme` | certmagic, DNS-01 wildcard | `acme.dns_provider` is set |

ACME mode issues one certificate covering the server domain, the tunnel domain
and `*.<tunnel_domain>` — the wildcard is the whole point, since tunnel
subdomains are unpredictable. DNS-01 is the only challenge that can issue a
wildcard, so provider API credentials are mandatory; HTTP-01 and TLS-ALPN are
explicitly disabled rather than left to fail on a server that may not own port
80 or 443.

Adding a DNS provider is one import plus one entry in the `dnsProviders`
registry in `tls/dns.go`; it only has to satisfy `libdns.RecordAppender` and
`libdns.RecordDeleter`. Cloudflare ships today. Weigh the dependency tree before
adding heavy SDKs (Route53 pulls all of aws-sdk-go-v2).

All three modes build on `baseTLSConfig()`: TLS 1.3 only, three cipher suites,
no ALPN advertisement. ACME mode deliberately layers certmagic's
`GetCertificate` onto that base rather than using certmagic's own `TLSConfig()`,
which would advertise `h2` and `acme-tls/1` and silently change what the
listener negotiates. Keep that property — there is a test asserting the two
modes share a posture.

## Traps

- **Never enable certmagic's on-demand issuance.** With a wildcard covering
  every tunnel subdomain it buys nothing, and it would let arbitrary SNI values
  drive ACME requests straight into the Let's Encrypt rate limit (50 certs per
  registered domain per week). `NewACME` manages a fixed name set and nothing
  else. If on-demand is ever genuinely needed, it must first check the name
  against the live tunnel manager.
- **ACME issuance is synchronous at startup.** A server that cannot present a
  certificate is useless, so bad DNS credentials must fail the boot rather than
  surface later as handshake errors. Only first issuance pays the propagation
  wait; after that the cert loads from `acme.cache_dir`.
- **`acme.cache_dir` holds the ACME account key and must persist.** A deployment
  that wipes it re-issues from scratch every time and burns rate limit. Mount it
  as a volume; back it up.
- **Use `ca: staging` while testing anything ACME-related.** Production rate
  limits are per registered domain per week and a failing loop exhausts them.
- **Authenticated clients bypass the per-IP tunnel cap and registration rate
  limit** (`tunnel.Manager.RegisterOwned`). Those limits exist to stop anonymous
  abuse; a fleet behind one NAT would otherwise lock itself out. Authenticated
  clients are bounded by their account limit and the global cap instead.
- **`validateMetricsAuth` returns true when the token is empty**
  (`proxy/handler.go`). Fail-open is intentional for `/stats` on a self-hosted
  box, but the admin API must never reuse it — write a fail-closed check.
- **Reservations live in SQLite, never in the manager's shard maps.**
  `shard.used` is cleared on `Unregister`, so anything stored there is freed
  when a client disconnects — the opposite of what a reservation means. The
  manager is consulted only to ask whether a name is *currently live*.
- **Reservation bandwidth participates in a `min()`.** The effective limit is
  the smallest of the server default, the reservation override and the client's
  request, so no party can raise a limit another one set. Keep it that way.
- **`admin`, `api`, `www` and friends are reserved subdomains**
  (`shared/utils/subdomain.go`) and cannot be claimed by clients. The admin panel
  can safely live on one of them.
- **The credential cache has a 30s TTL.** Disabling, deleting or rotating a
  credential must call `Authenticator.Invalidate(clientID)`, or the old token
  keeps working until the TTL lapses.
- **`Authenticator.Close()` must run before the store is closed.** It drains the
  background last-seen writes; skipping it races the database shutdown.
- **Data connections re-validate the token against the group**, not against the
  server token (`tcp/data_connection_handler.go`). The group records whatever
  token registered the tunnel, so per-client credentials work — do not reintroduce
  a shared-token comparison ahead of it.
