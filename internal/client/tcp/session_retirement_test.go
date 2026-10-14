package tcp

import (
	"net"
	"testing"
	"time"

	"drip/internal/shared/mux"
	"github.com/hashicorp/yamux"
	"go.uber.org/zap"
)

func retirementClient(t *testing.T) (*PoolClient, *yamux.Session, *yamux.Session) {
	t.Helper()
	left, right := net.Pipe()
	local, err := yamux.Server(left, mux.NewClientConfig())
	if err != nil {
		t.Fatal(err)
	}
	peer, err := yamux.Client(right, mux.NewServerConfig())
	if err != nil {
		t.Fatal(err)
	}
	c := NewPoolClient(&ConnectorConfig{ServerAddr: "localhost:443", LocalPort: 3000}, zap.NewNop())
	c.dataSessions["idle"] = &sessionHandle{id: "idle", conn: left, session: local}
	t.Cleanup(func() { _ = c.Close(); _ = peer.Close() })
	return c, local, peer
}

func TestSessionRetirementPreservesStreamBeforeHandlerStarts(t *testing.T) {
	c, local, peer := retirementClient(t)
	out, err := peer.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	in, err := local.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if c.retireDataSession("idle") {
		t.Fatal("retired a session with an uncounted pending handler")
	}
	if local.IsClosed() {
		t.Fatal("active session was closed")
	}
}

func TestIdleSessionRetiresAfterGoAway(t *testing.T) {
	c, local, _ := retirementClient(t)
	if !c.retireDataSession("idle") {
		t.Fatal("idle session did not start draining")
	}
	select {
	case <-local.CloseChan():
	case <-time.After(time.Second):
		t.Fatal("idle session did not close")
	}
}

func TestIdleSessionBatchSkipsBusyOldestCandidate(t *testing.T) {
	c, local, peer := retirementClient(t)
	busy, err := peer.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	busyLocal, err := local.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer busyLocal.Close()
	c.dataSessions["idle"].lastActive.Store(1)
	var idleSessions []*yamux.Session
	for _, id := range []string{"older", "newer"} {
		left, right := net.Pipe()
		session, err := yamux.Server(left, mux.NewClientConfig())
		if err != nil {
			t.Fatal(err)
		}
		remote, err := yamux.Client(right, mux.NewServerConfig())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = remote.Close() })
		h := &sessionHandle{id: id, conn: left, session: session}
		h.lastActive.Store(int64(len(idleSessions) + 2))
		c.dataSessions[id] = h
		idleSessions = append(idleSessions, session)
	}
	if got := c.removeIdleSessions(2); got != 2 {
		t.Fatalf("retiring %d sessions, want 2", got)
	}
	if local.IsClosed() {
		t.Fatal("busy oldest session was closed")
	}
	for _, session := range idleSessions {
		select {
		case <-session.CloseChan():
		case <-time.After(time.Second):
			t.Fatal("idle session did not retire")
		}
	}
}
