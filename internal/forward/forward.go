// Package forward bridges TCP listeners to multiplexed streams and vice
// versa. A Session is any object with OpenStream/AcceptStream methods,
// typically transport.Session but decoupled so tests can use raw yamux.
//
// Wire format on each stream:
//
//	byte 0   : tag length N (1..255)
//	bytes 1..: tag bytes (ASCII, e.g. "http")
//	rest     : opaque TCP stream bytes
package forward

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Session abstracts the multiplexed transport.
type Session interface {
	OpenStream() (net.Conn, error)
	AcceptStream() (net.Conn, error)
}

// ListenSpec mirrors config.ListenSpec (kept local to avoid a cross-import).
type ListenSpec struct {
	Name       string
	LocalPort  int // 0 → pick a free port (test convenience)
	RemotePort int
}

// ---------- tag framing ----------

const maxTagLen = 255

func writeTag(w io.Writer, tag string) error {
	if len(tag) == 0 || len(tag) > maxTagLen {
		return fmt.Errorf("invalid tag length %d", len(tag))
	}
	buf := append([]byte{byte(len(tag))}, tag...)
	_, err := w.Write(buf)
	return err
}

func readTag(r io.Reader) (string, error) {
	lenBuf := make([]byte, 1)
	if _, err := io.ReadFull(r, lenBuf); err != nil {
		return "", err
	}
	n := int(lenBuf[0])
	if n == 0 {
		return "", errors.New("zero tag length")
	}
	tagBuf := make([]byte, n)
	if _, err := io.ReadFull(r, tagBuf); err != nil {
		return "", err
	}
	return string(tagBuf), nil
}

// ---------- TCP listener (client side of a stream name) ----------

// TCPListener binds 127.0.0.1 TCP listeners for each ListenSpec. Each
// inbound TCP connection opens a tagged stream on sess and pumps bytes.
type TCPListener struct {
	sess      Session
	specs     []ListenSpec
	log       *slog.Logger
	listeners map[string]*net.TCPListener
	bytesIn   atomic.Uint64
	bytesOut  atomic.Uint64
	closed    atomic.Bool
	wg        sync.WaitGroup
}

func NewTCPListener(sess Session, specs []ListenSpec, log *slog.Logger) *TCPListener {
	return &TCPListener{
		sess:      sess,
		specs:     specs,
		log:       log,
		listeners: map[string]*net.TCPListener{},
	}
}

// Start binds all listeners. Returns error if any bind fails (after
// cleaning up the ones that succeeded).
func (l *TCPListener) Start() error {
	for _, spec := range l.specs {
		addr := "127.0.0.1:" + strconv.Itoa(spec.LocalPort)
		tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
		if err != nil {
			l.Close()
			return fmt.Errorf("resolve %s: %w", addr, err)
		}
		ln, err := net.ListenTCP("tcp", tcpAddr)
		if err != nil {
			l.Close()
			return fmt.Errorf("listen %s: %w", addr, err)
		}
		l.listeners[spec.Name] = ln
		l.log.Info("tcp listener bound",
			"name", spec.Name,
			"addr", ln.Addr().String(),
			"remote_port", spec.RemotePort)

		l.wg.Add(1)
		go l.acceptLoop(spec.Name, ln)
	}
	return nil
}

// BoundPorts returns the actual local port for each named listener (useful
// when LocalPort=0 was passed).
func (l *TCPListener) BoundPorts() map[string]int {
	out := map[string]int{}
	for name, ln := range l.listeners {
		out[name] = ln.Addr().(*net.TCPAddr).Port
	}
	return out
}

func (l *TCPListener) acceptLoop(name string, ln *net.TCPListener) {
	defer l.wg.Done()
	for {
		c, err := ln.Accept()
		if err != nil {
			if l.closed.Load() {
				return
			}
			l.log.Warn("tcp accept failed", "name", name, "err", err)
			return
		}
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			l.handle(name, c)
		}()
	}
}

