package tcp

import (
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

func TestOpenStreamAttemptExhaustsDrainingSessions(t *testing.T) {
	group := unavailableSessionGroup(t, 16)
	conn, err := group.openStreamAttempt()
	if conn != nil {
		conn.Close()
		t.Fatal("opened a draining session")
	}
	if !errors.Is(err, yamux.ErrRemoteGoAway) {
		t.Fatalf("open error = %v", err)
	}
	if group.SessionCount() != 16 {
		t.Fatal("draining sessions were closed while they may still have streams")
	}
}

func TestOpenStreamFallbackPrefersDataBeforePrimary(t *testing.T) {
	group := unavailableSessionGroup(t, 2)
	data, dataPeer := newYamuxSessionPair(t)
	primary, primaryPeer := newYamuxSessionPair(t)
	t.Cleanup(func() { data.Close(); dataPeer.Close(); primary.Close(); primaryPeer.Close() })
	group.Sessions["healthy"] = data
	group.Sessions[primarySessionID] = primary
	// Give the healthy data session more streams than the failed candidates.
	held, err := data.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	heldPeer, err := dataPeer.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer heldPeer.Close()
	for _, peer := range []struct {
		session *yamux.Session
		label   string
	}{{dataPeer, "data"}, {primaryPeer, "main"}} {
		go func() {
			stream, err := peer.session.Accept()
			if err != nil {
				return
			}
			defer stream.Close()
			_, _ = io.WriteString(stream, peer.label)
		}()
	}
	stream, err := group.openStreamAttempt()
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_ = stream.SetReadDeadline(time.Now().Add(time.Second))
	label := make([]byte, 4)
	if _, err := io.ReadFull(stream, label); err != nil {
		t.Fatal(err)
	}
	if string(label) != "data" {
		t.Fatalf("fallback chose %q before a healthy data session", label)
	}
}

func unavailableSessionGroup(b testing.TB, count int) *ConnectionGroup {
	b.Helper()
	group := &ConnectionGroup{Sessions: make(map[string]*yamux.Session), stopCh: make(chan struct{})}
	b.Cleanup(group.Close)
	for i := 0; i < count; i++ {
		left, right := net.Pipe()
		cfg := yamux.DefaultConfig()
		cfg.EnableKeepAlive = false
		cfg.LogOutput = io.Discard
		local, err := yamux.Client(left, cfg)
		if err != nil {
			b.Fatal(err)
		}
		peer, err := yamux.Server(right, cfg)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = peer.Close() })
		group.Sessions[fmt.Sprintf("data-%d", i)] = local
		if err := peer.GoAway(); err != nil {
			b.Fatal(err)
		}
		if _, err := peer.Ping(); err != nil {
			b.Fatal(err)
		}
	}
	return group
}

// One retry round, excluding the fixed network-retry backoff.
func benchmarkUnavailableAttempt(group *ConnectionGroup) {
	conn, err := group.openStreamAttempt()
	if err == nil {
		_ = conn.Close()
	}
}

func BenchmarkConnectionGroupUnavailableAttempt(b *testing.B) {
	for _, count := range []int{8, 64, 256} {
		b.Run(fmt.Sprintf("sessions_%d", count), func(b *testing.B) {
			group := unavailableSessionGroup(b, count)
			b.ReportAllocs()
			for b.Loop() {
				benchmarkUnavailableAttempt(group)
			}
		})
	}
}
