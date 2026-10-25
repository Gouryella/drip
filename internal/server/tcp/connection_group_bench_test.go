package tcp

import (
	"fmt"
	"net"
	"testing"

	"drip/internal/shared/mux"
	"go.uber.org/zap"
)

func BenchmarkConnectionGroupOpenStream(b *testing.B) {
	for _, count := range []int{1, 8, 64} {
		b.Run(fmt.Sprintf("sessions_%d", count), func(b *testing.B) {
			group := &ConnectionGroup{Sessions: make(map[string]*mux.Session), stopCh: make(chan struct{}), logger: zap.NewNop()}
			b.Cleanup(group.Close)
			closed := make(chan struct{}, count)
			for i := 0; i < count; i++ {
				left, right := net.Pipe()
				cfg := mux.NewOptimizedConfig()
				cfg.EnableKeepAlive = false
				client, err := mux.Client(left, cfg)
				if err != nil {
					b.Fatal(err)
				}
				server, err := mux.Server(right, cfg)
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() { _ = server.Close() })
				group.Sessions[fmt.Sprintf("data-%d", i)] = client
				go func() {
					for {
						stream, err := server.Accept()
						if err != nil {
							return
						}
						_ = stream.Close()
						select {
						case closed <- struct{}{}:
						case <-server.CloseChan():
							return
						}
					}
				}()
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				stream, err := group.OpenStream()
				if err != nil {
					b.Fatal(err)
				}
				_ = stream.Close()
				<-closed
			}
		})
	}
}
