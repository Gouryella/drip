package tcp

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"

	json "github.com/goccy/go-json"
	"go.uber.org/zap"

	"drip/internal/server/tunnel"
	"drip/internal/shared/protocol"
)

func TestIsAuthTokenAccepted(t *testing.T) {
	tests := []struct {
		name            string
		providedToken   string
		configuredToken string
		allowAnonymous  bool
		want            bool
	}{
		{
			name:            "configured token matches",
			providedToken:   "server-secret",
			configuredToken: "server-secret",
			want:            true,
		},
		{
			name:            "configured token rejects wrong token",
			providedToken:   "wrong-secret",
			configuredToken: "server-secret",
			want:            false,
		},
		{
			name:            "empty configured token rejects by default",
			providedToken:   "",
			configuredToken: "",
			want:            false,
		},
		{
			name:            "explicit anonymous accepts empty client token",
			providedToken:   "",
			configuredToken: "",
			allowAnonymous:  true,
			want:            true,
		},
		{
			name:            "explicit anonymous rejects arbitrary client token without configured token",
			providedToken:   "wrong-secret",
			configuredToken: "",
			allowAnonymous:  true,
			want:            false,
		},
		{
			name:            "explicit anonymous accepts empty client token even when token is configured",
			providedToken:   "",
			configuredToken: "server-secret",
			allowAnonymous:  true,
			want:            true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isAuthTokenAccepted(tt.providedToken, tt.configuredToken, tt.allowAnonymous)
			if got != tt.want {
				t.Fatalf("isAuthTokenAccepted() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConnectionRejectsRegistrationWhenServerTokenMissing(t *testing.T) {
	_, errResp, err := runRegisterRequest(t, "", false, protocol.RegisterRequest{
		Token:      "",
		TunnelType: protocol.TunnelTypeHTTP,
		LocalPort:  3000,
	})

	if err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("Handle() error = %v, want authentication failure", err)
	}
	if errResp.Code != authFailureCode {
		t.Fatalf("error code = %q, want %q", errResp.Code, authFailureCode)
	}
	if errResp.Message != authFailureMessage {
		t.Fatalf("error message = %q, want %q", errResp.Message, authFailureMessage)
	}
}

func TestConnectionAllowsExplicitAnonymousRegistration(t *testing.T) {
	resp, _, err := runRegisterRequest(t, "", true, protocol.RegisterRequest{
		Token:           "",
		CustomSubdomain: "anonymous-auth-test",
		TunnelType:      protocol.TunnelTypeHTTP,
		LocalPort:       3000,
	})

	if err != nil && strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("Handle() returned authentication failure for explicit anonymous registration: %v", err)
	}
	if resp.Subdomain != "anonymous-auth-test" {
		t.Fatalf("subdomain = %q, want anonymous-auth-test", resp.Subdomain)
	}
}

func TestConnectionRejectsRegistrationWithWrongToken(t *testing.T) {
	_, errResp, err := runRegisterRequest(t, "server-secret", false, protocol.RegisterRequest{
		Token:      "wrong-secret",
		TunnelType: protocol.TunnelTypeHTTP,
		LocalPort:  3000,
	})

	if err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("Handle() error = %v, want authentication failure", err)
	}
	if errResp.Code != authFailureCode {
		t.Fatalf("error code = %q, want %q", errResp.Code, authFailureCode)
	}
	if errResp.Message != authFailureMessage {
		t.Fatalf("error message = %q, want %q", errResp.Message, authFailureMessage)
	}
}

func TestDataConnectionRejectsEmptyTokenWhenServerTokenMissing(t *testing.T) {
	resp, err := runDataConnectRequest(t, "", false, "", protocol.DataConnectRequest{
		TunnelID:     "missing-token-tunnel",
		Token:        "",
		ConnectionID: "data-1",
	})

	if err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("Handle() error = %v, want authentication failure", err)
	}
	if resp.Accepted {
		t.Fatal("data connection was accepted without server token or explicit anonymous mode")
	}
	if !strings.Contains(resp.Message, authFailureCode) {
		t.Fatalf("response message = %q, want auth failure code", resp.Message)
	}
	if !strings.Contains(resp.Message, authFailureMessage) {
		t.Fatalf("response message = %q, want generic auth failure message", resp.Message)
	}
}

func TestDataConnectionRejectsWrongToken(t *testing.T) {
	resp, err := runDataConnectRequest(t, "server-secret", false, "server-secret", protocol.DataConnectRequest{
		TunnelID:     "wrong-token-tunnel",
		Token:        "wrong-secret",
		ConnectionID: "data-1",
	})

	if err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("Handle() error = %v, want authentication failure", err)
	}
	if resp.Accepted {
		t.Fatal("data connection was accepted with wrong token")
	}
	if !strings.Contains(resp.Message, authFailureCode) {
		t.Fatalf("response message = %q, want auth failure code", resp.Message)
	}
}

