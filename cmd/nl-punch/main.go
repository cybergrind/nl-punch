// Command nl-punch is the symmetric UDP hole-punching TCP forwarder.
// Two processes (one per peer) run against the same signaling endpoint
// with opposite roles (peer-a + peer-b). They establish a single ICE UDP
// association, layer KCP+yamux on top, and pump configured TCP ports
// over multiplexed streams.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"nl-punch/internal/config"
	"nl-punch/internal/forward"
	icepkg "nl-punch/internal/ice"
	"nl-punch/internal/signaling"
	"nl-punch/internal/transport"
)

func main() {
	var (
		configPath = flag.String("config", "", "path to JSON config file (required)")
		role       = flag.String("role", "", "peer-a or peer-b (required)")
		session   = flag.String("session", "", "override config.session_id")
		sigURL    = flag.String("signaling", "", "override config.signaling.url")
		secret    = flag.String("secret", os.Getenv("NLPUNCH_SECRET"), "override config.signaling.shared_secret; default $NLPUNCH_SECRET")
		logLevel  = flag.String("log-level", "info", "debug|info|warn|error")
	)
	flag.Parse()

	if *configPath == "" || *role == "" {
		fmt.Fprintln(os.Stderr, "usage: nl-punch --config PATH --role peer-a|peer-b [--session ID] [--signaling URL]")
		os.Exit(2)
	}

	log := newLogger(*logLevel)

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("load config", "err", err)
		os.Exit(1)
	}
	if *session != "" {
		cfg.SessionID = *session
	}
	if *sigURL != "" {
		cfg.Signaling.URL = *sigURL
	}
	if *secret != "" {
		cfg.Signaling.SharedSecret = *secret
	}
	if _, ok := cfg.Peers[*role]; !ok {
		log.Error("role not in config.peers", "role", *role)
		os.Exit(1)
	}

	log.Info("starting",
		"role", *role,
		"session", cfg.SessionID,
		"signaling", cfg.Signaling.URL,
	)

	ctx, cancel := context.WithCancel(context.Background())
	go waitSignal(log, cancel)

	sigClient := signaling.NewClient(cfg.Signaling.URL, cfg.Signaling.SharedSecret)
	agent := icepkg.New(icepkg.Config{
		Signaling:           sigClient,
		SessionID:           cfg.SessionID,
		Role:                *role,
		Logger:              log,
		STUN:                cfg.ICE.STUN,
		TURN:                toICETURN(cfg.ICE.TURN),
		FailedTimeout:       60 * time.Second,
		DisconnectedTimeout: 30 * time.Second,
		PionLogLevel:        pionLogLevelFromSlog(*logLevel),
	})

	runReconnectLoop(ctx, log, cfg, *role, agent)
}

