// High-performance test HTTP server for benchmarking
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"runtime"
)

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

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("Test server listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
