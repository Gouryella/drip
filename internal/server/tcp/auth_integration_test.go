package tcp

import (
	"net"
	"path/filepath"
	"testing"
	"time"

	json "github.com/goccy/go-json"
	"go.uber.org/zap"

	"drip/internal/server/auth"
	"drip/internal/server/reservations"
	"drip/internal/server/store"
	"drip/internal/server/tunnel"
	"drip/internal/shared/protocol"
)

// authTestServer is a listener wired to a real control plane database.
type authTestServer struct {
	listener *Listener
	manager  *tunnel.Manager
	store    *store.Store
	addr     string
}

func newAuthTestServer(t *testing.T, requireAuth bool) *authTestServer {
	t.Helper()
	return newTestServer(t, requireAuth, false)
}

// newTestServer starts a listener backed by a real control plane database.
func newTestServer(t *testing.T, requireAuth, reservationsOnly bool) *authTestServer {
	t.Helper()

	logger := zap.NewNop()

	s, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	authenticator := auth.New(auth.Config{
		Store:          s,
		AllowAnonymous: !requireAuth,
		Logger:         logger,
	})
	t.Cleanup(authenticator.Close)

	manager := tunnel.NewManager(logger)
	t.Cleanup(manager.Shutdown)

	portAlloc, err := NewPortAllocator(38000, 38050)
	if err != nil {
		t.Fatalf("NewPortAllocator() error = %v", err)
	}

	listener := NewListener(ListenerConfig{
		Address:       "127.0.0.1:0",
		Authenticator: authenticator,
		Resolver:      reservations.New(s, reservationsOnly, logger),
		Manager:       manager,
		Logger:        logger,
		PortAlloc:     portAlloc,
		Domain:        "local.test",
		TunnelDomain:  "local.test",
		PublicPort:    443,
	})
	if err := listener.Start(); err != nil {
		t.Fatalf("listener.Start() error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Stop() })

	return &authTestServer{
		listener: listener,
		manager:  manager,
		store:    s,
		addr:     listener.Addr().String(),
	}
}

// seedCredential creates an account and client, returning the plaintext token.
func (ts *authTestServer) seedCredential(t *testing.T, maxTunnels int) string {
	t.Helper()

	token, _, _ := ts.seedAccountCredential(t, "acme", maxTunnels)
	return token
}

// seedAccountCredential creates a named account with one client credential.
func (ts *authTestServer) seedAccountCredential(t *testing.T, accountName string, maxTunnels int) (string, *store.Account, *store.Client) {
	t.Helper()

	acct, err := ts.store.CreateAccount(t.Context(), accountName, maxTunnels)
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	cred, err := auth.GenerateCredential()
	if err != nil {
		t.Fatalf("GenerateCredential() error = %v", err)
	}

	client := &store.Client{
		ID:         cred.ID,
		AccountID:  acct.ID,
		Name:       "e2e",
		SecretHash: auth.HashSecret(cred.Secret),
		Enabled:    true,
	}
	if err := ts.store.CreateClient(t.Context(), client); err != nil {
		t.Fatalf("CreateClient() error = %v", err)
	}

	return cred.String(), acct, client
}

// register performs a registration handshake and returns the reply frame.
func (ts *authTestServer) register(t *testing.T, req protocol.RegisterRequest) *protocol.Frame {
	t.Helper()

	conn, err := net.DialTimeout("tcp", ts.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("SetDeadline() error = %v", err)
	}

	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal error = %v", err)
	}
	if err := protocol.WriteFrame(conn, protocol.NewFrame(protocol.FrameTypeRegister, payload)); err != nil {
		t.Fatalf("WriteFrame() error = %v", err)
	}

	frame, err := protocol.ReadFrame(conn)
	if err != nil {
		t.Fatalf("ReadFrame() error = %v", err)
	}
	return frame
}

func decodeError(t *testing.T, frame *protocol.Frame) protocol.ErrorMessage {
	t.Helper()

	var msg protocol.ErrorMessage
	if err := json.Unmarshal(frame.Payload, &msg); err != nil {
		t.Fatalf("unmarshal error frame: %v", err)
	}
	return msg
}

// tamperCredential changes the final character of a token so the secret is
// guaranteed to differ regardless of what the random secret ended with.
func tamperCredential(token string) string {
	last := token[len(token)-1]
	replacement := byte('x')
	if last == replacement {
		replacement = 'y'
	}
	return token[:len(token)-1] + string(replacement)
}

func TestRegistrationAcceptsValidCredential(t *testing.T) {
	ts := newAuthTestServer(t, true)
	token := ts.seedCredential(t, 0)

	frame := ts.register(t, protocol.RegisterRequest{
		Token:      token,
		TunnelType: protocol.TunnelTypeHTTP,
		LocalPort:  8080,
	})

	if frame.Type != protocol.FrameTypeRegisterAck {
		t.Fatalf("frame type = %s, want RegisterAck (error: %+v)", frame.Type, decodeError(t, frame))
	}

	var resp protocol.RegisterResponse
	if err := json.Unmarshal(frame.Payload, &resp); err != nil {
		t.Fatalf("unmarshal ack: %v", err)
	}
	if resp.Subdomain == "" {
		t.Fatal("RegisterResponse.Subdomain is empty")
	}

	// The tunnel must carry the control-plane identity that authorised it.
	conn, ok := ts.manager.Get(resp.Subdomain)
	if !ok {
		t.Fatalf("manager.Get(%q) = not found", resp.Subdomain)
	}
	clientID, accountID := conn.Owner()
	if clientID == "" || accountID == "" {
		t.Fatalf("Owner() = (%q, %q), want both populated", clientID, accountID)
	}
}

func TestRegistrationRejectsWrongSecret(t *testing.T) {
	ts := newAuthTestServer(t, true)
	token := ts.seedCredential(t, 0)

	frame := ts.register(t, protocol.RegisterRequest{
		Token:      tamperCredential(token),
		TunnelType: protocol.TunnelTypeHTTP,
		LocalPort:  8080,
	})

	if frame.Type != protocol.FrameTypeError {
		t.Fatalf("frame type = %s, want Error", frame.Type)
	}
	if code := decodeError(t, frame).Code; code != "authentication_failed" {
		t.Fatalf("error code = %q, want authentication_failed", code)
	}
}

func TestRegistrationRejectsMissingCredentialWhenAuthRequired(t *testing.T) {
	ts := newAuthTestServer(t, true)

	frame := ts.register(t, protocol.RegisterRequest{
		TunnelType: protocol.TunnelTypeHTTP,
		LocalPort:  8080,
	})

	if frame.Type != protocol.FrameTypeError {
		t.Fatalf("frame type = %s, want Error", frame.Type)
	}
}

func TestRegistrationRejectsDisabledClient(t *testing.T) {
	ts := newAuthTestServer(t, true)
	token := ts.seedCredential(t, 0)

	cred, err := auth.ParseCredential(token)
	if err != nil {
		t.Fatalf("ParseCredential() error = %v", err)
	}
	client, err := ts.store.GetClient(t.Context(), cred.ID)
	if err != nil {
		t.Fatalf("GetClient() error = %v", err)
	}
	client.Enabled = false
	if err := ts.store.UpdateClient(t.Context(), client); err != nil {
		t.Fatalf("UpdateClient() error = %v", err)
	}

	frame := ts.register(t, protocol.RegisterRequest{
		Token:      token,
		TunnelType: protocol.TunnelTypeHTTP,
		LocalPort:  8080,
	})

	if frame.Type != protocol.FrameTypeError {
		t.Fatalf("frame type = %s, want Error", frame.Type)
	}
}

// An anonymous server must keep working for self-hosted single-user setups.
func TestRegistrationAllowsAnonymousWhenNotRequired(t *testing.T) {
	ts := newAuthTestServer(t, false)

	frame := ts.register(t, protocol.RegisterRequest{
		TunnelType: protocol.TunnelTypeHTTP,
		LocalPort:  8080,
	})

	if frame.Type != protocol.FrameTypeRegisterAck {
		t.Fatalf("frame type = %s, want RegisterAck (error: %+v)", frame.Type, decodeError(t, frame))
	}
}
