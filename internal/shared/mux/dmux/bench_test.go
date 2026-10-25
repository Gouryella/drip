package dmux

import (
	"io"
	"net"
	"testing"
)

func benchDmuxDrain(b *testing.B) *Session {
	b.Helper()
	cs, ss := newPair(b)
	go func() {
		for {
			st, err := ss.Accept()
			if err != nil {
				return
			}
			go func() {
				defer st.Close()
				_, _ = io.Copy(io.Discard, st)
			}()
		}
	}()
	return cs
}

func benchDmuxEcho(b *testing.B) *Session {
	b.Helper()
	cs, ss := newPair(b)
	go func() {
		for {
			st, err := ss.Accept()
			if err != nil {
				return
			}
			go func() {
				defer st.Close()
				buf := make([]byte, 32*1024)
				for {
					n, err := st.Read(buf)
					if n > 0 {
						if _, werr := st.Write(buf[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return cs
}

// BenchmarkRawTCPEcho1KB is the floor: no multiplexer, just two syscalls per
// round trip.
func BenchmarkRawTCPEcho1KB(b *testing.B) {
	c1, c2 := tcpPair(b)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := c2.Read(buf)
			if n > 0 {
				if _, werr := c2.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	msg := make([]byte, 1024)
	rbuf := make([]byte, 1024)
	b.SetBytes(int64(len(msg)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c1.Write(msg); err != nil {
			b.Fatal(err)
		}
		if _, err := io.ReadFull(c1, rbuf); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOpenClose(b *testing.B) {
	cs := benchDmuxDrain(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st, err := cs.Open()
		if err != nil {
			b.Fatal(err)
		}
		if err := st.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEcho1KB(b *testing.B) {
	cs := benchDmuxEcho(b)
	st, err := cs.Open()
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	msg := make([]byte, 1024)
	rbuf := make([]byte, 1024)
	b.SetBytes(int64(len(msg)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.Write(msg); err != nil {
			b.Fatal(err)
		}
		if _, err := io.ReadFull(st, rbuf); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWrite32KB(b *testing.B) {
	cs := benchDmuxDrain(b)
	st, err := cs.Open()
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	msg := make([]byte, 32*1024)
	b.SetBytes(int64(len(msg)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.Write(msg); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkParallelEcho measures many concurrent streams on one session.
func BenchmarkParallelEcho(b *testing.B) {
	cs := benchDmuxEcho(b)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		st, err := cs.Open()
		if err != nil {
			b.Error(err)
			return
		}
		defer st.Close()
		msg := make([]byte, 1024)
		rbuf := make([]byte, 1024)
		for pb.Next() {
			if _, err := st.Write(msg); err != nil {
				b.Error(err)
				return
			}
			if _, err := io.ReadFull(st, rbuf); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

var _ net.Conn = (*Stream)(nil)
