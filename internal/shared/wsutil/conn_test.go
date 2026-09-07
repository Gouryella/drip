package wsutil

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type blockedWriteConn struct {
	net.Conn
	block     atomic.Bool
	entered   chan struct{}
	closed    chan struct{}
	once      sync.Once
	writeOnce sync.Once
}

func (c *blockedWriteConn) Write(p []byte) (int, error) {
	if c.block.Load() {
		c.writeOnce.Do(func() { close(c.entered) })
		<-c.closed
		return 0, net.ErrClosed
	}
	return c.Conn.Write(p)
}

func (c *blockedWriteConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func TestCloseInterruptsBlockedWrite(t *testing.T) {
	testBlockedWrite(t, false)
}

func TestWriteDeadlineInterruptsBlockedWrite(t *testing.T) {
	testBlockedWrite(t, true)
}

func testBlockedWrite(t *testing.T, useDeadline bool) {
	peers := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err == nil {
			peers <- ws
		}
	}))
	defer server.Close()
	var underlying *blockedWriteConn
	dialer := websocket.Dialer{NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		underlying = &blockedWriteConn{Conn: conn, entered: make(chan struct{}), closed: make(chan struct{})}
		return underlying, nil
	}}
	ws, _, err := dialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	peer := <-peers
	defer peer.Close()
	defer underlying.Close()
	conn := NewConn(ws)
	underlying.block.Store(true)
	writeDone := make(chan struct{})
	go func() { _, _ = conn.Write([]byte("blocked")); close(writeDone) }()
	<-underlying.entered
	closeDone := make(chan struct{})
	go func() {
		if useDeadline {
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Millisecond))
		} else {
			_ = conn.Close()
		}
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(200 * time.Millisecond):
		_ = underlying.Close()
		<-closeDone
		t.Error("Close waited for a blocked writer")
	}
	select {
	case <-writeDone:
	case <-time.After(time.Second):
		t.Fatal("writer did not stop after Close")
	}
}
