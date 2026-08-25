package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"drip/internal/server/store"

	"go.uber.org/zap"
)

// Authentication outcomes. Callers should log these but report a single generic
// message to the client so a probe cannot distinguish "no such credential" from
// "credential disabled".
var (
	// ErrInvalidCredential means the token was not recognised.
	ErrInvalidCredential = errors.New("invalid credential")
	// ErrClientDisabled means the credential exists but was turned off.
	ErrClientDisabled = errors.New("client credential is disabled")
	// ErrAccountDisabled means the owning account was turned off.
	ErrAccountDisabled = errors.New("account is disabled")
	// ErrAuthRequired means the server requires a credential and none was given.
	ErrAuthRequired = errors.New("authentication required")
)

// DefaultCacheTTL bounds how long a disabled or rotated credential can keep
// working if the admin API forgets to invalidate it explicitly.
const DefaultCacheTTL = 30 * time.Second

// Identity is the result of authenticating a registration token.
type Identity struct {
	// Client and Account are set when the token was a stored credential.
	Client  *store.Client
	Account *store.Account
	// Legacy is true when the token matched the shared server token.
	Legacy bool
	// Anonymous is true when the server requires no authentication at all.
	Anonymous bool
}

// ClientID returns the credential ID, or "" for legacy/anonymous identities.
func (i *Identity) ClientID() string {
	if i == nil || i.Client == nil {
		return ""
	}
	return i.Client.ID
}

// AccountID returns the owning account ID, or "" for legacy/anonymous identities.
func (i *Identity) AccountID() string {
	if i == nil || i.Account == nil {
		return ""
	}
	return i.Account.ID
}

// IsStored reports whether this identity came from the control plane database,
// which is what reservation lookups require.
func (i *Identity) IsStored() bool {
	return i != nil && i.Client != nil
}

// Config configures an Authenticator.
type Config struct {
	// Store enables credential-based identity. Nil falls back to legacy or
	// anonymous behavior.
	Store *store.Store
	// LegacyToken is the single shared server token. Empty disables it.
	LegacyToken string
	// AllowAnonymous permits registration with no token at all. This preserves
	// the historical open-server default for self-hosted single-user setups.
	AllowAnonymous bool
	// CacheTTL bounds credential cache staleness. Zero uses DefaultCacheTTL.
	CacheTTL time.Duration
	Logger   *zap.Logger
}

type cacheEntry struct {
	client    *store.Client
	account   *store.Account
	expiresAt time.Time
}

// Authenticator resolves registration tokens to identities, with a short-lived
// cache so reconnect storms do not hammer SQLite.
type Authenticator struct {
	store          *store.Store
	legacyToken    string
	allowAnonymous bool
	cacheTTL       time.Duration
	logger         *zap.Logger

	mu    sync.RWMutex
	cache map[string]cacheEntry

	// wg tracks in-flight background writes so Close can drain them; closing
	// guards against new ones being started during shutdown.
	wg        sync.WaitGroup
	closeOnce sync.Once
	closed    atomic.Bool
	stopCh    chan struct{}
}

// New builds an Authenticator from cfg.
func New(cfg Config) *Authenticator {
	ttl := cfg.CacheTTL
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	return &Authenticator{
		store:          cfg.Store,
		legacyToken:    cfg.LegacyToken,
		allowAnonymous: cfg.AllowAnonymous,
		cacheTTL:       ttl,
		logger:         logger,
		cache:          make(map[string]cacheEntry),
		stopCh:         make(chan struct{}),
	}
}

// StartPurgeTask periodically evicts expired cache entries so the map does not
// grow without bound. It stops when Close is called.
func (a *Authenticator) StartPurgeTask(interval time.Duration) {
	if interval <= 0 {
		interval = DefaultCacheTTL * 10
	}

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				a.PurgeExpired()
			case <-a.stopCh:
				return
			}
		}
	}()
}

// StoreEnabled reports whether credential-based identity is active.
func (a *Authenticator) StoreEnabled() bool {
	return a != nil && a.store != nil
}

