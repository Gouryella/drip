package tcp

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	json "github.com/goccy/go-json"
	"go.uber.org/zap"

	"drip/internal/shared/protocol"
)

type fakeDataSessionDialer struct {
	conn net.Conn
}

func (d *fakeDataSessionDialer) DialContext(context.Context) (net.Conn, error) {
	return d.conn, nil
}

type blockingClearReadDeadlineConn struct {
	net.Conn
	clearStarted chan struct{}
	unblockClear chan struct{}
	clearOnce    sync.Once
	closed       atomic.Bool
}

func (c *blockingClearReadDeadlineConn) SetReadDeadline(deadline time.Time) error {
	if deadline.IsZero() {
		c.clearOnce.Do(func() { close(c.clearStarted) })
		<-c.unblockClear
	}
	return c.Conn.SetReadDeadline(deadline)
}

func (c *blockingClearReadDeadlineConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

func TestAddDataSessionDoesNotRegisterAfterClose(t *testing.T) {
	serverSide, rawClientSide := net.Pipe()
	defer serverSide.Close()

	clientSide := &blockingClearReadDeadlineConn{
		Conn:         rawClientSide,
		clearStarted: make(chan struct{}),
		unblockClear: make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	client := &PoolClient{
		token:        "token",
		tunnelID:     "tunnel-id",
		ctx:          ctx,
		cancel:       cancel,
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
		dataSessions: make(map[string]*sessionHandle),
		logger:       zap.NewNop(),
		dialer:       &fakeDataSessionDialer{conn: clientSide},
	}

	serverErr := make(chan error, 1)
	go func() {
		frame, err := protocol.ReadFrame(serverSide)
		if err != nil {
			serverErr <- err
			return
		}
		defer frame.Release()

		if frame.Type != protocol.FrameTypeDataConnect {
			serverErr <- errors.New("expected data connect frame")
			return
		}

		payload, err := json.Marshal(protocol.DataConnectResponse{Accepted: true})
		if err != nil {
			serverErr <- err
			return
		}
		serverErr <- protocol.WriteFrame(serverSide, protocol.NewFrame(protocol.FrameTypeDataConnectAck, payload))
	}()

	result := make(chan error, 1)
	go func() {
		result <- client.addDataSession()
	}()

	select {
	case <-clientSide.clearStarted:
	case <-time.After(time.Second):
		t.Fatalf("addDataSession did not reach post-handshake deadline reset")
	}

	if err := client.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	close(clientSide.unblockClear)

	select {
	case err := <-result:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("addDataSession() error = %v, want net.ErrClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("addDataSession did not return after client Close")
	}

	if !clientSide.closed.Load() {
		t.Fatalf("data connection was not closed after client Close")
	}

	client.mu.RLock()
	sessionCount := len(client.dataSessions)
	client.mu.RUnlock()
	if sessionCount != 0 {
		t.Fatalf("dataSessions length = %d, want 0", sessionCount)
	}

	select {
	case err := <-serverErr:
		if err != nil {
			t.Fatalf("server handshake error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("server handshake goroutine did not finish")
	}
}
