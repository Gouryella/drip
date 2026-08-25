package admin

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"drip/internal/server/auth"
	"drip/internal/server/store"

	"go.uber.org/zap"
)

// minPasswordLength is the floor for admin passwords. Argon2id covers the
// hashing cost; this stops the obviously weak ones.
const minPasswordLength = 12

func validatePassword(password string) error {
	if utf8.RuneCountInString(password) < minPasswordLength {
		return fmt.Errorf("password must be at least %d characters", minPasswordLength)
	}
	return nil
}

// ---- accounts ----

type accountView struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Enabled    bool      `json:"enabled"`
	MaxTunnels int       `json:"max_tunnels"`
	CreatedAt  time.Time `json:"created_at"`
}

func toAccountView(a *store.Account) accountView {
	return accountView{
		ID:         a.ID,
		Name:       a.Name,
		Enabled:    a.Enabled,
		MaxTunnels: a.MaxTunnels,
		CreatedAt:  a.CreatedAt,
	}
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.store.ListAccounts(r.Context())
	if err != nil {
		s.internal(w, "list accounts", err)
		return
	}

	out := make([]accountView, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, toAccountView(a))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string `json:"name"`
		MaxTunnels int    `json:"max_tunnels"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	account, err := s.store.CreateAccount(r.Context(), req.Name, req.MaxTunnels)
	if err != nil {
		writeError(w, storeStatus(err), err.Error())
		return
	}

	s.audit(r, "account.create", "account", account.ID, account.Name)
	writeJSON(w, http.StatusCreated, toAccountView(account))
}

func (s *Server) handleUpdateAccount(w http.ResponseWriter, r *http.Request) {
	account, err := s.store.GetAccount(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, storeStatus(err), err.Error())
		return
	}

	var req struct {
		Name       *string `json:"name"`
		Enabled    *bool   `json:"enabled"`
		MaxTunnels *int    `json:"max_tunnels"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name != nil {
		account.Name = *req.Name
	}
	if req.Enabled != nil {
		account.Enabled = *req.Enabled
	}
	if req.MaxTunnels != nil {
		account.MaxTunnels = *req.MaxTunnels
	}

	if err := s.store.UpdateAccount(r.Context(), account); err != nil {
		writeError(w, storeStatus(err), err.Error())
		return
	}

	// Disabling an account must take effect now, not when the cache lapses.
	s.invalidateAccount(r, account.ID)

	s.audit(r, "account.update", "account", account.ID, account.Name)
	writeJSON(w, http.StatusOK, toAccountView(account))
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	s.invalidateAccount(r, id)

	if err := s.store.DeleteAccount(r.Context(), id); err != nil {
		writeError(w, storeStatus(err), err.Error())
		return
	}

	s.audit(r, "account.delete", "account", id, "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ---- clients ----

type clientView struct {
	ID         string     `json:"id"`
	AccountID  string     `json:"account_id"`
	Name       string     `json:"name"`
	Enabled    bool       `json:"enabled"`
	Bandwidth  string     `json:"bandwidth,omitempty"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	LastSeenIP string     `json:"last_seen_ip,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

func toClientView(c *store.Client) clientView {
	return clientView{
		ID:         c.ID,
		AccountID:  c.AccountID,
		Name:       c.Name,
		Enabled:    c.Enabled,
		Bandwidth:  c.Bandwidth,
		LastSeenAt: c.LastSeenAt,
		LastSeenIP: c.LastSeenIP,
		CreatedAt:  c.CreatedAt,
	}
}

func (s *Server) handleListClients(w http.ResponseWriter, r *http.Request) {
	clients, err := s.store.ListClients(r.Context(), r.URL.Query().Get("account_id"))
	if err != nil {
		s.internal(w, "list clients", err)
		return
	}

	out := make([]clientView, 0, len(clients))
	for _, c := range clients {
		out = append(out, toClientView(c))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateClient(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccountID string `json:"account_id"`
		Name      string `json:"name"`
		Bandwidth string `json:"bandwidth"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	cred, err := auth.GenerateCredential()
	if err != nil {
		s.internal(w, "generate credential", err)
		return
	}

	client := &store.Client{
		ID:         cred.ID,
		AccountID:  req.AccountID,
		Name:       req.Name,
		SecretHash: auth.HashSecret(cred.Secret),
		Enabled:    true,
		Bandwidth:  req.Bandwidth,
	}
	if err := s.store.CreateClient(r.Context(), client); err != nil {
		writeError(w, storeStatus(err), err.Error())
		return
	}

	s.audit(r, "client.create", "client", client.ID, client.Name)

	// The token is returned exactly once, here. It is never recoverable later.
	writeJSON(w, http.StatusCreated, struct {
		clientView
		Token string `json:"token"`
	}{
		clientView: toClientView(client),
		Token:      cred.String(),
	})
}

func (s *Server) handleUpdateClient(w http.ResponseWriter, r *http.Request) {
	client, err := s.store.GetClient(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, storeStatus(err), err.Error())
		return
	}

	var req struct {
		Name      *string `json:"name"`
		Enabled   *bool   `json:"enabled"`
		Bandwidth *string `json:"bandwidth"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name != nil {
		client.Name = *req.Name
	}
	if req.Enabled != nil {
		client.Enabled = *req.Enabled
	}
	if req.Bandwidth != nil {
		client.Bandwidth = *req.Bandwidth
	}

	if err := s.store.UpdateClient(r.Context(), client); err != nil {
		writeError(w, storeStatus(err), err.Error())
		return
	}

	s.invalidateClient(client.ID)

	s.audit(r, "client.update", "client", client.ID, client.Name)
	writeJSON(w, http.StatusOK, toClientView(client))
}

func (s *Server) handleRotateClient(w http.ResponseWriter, r *http.Request) {
	client, err := s.store.GetClient(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, storeStatus(err), err.Error())
		return
	}

	cred, err := auth.GenerateCredential()
	if err != nil {
		s.internal(w, "generate credential", err)
		return
	}
	cred.ID = client.ID

	if err := s.store.RotateClientSecret(r.Context(), client.ID, auth.HashSecret(cred.Secret)); err != nil {
		writeError(w, storeStatus(err), err.Error())
		return
	}

	// The previous token must stop working immediately, not in 30 seconds.
	s.invalidateClient(client.ID)

	s.audit(r, "client.rotate", "client", client.ID, client.Name)
	writeJSON(w, http.StatusOK, map[string]string{"token": cred.String()})
}

func (s *Server) handleDeleteClient(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	s.invalidateClient(id)

	if err := s.store.DeleteClient(r.Context(), id); err != nil {
		writeError(w, storeStatus(err), err.Error())
		return
	}

	s.audit(r, "client.delete", "client", id, "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ---- reservations ----

type reservationView struct {
	ID         string    `json:"id"`
	AccountID  string    `json:"account_id"`
	ClientID   *string   `json:"client_id"`
	TunnelType string    `json:"tunnel_type"`
	Subdomain  string    `json:"subdomain,omitempty"`
	TCPPort    int       `json:"tcp_port,omitempty"`
	Bandwidth  string    `json:"bandwidth,omitempty"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
}

func toReservationView(res *store.Reservation) reservationView {
	return reservationView{
		ID:         res.ID,
		AccountID:  res.AccountID,
		ClientID:   res.ClientID,
		TunnelType: res.TunnelType,
		Subdomain:  res.Subdomain,
		TCPPort:    res.TCPPort,
		Bandwidth:  res.Bandwidth,
		Enabled:    res.Enabled,
		CreatedAt:  res.CreatedAt,
	}
}

func (s *Server) handleListReservations(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListReservations(r.Context(), r.URL.Query().Get("account_id"))
	if err != nil {
		s.internal(w, "list reservations", err)
		return
	}

	out := make([]reservationView, 0, len(list))
	for _, res := range list {
		out = append(out, toReservationView(res))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateReservation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccountID string  `json:"account_id"`
		ClientID  *string `json:"client_id"`
		Subdomain string  `json:"subdomain"`
		TCPPort   int     `json:"tcp_port"`
		Bandwidth string  `json:"bandwidth"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if (strings.TrimSpace(req.Subdomain) == "") == (req.TCPPort == 0) {
		writeError(w, http.StatusBadRequest, "provide exactly one of subdomain or tcp_port")
		return
	}

	reservation := &store.Reservation{
		AccountID: req.AccountID,
		ClientID:  req.ClientID,
		Subdomain: req.Subdomain,
		TCPPort:   req.TCPPort,
		Bandwidth: req.Bandwidth,
		Enabled:   true,
	}
	if req.Subdomain != "" {
		reservation.TunnelType = store.TunnelTypeHTTP
	} else {
		reservation.TunnelType = store.TunnelTypeTCP
	}

	if err := s.checkReservationClient(r, reservation); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := s.store.CreateReservation(r.Context(), reservation); err != nil {
		writeError(w, storeStatus(err), err.Error())
		return
	}

	s.audit(r, "reservation.create", "reservation", reservation.ID, reservation.Target())
	writeJSON(w, http.StatusCreated, toReservationView(reservation))
}

func (s *Server) handleUpdateReservation(w http.ResponseWriter, r *http.Request) {
	reservation, err := s.store.GetReservation(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, storeStatus(err), err.Error())
		return
	}

	var req struct {
		ClientID  *string `json:"client_id"`
		Enabled   *bool   `json:"enabled"`
		Bandwidth *string `json:"bandwidth"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.ClientID != nil {
		if *req.ClientID == "" {
			reservation.ClientID = nil
		} else {
			reservation.ClientID = req.ClientID
		}
	}
	if req.Enabled != nil {
		reservation.Enabled = *req.Enabled
	}
	if req.Bandwidth != nil {
		reservation.Bandwidth = *req.Bandwidth
	}

	if err := s.checkReservationClient(r, reservation); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := s.store.UpdateReservation(r.Context(), reservation); err != nil {
		writeError(w, storeStatus(err), err.Error())
		return
	}

	s.audit(r, "reservation.update", "reservation", reservation.ID, reservation.Target())
	writeJSON(w, http.StatusOK, toReservationView(reservation))
}

func (s *Server) handleDeleteReservation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if err := s.store.DeleteReservation(r.Context(), id); err != nil {
		writeError(w, storeStatus(err), err.Error())
		return
	}

	s.audit(r, "reservation.delete", "reservation", id, "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// checkReservationClient rejects binding a reservation to a client that belongs
// to a different account, which would otherwise let one account pin a name onto
// another account's credential.
func (s *Server) checkReservationClient(r *http.Request, reservation *store.Reservation) error {
	if reservation.ClientID == nil || *reservation.ClientID == "" {
		return nil
	}

	client, err := s.store.GetClient(r.Context(), *reservation.ClientID)
	if err != nil {
		return fmt.Errorf("client %s not found", *reservation.ClientID)
	}
	if client.AccountID != reservation.AccountID {
		return fmt.Errorf("client %s belongs to a different account", client.ID)
	}
	return nil
}

// ---- deployment ----

type serverInfoView struct {
	Domain       string `json:"domain"`
	TunnelDomain string `json:"tunnel_domain"`
	PublicPort   int    `json:"public_port"`
	TLS          bool   `json:"tls"`
}

// handleServerInfo tells the panel how clients reach this deployment, so it can
// render real URLs and a runnable connection command instead of placeholders.
func (s *Server) handleServerInfo(w http.ResponseWriter, _ *http.Request) {
	d := s.deployment
	if d.TunnelDomain == "" {
		d.TunnelDomain = d.Domain
	}
	writeJSON(w, http.StatusOK, serverInfoView(d))
}

// ---- live tunnels and audit ----

type tunnelView struct {
	Subdomain         string    `json:"subdomain"`
	TunnelType        string    `json:"tunnel_type"`
	ClientID          string    `json:"client_id,omitempty"`
	AccountID         string    `json:"account_id,omitempty"`
	BytesIn           int64     `json:"bytes_in"`
	BytesOut          int64     `json:"bytes_out"`
	ActiveConnections int64     `json:"active_connections"`
	LastActive        time.Time `json:"last_active"`
}

func (s *Server) handleListTunnels(w http.ResponseWriter, _ *http.Request) {
	out := make([]tunnelView, 0)
	if s.manager == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}

	for _, conn := range s.manager.List() {
		if conn == nil {
			continue
		}
		clientID, accountID := conn.Owner()
		out = append(out, tunnelView{
			Subdomain:         conn.Subdomain,
			TunnelType:        string(conn.GetTunnelType()),
			ClientID:          clientID,
			AccountID:         accountID,
			BytesIn:           conn.GetBytesIn(),
			BytesOut:          conn.GetBytesOut(),
			ActiveConnections: conn.GetActiveConnections(),
			LastActive:        conn.LastActive,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type auditView struct {
	ID         int64     `json:"id"`
	At         time.Time `json:"at"`
	ActorType  string    `json:"actor_type"`
	ActorID    string    `json:"actor_id,omitempty"`
	Action     string    `json:"action"`
	TargetType string    `json:"target_type,omitempty"`
	TargetID   string    `json:"target_id,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	IP         string    `json:"ip,omitempty"`
}

func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}

	entries, err := s.store.ListAudit(r.Context(), limit)
	if err != nil {
		s.internal(w, "list audit", err)
		return
	}

	out := make([]auditView, 0, len(entries))
	for _, e := range entries {
		out = append(out, auditView{
			ID: e.ID, At: e.At, ActorType: e.ActorType, ActorID: e.ActorID,
			Action: e.Action, TargetType: e.TargetType, TargetID: e.TargetID,
			Detail: e.Detail, IP: e.IP,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- helpers ----

// internal logs the cause and returns an opaque error, so database details
// never reach the browser.
func (s *Server) internal(w http.ResponseWriter, what string, err error) {
	s.logger.Error("Admin request failed", zap.String("operation", what), zap.Error(err))
	writeError(w, http.StatusInternalServerError, "internal error")
}

// invalidateClient drops a credential from the registration cache.
func (s *Server) invalidateClient(clientID string) {
	if s.authenticator != nil {
		s.authenticator.Invalidate(clientID)
	}
}

// invalidateAccount drops every credential belonging to an account, so
// disabling or deleting the account takes effect on the next registration.
func (s *Server) invalidateAccount(r *http.Request, accountID string) {
	if s.authenticator == nil {
		return
	}

	clients, err := s.store.ListClients(r.Context(), accountID)
	if err != nil {
		// Fall back to clearing everything: a stale cache entry here would let
		// a disabled account keep registering tunnels.
		s.logger.Warn("Failed to list account clients; clearing credential cache",
			zap.String("account_id", accountID), zap.Error(err))
		s.authenticator.InvalidateAll()
		return
	}
	for _, c := range clients {
		s.authenticator.Invalidate(c.ID)
	}
}
