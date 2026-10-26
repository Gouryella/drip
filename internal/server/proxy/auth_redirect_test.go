package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"go.uber.org/zap"

	"drip/internal/server/tunnel"
	"drip/internal/shared/protocol"
)

func TestIsValidRedirect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		target string
		want   bool
	}{
		{name: "root", target: "/", want: true},
		{name: "absolute path", target: "/dashboard", want: true},
		{name: "path with query", target: "/dashboard?tab=1", want: true},
		{name: "path with fragment", target: "/dashboard#section", want: true},
		{name: "empty", target: "", want: false},
		{name: "login page", target: "/_drip/login", want: false},
		{name: "relative path", target: "dashboard", want: false},
		{name: "absolute URL", target: "https://evil.example", want: false},
		{name: "scheme-relative URL", target: "//evil.example", want: false},
		{name: "backslash scheme-relative URL", target: `/\evil.example`, want: false},
		{name: "triple slash URL", target: "///evil.example", want: false},
		{name: "backslash in path", target: `/foo\bar`, want: false},
		{name: "newline", target: "/foo\nbar", want: false},
		{name: "tab", target: "/\tbar", want: false},
		{name: "null byte", target: "/foo\x00bar", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isValidRedirect(tt.target); got != tt.want {
				t.Fatalf("isValidRedirect(%q) = %v, want %v", tt.target, got, tt.want)
			}
		})
	}
}

func TestLoginRedirectSanitization(t *testing.T) {
	h := NewHandler(HandlerConfig{})
	conn := tunnel.NewConnection("demo", nil, zap.NewNop())
	conn.SetProxyAuth(&protocol.ProxyAuth{
		Enabled:  true,
		Type:     "password",
		Password: "s3cret",
	})

	tests := []struct {
		name     string
		redirect string
		want     string
	}{
		{name: "valid path", redirect: "/dashboard", want: "/dashboard"},
		{name: "valid path with query", redirect: "/dashboard?tab=1", want: "/dashboard?tab=1"},
		{name: "empty", redirect: "", want: "/"},
		{name: "open redirect", redirect: "//evil.example", want: "/"},
		{name: "backslash open redirect", redirect: `/\evil.example`, want: "/"},
		{name: "absolute redirect", redirect: "https://evil.example", want: "/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			form := url.Values{
				"password": {"s3cret"},
				"redirect": {tt.redirect},
			}
			r := httptest.NewRequest(http.MethodPost, "http://demo.example/_drip/login", strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()

			h.handleProxyLoginWithRateLimit(w, r, conn, "demo", "")

			if w.Code != http.StatusSeeOther {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
			}
			if got := w.Header().Get("Location"); got != tt.want {
				t.Fatalf("Location = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestServeLoginPageSanitizesRedirectField(t *testing.T) {
	h := NewHandler(HandlerConfig{})
	r := httptest.NewRequest(http.MethodGet, "http://demo.example/", nil)
	r.URL.Path = `/\evil.example`
	w := httptest.NewRecorder()

	h.serveLoginPage(w, r, "demo", "")

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
	body := w.Body.String()
	if strings.Contains(body, "evil.example") {
		t.Fatal("login page rendered an unsafe redirect target")
	}
	if !strings.Contains(body, `name="redirect" value="/"`) {
		t.Fatal("login page did not fall back to the root redirect")
	}
}