func (l *TCPListener) handle(name string, c net.Conn) {
	defer c.Close()
	s, err := l.sess.OpenStream()
	if err != nil {
		l.log.Warn("open stream failed", "name", name, "err", err)
		return
	}
	defer s.Close()

	if err := writeTag(s, name); err != nil {
		l.log.Warn("write tag failed", "name", name, "err", err)
		return
	}
	pump(c, s, &l.bytesOut, &l.bytesIn)
}

func (l *TCPListener) Close() error {
	if !l.closed.CompareAndSwap(false, true) {
		return nil
	}
	for _, ln := range l.listeners {
		ln.Close()
	}
	// Best-effort wait; caller normally exits the process after Close.
	done := make(chan struct{})
	go func() { l.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	return nil
}

// Stats returns cumulative byte counters for this listener-side forwarder.
func (l *TCPListener) Stats() (in, out uint64) {
	return l.bytesIn.Load(), l.bytesOut.Load()
}

// ---------- Accept router (server side of a stream name) ----------

// AcceptRouter pulls inbound streams off sess, reads the tag, and dials
// the matching local TCP port.
type AcceptRouter struct {
	sess     Session
	dials    map[string]int // name → local port
	log      *slog.Logger
	bytesIn  atomic.Uint64
	bytesOut atomic.Uint64
	closed   atomic.Bool
	wg       sync.WaitGroup
}

func NewAcceptRouter(sess Session, dials map[string]int, log *slog.Logger) *AcceptRouter {
	return &AcceptRouter{sess: sess, dials: dials, log: log}
}

// Run blocks until the session errors out or Close is called.
func (a *AcceptRouter) Run() {
	for {
		s, err := a.sess.AcceptStream()
		if err != nil {
			if !a.closed.Load() {
				a.log.Info("accept stream stopped", "err", err)
			}
			return
		}
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.handle(s)
		}()
	}
}

func (a *AcceptRouter) handle(s net.Conn) {
	defer s.Close()
	// Tag header must arrive within a short window; peer is misbehaving
	// otherwise. Keep it generous enough to tolerate slow ICE links.
	s.SetReadDeadline(time.Now().Add(10 * time.Second))
	tag, err := readTag(s)
	if err != nil {
		a.log.Warn("read tag failed", "err", err)
		return
	}
	s.SetReadDeadline(time.Time{})

	port, ok := a.dials[tag]
	if !ok {
		a.log.Warn("unknown stream tag", "tag", tag)
		return
	}
	addr := "127.0.0.1:" + strconv.Itoa(port)
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		a.log.Warn("local dial failed", "tag", tag, "addr", addr, "err", err)
		return
	}
	defer c.Close()
	pump(s, c, &a.bytesIn, &a.bytesOut)
}

// Close stops the router. Safe to call multiple times.
func (a *AcceptRouter) Close() error {
	a.closed.Store(true)
	// We can't interrupt AcceptStream without closing the session, which
	// is the caller's responsibility. Best-effort wait for in-flight streams.
	done := make(chan struct{})
	go func() { a.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	return nil
}

// Stats returns cumulative byte counters for this router-side forwarder.
func (a *AcceptRouter) Stats() (in, out uint64) {
	return a.bytesIn.Load(), a.bytesOut.Load()
}

// ---------- bidirectional byte pump ----------

// pump copies bytes in both directions between a and b until either side
// closes. Counters track bytes received from each side.
//
// When one direction's source goes EOF we full-close both conns. yamux
// streams don't support half-close, so propagating an orderly shutdown
// via the stream always means closing it; losing the theoretical "TCP
// half-close after request" behavior is fine for our workloads (NL RPC,
// Redis, CAS) — clients close after reading the response anyway.
func pump(a, b net.Conn, aToB, bToA *atomic.Uint64) {
	done := make(chan struct{}, 2)
	closeBoth := func() {
		a.Close()
		b.Close()
	}
	go func() {
		n, _ := io.Copy(b, a)
		aToB.Add(uint64(n))
		closeBoth()
		done <- struct{}{}
	}()
	go func() {
		n, _ := io.Copy(a, b)
		bToA.Add(uint64(n))
		closeBoth()
		done <- struct{}{}
	}()
	<-done
	<-done
}
