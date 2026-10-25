package pool

import (
	"runtime"
	"sync"
)

// NumCPU returns the number of logical CPUs available
func NumCPU() int {
	return runtime.GOMAXPROCS(0)
}

// WorkerPool is a fixed-size goroutine pool for handling tasks
type WorkerPool struct {
	workers  int
	jobQueue chan func()
	wg       sync.WaitGroup
	once     sync.Once
	closed   bool
	mu       sync.RWMutex
}

// NewWorkerPool creates a new worker pool with the specified number of workers
func NewWorkerPool(workers int, queueSize int) *WorkerPool {
	if workers <= 0 {
		workers = 50 // Default worker count
	}
	if queueSize <= 0 {
		queueSize = 1000 // Default queue size
	}

	pool := &WorkerPool{
		workers:  workers,
		jobQueue: make(chan func(), queueSize),
	}

	// Start worker goroutines
	for i := 0; i < workers; i++ {
		pool.wg.Add(1)
		go pool.worker()
	}

	return pool
}

// worker is the worker goroutine that processes jobs from the queue
func (p *WorkerPool) worker() {
	defer p.wg.Done()

	for job := range p.jobQueue {
		if job != nil {
			job()
		}
	}
}

// Submit submits a job to the worker pool
// Returns false if the pool is closed or the queue is full (job is NOT executed)
func (p *WorkerPool) Submit(job func()) bool {
	if job == nil {
		return false
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.closed {
		return false
	}

	select {
	case p.jobQueue <- job:
		return true
	default:
		return false
	}
}

// SubmitWait submits a job and waits for it to complete
func (p *WorkerPool) SubmitWait(job func()) {
	if job == nil {
		return
	}

	done := make(chan struct{})
	wrappedJob := func() {
		defer close(done)
		job()
	}

	if p.Submit(wrappedJob) {
		<-done
	} else {
		// Pool is closed or full, execute directly
		job()
	}
}

// Close gracefully shuts down the worker pool
// It waits for all pending jobs to complete
func (p *WorkerPool) Close() {
	p.once.Do(func() {
		p.mu.Lock()
		p.closed = true
		close(p.jobQueue)
		p.mu.Unlock()

		p.wg.Wait()
	})
}

// IsClosed returns true if the pool is closed
func (p *WorkerPool) IsClosed() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.closed
}
