package tls

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/caddyserver/certmagic"
	"go.uber.org/zap"
)

// Certificate authority shorthands accepted in configuration. Anything else is
// treated as a literal ACME directory URL.
const (
	CAProduction = "production"
	CAStaging    = "staging"
)

// DefaultCacheDir is where certificates and the ACME account key are kept when
// no directory is configured.
func DefaultCacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "drip-certs")
	}
	return filepath.Join(home, ".drip", "certs")
}

// ACMEConfig configures certificate issuance over ACME.
type ACMEConfig struct {
	// Domain and TunnelDomain determine the certificate names when Names is
	// empty: the server domain, the tunnel domain, and *.<tunnel domain>.
	Domain       string
	TunnelDomain string
	// Names overrides the derived certificate names.
	Names []string

	// Email is the ACME account contact. Let's Encrypt uses it for expiry
	// warnings; it is optional but strongly recommended.
	Email string
	// CA is "production", "staging", or an ACME directory URL.
	CA string
	// CacheDir stores certificates and the ACME account key.
	CacheDir string

	// DNS holds the DNS provider credentials used for the DNS-01 challenge.
	DNS DNSProviderConfig
	// PropagationTimeout bounds the wait for the challenge TXT record to become
	// visible. Zero uses the certmagic default of two minutes.
	PropagationTimeout time.Duration
	// Resolvers optionally pins the DNS servers used for propagation checks.
	// Useful when the host's resolver serves stale or split-horizon answers.
	Resolvers []string

	Logger *zap.Logger
}

// ACMEManager owns the certmagic machinery for a running server.
type ACMEManager struct {
	magic  *certmagic.Config
	cache  *certmagic.Cache
	names  []string
	logger *zap.Logger
}

// resolveCA maps a configured CA shorthand to an ACME directory URL.
func resolveCA(ca string) string {
	switch strings.ToLower(strings.TrimSpace(ca)) {
	case "", CAProduction:
		return certmagic.LetsEncryptProductionCA
	case CAStaging:
		return certmagic.LetsEncryptStagingCA
	default:
		return ca
	}
}

// NewACME builds an ACME manager and obtains the certificates it needs.
//
// Issuance is synchronous: a server that cannot present a certificate is not
// useful, so a misconfigured DNS token should fail startup loudly rather than
// surface later as handshake errors. Only the first run pays the DNS
// propagation wait; afterwards the certificate is loaded from the cache
// directory and renewed in the background.
//
// Only the names derived from the configured domains are ever issued. On-demand
// issuance is deliberately not enabled: with a wildcard covering every tunnel
// subdomain it buys nothing, and it would let arbitrary SNI values drive ACME
// requests straight into the Let's Encrypt rate limit.
func NewACME(ctx context.Context, cfg ACMEConfig) (*ACMEManager, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	names := cfg.Names
	if len(names) == 0 {
		names = WildcardNames(cfg.Domain, cfg.TunnelDomain)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no certificate names: set a domain or acme.domains")
	}

	provider, err := newDNSProvider(cfg.DNS)
	if err != nil {
		return nil, err
	}

	cacheDir := cfg.CacheDir
	if cacheDir == "" {
		cacheDir = DefaultCacheDir()
	}
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create certificate cache directory: %w", err)
	}

	solver := &certmagic.DNS01Solver{
		DNSManager: certmagic.DNSManager{
			DNSProvider:        provider,
			PropagationTimeout: cfg.PropagationTimeout,
			Resolvers:          cfg.Resolvers,
			Logger:             logger,
		},
	}

	// The cache needs a config to manage each certificate, and the config needs
	// the cache; certmagic's intended pattern is to close over the variable.
	var magic *certmagic.Config
	cache := certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) {
			return magic, nil
		},
	})

	magic = certmagic.New(cache, certmagic.Config{
		Storage: &certmagic.FileStorage{Path: cacheDir},
		Logger:  logger,
	})

	magic.Issuers = []certmagic.Issuer{
		certmagic.NewACMEIssuer(magic, certmagic.ACMEIssuer{
			CA:          resolveCA(cfg.CA),
			Email:       cfg.Email,
			Agreed:      true,
			DNS01Solver: solver,
			// Wildcards require DNS-01, and the server does not necessarily own
			// port 80 or 443, so the other two challenge types are turned off
			// rather than left to fail.
			DisableHTTPChallenge:    true,
			DisableTLSALPNChallenge: true,
			Logger:                  logger,
		}),
	}

	logger.Info("Obtaining ACME certificates",
		zap.Strings("names", names),
		zap.String("ca", resolveCA(cfg.CA)),
		zap.String("dns_provider", cfg.DNS.Name),
		zap.String("cache_dir", cacheDir),
	)

	if err := magic.ManageSync(ctx, names); err != nil {
		cache.Stop()
		return nil, fmt.Errorf("failed to obtain certificates for %s: %w",
			strings.Join(names, ", "), err)
	}

	logger.Info("ACME certificates ready", zap.Strings("names", names))

	return &ACMEManager{
		magic:  magic,
		cache:  cache,
		names:  names,
		logger: logger,
	}, nil
}

// TLSConfig returns the server TLS configuration backed by managed certificates.
//
// It layers certmagic's certificate lookup onto the project's fixed posture
// rather than using certmagic's own TLSConfig, so that ACME mode negotiates
// exactly what manual mode does: TLS 1.3 only, no ALPN advertisement.
func (m *ACMEManager) TLSConfig() *tls.Config {
	cfg := baseTLSConfig()
	cfg.GetCertificate = m.magic.GetCertificate
	return cfg
}

// Names returns the certificate names under management.
func (m *ACMEManager) Names() []string {
	return m.names
}

// Close stops background renewal.
func (m *ACMEManager) Close() {
	if m == nil || m.cache == nil {
		return
	}
	m.cache.Stop()
}
