// Package tls builds the server's TLS configuration. Three modes are supported:
// no TLS (a reverse proxy terminates it), manual certificate files, and ACME
// with a DNS-01 wildcard obtained by the server itself.
package tls

import (
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Mode selects how the server obtains its certificate.
type Mode string

const (
	// ModeNone runs plain TCP. A reverse proxy is expected to terminate TLS.
	ModeNone Mode = "none"
	// ModeManual loads a certificate and key from disk.
	ModeManual Mode = "manual"
	// ModeACME obtains and renews certificates over ACME, using a DNS-01
	// wildcard so that unpredictable tunnel subdomains are covered.
	ModeACME Mode = "acme"
)

// ParseMode validates a configured mode string.
func ParseMode(s string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(s))) {
	case ModeNone:
		return ModeNone, nil
	case ModeManual:
		return ModeManual, nil
	case ModeACME:
		return ModeACME, nil
	case "":
		return "", fmt.Errorf("tls mode is empty")
	default:
		return "", fmt.Errorf("unknown tls mode %q: want none, manual or acme", s)
	}
}

// baseTLSConfig returns the project's fixed TLS posture: TLS 1.3 only, with the
// three TLS 1.3 cipher suites. Every mode layers its certificate source on top
// of this so the posture cannot drift between modes.
func baseTLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		MaxVersion: tls.VersionTLS13,
		CipherSuites: []uint16{
			tls.TLS_AES_128_GCM_SHA256,
			tls.TLS_AES_256_GCM_SHA384,
			tls.TLS_CHACHA20_POLY1305_SHA256,
		},
		ClientSessionCache: tls.NewLRUClientSessionCache(128),
	}
}

// LoadManual builds a TLS config from a certificate and key on disk.
func LoadManual(certFile, keyFile string) (*tls.Config, error) {
	if certFile == "" || keyFile == "" {
		return nil, fmt.Errorf("both a certificate and a key file are required in manual TLS mode")
	}

	if _, err := os.Stat(certFile); err != nil {
		return nil, fmt.Errorf("certificate file %s: %w", certFile, err)
	}
	if _, err := os.Stat(keyFile); err != nil {
		return nil, fmt.Errorf("key file %s: %w", keyFile, err)
	}

	cert, err := tls.LoadX509KeyPair(filepath.Clean(certFile), filepath.Clean(keyFile))
	if err != nil {
		return nil, fmt.Errorf("failed to load certificate: %w", err)
	}

	cfg := baseTLSConfig()
	cfg.Certificates = []tls.Certificate{cert}
	return cfg, nil
}

// WildcardNames returns the certificate names covering a deployment: the server
// domain, the tunnel domain, and the wildcard under the tunnel domain that every
// tunnel subdomain needs.
//
// Duplicates are removed, so the common case where the server and tunnel domain
// are the same yields two names rather than three.
func WildcardNames(domain, tunnelDomain string) []string {
	if tunnelDomain == "" {
		tunnelDomain = domain
	}

	candidates := []string{domain, tunnelDomain, "*." + tunnelDomain}

	seen := make(map[string]bool, len(candidates))
	names := make([]string, 0, len(candidates))
	for _, name := range candidates {
		name = strings.TrimSpace(strings.ToLower(name))
		if name == "" || name == "*." || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}

	return names
}
