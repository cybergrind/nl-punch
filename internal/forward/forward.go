// Package forward bridges TCP listeners to multiplexed streams and vice
// versa. A Session is any object with OpenStream/AcceptStream methods,
// typically transport.Session but decoupled so tests can use raw yamux
// or quic.
//
// Wire format on each stream (header):
//
//	byte 0     : tag length N (1..255)
//	bytes 1..N : tag bytes (ASCII, e.g. "http")
//	bytes N+1..N+8 : 64-bit BE session id (per TCP connection)
//	byte  N+9    : stripe index (0..numStripes-1)
//	byte  N+10   : numStripes (1 = unstriped, 2..255 = striped fan-out)
//
// After the header, striped streams carry length-prefixed chunks:
//
//	bytes 0..3 : 32-bit BE chunk length L
//	bytes 0..3 : 32-bit BE chunk seq within this stripe
//	bytes 8..  : L bytes of payload
//
// Unstriped streams (numStripes=1) carry raw TCP bytes after the header
// for backward-compat with single-stream forwarders.
package forward

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// numStripes is how many parallel streams one TCP connection is sliced
// across. 1 = no striping; one TCP → one stream.
//
// Striping was tried at numStripes=4 in two modes:
//  1. Round-robin chunk dispatch: HOL-blocks on the slowest stream;
//     30% throughput regression vs un-striped on the cluster bench.
//  2. Work-stealing chunk dispatch: 0 bytes delivered on cluster (still
//     debugging — the receiver-side TCP pipe got broken, suspect a
//     race in acceptRouter session-id pairing under packet loss).
//
// Concurrent TCP connections still benefit from multi-cwnd via the
// transport-layer Session.OpenStream() round-robin across N=4 QUIC
// sub-connections (transport.go); single-TCP-stripes-bytes is a net
// loss on this WAN.
const numStripes = 1

// chunkSize is the max payload per stripe chunk; only used when
// numStripes > 1.
const chunkSize = 64 * 1024

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

// ---------- header framing ----------

const maxTagLen = 255

// streamHeader is the per-stream framing: tag + striped-session metadata.
type streamHeader struct {
	tag         string
	sessionID   uint64 // 0 = unstriped, otherwise groups N stripes
	stripeIdx   uint8
	numStripes  uint8
}

func writeStreamHeader(w io.Writer, h streamHeader) error {
	if len(h.tag) == 0 || len(h.tag) > maxTagLen {
		return fmt.Errorf("invalid tag length %d", len(h.tag))
	}
	buf := make([]byte, 0, 1+len(h.tag)+8+1+1)
	buf = append(buf, byte(len(h.tag)))
	buf = append(buf, h.tag...)
	var sid [8]byte
	binary.BigEndian.PutUint64(sid[:], h.sessionID)
	buf = append(buf, sid[:]...)
	buf = append(buf, h.stripeIdx, h.numStripes)
	_, err := w.Write(buf)
	return err
}

func readStreamHeader(r io.Reader) (streamHeader, error) {
	var h streamHeader
	lenBuf := make([]byte, 1)
	if _, err := io.ReadFull(r, lenBuf); err != nil {
		return h, err
	}
	n := int(lenBuf[0])
	if n == 0 {
		return h, errors.New("zero tag length")
	}
	tagBuf := make([]byte, n)
	if _, err := io.ReadFull(r, tagBuf); err != nil {
		return h, err
	}
	h.tag = string(tagBuf)
	var meta [10]byte
	if _, err := io.ReadFull(r, meta[:]); err != nil {
		return h, err
	}
	h.sessionID = binary.BigEndian.Uint64(meta[:8])
	h.stripeIdx = meta[8]
	h.numStripes = meta[9]
	if h.numStripes == 0 {
		return h, errors.New("zero stripe count")
	}
	return h, nil
}

// ---------- striping codec ----------

// chunkHdr is 8 bytes: 4-byte length, 4-byte seq.
const chunkHdrSize = 8

