// Package transport carries many concurrent bidirectional streams between
// two peers over a single UDP PacketConn using N parallel QUIC connections.
//
// One peer calls Serve (passive), the other calls Dial. The choice is
// arbitrary but must agree; nl-punch uses peer-a for Serve.
//
// Why N parallel QUIC connections instead of one? Each QUIC connection has
// its own congestion window. On a high-RTT lossy WAN, a single connection
// hits cwnd-bound throughput surprisingly fast (~14 KiB/s observed on the
// .166↔.132 path). N independent cwnds in parallel do not interfere with
// each other; aggregate throughput scales close to linearly with N until
// the actual link is saturated. We round-robin OpenStream() across the
// connections and fan-in AcceptStream() from all of them.
package transport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
)

// alpn is the ALPN identifier our peers handshake with.
const alpn = "nl-punch/1"

// numSubconns is how many parallel QUIC connections share one ICE
// PacketConn. Each gets an independent cwnd; aggregate throughput
// scales close to linearly until the wire is saturated. 4 is a
// pragmatic default — enough headroom on a typical asymmetric WAN
// without burning extra handshake CPU at startup.
const numSubconns = 4

// Session is one multiplexed tunnel between two peers, fanned across
// N parallel QUIC connections.
type Session struct {
	conns []*quic.Conn
	pc    net.PacketConn // borrowed; not closed here
	bufPC *bufferedPacketConn
	tr    *quic.Transport

	// Round-robin counter for OpenStream.
	next atomic.Uint64

	// Fan-in for AcceptStream: one goroutine per subconn pumps incoming
	// streams into acceptCh.
	acceptCh chan acceptResult
	acceptWG sync.WaitGroup
	closed   atomic.Bool
}

type acceptResult struct {
	stream net.Conn
	err    error
	conn   *quic.Conn // for LocalAddr/RemoteAddr
}

// bufferedPacketConn drains the underlying PacketConn into a large
// in-process queue and serves ReadFrom from that queue. quic-go can't
// tune the OS receive buffer on our pion/ICE PacketConn (it isn't a
// *net.UDPConn), so without this shim the read goroutine starves and
// throughput collapses.
type bufferedPacketConn struct {
	net.PacketConn
	queue chan packet
	done  chan struct{}
}

type packet struct {
	data []byte
	addr net.Addr
}

func newBufferedPacketConn(pc net.PacketConn) *bufferedPacketConn {
	b := &bufferedPacketConn{
		PacketConn: pc,
		queue:      make(chan packet, 4096),
		done:       make(chan struct{}),
	}
	go b.drain()
	return b
}

func (b *bufferedPacketConn) drain() {
	// 64 KiB is the UDP datagram payload max (65507 actual). Big enough
	// for any QUIC packet size we configure, including the 32 KiB
	// fragmenting datagrams in quicConfig().
	buf := make([]byte, 64*1024)
	for {
		n, addr, err := b.PacketConn.ReadFrom(buf)
		if err != nil {
			close(b.done)
			return
		}
		p := packet{data: make([]byte, n), addr: addr}
		copy(p.data, buf[:n])
		select {
		case b.queue <- p:
		default:
			// Queue full: drop. Better than blocking the drain loop.
		}
	}
}

func (b *bufferedPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	select {
	case pkt, ok := <-b.queue:
		if !ok {
			return 0, nil, net.ErrClosed
		}
		n := copy(p, pkt.data)
		return n, pkt.addr, nil
	case <-b.done:
		return 0, nil, net.ErrClosed
	}
}

func quicConfig() *quic.Config {
	return &quic.Config{
		KeepAlivePeriod: 1 * time.Second,
		MaxIdleTimeout:  5 * time.Minute,
		// Generous initial windows so a lossy WAN doesn't bottleneck
		// before the auto-tuner has caught up.
		InitialStreamReceiveWindow:     4 * 1024 * 1024,
		MaxStreamReceiveWindow:         16 * 1024 * 1024,
		InitialConnectionReceiveWindow: 8 * 1024 * 1024,
		MaxConnectionReceiveWindow:     64 * 1024 * 1024,
		MaxIncomingStreams:             1024,
		MaxIncomingUniStreams:          1024,
		EnableDatagrams:                false,
		// Let quic-go's PMTU discovery probe upward from its safe 1252
		// default. Tried fixed sizes (1452 / 4K / 8K / 32K) — anything
		// above the path MTU got dropped by pion's iceConn writer
		// (silently truncated, QUIC thought it sent), and even 1452
		// failed handshake on this WAN, suggesting path MTU is below
		// 1500 (VPN / PPPoE / similar overhead somewhere). PMTUD will
		// land on whatever the path actually carries.
	}
}

func generateSelfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("ecdsa key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "nl-punch"},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create cert: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("marshal key: %w", err)
	}
	return tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
}

func serverTLSConfig() (*tls.Config, error) {
	cert, err := generateSelfSignedCert()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{alpn},
		MinVersion:   tls.VersionTLS13,
	}, nil
}

func clientTLSConfig() *tls.Config {
	return &tls.Config{
		// The path is already authenticated by signaling shared_secret +
		// ICE; the QUIC TLS layer here is just for key exchange and
		// confidentiality. No PKI to verify against, so we don't pin.
		InsecureSkipVerify: true,
		NextProtos:         []string{alpn},
		MinVersion:         tls.VersionTLS13,
	}
}

