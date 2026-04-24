// Package transport wraps kcp-go (reliable UDP) + yamux (stream multiplex)
// into a Session that carries many concurrent bidirectional streams over
// a single UDP PacketConn.
//
// One peer calls Serve (passive, yamux server), the other calls Dial
// (active, yamux client). The choice is arbitrary but must agree; in
// nl-punch we use role = peer-a for Serve.
package transport

import (
	"fmt"
	"net"
	"time"

	"github.com/hashicorp/yamux"
	kcp "github.com/xtaci/kcp-go/v5"
)

// Session is one multiplexed tunnel between two peers.
type Session struct {
	kcp   net.Conn    // raw kcp session
	mux   *yamux.Session
	lst   *kcp.Listener // only set on Serve side, for cleanup
	owned net.PacketConn // only set when we should close it ourselves
}

// yamuxConfig returns our tuned yamux config. Keepalive is enabled with a
// short interval because yamux Pings are our only guaranteed outbound
// signal when one side is read-only (e.g. during a bulk server→client
// transfer). In Round F we observed that a peer sitting quiet for >1 s
// had its pion/ICE receive path stall, starving the inbound KCP stream;
// a 1 s Ping cadence keeps outbound UDP flowing from every peer at all
// times and restored server→client flow. ConnectionWriteTimeout stays
// generous so a single stalled Ping doesn't take the session down.
func yamuxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.EnableKeepAlive = true
	c.KeepAliveInterval = 1 * time.Second
	c.ConnectionWriteTimeout = 60 * time.Second
	return c
}

func applyProfile(conn *kcp.UDPSession, p KCPProfile) {
	conn.SetNoDelay(p.NoDelay, p.Interval, p.Resend, p.NC)
	conn.SetWindowSize(p.SendWindow, p.RecvWindow)
	if p.MTU > 0 {
		conn.SetMtu(p.MTU)
	}
	// Disable the stream-mode coalescer: we already get stream multiplexing
	// from yamux, and stream-mode's write coalescing adds latency.
	conn.SetStreamMode(true)
	conn.SetACKNoDelay(p.NC == 1)
}

// Serve wraps an existing PacketConn as the passive side of a session.
// It blocks until one peer connects and the yamux handshake succeeds.
func Serve(pc net.PacketConn, p KCPProfile) (*Session, error) {
	// No block cipher, no FEC — we rely on the ICE path already being
	// a closed peer-to-peer pair; encryption is future work.
	lst, err := kcp.ServeConn(nil, 0, 0, pc)
	if err != nil {
		return nil, fmt.Errorf("kcp.ServeConn: %w", err)
	}
	kSess, err := lst.AcceptKCP()
	if err != nil {
		lst.Close()
		return nil, fmt.Errorf("kcp accept: %w", err)
	}
	applyProfile(kSess, p)

	mux, err := yamux.Server(kSess, yamuxConfig())
	if err != nil {
		kSess.Close()
		lst.Close()
		return nil, fmt.Errorf("yamux server: %w", err)
	}
	return &Session{kcp: kSess, mux: mux, lst: lst}, nil
}

// Dial wraps an existing PacketConn as the active side of a session.
// raddr is the peer's UDP address (as seen on this PacketConn).
func Dial(pc net.PacketConn, raddr string, p KCPProfile) (*Session, error) {
	kSess, err := kcp.NewConn(raddr, nil, 0, 0, pc)
	if err != nil {
		return nil, fmt.Errorf("kcp.NewConn: %w", err)
	}
	applyProfile(kSess, p)

	mux, err := yamux.Client(kSess, yamuxConfig())
	if err != nil {
		kSess.Close()
		return nil, fmt.Errorf("yamux client: %w", err)
	}
	return &Session{kcp: kSess, mux: mux}, nil
}

// OpenStream starts a new outbound logical stream.
func (s *Session) OpenStream() (net.Conn, error) {
	return s.mux.OpenStream()
}

// AcceptStream waits for the peer to open an inbound logical stream.
func (s *Session) AcceptStream() (net.Conn, error) {
	return s.mux.AcceptStream()
}

// NumActiveStreams returns current open stream count (for stats logging).
func (s *Session) NumActiveStreams() int {
	return s.mux.NumStreams()
}

// Wait blocks until the session is shut down (keepalive failure, peer close).
func (s *Session) Wait() <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		<-s.mux.CloseChan()
		close(ch)
	}()
	return ch
}

// Close tears down everything this session owns.
func (s *Session) Close() error {
	err := s.mux.Close()
	if s.kcp != nil {
		s.kcp.Close()
	}
	if s.lst != nil {
		s.lst.Close()
	}
	if s.owned != nil {
		s.owned.Close()
	}
	return err
}
