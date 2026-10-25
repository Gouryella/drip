package mux

import (
	"net"
	"testing"
	"time"
)

func TestStreamCloseInterruptsReadWithoutClosingPeer(t *testing.T) {
	left, right := net.Pipe()
	client, err := Client(left, NewClientConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := Server(right, NewServerConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	stream, err := OpenStream(client)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := server.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	done := make(chan error, 1)
	go func() { _, err := stream.Read(make([]byte, 1)); done <- err }()
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("read succeeded on closed stream")
		}
	case <-time.After(time.Second):
		t.Fatal("read remained blocked after Close")
	}
	if client.IsClosed() || server.IsClosed() {
		t.Fatal("closing one stream closed the shared session")
	}
}
