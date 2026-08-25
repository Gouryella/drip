package cli

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIsFatalTunnelError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		err           error
		connectedOnce bool
		want          bool
	}{
		{name: "network failure retries", err: errors.New("dial tcp: connection refused")},
		{name: "bad token is fatal", err: errors.New("Invalid authentication token"), want: true},
		{name: "reserved subdomain is fatal", err: errors.New("subdomain is reserved"), want: true},
		{name: "transport mismatch is fatal", err: errors.New("server only supports wss"), want: true},
		{name: "taken subdomain is fatal on first connect", err: errors.New("subdomain is already taken"), want: true},
		{
			name:          "taken subdomain retries after the tunnel owned it",
			err:           errors.New("subdomain is already taken"),
			connectedOnce: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := isFatalTunnelError(tt.err, tt.connectedOnce); got != tt.want {
				t.Fatalf("isFatalTunnelError(%v, %v) = %v, want %v", tt.err, tt.connectedOnce, got, tt.want)
			}
		})
	}
}

func TestNextBackoffDoublesUpToTheCap(t *testing.T) {
	t.Parallel()

	backoff := superviseMinBackoff
	for i := 0; i < 20; i++ {
		next := nextBackoff(backoff)
		if next < backoff {
			t.Fatalf("nextBackoff(%s) = %s, want a value that does not shrink", backoff, next)
		}
		if next > superviseMaxBackoff {
			t.Fatalf("nextBackoff(%s) = %s, want at most %s", backoff, next, superviseMaxBackoff)
		}
		backoff = next
	}

	if backoff != superviseMaxBackoff {
		t.Fatalf("backoff settled at %s, want the cap %s", backoff, superviseMaxBackoff)
	}
}

func TestJitterBackoffStaysWithinTwentyPercent(t *testing.T) {
	t.Parallel()

	const backoff = 10 * time.Second
	low := backoff - backoff/5
	high := backoff + backoff/5

	for i := 0; i < 100; i++ {
		got := jitterBackoff(backoff)
		if got < low || got > high {
			t.Fatalf("jitterBackoff(%s) = %s, want between %s and %s", backoff, got, low, high)
		}
	}
}

func TestSleepContextReportsCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if sleepContext(ctx, time.Hour) {
		t.Fatal("sleepContext() = true for a cancelled context, want false")
	}

	if !sleepContext(context.Background(), time.Millisecond) {
		t.Fatal("sleepContext() = false after the timer fired, want true")
	}
}

func TestSuperviseTunnelsRejectsAnEmptyTunnelList(t *testing.T) {
	t.Parallel()

	if err := superviseTunnels(context.Background(), nil, nil, nil); err == nil {
		t.Fatal("superviseTunnels() expected an error with no tunnels")
	}
}
