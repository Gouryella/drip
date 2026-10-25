package pool

import (
	"bufio"
	"io"
	"sync"
)

// BufioReaderPool provides a pool of bufio.Reader instances.
var BufioReaderPool = sync.Pool{
	New: func() interface{} {
		return bufio.NewReaderSize(nil, 32*1024)
	},
}

// BufioWriterPool provides a pool of bufio.Writer instances.
var BufioWriterPool = sync.Pool{
	New: func() interface{} {
		return bufio.NewWriterSize(nil, 4096)
	},
}

// GetReader gets a bufio.Reader from the pool and resets it to read from r.
func GetReader(r io.Reader) *bufio.Reader {
	reader := BufioReaderPool.Get().(*bufio.Reader)
	reader.Reset(r)
	return reader
}

// PutReader returns a bufio.Reader to the pool.
func PutReader(reader *bufio.Reader) {
	reader.Reset(nil)
	BufioReaderPool.Put(reader)
}

// GetWriter gets a bufio.Writer from the pool and resets it to write to w.
func GetWriter(w io.Writer) *bufio.Writer {
	writer := BufioWriterPool.Get().(*bufio.Writer)
	writer.Reset(w)
	return writer
}

// PutWriter returns a bufio.Writer to the pool.
func PutWriter(writer *bufio.Writer) {
	writer.Reset(nil)
	BufioWriterPool.Put(writer)
}
