package store

// migrations are applied in order. Never edit an applied migration; append a
// new one instead. The index in this slice is the version number (1-based).
var migrations = []string{
	// 1: control-plane foundation
	`
CREATE TABLE accounts (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL UNIQUE,
	enabled     INTEGER NOT NULL DEFAULT 1,
	max_tunnels INTEGER NOT NULL DEFAULT 0,
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);

CREATE TABLE clients (
	id           TEXT PRIMARY KEY,
	account_id   TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	name         TEXT NOT NULL,
	secret_hash  TEXT NOT NULL,
	enabled      INTEGER NOT NULL DEFAULT 1,
	bandwidth    TEXT NOT NULL DEFAULT '',
	last_seen_at INTEGER,
	last_seen_ip TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL,
	UNIQUE(account_id, name)
);
CREATE INDEX idx_clients_account ON clients(account_id);

CREATE TABLE tunnel_reservations (
	id          TEXT PRIMARY KEY,
	account_id  TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	client_id   TEXT REFERENCES clients(id) ON DELETE SET NULL,
	tunnel_type TEXT NOT NULL,
	subdomain   TEXT NOT NULL DEFAULT '',
	tcp_port    INTEGER NOT NULL DEFAULT 0,
	bandwidth   TEXT NOT NULL DEFAULT '',
	enabled     INTEGER NOT NULL DEFAULT 1,
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);
-- Partial unique indexes: a subdomain or a TCP port may be reserved once
-- globally, but the unused column (empty string / 0) must not collide.
CREATE UNIQUE INDEX idx_reservations_subdomain
	ON tunnel_reservations(subdomain) WHERE subdomain <> '';
CREATE UNIQUE INDEX idx_reservations_tcp_port
	ON tunnel_reservations(tcp_port) WHERE tcp_port <> 0;
CREATE INDEX idx_reservations_account ON tunnel_reservations(account_id);
CREATE INDEX idx_reservations_client ON tunnel_reservations(client_id);

CREATE TABLE active_sessions (
	id             TEXT PRIMARY KEY,
	account_id     TEXT NOT NULL,
	client_id      TEXT NOT NULL,
	reservation_id TEXT,
	tunnel_type    TEXT NOT NULL,
	subdomain      TEXT NOT NULL DEFAULT '',
	tcp_port       INTEGER NOT NULL DEFAULT 0,
	local_port     INTEGER NOT NULL DEFAULT 0,
	remote_ip      TEXT NOT NULL DEFAULT '',
	started_at     INTEGER NOT NULL
);
CREATE INDEX idx_sessions_account ON active_sessions(account_id);
CREATE INDEX idx_sessions_client ON active_sessions(client_id);
CREATE UNIQUE INDEX idx_sessions_subdomain
	ON active_sessions(subdomain) WHERE subdomain <> '';

CREATE TABLE admin_users (
	id            TEXT PRIMARY KEY,
	username      TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	role          TEXT NOT NULL DEFAULT 'admin',
	enabled       INTEGER NOT NULL DEFAULT 1,
	last_login_at INTEGER,
	created_at    INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL
);

CREATE TABLE audit_log (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	at          INTEGER NOT NULL,
	actor_type  TEXT NOT NULL,
	actor_id    TEXT NOT NULL DEFAULT '',
	action      TEXT NOT NULL,
	target_type TEXT NOT NULL DEFAULT '',
	target_id   TEXT NOT NULL DEFAULT '',
	detail      TEXT NOT NULL DEFAULT '',
	ip          TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_audit_at ON audit_log(at DESC);
`,

	// 2: admin panel sessions
	`
CREATE TABLE admin_sessions (
	-- id is the SHA-256 of the session token, never the token itself, so a
	-- database leak cannot be replayed as a live session.
	id           TEXT PRIMARY KEY,
	user_id      TEXT NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
	created_at   INTEGER NOT NULL,
	expires_at   INTEGER NOT NULL,
	last_seen_at INTEGER NOT NULL,
	ip           TEXT NOT NULL DEFAULT '',
	user_agent   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_admin_sessions_user ON admin_sessions(user_id);
CREATE INDEX idx_admin_sessions_expires ON admin_sessions(expires_at);
`,
}
