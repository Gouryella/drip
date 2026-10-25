package netutil

import (
	"net/http/httptest"
	"testing"
)

func TestExtractClientIPIgnoresForwardedHeadersByDefault(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest("GET", "http://example.test/", nil)
	req.RemoteAddr = "127.0.0.1:43210"
	req.Header.Set("X-Forwarded-For", "203.0.113.10")
	req.Header.Set("X-Real-IP", "203.0.113.11")

	if got := ExtractClientIP(req); got != "127.0.0.1" {
		t.Fatalf("ExtractClientIP() = %q, want direct remote IP", got)
	}
}

func TestExtractClientIPWithTrustedProxyUsesRightmostUntrustedForwardedIP(t *testing.T) {
	t.Parallel()

	trusted, err := NewTrustedProxySet([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("NewTrustedProxySet() error = %v", err)
	}

	req := httptest.NewRequest("GET", "http://example.test/", nil)
	req.RemoteAddr = "127.0.0.1:43210"
	req.Header.Set("X-Forwarded-For", "198.51.100.200, 203.0.113.25")

	if got := ExtractClientIPWithTrustedProxies(req, trusted); got != "203.0.113.25" {
		t.Fatalf("ExtractClientIPWithTrustedProxies() = %q, want rightmost untrusted XFF IP", got)
	}
}

func TestExtractClientIPWithTrustedProxyWalksTrustedChain(t *testing.T) {
	t.Parallel()

	trusted, err := NewTrustedProxySet([]string{"127.0.0.1", "10.0.0.0/8"})
	if err != nil {
		t.Fatalf("NewTrustedProxySet() error = %v", err)
	}

	req := httptest.NewRequest("GET", "http://example.test/", nil)
	req.RemoteAddr = "127.0.0.1:43210"
	req.Header.Set("X-Forwarded-For", "203.0.113.25, 10.1.2.3")

	if got := ExtractClientIPWithTrustedProxies(req, trusted); got != "203.0.113.25" {
		t.Fatalf("ExtractClientIPWithTrustedProxies() = %q, want first untrusted IP before trusted chain", got)
	}
}

func TestExtractClientIPWithUntrustedRemoteIgnoresForwardedHeaders(t *testing.T) {
	t.Parallel()

	trusted, err := NewTrustedProxySet([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatalf("NewTrustedProxySet() error = %v", err)
	}

	req := httptest.NewRequest("GET", "http://example.test/", nil)
	req.RemoteAddr = "198.51.100.50:43210"
	req.Header.Set("X-Forwarded-For", "203.0.113.25")

	if got := ExtractClientIPWithTrustedProxies(req, trusted); got != "198.51.100.50" {
		t.Fatalf("ExtractClientIPWithTrustedProxies() = %q, want direct remote IP", got)
	}
}

func TestExtractClientIPWithTrustedProxyFallsBackToRealIPHeader(t *testing.T) {
	t.Parallel()

	trusted, err := NewTrustedProxySet([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("NewTrustedProxySet() error = %v", err)
	}

	req := httptest.NewRequest("GET", "http://example.test/", nil)
	req.RemoteAddr = "127.0.0.1:43210"
	req.Header.Set("X-Real-IP", "2001:db8::1")

	if got := ExtractClientIPWithTrustedProxies(req, trusted); got != "2001:db8::1" {
		t.Fatalf("ExtractClientIPWithTrustedProxies() = %q, want X-Real-IP fallback", got)
	}
}

func TestExtractClientIPWithTrustedProxyPrefersForwardedForOverRealIP(t *testing.T) {
	t.Parallel()

	trusted, err := NewTrustedProxySet([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("NewTrustedProxySet() error = %v", err)
	}

	req := httptest.NewRequest("GET", "http://example.test/", nil)
	req.RemoteAddr = "127.0.0.1:43210"
	req.Header.Set("X-Forwarded-For", "203.0.113.25")
	req.Header.Set("X-Real-IP", "198.51.100.7")

	if got := ExtractClientIPWithTrustedProxies(req, trusted); got != "203.0.113.25" {
		t.Fatalf("ExtractClientIPWithTrustedProxies() = %q, want XFF address to take precedence over X-Real-IP", got)
	}
}

func TestExtractClientIPWithTrustedProxyRejectsUnparseableRealIP(t *testing.T) {
	t.Parallel()

	trusted, err := NewTrustedProxySet([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("NewTrustedProxySet() error = %v", err)
	}

	req := httptest.NewRequest("GET", "http://example.test/", nil)
	req.RemoteAddr = "127.0.0.1:43210"
	req.Header.Set("X-Real-IP", "not-an-ip")

	if got := ExtractClientIPWithTrustedProxies(req, trusted); got != "127.0.0.1" {
		t.Fatalf("ExtractClientIPWithTrustedProxies() = %q, want remote IP when X-Real-IP is unparseable", got)
	}
}

// With 0.0.0.0/0 trusted, every peer is trusted, the XFF walk skips every
// (trusted) entry, and X-Real-IP is taken as-is - the caller chooses the
// resolved IP outright. This is why the docs forbid 0.0.0.0/0.
func TestExtractClientIPWithEverythingTrustedBelievesRealIPHeader(t *testing.T) {
	t.Parallel()

	trusted, err := NewTrustedProxySet([]string{"0.0.0.0/0"})
	if err != nil {
		t.Fatalf("NewTrustedProxySet() error = %v", err)
	}

	req := httptest.NewRequest("GET", "http://example.test/", nil)
	req.RemoteAddr = "203.0.113.5:443"
	req.Header.Set("X-Forwarded-For", "203.0.113.77, 198.51.100.44")
	req.Header.Set("X-Real-IP", "198.51.100.99")

	if got := ExtractClientIPWithTrustedProxies(req, trusted); got != "198.51.100.99" {
		t.Fatalf("ExtractClientIPWithTrustedProxies() = %q, want attacker-chosen X-Real-IP under 0.0.0.0/0", got)
	}
}