// readChunk reads one length-prefixed chunk from a striped stream.
// Returns (seq, payload, err). io.EOF is returned cleanly.
func readChunk(r io.Reader) (uint32, []byte, error) {
	var hdr [chunkHdrSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint32(hdr[0:4])
	seq := binary.BigEndian.Uint32(hdr[4:8])
	if length == 0 {
		return seq, nil, nil // zero-length chunk = keepalive / heartbeat
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return seq, nil, err
	}
	return seq, buf, nil
}

func writeChunk(w io.Writer, seq uint32, payload []byte) error {
	var hdr [chunkHdrSize]byte
	binary.BigEndian.PutUint32(hdr[0:4], uint32(len(payload)))
	binary.BigEndian.PutUint32(hdr[4:8], seq)
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
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

	// Open numStripes parallel streams. Each lands on a different
	// transport sub-connection (round-robin in transport.Session.OpenStream)
	// so each gets its own QUIC cwnd. The session id ties them together
	// on the receiving side.
	sessionID := rand.Uint64()
	if sessionID == 0 {
		sessionID = 1 // 0 reserved for "unstriped"
	}
	streams := make([]net.Conn, 0, numStripes)
	for i := 0; i < numStripes; i++ {
		s, err := l.sess.OpenStream()
		if err != nil {
			l.log.Warn("open stripe failed", "name", name, "stripe", i, "err", err)
			for _, ss := range streams {
				ss.Close()
			}
			return
		}
		hdr := streamHeader{
			tag:        name,
			sessionID:  sessionID,
			stripeIdx:  uint8(i),
			numStripes: uint8(numStripes),
		}
		if err := writeStreamHeader(s, hdr); err != nil {
			l.log.Warn("write header failed", "name", name, "stripe", i, "err", err)
			for _, ss := range streams {
				ss.Close()
			}
			s.Close()
			return
		}
		streams = append(streams, s)
	}

	pumpStriped(c, streams, &l.bytesOut, &l.bytesIn)
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

// AcceptRouter pulls inbound streams off sess, reads the header, groups
// stripes belonging to the same TCP-conn session, and dials the matching
// local TCP port once all stripes for a session arrive.
type AcceptRouter struct {
	sess     Session
	dials    map[string]int // name → local port
	log      *slog.Logger
	bytesIn  atomic.Uint64
	bytesOut atomic.Uint64
	closed   atomic.Bool
	wg       sync.WaitGroup

	groupsMu sync.Mutex
	groups   map[uint64]*stripeGroup
}

// stripeGroup collects N stripes belonging to one TCP-conn session. Once
// all numStripes streams arrive, the dial fires and pumpStriped takes over.
type stripeGroup struct {
	tag        string
	numStripes uint8
	streams    []net.Conn
	ready      chan struct{}
}

func NewAcceptRouter(sess Session, dials map[string]int, log *slog.Logger) *AcceptRouter {
	return &AcceptRouter{
		sess:   sess,
		dials:  dials,
		log:    log,
		groups: map[uint64]*stripeGroup{},
	}
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
	// Header must arrive within a short window.
	s.SetReadDeadline(time.Now().Add(15 * time.Second))
	hdr, err := readStreamHeader(s)
	if err != nil {
		a.log.Warn("read header failed", "err", err)
		s.Close()
		return
	}
	s.SetReadDeadline(time.Time{})

	port, ok := a.dials[hdr.tag]
	if !ok {
		a.log.Warn("unknown stream tag", "tag", hdr.tag)
		s.Close()
		return
	}

	// Find or create the stripe group for this session id.
	a.groupsMu.Lock()
	g, exists := a.groups[hdr.sessionID]
	if !exists {
		g = &stripeGroup{
			tag:        hdr.tag,
			numStripes: hdr.numStripes,
			streams:    make([]net.Conn, hdr.numStripes),
			ready:      make(chan struct{}),
		}
		a.groups[hdr.sessionID] = g
	}
	if int(hdr.stripeIdx) >= len(g.streams) {
		a.groupsMu.Unlock()
		a.log.Warn("stripe index out of range", "idx", hdr.stripeIdx, "n", len(g.streams))
		s.Close()
		return
	}
	g.streams[hdr.stripeIdx] = s
	complete := allReady(g.streams)
	if complete {
		delete(a.groups, hdr.sessionID)
	}
	a.groupsMu.Unlock()

	if !complete {
		// Wait for siblings; the goroutine that completes the group is the
		// one that owns the dial + pump.
		<-g.ready
		return
	}

	close(g.ready)

	addr := "127.0.0.1:" + strconv.Itoa(port)
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		a.log.Warn("local dial failed", "tag", hdr.tag, "addr", addr, "err", err)
		for _, st := range g.streams {
			st.Close()
		}
		return
	}
	defer c.Close()

	// On the receiving side: stripes carry tunnel→TCP bytes (a→c is "in"),
	// TCP→stripes carries client request bytes (c→a is "out").
	pumpStripedReceiver(c, g.streams, &a.bytesIn, &a.bytesOut)
}

func allReady(streams []net.Conn) bool {
	for _, s := range streams {
		if s == nil {
			return false
		}
	}
	return true
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

// ---------- striped bidirectional byte pump ----------

// pumpStriped runs on the LISTENER side: TCP conn `c` is the bench/CAS
// client. We chunk c→stripes for the upload direction and reassemble
// stripes→c for the download direction. tcpToTun and tunToTcp are byte
// counters.
func pumpStriped(c net.Conn, streams []net.Conn, tcpToTun, tunToTcp *atomic.Uint64) {
	closeAll := func() {
		c.Close()
		for _, s := range streams {
			s.Close()
		}
	}

	done := make(chan struct{}, 2)
	// TCP → stripes: round-robin chunks, each stripe gets its own seq.
	go func() {
		writeStripes(c, streams, tcpToTun)
		closeAll()
		done <- struct{}{}
	}()
	// stripes → TCP: reassemble in global seq order.
	go func() {
		readStripesToTCP(streams, c, tunToTcp)
		closeAll()
		done <- struct{}{}
	}()
	<-done
	<-done
}

// pumpStripedReceiver runs on the ACCEPT side: TCP conn `c` is the local
// service we just dialed. Symmetric to pumpStriped: stripes→c is the
// "tunnel-in" direction, c→stripes is "tunnel-out".
func pumpStripedReceiver(c net.Conn, streams []net.Conn, tunToTcp, tcpToTun *atomic.Uint64) {
	closeAll := func() {
		c.Close()
		for _, s := range streams {
			s.Close()
		}
	}
	done := make(chan struct{}, 2)
	go func() {
		readStripesToTCP(streams, c, tunToTcp)
		closeAll()
		done <- struct{}{}
	}()
	go func() {
		writeStripes(c, streams, tcpToTun)
		closeAll()
		done <- struct{}{}
	}()
	<-done
	<-done
}

// writeStripes reads chunks from src, tags each with a monotonic seq,
// and dispatches via an unbuffered channel to N writer goroutines (one
// per stream). Whichever stream's writer pulls from the channel first
// gets the chunk — natural work-stealing. Slow streams pick up fewer
// chunks; fast streams more.
func writeStripes(src io.Reader, streams []net.Conn, counter *atomic.Uint64) {
	type chunk struct {
		seq     uint32
		payload []byte
	}
	// Unbuffered: each pull happens only when a stream is actually ready
	// to write, so no premature commitment of a chunk to a slow stream.
	dispatch := make(chan chunk)

	// N writer goroutines.
	var wg sync.WaitGroup
	for _, s := range streams {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range dispatch {
				if err := writeChunk(s, c.seq, c.payload); err != nil {
					return
				}
			}
		}()
	}

	// Reader → dispatcher.
	buf := make([]byte, chunkSize)
	var seq uint32
	for {
		n, err := src.Read(buf)
		if n > 0 {
			payload := make([]byte, n)
			copy(payload, buf[:n])
			dispatch <- chunk{seq: seq, payload: payload}
			counter.Add(uint64(n))
			seq++
		}
		if err != nil {
			break
		}
	}
	close(dispatch)
	wg.Wait()

	// Send a zero-length EOF marker on every stream so each receiver
	// reader knows this side is done. We send these *after* all data
	// has been flushed.
	for _, s := range streams {
		_ = writeChunk(s, seq, nil)
	}
}

// readStripesToTCP reads chunks from N stripes in parallel, reassembles
// them by global seq number, and writes the in-order bytes to dst.
//
// Reassembly uses a small reorder buffer. The sender writes in
// round-robin order (seq 0 to stripe 0, seq 1 to stripe 1, ...), so any
// individual stripe sees a strictly-increasing seq subset. When all N
// stripes report the next seq we expect, we flush; if some stripes are
// running ahead, their chunks land in the buffer and wait.
func readStripesToTCP(streams []net.Conn, dst io.Writer, counter *atomic.Uint64) {
	type stripeChunk struct {
		seq     uint32
		payload []byte
		stripe  int
		eof     bool
	}
	in := make(chan stripeChunk, len(streams)*8)

	var wg sync.WaitGroup
	for i, s := range streams {
		i, s := i, s
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				seq, payload, err := readChunk(s)
				if len(payload) == 0 && err == nil {
					// terminator
					in <- stripeChunk{seq: seq, stripe: i, eof: true}
					return
				}
				if err != nil {
					in <- stripeChunk{stripe: i, eof: true}
					return
				}
				in <- stripeChunk{seq: seq, payload: payload, stripe: i}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(in)
	}()

	// Reorder buffer: map seq → chunk.
	pending := map[uint32][]byte{}
	var nextSeq uint32
	eofs := 0
	for c := range in {
		if c.eof {
			eofs++
			if eofs == len(streams) {
				break
			}
			continue
		}
		if c.seq == nextSeq {
			if _, err := dst.Write(c.payload); err != nil {
				return
			}
			counter.Add(uint64(len(c.payload)))
			nextSeq++
			// Drain any sequential pending chunks.
			for {
				p, ok := pending[nextSeq]
				if !ok {
					break
				}
				delete(pending, nextSeq)
				if _, err := dst.Write(p); err != nil {
					return
				}
				counter.Add(uint64(len(p)))
				nextSeq++
			}
		} else {
			pending[c.seq] = c.payload
		}
	}
	// Drain any final in-order remainder.
	for {
		p, ok := pending[nextSeq]
		if !ok {
			break
		}
		delete(pending, nextSeq)
		if _, err := dst.Write(p); err != nil {
			return
		}
		counter.Add(uint64(len(p)))
		nextSeq++
	}
}
