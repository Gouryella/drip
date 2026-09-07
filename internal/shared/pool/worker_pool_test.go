package pool

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkerPoolSubmitAfterCloseReturnsFalse(t *testing.T) {
	t.Parallel()

	pool := NewWorkerPool(1, 1)
	pool.Close()

	if pool.Submit(func() {}) {
		t.Fatal("Submit returned true after Close")
	}
}

func TestWorkerPoolSubmitCloseConcurrentDoesNotPanic(t *testing.T) {
	t.Parallel()

	for iteration := 0; iteration < 100; iteration++ {
		pool := NewWorkerPool(4, 32)
		start := make(chan struct{})
		var submitters sync.WaitGroup
		var executed atomic.Int64

		for i := 0; i < 64; i++ {
			submitters.Add(1)
			go func() {
				defer submitters.Done()
				<-start

				deadline := time.Now().Add(10 * time.Millisecond)
				for time.Now().Before(deadline) {
					pool.Submit(func() {
						executed.Add(1)
					})
				}
			}()
		}

		close(start)
		time.Sleep(time.Microsecond)
		pool.Close()
		submitters.Wait()

		if !pool.IsClosed() {
			t.Fatal("pool should report closed after Close")
		}
	}
}
