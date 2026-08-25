package tls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
)

// writeSelfSignedPair writes a throwaway certificate and key, returning paths.
func writeSelfSignedPair(t *testing.T) (certPath, keyPath string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "drip.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     []string{"drip.test"},
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate() error = %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalECPrivateKey() error = %v", err)
	}

	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")

	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	return certPath, keyPath
}

func TestParseMode(t *testing.T) {
	cases := []struct {
		in      string
		want    Mode
		wantErr bool
	}{
		{"none", ModeNone, false},
		{"manual", ModeManual, false},
		{"acme", ModeACME, false},
		{"ACME", ModeACME, false},
		{"  manual  ", ModeManual, false},
		{"", "", true},
		{"autocert", "", true},
	}

	for _, tc := range cases {
		got, err := ParseMode(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseMode(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMode(%q) error = %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestWildcardNames(t *testing.T) {
	cases := []struct {
		name         string
		domain       string
		tunnelDomain string
		want         []string
	}{
		{
			name:   "same domain collapses to two names",
			domain: "example.com",
			want:   []string{"example.com", "*.example.com"},
		},
		{
			name:         "separate tunnel domain keeps all three",
			domain:       "connect.example.com",
			tunnelDomain: "tunnels.example.com",
			want:         []string{"connect.example.com", "tunnels.example.com", "*.tunnels.example.com"},
		},
		{
			name:         "case is normalised",
			domain:       "Example.COM",
			tunnelDomain: "Example.com",
			want:         []string{"example.com", "*.example.com"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := WildcardNames(tc.domain, tc.tunnelDomain)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("WildcardNames(%q, %q) = %v, want %v",
					tc.domain, tc.tunnelDomain, got, tc.want)
			}
		})
	}
}

// Every tunnel lives on an unpredictable subdomain, so the wildcard is the
// entire point of ACME mode; losing it would break every tunnel URL.
func TestWildcardNamesAlwaysCoversTunnelSubdomains(t *testing.T) {
	names := WildcardNames("example.com", "tunnels.example.com")

	found := false
	for _, n := range names {
		if n == "*.tunnels.example.com" {
			found = true
		}
	}
	if !found {
		t.Fatalf("WildcardNames() = %v, want it to include *.tunnels.example.com", names)
	}
}

func TestLoadManual(t *testing.T) {
	certPath, keyPath := writeSelfSignedPair(t)

	cfg, err := LoadManual(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadManual() error = %v", err)
	}

	if len(cfg.Certificates) != 1 {
		t.Fatalf("Certificates = %d, want 1", len(cfg.Certificates))
	}
	if cfg.MinVersion != tls.VersionTLS13 || cfg.MaxVersion != tls.VersionTLS13 {
		t.Fatalf("TLS version range = [%d, %d], want TLS 1.3 only", cfg.MinVersion, cfg.MaxVersion)
	}
}

func TestLoadManualRejectsMissingFiles(t *testing.T) {
	certPath, keyPath := writeSelfSignedPair(t)

	if _, err := LoadManual("", keyPath); err == nil {
		t.Error("LoadManual() with no certificate = nil error, want failure")
	}
	if _, err := LoadManual(certPath, ""); err == nil {
		t.Error("LoadManual() with no key = nil error, want failure")
	}
	if _, err := LoadManual(filepath.Join(t.TempDir(), "absent.pem"), keyPath); err == nil {
		t.Error("LoadManual() with an absent certificate = nil error, want failure")
	}
}

func TestResolveCA(t *testing.T) {
	cases := map[string]string{
		"":                      certmagic.LetsEncryptProductionCA,
		"production":            certmagic.LetsEncryptProductionCA,
		"staging":               certmagic.LetsEncryptStagingCA,
		"STAGING":               certmagic.LetsEncryptStagingCA,
		"https://acme.test/dir": "https://acme.test/dir",
	}

	for in, want := range cases {
		if got := resolveCA(in); got != want {
			t.Errorf("resolveCA(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewDNSProvider(t *testing.T) {
	for _, name := range []string{"cloudflare", "hostinger", "HOSTINGER", " hostinger "} {
		provider, err := newDNSProvider(DNSProviderConfig{Name: name, APIToken: "token"})
		if err != nil {
			t.Fatalf("newDNSProvider(%q) error = %v", name, err)
		}
		if provider == nil {
			t.Fatalf("newDNSProvider(%q) = nil provider", name)
		}
	}
}

func TestNewDNSProviderErrors(t *testing.T) {
	if _, err := newDNSProvider(DNSProviderConfig{}); err == nil {
		t.Error("newDNSProvider() with no name = nil error, want failure")
	}
	if _, err := newDNSProvider(DNSProviderConfig{Name: "route53", APIToken: "t"}); err == nil {
		t.Error("newDNSProvider() with an unsupported name = nil error, want failure")
	}
	if _, err := newDNSProvider(DNSProviderConfig{Name: "cloudflare"}); err == nil {
		t.Error("newDNSProvider() with no token = nil error, want failure")
	}
	if _, err := newDNSProvider(DNSProviderConfig{Name: "hostinger"}); err == nil {
		t.Error("newDNSProvider() with no hostinger token = nil error, want failure")
	}
}

func TestSupportedDNSProvidersIsSorted(t *testing.T) {
	names := SupportedDNSProviders()
	if len(names) == 0 {
		t.Fatal("SupportedDNSProviders() is empty")
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("SupportedDNSProviders() = %v, want sorted", names)
		}
	}
}

// ACME mode must negotiate exactly what manual mode does, so a deployment does
// not silently change TLS posture when it switches to automatic certificates.
func TestACMEAndManualShareTLSPosture(t *testing.T) {
	certPath, keyPath := writeSelfSignedPair(t)

	manual, err := LoadManual(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadManual() error = %v", err)
	}

	acme := (&ACMEManager{magic: &certmagic.Config{}}).TLSConfig()

	if manual.MinVersion != acme.MinVersion || manual.MaxVersion != acme.MaxVersion {
		t.Fatalf("version range differs: manual [%d,%d], acme [%d,%d]",
			manual.MinVersion, manual.MaxVersion, acme.MinVersion, acme.MaxVersion)
	}
	if !reflect.DeepEqual(manual.CipherSuites, acme.CipherSuites) {
		t.Fatalf("cipher suites differ: manual %v, acme %v", manual.CipherSuites, acme.CipherSuites)
	}
	if len(acme.NextProtos) != 0 {
		t.Fatalf("acme NextProtos = %v, want none advertised", acme.NextProtos)
	}
	if acme.GetCertificate == nil {
		t.Fatal("acme GetCertificate is nil")
	}
}
