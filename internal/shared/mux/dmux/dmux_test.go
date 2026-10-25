package dmux

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

func tcpPair(tb testing.TB) (net.Conn, net.Conn) {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			ch <- c
		}
	}()
	c1, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		tb.Fatal(err)
	}
	c2 := <-ch
	if c2 == nil {
		tb.Fatal("accept failed")
	}
	return c1, c2
}

func testConfig() *Config {
	cfg := DefaultConfig()
	cfg.EnableKeepAlive = false
	cfg.ConnectionWriteTimeout = 5 * time.Second
	return cfg
}

// newPair returns a client and server session over loopback TCP.
func newPair(tb testing.TB) (*Session, *Session) {
	tb.Helper()
	c1, c2 := tcpPair(tb)
	cfg := testConfig()
	cs, err := Client(c1, cfg)
	if err != nil {
		tb.Fatal(err)
	}
	ss, err := Server(c2, cfg)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		_ = cs.Close()
		_ = ss.Close()
		_ = c1.Close()
		_ = c2.Close()
	})
	return cs, ss
}

// newEchoPair starts a server that echoes every stream back to the client.
func newEchoPair(tb testing.TB) (*Session, *Session) {
	tb.Helper()
	cs, ss := newPair(tb)
	go func() {
		for {
			st, err := ss.Accept()
			if err != nil {
				return
			}
			go func() {
				defer st.Close()
				_, _ = io.Copy(st, st)
			}()
		}
	}()
	return cs, ss
}

func TestEchoRoundTrip(t *testing.T) {
	cs, _ := newEchoPair(t)

	st, err := cs.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	payload := []byte("hello dmux")
	if _, err := st.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := st.CloseWrite(); err != nil {
		t.Fatal(err)
	}

	got, err := io.ReadAll(st)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo = %q, want %q", got, payload)
	}
}

func TestManyConcurrentStreams(t *testing.T) {
	cs, _ := newEchoPair(t)

	const n = 200
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, err := cs.Open()
			if err != nil {
				errCh <- err
				return
			}
			defer st.Close()
			payload := bytes.Repeat([]byte{byte(i)}, 128+i)
			if _, err := st.Write(payload); err != nil {
				errCh <- err
				return
			}
			if err := st.CloseWrite(); err != nil {
				errCh <- err
				return
			}
			got, err := io.ReadAll(st)
			if err != nil {
				errCh <- err
				return
			}
			if !bytes.Equal(got, payload) {
				errCh <- errors.New("payload mismatch")
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

func TestLargeTransferFlowControl(t *testing.T) {
	cs, _ := newEchoPair(t)

	st, err := cs.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	const size = 4 << 20 // several windows worth
	payload := make([]byte, size)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = st.Write(payload)
		_ = st.CloseWrite()
	}()

	got, err := io.ReadAll(st)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != size {
		t.Fatalf("got %d bytes, want %d", len(got), size)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("payload mismatch")
	}
}

func TestHalfCloseAndEOF(t *testing.T) {
	cs, ss := newPair(t)

	st, err := cs.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	peerCh := make(chan *Stream, 1)
	go func() {
		peer, err := ss.Accept()
		if err == nil {
			peerCh <- peer
		}
	}()

	if _, err := st.Write([]byte("bye")); err != nil {
		t.Fatal(err)
	}
	if err := st.CloseWrite(); err != nil {
		t.Fatal(err)
	}

	select {
	case peer := <-peerCh:
		defer peer.Close()
		got, err := io.ReadAll(peer)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "bye" {
			t.Fatalf("peer read %q", got)
		}
		if _, err := peer.Write([]byte("ack")); err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("accept timeout")
	}

	var buf [8]byte
	n, err := st.Read(buf[:])
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "ack" {
		t.Fatalf("client read %q", buf[:n])
	}
}

func TestReadDeadline(t *testing.T) {
	cs, _ := newEchoPair(t)

	st, err := cs.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var buf [8]byte
	_, err = st.Read(buf[:])
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read error = %v, want deadline exceeded", err)
	}
}