func runRegisterRequest(t *testing.T, serverToken string, allowAnonymous bool, req protocol.RegisterRequest) (protocol.RegisterResponse, protocol.ErrorMessage, error) {
	t.Helper()

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	logger := zap.NewNop()
	manager := tunnel.NewManager(logger)
	portAlloc, err := NewPortAllocator(20000, 20100)
	if err != nil {
		t.Fatalf("NewPortAllocator() failed: %v", err)
	}
	groupManager := NewConnectionGroupManager(logger)
	t.Cleanup(groupManager.Close)

	conn := NewConnection(ConnectionConfig{
		Conn:           serverConn,
		AuthToken:      serverToken,
		AllowAnonymous: allowAnonymous,
		Manager:        manager,
		Logger:         logger,
		PortAlloc:      portAlloc,
		Domain:         "example.com",
		TunnelDomain:   "example.com",
		PublicPort:     443,
		GroupManager:   groupManager,
	})
	conn.SetAllowedTransports([]string{"tcp"})
	conn.SetAllowedTunnelTypes([]string{"http", "https", "tcp"})

	errCh := make(chan error, 1)
	go func() {
		errCh <- conn.Handle()
	}()

	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("json.Marshal() failed: %v", err)
	}
	if err := protocol.WriteFrame(clientConn, protocol.NewFrame(protocol.FrameTypeRegister, payload)); err != nil {
		t.Fatalf("WriteFrame() failed: %v", err)
	}

	frame := readFrameWithDeadline(t, clientConn)
	defer frame.Release()

	switch frame.Type {
	case protocol.FrameTypeError:
		var errMsg protocol.ErrorMessage
		if err := json.Unmarshal(frame.Payload, &errMsg); err != nil {
			t.Fatalf("failed to unmarshal error response: %v", err)
		}
		return protocol.RegisterResponse{}, errMsg, waitForHandler(t, errCh)
	case protocol.FrameTypeRegisterAck:
		var registerResp protocol.RegisterResponse
		if err := json.Unmarshal(frame.Payload, &registerResp); err != nil {
			t.Fatalf("failed to unmarshal register response: %v", err)
		}
		_ = clientConn.Close()
		err := waitForHandler(t, errCh)
		return registerResp, protocol.ErrorMessage{}, err
	default:
		t.Fatalf("response frame type = %s, want error or register ack", frame.Type)
		return protocol.RegisterResponse{}, protocol.ErrorMessage{}, nil
	}
}

func runDataConnectRequest(t *testing.T, serverToken string, allowAnonymous bool, groupToken string, req protocol.DataConnectRequest) (protocol.DataConnectResponse, error) {
	t.Helper()

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	logger := zap.NewNop()
	groupManager := NewConnectionGroupManager(logger)
	t.Cleanup(groupManager.Close)
	if groupToken != "" || allowAnonymous {
		groupManager.CreateGroup(req.TunnelID, groupToken, nil, protocol.TunnelTypeHTTP)
	}

	handler := NewDataConnectionHandler(
		serverConn,
		bufio.NewReader(serverConn),
		serverToken,
		allowAnonymous,
		groupManager,
		make(chan struct{}),
		logger,
	)

	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("json.Marshal() failed: %v", err)
	}
	frame := protocol.NewFrame(protocol.FrameTypeDataConnect, payload)

	errCh := make(chan error, 1)
	go func() {
		errCh <- handler.Handle(frame)
	}()

	ack := readFrameWithDeadline(t, clientConn)
	defer ack.Release()
	if ack.Type != protocol.FrameTypeDataConnectAck {
		t.Fatalf("ack frame type = %s, want %s", ack.Type, protocol.FrameTypeDataConnectAck)
	}

	var resp protocol.DataConnectResponse
	if err := json.Unmarshal(ack.Payload, &resp); err != nil {
		t.Fatalf("failed to unmarshal data connect response: %v", err)
	}

	return resp, waitForHandler(t, errCh)
}

func readFrameWithDeadline(t *testing.T, conn net.Conn) *protocol.Frame {
	t.Helper()

	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline() failed: %v", err)
	}
	frame, err := protocol.ReadFrame(conn)
	if err != nil {
		t.Fatalf("ReadFrame() failed: %v", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	return frame
}

func waitForHandler(t *testing.T, errCh <-chan error) error {
	t.Helper()

	select {
	case err := <-errCh:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return")
		return nil
	}
}
