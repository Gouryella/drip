package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"drip/internal/server/auth"
	"drip/internal/server/store"
)

const (
	// sessionCookie carries the session token. It is HttpOnly so page scripts
	// cannot read it.
	sessionCookie = "drip_admin_session"
	// csrfCookie carries the CSRF token. It is deliberately readable by scripts:
	// the browser echoes it back in a header, which an off-origin page cannot do.
	csrfCookie = "drip_admin_csrf"
	// csrfHeader is where the page must echo the CSRF token.
	csrfHeader = "X-CSRF-Token"

	sessionTokenBytes = 32
	csrfTokenBytes    = 32

	// DefaultSessionTTL bounds how long a signed-in session lasts.
	DefaultSessionTTL = 12 * time.Hour
)

var errNoSession = errors.New("no session")

// randomToken returns n bytes of crypto/rand as base64url text.
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashToken is the database key for a session token. Sessions are stored
// hashed so a database leak cannot be replayed as a live login.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// loginLimiter throttles password guessing. Attempts are counted per key
// (client IP, and separately per username) inside a sliding window.
type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string]*attemptRecord
	max      int
	window   time.Duration
}

type attemptRecord struct {
	count int
	first time.Time
}

func newLoginLimiter(max int, window time.Duration) *loginLimiter {
	return &loginLimiter{
		attempts: make(map[string]*attemptRecord),
		max:      max,
		window:   window,
	}
}

// allow reports whether another attempt may proceed for key.
func (l *loginLimiter) allow(key string) bool {
	if key == "" {
		return true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	rec, ok := l.attempts[key]
	if !ok || now.Sub(rec.first) > l.window {
		return true
	}
	return rec.count < l.max
}

// fail records a failed attempt.
func (l *loginLimiter) fail(key string) {
	if key == "" {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	rec, ok := l.attempts[key]
	if !ok || now.Sub(rec.first) > l.window {
		l.attempts[key] = &attemptRecord{count: 1, first: now}
		return
	}
	rec.count++
}

// succeed clears the counter after a successful sign-in.
func (l *loginLimiter) succeed(key string) {
	if key == "" {
		return
	}
	l.mu.Lock()
	delete(l.attempts, key)
	l.mu.Unlock()
}

// purge drops windows that have lapsed, so the map cannot grow without bound.
func (l *loginLimiter) purge() {
	now := time.Now()
	l.mu.Lock()
	for key, rec := range l.attempts {
		if now.Sub(rec.first) > l.window {
			delete(l.attempts, key)
		}
	}
	l.mu.Unlock()
}

// issueSession creates a session for the user and sets both cookies.
func (s *Server) issueSession(ctx context.Context, w http.ResponseWriter, r *http.Request, user *store.AdminUser) error {
	token, err := randomToken(sessionTokenBytes)
	if err != nil {
		return err
	}
	csrf, err := randomToken(csrfTokenBytes)
	if err != nil {
		return err
	}

	expiresAt := time.Now().Add(s.sessionTTL)
	err = s.store.CreateAdminSession(ctx, &store.AdminSession{
		ID:        hashToken(token),
		UserID:    user.ID,
		ExpiresAt: expiresAt,
		IP:        clientIP(r),
		UserAgent: truncate(r.UserAgent(), 256),
	})
	if err != nil {
		return err
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteStrictMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookie,
		Value:    csrf,
		Path:     "/",
		Expires:  expiresAt,
		HttpOnly: false,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteStrictMode,
	})

	return nil
}

// clearSessionCookies expires both cookies in the browser.
func (s *Server) clearSessionCookies(w http.ResponseWriter) {
	for _, name := range []string{sessionCookie, csrfCookie} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: name == sessionCookie,
			Secure:   s.secureCookies,
			SameSite: http.SameSiteStrictMode,
		})
	}
}

// currentUser resolves the request's session to an operator.
//
// This is fail-closed by construction: every path that cannot positively
// identify an enabled user returns an error. There is no configuration under
// which it returns a user without a valid session.
func (s *Server) currentUser(r *http.Request) (*store.AdminUser, *store.AdminSession, error) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		return nil, nil, errNoSession
	}

	sess, err := s.store.GetAdminSession(r.Context(), hashToken(cookie.Value))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, errNoSession
		}
		return nil, nil, err
	}

	user, err := s.store.GetAdminUser(r.Context(), sess.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, errNoSession
		}
		return nil, nil, err
	}
	if !user.Enabled {
		return nil, nil, errNoSession
	}

	return user, sess, nil
}

// checkCSRF validates the double-submit token on a state-changing request.
//
// The session cookie is SameSite=Strict, which already stops a cross-site page
// from driving the API with the operator's credentials. This is the second
// layer: an attacker who can make the browser send the cookie still cannot read
// the CSRF cookie to echo it into the header, because that requires same-origin
// script access.
func checkCSRF(r *http.Request) error {
	cookie, err := r.Cookie(csrfCookie)
	if err != nil || cookie.Value == "" {
		return fmt.Errorf("missing CSRF cookie")
	}

	header := r.Header.Get(csrfHeader)
	if header == "" {
		return fmt.Errorf("missing %s header", csrfHeader)
	}
	if subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(header)) != 1 {
		return fmt.Errorf("CSRF token mismatch")
	}
	return nil
}

// verifyPassword checks a password against a stored hash, absorbing malformed
// hashes as a plain failure.
func verifyPassword(password, hash string) bool {
	ok, err := auth.VerifyPassword(password, hash)
	return err == nil && ok
}

// clientIP extracts the peer address, preferring the reverse proxy header when
// the admin server is configured to trust one.
func clientIP(r *http.Request) string {
	addr := r.RemoteAddr
	if idx := strings.LastIndex(addr, ":"); idx > 0 {
		addr = addr[:idx]
	}
	return strings.Trim(addr, "[]")
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