// Serve wraps an existing PacketConn as the passive side of a session.
// It blocks until N peer connections complete the QUIC handshake.
func Serve(pc net.PacketConn) (*Session, error) {
	tlsConf, err := serverTLSConfig()
	if err != nil {
		return nil, err
	}
	bufPC := newBufferedPacketConn(pc)
	tr := &quic.Transport{Conn: bufPC}
	lst, err := tr.Listen(tlsConf, quicConfig())
	if err != nil {
		return nil, fmt.Errorf("quic listen: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Sequential Accept is fine: the peer dials all N in parallel, so
	// they queue at the listener; we just drain the queue.
	conns := make([]*quic.Conn, 0, numSubconns)
	for i := 0; i < numSubconns; i++ {
		c, err := lst.Accept(ctx)
		if err != nil {
			lst.Close()
			for _, cc := range conns {
				cc.CloseWithError(0, "abort")
			}
			return nil, fmt.Errorf("quic accept #%d: %w", i, err)
		}
		conns = append(conns, c)
	}
	lst.Close()
	return newSession(conns, pc, bufPC, tr), nil
}

// Dial wraps an existing PacketConn as the active side of a session.
// raddr is the peer's UDP address. It opens N parallel QUIC connections
// concurrently (sequential dialing would multiply the TLS-handshake RTT
// cost N-fold).
func Dial(pc net.PacketConn, raddr string) (*Session, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", raddr)
	if err != nil {
		return nil, fmt.Errorf("resolve raddr: %w", err)
	}
	bufPC := newBufferedPacketConn(pc)
	tr := &quic.Transport{Conn: bufPC}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	// Sequential dial. Parallel dials over a single shared PacketConn
	// caused 3-of-4 handshakes to time out on the cluster: pion's ICE
	// path can't handle the burst when 4 QUIC Initial flights race for
	// the wire simultaneously. ~1.5 s per handshake × 4 ≈ 6 s startup
	// is acceptable; reconnect is rare.
	conns := make([]*quic.Conn, 0, numSubconns)
	for i := 0; i < numSubconns; i++ {
		c, err := tr.Dial(ctx, udpAddr, clientTLSConfig(), quicConfig())
		if err != nil {
			for _, cc := range conns {
				cc.CloseWithError(0, "abort")
			}
			return nil, fmt.Errorf("quic dial #%d: %w", i, err)
		}
		conns = append(conns, c)
	}
	return newSession(conns, pc, bufPC, tr), nil
}

func newSession(conns []*quic.Conn, pc net.PacketConn, bufPC *bufferedPacketConn, tr *quic.Transport) *Session {
	s := &Session{
		conns:    conns,
		pc:       pc,
		bufPC:    bufPC,
		tr:       tr,
		acceptCh: make(chan acceptResult, 64),
	}
	for _, c := range conns {
		s.acceptWG.Add(1)
		go s.acceptLoop(c)
	}
	return s
}

func (s *Session) acceptLoop(c *quic.Conn) {
	defer s.acceptWG.Done()
	for {
		st, err := c.AcceptStream(context.Background())
		if err != nil {
			if !s.closed.Load() {
				select {
				case s.acceptCh <- acceptResult{err: err, conn: c}:
				default:
				}
			}
			return
		}
		conn := &quicStreamConn{Stream: st, local: c.LocalAddr(), remote: c.RemoteAddr()}
		select {
		case s.acceptCh <- acceptResult{stream: conn, conn: c}:
		case <-c.Context().Done():
			return
		}
	}
}

// quicStreamConn adapts quic.Stream to net.Conn.
type quicStreamConn struct {
	*quic.Stream
	local  net.Addr
	remote net.Addr
}

func (q *quicStreamConn) LocalAddr() net.Addr  { return q.local }
func (q *quicStreamConn) RemoteAddr() net.Addr { return q.remote }

// OpenStream round-robins across the parallel QUIC connections so that
// concurrent streams spread their cwnd pressure rather than queueing
// behind one bottleneck cwnd.
func (s *Session) OpenStream() (net.Conn, error) {
	i := s.next.Add(1) % uint64(len(s.conns))
	c := s.conns[i]
	st, err := c.OpenStreamSync(context.Background())
	if err != nil {
		return nil, err
	}
	return &quicStreamConn{Stream: st, local: c.LocalAddr(), remote: c.RemoteAddr()}, nil
}

// AcceptStream waits for the peer to open an inbound logical stream on
// any of the parallel QUIC connections.
func (s *Session) AcceptStream() (net.Conn, error) {
	r, ok := <-s.acceptCh
	if !ok {
		return nil, net.ErrClosed
	}
	if r.err != nil {
		return nil, r.err
	}
	return r.stream, nil
}

// NumActiveStreams returns 0; quic-go doesn't expose live counts and
// forward.go owns the byte stats that matter for ops.
func (s *Session) NumActiveStreams() int { return 0 }

// Wait blocks until any subconnection is shut down. We treat the failure
// of one subconn as the whole session ending — the reconnect loop will
// rebuild all N from scratch, which is correct because connection IDs
// don't survive a peer restart anyway.
func (s *Session) Wait() <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		cases := make([]<-chan struct{}, len(s.conns))
		for i, c := range s.conns {
			cases[i] = c.Context().Done()
		}
		// Block on the first that fires.
		done := make(chan struct{}, len(cases))
		for _, d := range cases {
			d := d
			go func() {
				<-d
				done <- struct{}{}
			}()
		}
		<-done
		close(ch)
	}()
	return ch
}

// Close tears down all subconnections.
func (s *Session) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	for _, c := range s.conns {
		_ = c.CloseWithError(0, "session closed")
	}
	// Drain the accept channel so any blocked acceptLoop goroutines exit.
	go func() {
		for range s.acceptCh {
		}
	}()
	s.acceptWG.Wait()
	close(s.acceptCh)
	return nil
}