func TestWriteDeadlineWithExhaustedWindow(t *testing.T) {
	cs, ss := newPair(t)

	st, err := cs.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Accept but never read, so the receive window drains.
	accepted := make(chan *Stream, 1)
	go func() {
		peer, err := ss.Accept()
		if err == nil {
			accepted <- peer
		}
	}()

	chunk := make([]byte, 32*1024)
	if err := st.SetWriteDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var err2 error
	for i := 0; i < 64; i++ { // 2MB is well past the 256KB window
		if _, err2 = st.Write(chunk); err2 != nil {
			break
		}
	}
	if !errors.Is(err2, os.ErrDeadlineExceeded) {
		t.Fatalf("Write error = %v, want deadline exceeded", err2)
	}
	peer := <-accepted
	_ = peer.Close()
}

func TestSessionCloseWakesStreams(t *testing.T) {
	cs, ss := newPair(t)

	st, err := cs.Open()
	if err != nil {
		t.Fatal(err)
	}

	readErr := make(chan error, 1)
	go func() {
		var buf [1]byte
		_, err := st.Read(buf[:])
		readErr <- err
	}()

	time.Sleep(20 * time.Millisecond)
	_ = ss.Close()

	select {
	case err := <-readErr:
		if err == nil {
			t.Fatal("Read returned nil error after session close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not wake after session close")
	}
}

func TestPing(t *testing.T) {
	cs, _ := newPair(t)
	rtt, err := cs.Ping()
	if err != nil {
		t.Fatal(err)
	}
	if rtt <= 0 {
		t.Fatalf("rtt = %v", rtt)
	}
}

func TestGoAwayRefusesNewStreams(t *testing.T) {
	cs, _ := newPair(t)
	if err := cs.GoAway(); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Open(); !errors.Is(err, ErrGoAway) {
		t.Fatalf("Open after GoAway = %v, want ErrGoAway", err)
	}
}

func TestStreamResetWakesReader(t *testing.T) {
	cs, ss := newPair(t)

	st, err := cs.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}

	peer, err := ss.Accept()
	if err != nil {
		t.Fatal(err)
	}

	readErr := make(chan error, 1)
	go func() {
		var buf [4]byte
		for {
			if _, err := peer.Read(buf[:]); err != nil {
				readErr <- err
				return
			}
		}
	}()

	// Closing the session must wake the blocked reader even though no FIN was
	// sent for this stream.
	_ = ss.Close()

	select {
	case err := <-readErr:
		if err == nil {
			t.Fatal("peer Read returned nil error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("peer Read did not wake")
	}
}

func TestConcurrentWriteAndRead(t *testing.T) {
	cs, ss := newPair(t)

	st, err := cs.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	peerCh := make(chan *Stream, 1)
	go func() {
		peer, err := ss.Accept()
		if err == nil {
			peerCh <- peer
		}
	}()
	peer := <-peerCh
	defer peer.Close()

	const chunks = 100
	const size = 1024
	var wg sync.WaitGroup
	wg.Add(2)
	var mu sync.Mutex
	var got []byte
	go func() {
		defer wg.Done()
		buf := make([]byte, size)
		for i := 0; i < chunks; i++ {
			if _, err := io.ReadFull(peer, buf); err != nil {
				t.Errorf("peer read: %v", err)
				return
			}
			mu.Lock()
			got = append(got, buf...)
			mu.Unlock()
		}
		_ = peer.CloseWrite()
	}()
	go func() {
		defer wg.Done()
		buf := make([]byte, size)
		for i := 0; i < chunks; i++ {
			if _, err := st.Write(buf); err != nil {
				t.Errorf("client write: %v", err)
				return
			}
		}
		_ = st.CloseWrite()
	}()
	wg.Wait()
	if len(got) != chunks*size {
		t.Fatalf("received %d bytes, want %d", len(got), chunks*size)
	}
}
