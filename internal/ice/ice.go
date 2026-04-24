// Package ice drives a pion/ice agent through the signaling rendezvous:
// gather candidates, exchange via signaling, start connectivity checks,
// hand back a *ice.Conn ready for a transport layer to wrap.
//
// Role convention: peer-a is ICE controlling, peer-b is controlled. Both
// sides post their own offer under their role and fetch the peer's offer
// under the other role; the controlled side additionally posts an answer
// the controlling side reads. We split offer/answer only because the
// ICE spec separates them; structurally they carry identical fields.
package ice

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	pionice "github.com/pion/ice/v3"
	pionlog "github.com/pion/logging"
	"github.com/pion/stun/v2"

	"nl-punch/internal/signaling"
)

// Config bundles everything needed to run one ICE session.
type Config struct {
	Signaling *signaling.Client
	SessionID string
	Role      string // "peer-a" (controlling) or "peer-b" (controlled)
	Logger    *slog.Logger

	// STUN servers in host:port form; converted to stun: URIs internally.
	STUN []string
	// TURN servers — optional; list of "turn:host:port?..." or parsed form.
	TURN []TURNServer

	// PollInterval for signaling GETs. Default 2s.
	PollInterval time.Duration

	// FailedTimeout is how long pion keeps running checks before giving up.
	// Default 25s; bump to 60s for high-loss / VPN paths.
	FailedTimeout time.Duration

	// DisconnectedTimeout defaults to 5s; 0 keeps the agent from going
	// disconnected on brief packet loss (useful during slow check convergence).
	DisconnectedTimeout time.Duration

	// PionLogLevel: 0=disabled, 1=error, 2=warn, 3=info, 4=debug, 5=trace.
	// Non-zero redirects pion's internal logs to Config.Logger's writer.
	PionLogLevel int
}

type TURNServer struct {
	URI  string
	User string
	Pass string
}

// Agent wraps our ICE + signaling logic. Reusable across reconnect cycles.
type Agent struct {
	cfg Config
}

func New(cfg Config) *Agent {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Agent{cfg: cfg}
}

// IsControlling reports whether this agent is the controlling ICE peer.
func (a *Agent) IsControlling() bool { return a.cfg.Role == "peer-a" }

