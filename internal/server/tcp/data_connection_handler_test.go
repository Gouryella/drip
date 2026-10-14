package tcp

import (
	"bufio"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	json "github.com/goccy/go-json"
	"github.com/hashicorp/yamux"
	"go.uber.org/zap"

	"drip/internal/shared/mux"
	"drip/internal/shared/protocol"
)

func TestConnectionGroupManagerEffectiveMaxDataConns(t *testing.T) {
	t.Parallel()

	manager := NewConnectionGroupManagerWithMaxDataConns(zap.NewNop(), 3)
	t.Cleanup(manager.Close)

	tests := []struct {
		name      string
		clientMax int
		want      int
	}{
		{name: "server cap wins", clientMax: 10, want: 3},
		{name: "client cap wins", clientMax: 2, want: 2},
		{name: "client disables data conns", clientMax: 0, want: 0},
		{name: "negative client disables data conns", clientMax: -1, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := manager.EffectiveMaxDataConns(tt.clientMax); got != tt.want {
				t.Fatalf("EffectiveMaxDataConns(%d) = %d, want %d", tt.clientMax, got, tt.want)
			}
		})
	}
}

func TestDataConnectionHandlerRejectsWhenDataSessionLimitExceeded(t *testing.T) {
	t.Parallel()

	manager, group := newTestConnectionGroup(t, 1)
	serverSession, clientSession := newYamuxSessionPair(t)
	t.Cleanup(func() {
		_ = serverSession.Close()
		_ = clientSession.Close()
	})
	group.AddSession("data-existing", serverSession)

	clientConn, resp, errCh := beginDataConnect(t, manager, protocol.DataConnectRequest{
		TunnelID:     group.TunnelID,
		Token:        "token",
		ConnectionID: "data-over-limit",
	})
	defer clientConn.Close()

	if resp.Accepted {
		t.Fatalf("data connection was accepted over limit: %+v", resp)
	}
	if !strings.Contains(resp.Message, "connection_limit_exceeded") {
		t.Fatalf("reject message = %q, want connection_limit_exceeded", resp.Message)
	}
	if got := group.DataSessionCount(); got != 1 {
		t.Fatalf("DataSessionCount() = %d, want existing session only", got)
	}
	if got := group.PendingDataSessionCount(); got != 0 {
		t.Fatalf("PendingDataSessionCount() = %d, want 0", got)
	}

	err := waitForDataHandler(t, errCh)
	if err == nil || !strings.Contains(err.Error(), "data connection limit exceeded") {
		t.Fatalf("handler error = %v, want data connection limit exceeded", err)
	}
}

func TestDataConnectionHandlerRejectsDuplicateConnectionID(t *testing.T) {
	t.Parallel()

	manager, group := newTestConnectionGroup(t, 2)
	serverSession, clientSession := newYamuxSessionPair(t)
	t.Cleanup(func() {
		_ = serverSession.Close()
		_ = clientSession.Close()
	})
	group.AddSession("data-1", serverSession)

	clientConn, resp, errCh := beginDataConnect(t, manager, protocol.DataConnectRequest{
		TunnelID:     group.TunnelID,
		Token:        "token",
		ConnectionID: "data-1",
	})
	defer clientConn.Close()

	if resp.Accepted {
		t.Fatalf("duplicate data connection was accepted: %+v", resp)
	}
	if !strings.Contains(resp.Message, "duplicate_connection_id") {
		t.Fatalf("reject message = %q, want duplicate_connection_id", resp.Message)
	}
	if serverSession.IsClosed() {
		t.Fatal("existing session was closed by rejected duplicate")
	}

	err := waitForDataHandler(t, errCh)
	if err == nil || !strings.Contains(err.Error(), "duplicate data connection id") {
		t.Fatalf("handler error = %v, want duplicate data connection id", err)
	}
}

func TestDataConnectionHandlerRejectsReservedConnectionIDs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		id   string
	}{
		{name: "empty", id: ""},
		{name: "primary", id: primarySessionID},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			manager, group := newTestConnectionGroup(t, 2)
			clientConn, resp, errCh := beginDataConnect(t, manager, protocol.DataConnectRequest{
				TunnelID:     group.TunnelID,
				Token:        "token",
				ConnectionID: tt.id,
			})
			defer clientConn.Close()

			if resp.Accepted {
				t.Fatalf("reserved data connection id %q was accepted: %+v", tt.id, resp)
			}
			if !strings.Contains(resp.Message, "invalid_connection_id") {
				t.Fatalf("reject message = %q, want invalid_connection_id", resp.Message)
			}
			if got := group.SessionCount(); got != 0 {
				t.Fatalf("SessionCount() = %d, want 0", got)
			}
			if got := group.PendingDataSessionCount(); got != 0 {
				t.Fatalf("PendingDataSessionCount() = %d, want 0", got)
			}

			err := waitForDataHandler(t, errCh)
			if err == nil || !strings.Contains(err.Error(), "connection id") {
				t.Fatalf("handler error = %v, want connection id validation error", err)
			}
		})
	}
}

