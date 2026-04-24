package ice

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	pionice "github.com/pion/ice/v3"

	"nl-punch/internal/signaling"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestAgent_connectLoopback(t *testing.T) {
	// Two ICE agents on loopback exchange credentials+candidates via an
	// in-memory signaling server, then open a reliable conn; bytes pumped
	// one way should arrive.
	sigSrv := signaling.NewServer("")
	ts := httptest.NewServer(sigSrv.Handler())
	defer ts.Close()

	log := quietLogger()
	aA := New(Config{
		Signaling: signaling.NewClient(ts.URL, ""),
		SessionID: "test",
		Role:      "peer-a",
		Logger:    log,
		// No STUN — host candidates on loopback are enough.
	})
	aB := New(Config{
		Signaling: signaling.NewClient(ts.URL, ""),
		SessionID: "test",
		Role:      "peer-b",
		Logger:    log,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)

	type result struct {
		conn *pionice.Conn
		err  error
	}
	resA := make(chan result, 1)
	resB := make(chan result, 1)

	go func() {
		defer wg.Done()
		c, err := aA.Connect(ctx)
		resA <- result{c, err}
	}()
	go func() {
		defer wg.Done()
		c, err := aB.Connect(ctx)
		resB <- result{c, err}
	}()

	wg.Wait()

	ra := <-resA
	rb := <-resB
	if ra.err != nil || rb.err != nil {
		t.Fatalf("Connect: a=%v b=%v", ra.err, rb.err)
	}
	defer ra.conn.Close()
	defer rb.conn.Close()

	// Send a byte from A to B via the underlying conn.
	if _, err := ra.conn.Write([]byte("hi")); err != nil {
		t.Fatalf("A.Write: %v", err)
	}
	rb.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 2)
	if _, err := io.ReadFull(rb.conn, buf); err != nil {
		t.Fatalf("B.Read: %v", err)
	}
	if string(buf) != "hi" {
		t.Errorf("recv = %q, want hi", buf)
	}

	// Verify we can also read the local/remote addrs (sanity).
	if ra.conn.LocalAddr() == nil {
		t.Error("A LocalAddr nil")
	}
	if rb.conn.RemoteAddr() == nil {
		t.Error("B RemoteAddr nil")
	}
}

func TestAgent_connectTimesOutWithoutPeer(t *testing.T) {
	sigSrv := signaling.NewServer("")
	ts := httptest.NewServer(sigSrv.Handler())
	defer ts.Close()

	aA := New(Config{
		Signaling: signaling.NewClient(ts.URL, ""),
		SessionID: "lonely",
		Role:      "peer-a",
		Logger:    quietLogger(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err := aA.Connect(ctx)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}