// Connect runs one full ICE handshake and returns the active connection.
// Blocks until ctx is done or the handshake completes.
func (a *Agent) Connect(ctx context.Context) (*pionice.Conn, error) {
	urls, err := parseURLs(a.cfg.STUN, a.cfg.TURN)
	if err != nil {
		return nil, err
	}

	agentCfg := &pionice.AgentConfig{
		Urls: urls,
		NetworkTypes: []pionice.NetworkType{
			pionice.NetworkTypeUDP4,
			pionice.NetworkTypeUDP6,
		},
		// Default candidate types (host + srflx + relay as configured).
	}
	if a.cfg.FailedTimeout > 0 {
		t := a.cfg.FailedTimeout
		agentCfg.FailedTimeout = &t
	}
	if a.cfg.DisconnectedTimeout > 0 {
		t := a.cfg.DisconnectedTimeout
		agentCfg.DisconnectedTimeout = &t
	}
	if a.cfg.PionLogLevel > 0 {
		lf := &pionlog.DefaultLoggerFactory{
			Writer:          os.Stderr,
			DefaultLogLevel: pionlog.LogLevel(a.cfg.PionLogLevel),
		}
		agentCfg.LoggerFactory = lf
	}

	agent, err := pionice.NewAgent(agentCfg)
	if err != nil {
		return nil, fmt.Errorf("new ice agent: %w", err)
	}

	// Stream local candidates into a slice as they're gathered; an empty
	// callback signals end-of-gathering.
	var (
		candMu sync.Mutex
		cands  []string
	)
	gatherDone := make(chan struct{})
	var gatherOnce sync.Once
	if err := agent.OnCandidate(func(c pionice.Candidate) {
		if c == nil {
			gatherOnce.Do(func() { close(gatherDone) })
			return
		}
		candMu.Lock()
		cands = append(cands, c.Marshal())
		candMu.Unlock()
	}); err != nil {
		agent.Close()
		return nil, fmt.Errorf("OnCandidate: %w", err)
	}

	agent.OnConnectionStateChange(func(s pionice.ConnectionState) {
		a.cfg.Logger.Info("ice state", "state", s.String(), "role", a.cfg.Role)
	})

	if err := agent.GatherCandidates(); err != nil {
		agent.Close()
		return nil, fmt.Errorf("GatherCandidates: %w", err)
	}

	// Wait for gathering to finish (bounded).
	select {
	case <-gatherDone:
	case <-ctx.Done():
		agent.Close()
		return nil, ctx.Err()
	case <-time.After(10 * time.Second):
		// Gathering past 10s with no end-of-candidates is unusual; proceed
		// with whatever we have. Not fatal.
	}

	ufrag, pwd, err := agent.GetLocalUserCredentials()
	if err != nil {
		agent.Close()
		return nil, fmt.Errorf("GetLocalUserCredentials: %w", err)
	}

	candMu.Lock()
	localCands := append([]string{}, cands...)
	candMu.Unlock()

	// Exchange offer/answer via signaling. Controlling side POSTs its offer
	// and waits for the controlled side's answer. Controlled side waits
	// for the controlling side's offer, then POSTs its own answer. A
	// per-attempt nonce (Gen) binds the offer/answer pair so a peer can
	// reject a stale answer left on the signaling server from a previous
	// attempt (or posted by a peer-b that read a stale offer during a
	// simultaneous-restart race). peer-a mints the Gen; peer-b echoes it.
	myGen, err := newGen()
	if err != nil {
		agent.Close()
		return nil, fmt.Errorf("newGen: %w", err)
	}
	myOffer := signaling.Offer{
		Ufrag:      ufrag,
		Pwd:        pwd,
		Candidates: localCands,
		Gen:        myGen,
	}

	var peer signaling.Offer
	if a.IsControlling() {
		if err := a.cfg.Signaling.PostOffer(ctx, a.cfg.SessionID, a.cfg.Role, myOffer); err != nil {
			agent.Close()
			return nil, fmt.Errorf("post offer: %w", err)
		}
		peer, err = a.cfg.Signaling.AwaitAnswerMatching(ctx, a.cfg.SessionID, a.cfg.PollInterval, myGen)
		if err != nil {
			agent.Close()
			return nil, fmt.Errorf("await answer: %w", err)
		}
	} else {
		// Poll for the controlling peer's offer first.
		peer, err = a.cfg.Signaling.AwaitOffer(ctx, a.cfg.SessionID, "peer-a", a.cfg.PollInterval)
		if err != nil {
			agent.Close()
			return nil, fmt.Errorf("await offer: %w", err)
		}
		// Echo peer-a's Gen so peer-a can match the pair.
		myOffer.Gen = peer.Gen
		if err := a.cfg.Signaling.PostAnswer(ctx, a.cfg.SessionID, myOffer); err != nil {
			agent.Close()
			return nil, fmt.Errorf("post answer: %w", err)
		}
	}

	// Install peer candidates.
	for _, raw := range peer.Candidates {
		c, err := pionice.UnmarshalCandidate(raw)
		if err != nil {
			a.cfg.Logger.Warn("bad remote candidate", "raw", raw, "err", err)
			continue
		}
		if err := agent.AddRemoteCandidate(c); err != nil {
			a.cfg.Logger.Warn("AddRemoteCandidate", "err", err)
		}
	}

	// Kick off connectivity checks.
	if a.IsControlling() {
		return agent.Dial(ctx, peer.Ufrag, peer.Pwd)
	}
	return agent.Accept(ctx, peer.Ufrag, peer.Pwd)
}

func parseURLs(stunHosts []string, turnServers []TURNServer) ([]*stun.URI, error) {
	var out []*stun.URI
	for _, h := range stunHosts {
		u, err := stun.ParseURI("stun:" + h)
		if err != nil {
			return nil, fmt.Errorf("parse stun %q: %w", h, err)
		}
		out = append(out, u)
	}
	for _, t := range turnServers {
		u, err := stun.ParseURI(t.URI)
		if err != nil {
			return nil, fmt.Errorf("parse turn %q: %w", t.URI, err)
		}
		u.Username = t.User
		u.Password = t.Pass
		out = append(out, u)
	}
	return out, nil
}

// newGen returns a short random hex string used as a per-attempt binding
// nonce between offer and answer.
func newGen() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// PacketConnAdapter wraps an *ice.Conn as a net.PacketConn with a fixed
// remote address, so it can be fed into kcp-go (which requires PacketConn).
type PacketConnAdapter struct {
	conn   *pionice.Conn
	remote net.Addr
}

func NewPacketConn(conn *pionice.Conn) *PacketConnAdapter {
	return &PacketConnAdapter{conn: conn, remote: conn.RemoteAddr()}
}

func (p *PacketConnAdapter) ReadFrom(b []byte) (int, net.Addr, error) {
	n, err := p.conn.Read(b)
	return n, p.remote, err
}

func (p *PacketConnAdapter) WriteTo(b []byte, _ net.Addr) (int, error) {
	return p.conn.Write(b)
}

func (p *PacketConnAdapter) Close() error                       { return p.conn.Close() }
func (p *PacketConnAdapter) LocalAddr() net.Addr                { return p.conn.LocalAddr() }
func (p *PacketConnAdapter) SetDeadline(t time.Time) error      { return p.conn.SetDeadline(t) }
func (p *PacketConnAdapter) SetReadDeadline(t time.Time) error  { return p.conn.SetReadDeadline(t) }
func (p *PacketConnAdapter) SetWriteDeadline(t time.Time) error { return p.conn.SetWriteDeadline(t) }

// RemoteAddr returns the fixed remote for kcp.NewConn.
func (p *PacketConnAdapter) RemoteAddr() net.Addr { return p.remote }
