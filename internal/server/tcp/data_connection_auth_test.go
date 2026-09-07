package tcp

import (
	"bufio"
	"net"
	"testing"

	"drip/internal/shared/protocol"
	"go.uber.org/zap"
)

func TestAnonymousRegistrationDoesNotAuthorizeJoiningPrivateGroup(t *testing.T) {
	manager := NewConnectionGroupManager(zap.NewNop())
	defer manager.Close()
	group := manager.CreateGroup("private", "server-token", nil, protocol.TunnelTypeHTTP)
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	handler := NewDataConnectionHandler(server, bufio.NewReader(server), "server-token", true, manager, make(chan struct{}), zap.NewNop())
	payload, err := protocol.MarshalJSON(protocol.DataConnectRequest{TunnelID: group.TunnelID, ConnectionID: "anonymous", Token: ""})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- handler.Handle(protocol.NewFrame(protocol.FrameTypeDataConnect, payload)) }()
	ack := readFrameWithDeadline(t, client)
	defer ack.Release()
	var response protocol.DataConnectResponse
	if err := protocol.UnmarshalJSON(ack.Payload, &response); err != nil {
		t.Fatal(err)
	}
	if response.Accepted {
		t.Fatal("anonymous request joined an authenticated connection group")
	}
	if err := waitForDataHandler(t, done); err == nil {
		t.Fatal("missing authentication error")
	}
}
