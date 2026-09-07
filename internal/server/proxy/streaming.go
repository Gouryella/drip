package proxy

import (
	"errors"
	"io"
	"net/http"
	"time"

	"drip/internal/shared/pool"
)

type streamingResponseWriter struct {
	w            http.ResponseWriter
	controller   *http.ResponseController
	writeTimeout time.Duration
}

func newStreamingResponseWriter(w http.ResponseWriter, writeTimeout time.Duration) *streamingResponseWriter {
	return &streamingResponseWriter{
		w:            w,
		controller:   http.NewResponseController(w),
		writeTimeout: writeTimeout,
	}
}

func (w *streamingResponseWriter) Write(p []byte) (int, error) {
	if err := w.refreshWriteDeadline(); err != nil {
		return 0, err
	}
	n, err := w.w.Write(p)
	if n > 0 {
		if flushErr := w.Flush(); err == nil && flushErr != nil {
			err = flushErr
		}
	}
	return n, err
}

func (w *streamingResponseWriter) Flush() error {
	if err := w.refreshWriteDeadline(); err != nil {
		return err
	}
	// An idle event stream may have no events for longer than writeTimeout.
	// Limit the actual flush, then let the next event establish its deadline.
	defer w.controller.SetWriteDeadline(time.Time{})
	err := w.controller.Flush()
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

func (w *streamingResponseWriter) refreshWriteDeadline() error {
	if w.writeTimeout <= 0 {
		return nil
	}
	err := w.controller.SetWriteDeadline(time.Now().Add(w.writeTimeout))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

func copyResponseBodyFlushing(w *streamingResponseWriter, body io.Reader) (int64, error) {
	bufPtr := pool.GetBuffer(pool.SizeSmall)
	defer pool.PutBuffer(bufPtr)

	buf := (*bufPtr)[:pool.SizeSmall]
	var written int64

	for {
		nr, er := body.Read(buf)
		if nr > 0 {
			nw, ew := w.Write(buf[:nr])
			if nw > 0 {
				written += int64(nw)
			}
			if ew != nil {
				return written, ew
			}
			if nr != nw {
				return written, io.ErrShortWrite
			}
		}
		if er != nil {
			if er == io.EOF {
				return written, nil
			}
			return written, er
		}
	}
}
