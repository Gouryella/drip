package netutil

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func tcpConnPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client, err := net.DialTCP("tcp", nil, ln.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	server, err := ln.AcceptTCP()
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); server.Close() })
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	_ = server.SetDeadline(time.Now().Add(3 * time.Second))
	return client, server
}

func TestPipePreservesResponseAfterHalfClose(t *testing.T) {
	for _, counted := range []bool{false, true} {
		name := "tcp"
		if counted {
			name = "counting_conn"
		}
		t.Run(name, func(t *testing.T) {
			client, a := tcpConnPair(t)
			b, backend := tcpConnPair(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var left, right net.Conn = a, b
			if counted {
				left = NewCountingConn(a, nil, nil)
				right = NewCountingConn(b, nil, nil)
			}
			pipeDone := make(chan error, 1)
			go func() { pipeDone <- Pipe(ctx, left, right) }()
			response := bytes.Repeat([]byte("response"), 8192)
			backendDone := make(chan error, 1)
			go func() {
				_, err := io.ReadAll(backend)
				if err == nil {
					_, err = backend.Write(response)
				}
				_ = backend.CloseWrite()
				backendDone <- err
			}()
			if _, err := client.Write([]byte("request")); err != nil {
				t.Fatal(err)
			}
			if err := client.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(client)
			if err != nil || !bytes.Equal(got, response) {
				t.Errorf("response after CloseWrite: %d bytes, err %v; want %d bytes", len(got), err, len(response))
			}
			cancel()
			select {
			case <-pipeDone:
			case <-time.After(time.Second):
				t.Fatal("Pipe did not finish")
			}
			<-backendDone
		})
	}
}
