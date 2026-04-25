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

	type res struct {
		s   *Session
		err error
	}
	aCh := make(chan res, 1)
	go func() {
		s, err := Serve(pcA)
		aCh <- res{s, err}
	}()

	bSess, err := Dial(pcB, pcA.LocalAddr().String())
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

// TestSession_bulkServerPush_afterClientHeader mirrors the bench's
// throughput-down shape at the transport layer: open one stream from the
// client, have the client write a tiny header, then have the server write
// a large payload. Reproduces the round-E finding that bytes pushed by
// the server after the client's header didn't arrive at the client.
func TestSession_bulkServerPush_afterClientHeader(t *testing.T) {
	a, b := pair(t)

	const payloadSize = 512 * 1024

	done := make(chan error, 1)
	go func() {
		s, err := a.AcceptStream()
		if err != nil {
			done <- err
			return
		}
		defer s.Close()
		var hdr [5]byte
		if _, err := io.ReadFull(s, hdr[:]); err != nil {
			done <- err
			return
		}
		payload := make([]byte, payloadSize)
		for i := range payload {
			payload[i] = byte(i * 7)
		}
		if _, err := s.Write(payload); err != nil {
			done <- err
			return
		}
		// s.Close() via defer will half/full-close so client sees EOF.
		done <- nil
	}()

	s, err := b.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if _, err := s.Write([]byte{'U', 0, 0, 0, 5}); err != nil {
		t.Fatalf("Write header: %v", err)
	}
	s.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadAll(s)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != payloadSize {
		t.Fatalf("received %d bytes, want %d", len(got), payloadSize)
	}
	if err := <-done; err != nil {
		t.Fatalf("server: %v", err)
	}
}

// TestSession_continuousServerPush mirrors exactly what the bench does:
// server writes in a loop for ~dur instead of one big s.Write. Goal is
// to reproduce the real-world throughput-down behavior locally.
func TestSession_continuousServerPush(t *testing.T) {
	a, b := pair(t)
	const dur = 500 * time.Millisecond

	type result struct{ wrote uint64; err error }
	svDone := make(chan result, 1)
	go func() {
		s, err := a.AcceptStream()
		if err != nil {
			svDone <- result{0, err}
			return
		}
		defer s.Close()
		var hdr [5]byte
		if _, err := io.ReadFull(s, hdr[:]); err != nil {
			svDone <- result{0, err}
			return
		}
		buf := make([]byte, 64*1024)
		deadline := time.Now().Add(dur)
		var wrote uint64
		for time.Now().Before(deadline) {
			n, err := s.Write(buf)
			wrote += uint64(n)
			if err != nil {
				svDone <- result{wrote, err}
				return
			}
		}
		svDone <- result{wrote, nil}
	}()

	s, err := b.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if _, err := s.Write([]byte{'U', 0, 0, 0, 5}); err != nil {
		t.Fatalf("Write header: %v", err)
	}
	s.SetReadDeadline(time.Now().Add(dur + 5*time.Second))
	got, err := io.ReadAll(s)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	sv := <-svDone
	if sv.err != nil {
		t.Fatalf("server: %v (wrote %d)", sv.err, sv.wrote)
	}
	if uint64(len(got)) != sv.wrote {
		t.Fatalf("client got %d, server wrote %d — bytes lost in transit", len(got), sv.wrote)
	}
	t.Logf("server wrote %d bytes in %v, client received all of it", sv.wrote, dur)
}

// TestQUICConfig_keepAliveKeepsICEWarm pins the contract that our QUIC
// keepalive fires often enough to be a reliable keep-warm signal for the
// underlying pion/ICE path. Anything larger than a couple of seconds
// leaves long silent windows on one side under asymmetric read-only
// loads (the throughput-down bug in round F), so we require ≤ 2 s.
func TestQUICConfig_keepAliveKeepsICEWarm(t *testing.T) {
	c := quicConfig()
	if c.KeepAlivePeriod == 0 {
		t.Fatalf("KeepAlivePeriod=0; we rely on QUIC PINGs as the keep-warm signal for pion/ICE")
	}
	if c.KeepAlivePeriod > 2*time.Second {
		t.Errorf("KeepAlivePeriod=%v, want ≤ 2s so a read-only peer still emits outbound UDP often enough for ICE receive to stay healthy", c.KeepAlivePeriod)
	}
	if c.MaxIdleTimeout < 10*c.KeepAlivePeriod {
		t.Errorf("MaxIdleTimeout=%v < 10× KeepAlivePeriod=%v; session would die on brief backpressure",
			c.MaxIdleTimeout, c.KeepAlivePeriod)
	}
}

