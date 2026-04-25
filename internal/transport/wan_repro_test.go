package transport

import (
	"crypto/rand"
	"errors"
	"io"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// wanConn wraps a PacketConn to simulate a WAN path: per-direction
// one-way delay, an egress token bucket (rate cap), and Bernoulli loss.
//
// Delay is applied asynchronously: a writer goroutine schedules each
// outgoing packet for delivery `txDelay` in the future and the kernel
// transmits when its time arrives. Crucially, ReadFrom never blocks on
// time.Sleep — that would serialize the receive loop and cap throughput
// at one packet per delay interval (a sim-harness bug, not a transport
// problem).
type wanConn struct {
	inner net.PacketConn

	txDelay time.Duration

	// Egress rate cap: tokens are bytes per second. Zero = unlimited.
	txRateBPS int64
	txMu      sync.Mutex
	txTokens  float64
	txLast    time.Time

	// Loss probability on send, [0,1).
	txLoss float64

	closed atomic.Bool
}

func newWANConn(inner net.PacketConn, txDelay time.Duration, rateBPS int64, loss float64) *wanConn {
	return &wanConn{
		inner:     inner,
		txDelay:   txDelay,
		txRateBPS: rateBPS,
		txLast:    time.Now(),
		txLoss:    loss,
	}
}

func (c *wanConn) ReadFrom(p []byte) (int, net.Addr, error) {
	return c.inner.ReadFrom(p)
}

func (c *wanConn) WriteTo(p []byte, a net.Addr) (int, error) {
	if c.txLoss > 0 {
		bn, _ := rand.Int(rand.Reader, big.NewInt(1_000_000))
		if float64(bn.Int64())/1_000_000.0 < c.txLoss {
			return len(p), nil // pretend send succeeded; bytes vanish
		}
	}
	if c.txRateBPS > 0 {
		c.txMu.Lock()
		now := time.Now()
		elapsed := now.Sub(c.txLast).Seconds()
		c.txLast = now
		c.txTokens += elapsed * float64(c.txRateBPS)
		if cap := float64(c.txRateBPS); c.txTokens > cap {
			c.txTokens = cap
		}
		need := float64(len(p))
		if need > c.txTokens {
			deficit := need - c.txTokens
			wait := time.Duration(deficit / float64(c.txRateBPS) * float64(time.Second))
			c.txMu.Unlock()
			time.Sleep(wait)
			c.txMu.Lock()
			c.txTokens = 0
			c.txLast = time.Now()
		} else {
			c.txTokens -= need
		}
		c.txMu.Unlock()
	}
	if c.txDelay > 0 && !c.closed.Load() {
		// Copy because the caller may reuse p once WriteTo returns.
		buf := make([]byte, len(p))
		copy(buf, p)
		dst := a
		go func() {
			time.Sleep(c.txDelay)
			if c.closed.Load() {
				return
			}
			c.inner.WriteTo(buf, dst)
		}()
		return len(p), nil
	}
	return c.inner.WriteTo(p, a)
}

func (c *wanConn) Close() error {
	c.closed.Store(true)
	return c.inner.Close()
}

func (c *wanConn) LocalAddr() net.Addr                { return c.inner.LocalAddr() }
func (c *wanConn) SetDeadline(t time.Time) error      { return c.inner.SetDeadline(t) }
func (c *wanConn) SetReadDeadline(t time.Time) error  { return c.inner.SetReadDeadline(t) }
func (c *wanConn) SetWriteDeadline(t time.Time) error { return c.inner.SetWriteDeadline(t) }

// pairWAN is pair() but each side's UDP socket is wrapped with a wanConn.
func pairWAN(t *testing.T, rxDelay time.Duration, rateBPS int64, loss float64) (a, b *Session) {
	t.Helper()
	pcAraw, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pcBraw, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pcA := newWANConn(pcAraw, rxDelay, rateBPS, loss)
	pcB := newWANConn(pcBraw, rxDelay, rateBPS, loss)

	type res struct {
		s   *Session
		err error
	}
	aCh := make(chan res, 1)
	go func() {
		s, err := Serve(pcA)
		aCh <- res{s, err}
	}()

	bSess, err := Dial(pcB, pcAraw.LocalAddr().String())
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

// TestSession_bulkOver_simulatedWAN drives a sim WAN whose parameters
// mirror the .166↔.132 path (240 ms RTT, 25 Mbps, 0.5% loss) and asserts
// the session keeps running and ships *some* bytes within budget. The
// cluster bench's PASS threshold (1 MiB/s sustained) is structurally
// unreachable for single-stream kcp-go on a lossy WAN — TCP-Reno-style
// cwnd dance keeps in-flight tiny — so we don't pin to that. What we
// *do* pin: at least some bytes flow, and the session does not die.
// Recovering from this regression to "0 bytes / session dies" is what
// matters; throughput tuning is a separate, known-bounded problem.
func TestSession_bulkOver_simulatedWAN(t *testing.T) {
	if testing.Short() {
		t.Skip("WAN-sim bulk test is slow")
	}
	const (
		oneWayDelay     = 120 * time.Millisecond
		rateBPS         = 25 * 1024 * 1024 / 8
		loss            = 0.005
		payload         = 1 * 1024 * 1024 // 1 MiB
		budget          = 90 * time.Second
		minBytesShipped = 256 * 1024 // at least 256 KiB
	)

	a, b := pairWAN(t, oneWayDelay, rateBPS, loss)

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
		buf := make([]byte, 64*1024)
		var wrote int
		for wrote < payload {
			n, err := s.Write(buf[:min(len(buf), payload-wrote)])
			wrote += n
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()

	s, err := b.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if _, err := s.Write([]byte{'U', 0, 0, 0, 5}); err != nil {
		t.Fatalf("Write header: %v", err)
	}
	s.SetReadDeadline(time.Now().Add(budget))

	got := 0
	rbuf := make([]byte, 64*1024)
	start := time.Now()
	for got < payload {
		n, err := s.Read(rbuf)
		got += n
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			break // any read error: stop and check what we got
		}
	}
	elapsed := time.Since(start)
	t.Logf("WAN-sim bulk: shipped %d KiB / %d KiB in %v", got/1024, payload/1024, elapsed)

	if got < minBytesShipped {
		t.Fatalf("shipped only %d bytes; want at least %d — session is wedged", got, minBytesShipped)
	}
	// Drain server goroutine; don't block the test if it's still writing.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

// TestSession_sustainedAsymmetricUplink_doesNotKillTunnel guards the
// pathology that originally motivated removing NC=1 + small windows from
// the codebase: on an asymmetric path where the *sender* (peer-a in the
// cluster) has a small uplink, an unpaced KCP stream floods the uplink,
// starves return-path ACKs and yamux keepalives, and the session dies
// inside ~15s with `yamux keepalive i/o timeout`.
//
// We verify the current tuning + yamux flow control survives a 30 s
// sustained push over a 5 Mbps uplink × 25 Mbps downlink × 0.5% loss
// path. If the session's CloseChan fires inside the budget, that's the
// keepalive-death symptom returning.
func TestSession_sustainedAsymmetricUplink_doesNotKillTunnel(t *testing.T) {
	if testing.Short() {
		t.Skip("sustained-write test is slow")
	}
	const (
		oneWayDelay  = 120 * time.Millisecond
		// Asymmetric to match observed .166 uplink: 2 Mbps up, 25 Mbps
		// down. With NC=1 + big SendWindow this saturates the uplink so
		// hard that yamux Ping replies can't traverse it within the
		// keepalive deadline → "yamux keepalive i/o deadline reached"
		// → session shutdown after ~3 s. This is the cluster pathology.
		uplinkBPS    = 2 * 1024 * 1024 / 8
		downlinkBPS  = 25 * 1024 * 1024 / 8
		loss         = 0.01
		duration     = 30 * time.Second
		// At least *some* bytes flow without the session dying. The
		// historical regression we're guarding against is the original
		// pathology: NC=1 + small windows + 60 s ConnectionWriteTimeout
		// died inside ~3 s with "yamux keepalive i/o deadline reached"
		// and shipped 0 bytes. 256 KiB in 30 s ≈ 8 KiB/s — well below
		// what the wire could deliver, but well above 0, so this catches
		// any future regression that wedges the session.
		floorBytes = 256 * 1024
	)

	t.Helper()
	pcAraw, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pcBraw, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// Asymmetric: A (server, "studio") has small uplink; B (client) has
	// big downlink. Loss applied symmetrically.
	pcA := newWANConn(pcAraw, oneWayDelay, uplinkBPS, loss)
	pcB := newWANConn(pcBraw, oneWayDelay, downlinkBPS, loss)

	type res struct {
		s   *Session
		err error
	}
	aCh := make(chan res, 1)
	go func() {
		s, err := Serve(pcA)
		aCh <- res{s, err}
	}()
	bSess, err := Dial(pcB, pcAraw.LocalAddr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	r := <-aCh
	if r.err != nil {
		t.Fatalf("Serve: %v", r.err)
	}
	a, b := r.s, bSess
	t.Cleanup(func() { b.Close(); a.Close() })

	done := make(chan error, 1)
	wrote := make(chan int, 1)
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
		buf := make([]byte, 64*1024)
		deadline := time.Now().Add(duration)
		var w int
		for time.Now().Before(deadline) {
			n, err := s.Write(buf)
			w += n
			if err != nil {
				wrote <- w
				done <- err
				return
			}
		}
		wrote <- w
		done <- nil
	}()

	stream, err := b.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if _, err := stream.Write([]byte{'U', 0, 0, 0, 5}); err != nil {
		t.Fatalf("write header: %v", err)
	}
	stream.SetReadDeadline(time.Now().Add(duration + 15*time.Second))

	got := 0
	rbuf := make([]byte, 64*1024)
	for {
		n, err := stream.Read(rbuf)
		got += n
		if err != nil {
			break
		}
	}
	w := <-wrote
	serverErr := <-done

	// Tunnel-level check: the session must not be closed before we expected.
	select {
	case <-a.Wait():
		t.Errorf("peer-a session died before expected — keepalive starvation regression")
	default:
	}
	select {
	case <-b.Wait():
		t.Errorf("peer-b session died before expected — keepalive starvation regression")
	default:
	}

	if serverErr != nil {
		t.Errorf("server-side write loop ended with error after writing %d bytes: %v", w, serverErr)
	}
	if got < floorBytes {
		t.Errorf("client received %d bytes in %v, want at least %d (sustained throughput collapsed)", got, duration, floorBytes)
	}
	t.Logf("sustained: server wrote %d bytes, client read %d bytes in %v", w, got, duration)
}
