package tcp

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestHTTPRequestHandlerLogsQueryKeysNotRawQuery(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = clientConn.Close()
	})

	core, observed := observer.New(zap.DebugLevel)
	logger := zap.New(core)

	done := make(chan error, 1)
	handedOff := false
	var mu sync.RWMutex
	go func() {
		h := NewHTTPRequestHandler(
			serverConn,
			bufio.NewReader(serverConn),
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}),
			nil,
			context.Background(),
			logger,
			&mu,
			&handedOff,
		)
		done <- h.Handle()
	}()

	request := "GET /private?token=secret-token&visible=plain HTTP/1.1\r\n" +
		"Host: example.test\r\n" +
		"Connection: close\r\n" +
		"\r\n"
	if _, err := clientConn.Write([]byte(request)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	if err := clientConn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 256)
	if _, err := clientConn.Read(buf); err != nil {
		t.Fatalf("read response: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("handler did not finish")
	}

	entries := observed.FilterMessage("Processing HTTP request on TCP port").All()
	if len(entries) != 1 {
		t.Fatalf("got %d processing log entries, want 1", len(entries))
	}

	fields := entries[0].ContextMap()
	if _, ok := fields["url"]; ok {
		t.Fatalf("log still contains raw url field: %#v", fields)
	}
	if got := fields["path"]; got != "/private" {
		t.Fatalf("path field = %v, want /private", got)
	}

	rendered := fmt.Sprint(fields)
	if strings.Contains(rendered, "secret-token") ||
		strings.Contains(rendered, "visible=plain") ||
		strings.Contains(rendered, "token=secret-token") {
		t.Fatalf("request log leaked raw query value: %s", rendered)
	}
}
