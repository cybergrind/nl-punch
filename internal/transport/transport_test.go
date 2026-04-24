package transport

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// pair creates a connected pair of Sessions over loopback UDP.
// Peer A is the yamux/KCP server; B is the client.
func pair(t *testing.T) (a, b *Session) {
	t.Helper()
	pcA, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pcB, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	prof := Profile("low-latency")

	type res struct {
		s   *Session
		err error
	}
	aCh := make(chan res, 1)
	go func() {
		s, err := Serve(pcA, prof)
		aCh <- res{s, err}
	}()

	bSess, err := Dial(pcB, pcA.LocalAddr().String(), prof)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	r := <-aCh
	if r.err != nil {
		t.Fatalf("Serve: %v", r.err)
	}

	t.Cleanup(func() {
		bSess.Close()
		r.s.Close()
	})
	return r.s, bSess
}

func TestSession_openAcceptStream(t *testing.T) {
	a, b := pair(t)

	acceptErr := make(chan error, 1)
	gotPayload := make(chan []byte, 1)
	go func() {
		s, err := a.AcceptStream()
		if err != nil {
			acceptErr <- err
			return
		}
		defer s.Close()
		buf, _ := io.ReadAll(s)
		gotPayload <- buf
		acceptErr <- nil
	}()

	s, err := b.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if _, err := s.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	s.Close()

	select {
	case err := <-acceptErr:
		if err != nil {
			t.Fatalf("accept: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("accept timeout")
	}
	got := <-gotPayload
	if string(got) != "hello" {
		t.Errorf("payload = %q, want hello", got)
	}
}

func TestSession_multipleConcurrentStreams(t *testing.T) {
	a, b := pair(t)

	const N = 8
	const msgLen = 13 // "stream-data-X"
	var wg sync.WaitGroup

	// Server: accept N streams, read msgLen bytes, echo them back, close.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			s, err := a.AcceptStream()
			if err != nil {
				t.Errorf("accept: %v", err)
				return
			}
			go func(s net.Conn) {
				defer s.Close()
				buf := make([]byte, msgLen)
				if _, err := io.ReadFull(s, buf); err != nil {
					return
				}
				s.Write(buf)
			}(s)
		}
	}()

	// Client: open N streams, send unique payload, verify echo, close.
	payload := []byte("stream-data-")
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			s, err := b.OpenStream()
			if err != nil {
				errs <- err
				return
			}
			defer s.Close()
			p := append(append([]byte{}, payload...), byte('0'+i))
			if _, err := s.Write(p); err != nil {
				errs <- err
				return
			}
			buf := make([]byte, msgLen)
			if _, err := io.ReadFull(s, buf); err != nil {
				errs <- err
				return
			}
			if string(buf) != string(p) {
				t.Errorf("stream %d echo mismatch: %q vs %q", i, buf, p)
			}
			errs <- nil
		}()
	}
	for i := 0; i < N; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Errorf("client %d: %v", i, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("client timeout")
		}
	}
	wg.Wait()
}

func TestProfile_lowLatencyDefault(t *testing.T) {
	// Unknown name → low-latency preset.
	p := Profile("never-heard-of-it")
	low := Profile("low-latency")
	if p != low {
		t.Errorf("Profile(unknown) = %+v, want %+v", p, low)
	}
}

func TestProfile_distinctPresets(t *testing.T) {
	lo := Profile("low-latency")
	bal := Profile("balanced")
	bw := Profile("bandwidth")
	if lo.Interval >= bal.Interval || bal.Interval >= bw.Interval {
		t.Errorf("intervals not monotonically increasing: lo=%d bal=%d bw=%d",
			lo.Interval, bal.Interval, bw.Interval)
	}
	if lo.NC != 1 {
		t.Errorf("low-latency NC=%d, want 1 (no congestion control)", lo.NC)
	}
	if bw.NC != 0 {
		t.Errorf("bandwidth NC=%d, want 0 (cwnd on)", bw.NC)
	}
}
