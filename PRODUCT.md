# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

Primary: the operator running a self-hosted Drip deployment — the person who owns
the server, the domain and the DNS credentials. Often a single administrator; at
most a small ops team. They are technical, live in a terminal, and reach for the
panel when a CLI command would be slower or when they need to see state across
many tunnels at once.

Confirmed for a later phase, not this one: end customers signing in to manage
only their own account's tunnels and reservations. The information architecture
should scope cleanly to a single account when that arrives; no customer-facing
work happens now.

## Product Purpose

Drip is a self-hosted tunnel service — a single Go binary that is both client and
server, exposing local services on public subdomains. This fork is becoming a
managed multi-tenant service: credentials identify machines, reservations pin
tunnels to those machines permanently, and a Windows service connects and binds
to its reserved tunnel.

The panel exists so an operator can see and change control-plane state — who may
connect, what names they own, what is live right now — without composing CLI
invocations against a SQLite database.

## Positioning

Self-hosted, single binary, no third-party server in the data path. Unlike hosted
tunnel services, the operator owns the domain, the certificates and the database.
Unlike bare tunnel tools, tunnels are pre-allocated and permanent rather than
ephemeral per-session URLs.

## Operating Context

The panel is served by the `drip server` process on its own port, separate from
tunnel traffic. It is reached over a private network, a VPN, or an SSH tunnel;
exposing it publicly is discouraged, particularly before first-run setup.

Operators arrive with a specific errand, usually one of:

- onboarding a machine: create a credential, reserve a name, hand over the token;
- answering "is it up?": check whether a tunnel is currently connected;
- revoking: disable or rotate a credential after a machine is lost or retired;
- auditing: see who changed what.

Everything the panel does is also available through `drip admin` on the server
host. The panel is the faster path, never the only one.

## Capabilities and Constraints

Manages accounts, client credentials, tunnel reservations, live tunnel state and
an audit log.

Hard technical constraints:

- Assets are compiled into the Go binary with `go:embed`. No build step, no
  bundler, no package manager, no CDN — vanilla HTML, CSS and JavaScript only.
- A strict Content-Security-Policy is served with every response: `default-src
  'none'`, `script-src 'self'`, no inline scripts, no off-origin anything.
- Session auth over cookies, with CSRF double-submit on every mutating request.
- Roles are `admin` (may change things) and `viewer` (read-only).

Product truths the design must respect:

- A client token is displayed exactly once, at creation or rotation, and is never
  recoverable afterwards.
- Destructive actions differ in blast radius: deleting an account cascades to its
  credentials and reservations; deleting a credential only unbinds its
  reservations; deleting a reservation releases the name for anyone to take.
- A fresh deployment has zero operators and must create the first one through an
  unauthenticated first-run screen.

## Brand Commitments

None binding. The repository's logo and marketing tagline are explicitly not
carried into the panel; the operator surface may establish its own identity.
The product name "Drip" is factual and stays.

## Evidence on Hand

Working control plane: SQLite schema, credential and reservation logic, and a
`drip admin` CLI, all in this repository. No customer list, no usage statistics,
no testimonials, no pricing — none exist and none may be invented.

## Product Principles

1. **The errand is short.** Operators arrive to do one thing. Getting to it must
   not require reading the screen top to bottom.
2. **State before controls.** What is true right now — connected, reserved,
   disabled — outranks the forms that change it.
3. **Irreversibility is visible.** Actions that cascade, release a name, or break
   a running deployment must read differently from ones that do not.
4. **Nothing is shown twice.** A token appears once; the interface must make that
   moment impossible to miss and impossible to lose by accident.
5. **The CLI is the floor.** The panel may be faster or clearer, never the only
   way to do something.

## Accessibility & Inclusion

No product-specific standard was established. Baseline expectations apply:
keyboard operability, visible focus, honest contrast in both colour schemes, and
no meaning carried by colour alone.