func TestDataConnectionHandlerAddsAndRemovesDataSession(t *testing.T) {
	t.Parallel()

	manager, group := newTestConnectionGroup(t, 2)
	clientConn, resp, errCh := beginDataConnect(t, manager, protocol.DataConnectRequest{
		TunnelID:     group.TunnelID,
		Token:        "token",
		ConnectionID: "data-1",
	})
	defer clientConn.Close()

	if !resp.Accepted {
		t.Fatalf("data connection rejected: %+v", resp)
	}

	clientSession, err := yamux.Server(clientConn, mux.NewClientConfig())
	if err != nil {
		t.Fatalf("yamux.Server failed: %v", err)
	}

	waitFor(t, func() bool {
		return group.DataSessionCount() == 1 && group.PendingDataSessionCount() == 0
	}, "data session to register")

	if got := group.SessionCount(); got != 1 {
		t.Fatalf("SessionCount() = %d, want 1", got)
	}

	_ = clientSession.Close()
	err = waitForDataHandler(t, errCh)
	if err != nil {
		t.Fatalf("handler returned error after session close: %v", err)
	}

	waitFor(t, func() bool {
		return group.DataSessionCount() == 0 && group.SessionCount() == 0
	}, "data session to be removed")
}

func TestConnectionGroupCloseClearsSessionsAndReservations(t *testing.T) {
	t.Parallel()

	_, group := newTestConnectionGroup(t, 2)
	serverSession, clientSession := newYamuxSessionPair(t)
	t.Cleanup(func() {
		_ = clientSession.Close()
	})

	group.AddSession(primarySessionID, serverSession)
	if err := group.ReserveDataSession("data-pending"); err != nil {
		t.Fatalf("ReserveDataSession failed: %v", err)
	}

	group.Close()

	if got := group.SessionCount(); got != 0 {
		t.Fatalf("SessionCount() = %d, want 0", got)
	}
	if got := group.PendingDataSessionCount(); got != 0 {
		t.Fatalf("PendingDataSessionCount() = %d, want 0", got)
	}
	if !serverSession.IsClosed() {
		t.Fatal("Close did not close registered session")
	}
	if err := group.ReserveDataSession("data-after-close"); !errors.Is(err, ErrConnectionGroupClosed) {
		t.Fatalf("ReserveDataSession after Close error = %v, want ErrConnectionGroupClosed", err)
	}
}

func newTestConnectionGroup(t *testing.T, maxDataConns int) (*ConnectionGroupManager, *ConnectionGroup) {
	t.Helper()

	manager := NewConnectionGroupManagerWithMaxDataConns(zap.NewNop(), maxDataConns)
	group := manager.CreateGroupWithMaxDataConns("subdomain", "token", nil, protocol.TunnelTypeTCP, manager.MaxDataConns())
	t.Cleanup(manager.Close)
	return manager, group
}

func beginDataConnect(t *testing.T, manager *ConnectionGroupManager, req protocol.DataConnectRequest) (net.Conn, protocol.DataConnectResponse, <-chan error) {
	t.Helper()

	serverConn, clientConn := net.Pipe()
	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}
	frame := protocol.NewFrame(protocol.FrameTypeDataConnect, payload)

	handler := NewDataConnectionHandler(
		serverConn,
		bufio.NewReader(serverConn),
		"token",
		false,
		manager,
		make(chan struct{}),
		zap.NewNop(),
	)
	errCh := make(chan error, 1)
	go func() {
		errCh <- handler.Handle(frame)
	}()

	if err := clientConn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("SetReadDeadline failed: %v", err)
	}
	ack, err := protocol.ReadFrame(clientConn)
	if err != nil {
		t.Fatalf("ReadFrame ack failed: %v", err)
	}
	defer ack.Release()
	_ = clientConn.SetReadDeadline(time.Time{})
	if ack.Type != protocol.FrameTypeDataConnectAck {
		t.Fatalf("ack type = %s, want %s", ack.Type, protocol.FrameTypeDataConnectAck)
	}

	var resp protocol.DataConnectResponse
	if err := json.Unmarshal(ack.Payload, &resp); err != nil {
		t.Fatalf("json.Unmarshal ack failed: %v", err)
	}

	return clientConn, resp, errCh
}

func newYamuxSessionPair(t *testing.T) (*yamux.Session, *yamux.Session) {
	t.Helper()

	serverConn, clientConn := net.Pipe()
	serverSession, err := yamux.Client(serverConn, mux.NewServerConfig())
	if err != nil {
		t.Fatalf("yamux.Client failed: %v", err)
	}
	clientSession, err := yamux.Server(clientConn, mux.NewClientConfig())
	if err != nil {
		t.Fatalf("yamux.Server failed: %v", err)
	}
	return serverSession, clientSession
}

func waitForDataHandler(t *testing.T, errCh <-chan error) error {
	t.Helper()

	select {
	case err := <-errCh:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return")
	}
	return nil
}

func waitFor(t *testing.T, condition func() bool, label string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", label)
}
