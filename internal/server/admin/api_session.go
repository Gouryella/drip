package admin

import (
	"net/http"
	"strings"
	"time"

	"drip/internal/server/auth"
	"drip/internal/server/store"

	"go.uber.org/zap"
)

// defaultAccountName is the account created alongside the first administrator.
const defaultAccountName = "default"

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type userView struct {
	ID          string     `json:"id"`
	Username    string     `json:"username"`
	Role        string     `json:"role"`
	Enabled     bool       `json:"enabled"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

func toUserView(u *store.AdminUser) userView {
	return userView{
		ID:          u.ID,
		Username:    u.Username,
		Role:        u.Role,
		Enabled:     u.Enabled,
		LastLoginAt: u.LastLoginAt,
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleBootstrapStatus reports whether the deployment still needs its first
// operator account. It leaks nothing beyond that single bit.
func (s *Server) handleBootstrapStatus(w http.ResponseWriter, r *http.Request) {
	count, err := s.store.CountAdminUsers(r.Context())
	if err != nil {
		s.logger.Error("Failed to count admin users", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"needs_setup": count == 0})
}

// handleBootstrap creates the first operator account.
//
// It is unauthenticated by necessity, so it is usable exactly once: the moment
// any admin user exists it refuses. A deployment therefore has a short window
// between first start and first sign-in, which is why the panel should not be
// exposed publicly before it is set up.
func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	count, err := s.store.CountAdminUsers(r.Context())
	if err != nil {
		s.logger.Error("Failed to count admin users", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if count > 0 {
		writeError(w, http.StatusConflict, "an administrator already exists; sign in instead")
		return
	}

	var req loginRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validatePassword(req.Password); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		s.logger.Error("Failed to hash password", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	user := &store.AdminUser{
		Username:     strings.ToLower(strings.TrimSpace(req.Username)),
		PasswordHash: hash,
		Role:         store.RoleAdmin,
		Enabled:      true,
	}
	if err := s.store.CreateAdminUser(r.Context(), user); err != nil {
		writeError(w, storeStatus(err), err.Error())
		return
	}

	// A fresh deployment gets an account to hang credentials off, so the
	// operator is never asked to choose between things that do not exist yet.
	// A single-tenant deployment can ignore accounts entirely from here on.
	if _, aerr := s.store.CreateAccount(r.Context(), defaultAccountName, 0); aerr != nil {
		s.logger.Warn("Failed to create the default account", zap.Error(aerr))
	}

	s.logger.Info("First administrator created", zap.String("username", user.Username))
	if err := s.store.AppendAudit(r.Context(), &store.AuditEntry{
		ActorType:  store.ActorSystem,
		Action:     "admin.bootstrap",
		TargetType: "admin_user",
		TargetID:   user.Username,
		IP:         clientIP(r),
	}); err != nil {
		s.logger.Warn("Failed to write audit entry", zap.Error(err))
	}

	if err := s.issueSession(r.Context(), w, r, user); err != nil {
		s.logger.Error("Failed to create session", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, toUserView(user))
}

// handleLogin authenticates an operator and issues a session.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	username := strings.ToLower(strings.TrimSpace(req.Username))
	ip := clientIP(r)

	// Throttle per source address and per username, so neither a single host
	// nor a single account can be ground down by guessing.
	if !s.limiter.allow(ip) || !s.limiter.allow("user:"+username) {
		writeError(w, http.StatusTooManyRequests, "too many sign-in attempts; wait a few minutes")
		return
	}

	user, err := s.store.GetAdminUserByUsername(r.Context(), username)
	if err != nil {
		// Hash anyway so a missing user and a wrong password take comparable
		// time, and the response is identical either way.
		_, _ = auth.HashPassword(req.Password)
		s.failLogin(ip, username, "unknown user")
		writeError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}

	if !verifyPassword(req.Password, user.PasswordHash) {
		s.failLogin(ip, username, "bad password")
		writeError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	if !user.Enabled {
		s.failLogin(ip, username, "disabled account")
		writeError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}

	s.limiter.succeed(ip)
	s.limiter.succeed("user:" + username)

	if err := s.issueSession(r.Context(), w, r, user); err != nil {
		s.logger.Error("Failed to create session", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := s.store.TouchAdminLogin(r.Context(), user.ID); err != nil {
		s.logger.Warn("Failed to record admin login", zap.Error(err))
	}
	s.logger.Info("Admin signed in",
		zap.String("username", user.Username),
		zap.String("ip", ip),
	)

	writeJSON(w, http.StatusOK, toUserView(user))
}

// failLogin records a failed attempt against both throttle keys.
func (s *Server) failLogin(ip, username, reason string) {
	s.limiter.fail(ip)
	s.limiter.fail("user:" + username)
	s.logger.Warn("Admin sign-in failed",
		zap.String("username", username),
		zap.String("ip", ip),
		zap.String("reason", reason),
	)
}

// handleWhoami returns the signed-in operator plus the CSRF token to echo.
func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, toUserView(userFrom(r)))
}

// handleLogout destroys the session server-side and clears the cookies.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil && cookie.Value != "" {
		if err := s.store.DeleteAdminSession(r.Context(), hashToken(cookie.Value)); err != nil {
			s.logger.Warn("Failed to delete session", zap.Error(err))
		}
	}
	s.clearSessionCookies(w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "signed out"})
}
