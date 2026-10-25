package tcp

import (
	"net"
	"testing"
	"time"

	"drip/internal/shared/protocol"
	"go.uber.org/zap"
)

func TestCloseInterruptsPrimaryRegistration(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	c := NewPoolClient(&ConnectorConfig{ServerAddr: "localhost:443", LocalPort: 3000, TunnelType: protocol.TunnelTypeHTTP}, zap.NewNop())
	c.dialer = &fakeDataSessionDialer{conn: client}
	defer c.Close()
	done := make(chan error, 1)
	go func() { done <- c.Connect() }()
	_ = server.SetReadDeadline(time.Now().Add(time.Second))
	frame, err := protocol.ReadFrame(server)
	if err != nil {
		t.Fatal(err)
	}
	frame.Release()
	_ = c.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Connect succeeded after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("Connect did not stop while waiting for registration ACK")
	}
	waitDone := make(chan struct{})
	go func() { c.Wait(); close(waitDone) }()
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("Wait did not finish after registration failure")
	}
}

func TestWaitAfterClosingUnconnectedClient(t *testing.T) {
	c := NewPoolClient(&ConnectorConfig{ServerAddr: "localhost:443", LocalPort: 3000}, zap.NewNop())
	_ = c.Close()
	done := make(chan struct{})
	go func() { c.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Wait blocked after closing an unconnected client")
	}
	if err := c.Connect(); err == nil {
		t.Fatal("Connect succeeded on a closed client")
	}
}