// Authenticate resolves token to an Identity.
func (a *Authenticator) Authenticate(ctx context.Context, token, remoteIP string) (*Identity, error) {
	// Stored credentials take priority: a token shaped like one is never
	// allowed to fall through to the shared-token comparison.
	if IsCredential(token) {
		if a.store == nil {
			return nil, ErrInvalidCredential
		}
		return a.authenticateCredential(ctx, token, remoteIP)
	}

	if a.legacyToken != "" {
		if subtle.ConstantTimeCompare([]byte(token), []byte(a.legacyToken)) != 1 {
			return nil, ErrInvalidCredential
		}
		return &Identity{Legacy: true}, nil
	}

	// No stored credential, no shared token configured.
	if a.allowAnonymous {
		return &Identity{Anonymous: true}, nil
	}
	if token == "" {
		return nil, ErrAuthRequired
	}
	return nil, ErrInvalidCredential
}

func (a *Authenticator) authenticateCredential(ctx context.Context, token, remoteIP string) (*Identity, error) {
	cred, err := ParseCredential(token)
	if err != nil {
		return nil, ErrInvalidCredential
	}

	client, account, err := a.lookup(ctx, cred.ID)
	if err != nil {
		return nil, err
	}

	if !VerifySecret(cred.Secret, client.SecretHash) {
		return nil, ErrInvalidCredential
	}
	if !client.Enabled {
		return nil, ErrClientDisabled
	}
	if !account.Enabled {
		return nil, ErrAccountDisabled
	}

	a.touch(client.ID, remoteIP)

	return &Identity{Client: client, Account: account}, nil
}

// lookup resolves a credential ID through the cache, falling back to the store.
func (a *Authenticator) lookup(ctx context.Context, id string) (*store.Client, *store.Account, error) {
	a.mu.RLock()
	entry, ok := a.cache[id]
	a.mu.RUnlock()
	if ok && time.Now().Before(entry.expiresAt) {
		return entry.client, entry.account, nil
	}

	client, err := a.store.GetClient(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, ErrInvalidCredential
		}
		return nil, nil, err
	}

	account, err := a.store.GetAccount(ctx, client.AccountID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Orphaned client row; treat as unusable rather than crashing.
			return nil, nil, ErrInvalidCredential
		}
		return nil, nil, err
	}

	a.mu.Lock()
	a.cache[id] = cacheEntry{
		client:    client,
		account:   account,
		expiresAt: time.Now().Add(a.cacheTTL),
	}
	a.mu.Unlock()

	return client, account, nil
}

// touch records last-seen metadata without blocking registration.
func (a *Authenticator) touch(clientID, remoteIP string) {
	if a.closed.Load() {
		return
	}

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.store.TouchClient(ctx, clientID, remoteIP); err != nil {
			a.logger.Debug("Failed to record client last seen",
				zap.String("client_id", clientID),
				zap.Error(err),
			)
		}
	}()
}

// Close stops accepting background writes and waits for in-flight ones to
// finish, so the database can be closed without racing a last-seen update.
func (a *Authenticator) Close() {
	if a == nil {
		return
	}
	a.closeOnce.Do(func() {
		a.closed.Store(true)
		close(a.stopCh)
	})
	a.wg.Wait()
}

// Invalidate drops a cached credential. The admin API must call this whenever a
// client is disabled, deleted, or has its secret rotated.
func (a *Authenticator) Invalidate(clientID string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	delete(a.cache, clientID)
	a.mu.Unlock()
}

// InvalidateAll clears the whole credential cache.
func (a *Authenticator) InvalidateAll() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.cache = make(map[string]cacheEntry)
	a.mu.Unlock()
}

// PurgeExpired drops stale cache entries so the map does not grow without bound
// when many one-off credentials connect.
func (a *Authenticator) PurgeExpired() {
	if a == nil {
		return
	}
	now := time.Now()
	a.mu.Lock()
	for id, entry := range a.cache {
		if now.After(entry.expiresAt) {
			delete(a.cache, id)
		}
	}
	a.mu.Unlock()
}
