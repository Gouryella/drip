package pool

import "sync"

const (
	SizeSmall  = 4 * 1024     // 4KB   - HTTP headers, small messages
	SizeMedium = 32 * 1024    // 32KB  - HTTP request/response bodies
	SizeLarge  = 256 * 1024   // 256KB - Data pipe, file transfers
	SizeXLarge = 1024 * 1024  // 1MB   - Large file transfers, bulk data
)

type BufferPool struct {
	small  sync.Pool
	medium sync.Pool
	large  sync.Pool
	xlarge sync.Pool
}

func NewBufferPool() *BufferPool {
	return &BufferPool{
		small: sync.Pool{
			New: func() interface{} {
				b := make([]byte, SizeSmall)
				return &b
			},
		},
		medium: sync.Pool{
			New: func() interface{} {
				b := make([]byte, SizeMedium)
				return &b
			},
		},
		large: sync.Pool{
			New: func() interface{} {
				b := make([]byte, SizeLarge)
				return &b
			},
		},
		xlarge: sync.Pool{
			New: func() interface{} {
				b := make([]byte, SizeXLarge)
				return &b
			},
		},
	}
}

func (p *BufferPool) Get(size int) *[]byte {
	switch {
	case size <= SizeSmall:
		return p.small.Get().(*[]byte)
	case size <= SizeMedium:
		return p.medium.Get().(*[]byte)
	case size <= SizeLarge:
		return p.large.Get().(*[]byte)
	default:
		return p.xlarge.Get().(*[]byte)
	}
}

func (p *BufferPool) Put(buf *[]byte) {
	if buf == nil {
		return
	}

	size := cap(*buf)
	*buf = (*buf)[:cap(*buf)]

	switch size {
	case SizeSmall:
		p.small.Put(buf)
	case SizeMedium:
		p.medium.Put(buf)
	case SizeLarge:
		p.large.Put(buf)
	case SizeXLarge:
		p.xlarge.Put(buf)
	}
	// Note: buffers with non-standard sizes are not pooled (let GC handle them)
}

// GetXLarge returns a 1MB buffer for bulk data transfers
func (p *BufferPool) GetXLarge() *[]byte {
	return p.xlarge.Get().(*[]byte)
}

// PutXLarge returns a 1MB buffer to the pool
func (p *BufferPool) PutXLarge(buf *[]byte) {
	if buf == nil || cap(*buf) != SizeXLarge {
		return
	}
	*buf = (*buf)[:cap(*buf)]
	p.xlarge.Put(buf)
}

var globalBufferPool = NewBufferPool()

func GetBuffer(size int) *[]byte {
	return globalBufferPool.Get(size)
}

func PutBuffer(buf *[]byte) {
	globalBufferPool.Put(buf)
}

// GetXLargeBuffer returns a 1MB buffer from the global pool
func GetXLargeBuffer() *[]byte {
	return globalBufferPool.GetXLarge()
}

// PutXLargeBuffer returns a 1MB buffer to the global pool
func PutXLargeBuffer(buf *[]byte) {
	globalBufferPool.PutXLarge(buf)
}
