package tcp

import (
	"net"
	"testing"
	"time"

	"drip/internal/shared/protocol"
	"go.uber.org/zap"
)

func TestConnectionCloseAfterHTTPHandoffCleansLifecycleButPreservesConn(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()

	tracked := &closeTrackingConn{Conn: server}
	conn := NewConnection(ConnectionConfig{
		Conn:   tracked,
		Logger: zap.NewNop(),
	})

	baseline := protocol.GetActiveConnections()
	protocol.RegisterConnection()

	conn.mu.Lock()
	conn.handedOff = true
	conn.mu.Unlock()

	conn.Close()

	if got, want := protocol.GetActiveConnections(), baseline; got != want {
		t.Fatalf("active protocol connections = %d, want %d", got, want)
	}

	select {
	case <-conn.stopCh:
	case <-time.After(time.Second):
		t.Fatal("stopCh was not closed")
	}

	select {
	case <-conn.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("context was not canceled")
	}

	if got := tracked.deadlineCount.Load(); got != 0 {
		t.Fatalf("SetDeadline called %d times on handed-off conn, want 0", got)
	}
	if got := tracked.closeCount.Load(); got != 0 {
		t.Fatalf("Close called %d times on handed-off conn, want 0", got)
	}

	_ = tracked.Close()
}
