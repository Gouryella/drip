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
