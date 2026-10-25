// High-performance test HTTP server for benchmarking and bulk-transfer tests.
package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime"
	"strconv"
	"time"
)

// fillPattern writes the deterministic byte pattern used by both the bulk
// download and upload tests: byte(i % 256). Verifiers reproduce it without
// storing the whole stream.
func fillChunk(chunk []byte, offset int64) []byte {
	for j := range chunk {
		chunk[j] = byte((offset + int64(j)) % 256) // #nosec G115
	}
	return chunk
}

func main() {
	port := flag.Int("port", 3000, "Port to listen on")
	flag.Parse()

	runtime.GOMAXPROCS(runtime.NumCPU())

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "OK")
	})

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"ok"}`)
	})

	// GET /large?bytes=N streams N deterministic bytes.
	http.HandleFunc("/large", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.ParseInt(r.URL.Query().Get("bytes"), 10, 64)
		if err != nil || n <= 0 {
			n = 1 << 30
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
		w.WriteHeader(http.StatusOK)
		chunk := make([]byte, 64*1024)
		var off int64
		for off < n {
			m := int64(len(chunk))
			if n-off < m {
				m = n - off
			}
			chunk = fillChunk(chunk[:m], off)
			if _, err := w.Write(chunk[:m]); err != nil {
				return
			}
			off += m
		}
	})

	// POST /upload (and PUT, to mimic S3-style uploads) reads the body and
	// returns its size and SHA-256.
	http.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodPut {
			http.Error(w, "POST or PUT required", http.StatusMethodNotAllowed)
			return
		}
		h := sha256.New()
		n, err := io.Copy(h, r.Body)
		if err != nil {
			http.Error(w, fmt.Sprintf("read: %v", err), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"bytes":%d,"sha256":"%x"}`, n, h.Sum(nil))
	})

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("Test server listening on %s", addr)

	srv := &http.Server{
		Addr:              addr,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       0, // bulk uploads are long-lived; no total read timeout
		WriteTimeout:      0, // bulk downloads are long-lived; no total write timeout
		IdleTimeout:       120 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}
