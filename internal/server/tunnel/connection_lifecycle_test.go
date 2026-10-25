package tunnel

import (
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestConnectionSendCloseConcurrentDoesNotPanic(t *testing.T) {
	t.Parallel()

	for iteration := 0; iteration < 100; iteration++ {
		conn := NewConnection("test-subdomain", nil, zap.NewNop())

		for i := 0; i < cap(conn.SendCh); i++ {
			conn.SendCh <- []byte("queued")
		}

		var wg sync.WaitGroup
		wg.Add(1)
		errCh := make(chan error, 1)
		go func() {
			defer wg.Done()
			errCh <- conn.Send([]byte("blocked"))
		}()

		time.Sleep(time.Microsecond)
		conn.Close()
		wg.Wait()

		if err := <-errCh; !errors.Is(err, ErrConnectionClosed) {
			t.Fatalf("Send() error = %v, want %v", err, ErrConnectionClosed)
		}
	}
}

func TestConnectionSendAfterCloseReturnsClosed(t *testing.T) {
	t.Parallel()

	conn := NewConnection("test-subdomain", nil, zap.NewNop())
	conn.Close()

	if err := conn.Send([]byte("data")); !errors.Is(err, ErrConnectionClosed) {
		t.Fatalf("Send() error = %v, want %v", err, ErrConnectionClosed)
	}
}
