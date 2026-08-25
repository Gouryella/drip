package store

import "time"

// TunnelType mirrors protocol.TunnelType as stored in the database.
// It is kept as a plain string here so the store package stays free of
// protocol imports and can be reused by the admin API.
const (
	TunnelTypeHTTP  = "http"
	TunnelTypeHTTPS = "https"
	TunnelTypeTCP   = "tcp"
)

// Account owns clients and reservations. A single-operator deployment can run
// with one account; the model exists so reservations have a stable owner that
// survives credential rotation.
type Account struct {
	ID         string
	Name       string
	Enabled    bool
	MaxTunnels int // 0 = unlimited
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Client is a credential that a tunnel client presents at registration.
// SecretHash is the hex-encoded SHA-256 of the credential secret; the plaintext
// secret is returned exactly once, at creation time.
type Client struct {
	ID         string
	AccountID  string
	Name       string
	SecretHash string
	Enabled    bool
	Bandwidth  string // optional per-client override, e.g. "1M"; empty = server default
	LastSeenAt *time.Time
	LastSeenIP string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Reservation pins a subdomain (HTTP/HTTPS) or a port (TCP) to an account.
// ClientID nil means any client on the account may bind it by requesting the
// name explicitly; ClientID set means only that client may bind it, and it is
// bound automatically when that client registers without naming a subdomain.
type Reservation struct {
	ID         string
	AccountID  string
	ClientID   *string
	TunnelType string
	Subdomain  string // set for http/https
	TCPPort    int    // set for tcp
	Bandwidth  string
	Enabled    bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Session is a live tunnel registration. Rows are written when a tunnel is
// registered and removed when it goes away; they are the panel's view of what
// is currently online and what can be pinned.
type Session struct {
	ID            string
	AccountID     string
	ClientID      string
	ReservationID *string
	TunnelType    string
	Subdomain     string
	TCPPort       int
	LocalPort     int
	RemoteIP      string
	StartedAt     time.Time
}

// AdminUser is a human operator of the admin panel. Unlike client credentials,
// the password is low-entropy human input and is hashed with Argon2id.
type AdminUser struct {
	ID           string
	Username     string
	PasswordHash string
	Role         string
	Enabled      bool
	LastLoginAt  *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Admin roles.
const (
	RoleAdmin  = "admin"
	RoleViewer = "viewer"
)

// AdminSession is a logged-in admin panel session. ID is the SHA-256 of the
// session token; the token itself lives only in the operator's cookie.
type AdminSession struct {
	ID         string
	UserID     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
	IP         string
	UserAgent  string
}

// AuditEntry records a mutating action for later review.
type AuditEntry struct {
	ID         int64
	At         time.Time
	ActorType  string // "admin", "client", "system"
	ActorID    string
	Action     string
	TargetType string
	TargetID   string
	Detail     string
	IP         string
}
