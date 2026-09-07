package pool

import "sync"

const (
	SizeSmall  = 4 * 1024    // 4KB   - HTTP headers, small messages
	SizeMedium = 32 * 1024   // 32KB  - HTTP request/response bodies
	SizeLarge  = 256 * 1024  // 256KB - Data pipe, file transfers
	SizeXLarge = 1024 * 1024 // 1MB   - Large file transfers, bulk data
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
	case size <= SizeXLarge:
		return p.xlarge.Get().(*[]byte)
	default:
		buf := make([]byte, size)
		return &buf
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
}

var globalBufferPool = NewBufferPool()

func GetBuffer(size int) *[]byte {
	return globalBufferPool.Get(size)
}

func PutBuffer(buf *[]byte) {
	globalBufferPool.Put(buf)
}