func runReconnectLoop(ctx context.Context, log *slog.Logger, cfg *config.Config, role string, agent *icepkg.Agent) {
	var backoff = time.Second
	const maxBackoff = 60 * time.Second

	for {
		if ctx.Err() != nil {
			return
		}
		started := time.Now()
		if err := runOnce(ctx, log, cfg, role, agent); err != nil {
			log.Warn("session ended", "err", err, "uptime", time.Since(started))
		}
		if ctx.Err() != nil {
			return
		}
		// Exponential backoff with ±20% jitter, capped at 60s.
		jitter := time.Duration(rand.Float64() * 0.4 * float64(backoff))
		wait := backoff - (backoff / 5) + jitter
		log.Info("reconnecting", "wait", wait)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func runOnce(ctx context.Context, log *slog.Logger, cfg *config.Config, role string, agent *icepkg.Agent) error {
	// ICE handshake.
	iceCtx, iceCancel := context.WithTimeout(ctx, 120*time.Second)
	iceConn, err := agent.Connect(iceCtx)
	iceCancel()
	if err != nil {
		return fmt.Errorf("ice connect: %w", err)
	}
	defer iceConn.Close()

	pair, _ := iceConnRemoteAddr(iceConn)
	log.Info("ice connected",
		"local", iceConn.LocalAddr(),
		"remote", iceConn.RemoteAddr(),
		"mode", candidateMode(pair),
	)

	pc := icepkg.NewPacketConn(iceConn)

	var sess *transport.Session
	if role == config.RolePeerA {
		sess, err = transport.Serve(pc)
	} else {
		sess, err = transport.Dial(pc, pc.RemoteAddr().String())
	}
	if err != nil {
		return fmt.Errorf("transport: %w", err)
	}
	defer sess.Close()
	log.Info("transport up", "role", role)

	me := cfg.Peers[role]
	other := cfg.Peers[config.Other(role)]

	// TCP listeners for this peer's exposed ports.
	listeners := forward.NewTCPListener(sess, toFwdListen(me.Listen), log)
	if err := listeners.Start(); err != nil {
		return fmt.Errorf("listeners: %w", err)
	}
	defer listeners.Close()

	// Dial map = peer's listen names → our local dial target.
	// Each side dials the port the *other* side's listener spec asks us to.
	// We resolve this by: if our config has a Dial entry, use LocalPort.
	// Otherwise we fall back to the other peer's RemotePort (their listener
	// asked for remote_port=X, so we dial 127.0.0.1:X here).
	dials := map[string]int{}
	for _, d := range me.Dial {
		dials[d.Name] = d.LocalPort
	}
	for _, l := range other.Listen {
		if _, already := dials[l.Name]; !already {
			dials[l.Name] = l.RemotePort
		}
	}
	router := forward.NewAcceptRouter(sess, dials, log)
	go router.Run()
	defer router.Close()

	// Stats ticker + session wait.
	statsT := time.NewTicker(30 * time.Second)
	defer statsT.Stop()
	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-sess.Wait():
			return fmt.Errorf("session closed")
		case <-statsT.C:
			lin, lout := listeners.Stats()
			rin, rout := router.Stats()
			log.Info("stats",
				"uptime", time.Since(start).Truncate(time.Second),
				"streams_active", sess.NumActiveStreams(),
				"bytes_in", lin+rin,
				"bytes_out", lout+rout,
				"mode", candidateMode(pair),
			)
		}
	}
}

// --- helpers ---

func iceConnRemoteAddr(c interface{ RemoteAddr() net.Addr }) (net.Addr, error) {
	return c.RemoteAddr(), nil
}

// candidateMode classifies the selected candidate type for log lines.
// Placeholder until we plug in Agent.GetSelectedCandidatePair in the future.
func candidateMode(_ net.Addr) string {
	// pion/ice v3 exposes GetSelectedCandidatePair on the *Agent, not on
	// the Conn. We could plumb it through; for v1 we just log "direct"
	// unless we wire relay detection later.
	return "direct"
}

func toICETURN(ts []config.TURNServer) []icepkg.TURNServer {
	out := make([]icepkg.TURNServer, 0, len(ts))
	for _, t := range ts {
		out = append(out, icepkg.TURNServer{URI: t.URI, User: t.User, Pass: t.Pass})
	}
	return out
}

func toFwdListen(ls []config.ListenSpec) []forward.ListenSpec {
	out := make([]forward.ListenSpec, 0, len(ls))
	for _, l := range ls {
		out = append(out, forward.ListenSpec{
			Name:       l.Name,
			LocalPort:  l.LocalPort,
			RemotePort: l.RemotePort,
		})
	}
	return out
}

func waitSignal(log *slog.Logger, cancel context.CancelFunc) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	s := <-ch
	log.Info("signal received, shutting down", "signal", s.String())
	cancel()
}

// pionLogLevelFromSlog maps our --log-level to pion's 0..5 scale. debug →
// pion trace; info+ → pion error. ICE internals get very chatty at trace.
func pionLogLevelFromSlog(level string) int {
	switch level {
	case "debug":
		return 4 // debug
	default:
		return 2 // warn
	}
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
