// Package main's end-to-end test: run two nl-punch processes in-process
// against an in-memory signaling server, tunnel a TCP echo over the link.
//
// Intentionally lives in cmd/nl-punch so we exercise the same wiring
// the production binary uses.
package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"nl-punch/internal/config"
	"nl-punch/internal/forward"
	icepkg "nl-punch/internal/ice"
	"nl-punch/internal/signaling"
	"nl-punch/internal/transport"
)

// TestEndToEnd_loopback runs the full pipeline — ICE → KCP → yamux → TCP
// forward — between two in-process peers on loopback, and verifies a
// byte round-trip.
func TestEndToEnd_loopback(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e in -short mode")
	}

	// 1. In-memory signaling.
	sigSrv := httptest.NewServer(signaling.NewServer("").Handler())
	defer sigSrv.Close()

	// 2. Echo server that peer-a will expose via "echo" stream.
	echoL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoL.Close()
	echoPort := echoL.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := echoL.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(c)
		}
	}()

	// 3. Config. Peer-b is the side that dials into its local echo;
	// peer-a is the side whose listener exposes the virtual port.
	// For this test we have peer-a LISTEN on 127.0.0.1:port (we pick 0 =
	// auto-assign) and the server-side router dial localhost:echoPort.
	cfg := &config.Config{
		SessionID: "e2e",
		Transport: config.TransportConfig{Profile: "low-latency"},
		Peers: map[string]config.Peer{
			"peer-a": {
				// Peer-a here is the Listener side — its TCP listener
				// accepts test client traffic and tunnels via "echo" stream.
				Listen: []config.ListenSpec{
					{LocalPort: 0, RemotePort: echoPort, Name: "echo"},
				},
			},
			"peer-b": {
				// Peer-b is the Dial side — it accepts the inbound stream
				// and dials 127.0.0.1:echoPort.
				Dial: []config.DialSpec{
					{LocalPort: echoPort, Name: "echo"},
				},
			},
		},
	}
	cfg.ICE.STUN = nil // host candidates only — loopback

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 4. Run both peers concurrently. Run peer-a through the ICE agent,
	// grab the net.Conn, wrap as PacketConn, serve KCP+yamux, and set up
	// forward layers.
	type peerOut struct {
		listenerPorts map[string]int
		teardown      func()
		err           error
	}
	peerAOut := make(chan peerOut, 1)
	peerBOut := make(chan peerOut, 1)

	mkSig := func() *signaling.Client { return signaling.NewClient(sigSrv.URL, "") }

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		agent := icepkg.New(icepkg.Config{
			Signaling: mkSig(),
			SessionID: cfg.SessionID,
			Role:      "peer-a",
			Logger:    log,
		})
		iceConn, err := agent.Connect(ctx)
		if err != nil {
			peerAOut <- peerOut{err: err}
			return
		}
		pc := icepkg.NewPacketConn(iceConn)
		sess, err := transport.Serve(pc, transport.Profile(cfg.Transport.Profile))
		if err != nil {
			iceConn.Close()
			peerAOut <- peerOut{err: err}
			return
		}
		lst := forward.NewTCPListener(sess,
			[]forward.ListenSpec{{Name: "echo", LocalPort: 0, RemotePort: echoPort}}, log)
		if err := lst.Start(); err != nil {
			sess.Close()
			iceConn.Close()
			peerAOut <- peerOut{err: err}
			return
		}
		peerAOut <- peerOut{
			listenerPorts: lst.BoundPorts(),
			teardown: func() {
				lst.Close()
				sess.Close()
				iceConn.Close()
			},
		}
	}()

	go func() {
		defer wg.Done()
		agent := icepkg.New(icepkg.Config{
			Signaling: mkSig(),
			SessionID: cfg.SessionID,
			Role:      "peer-b",
			Logger:    log,
		})
		iceConn, err := agent.Connect(ctx)
		if err != nil {
			peerBOut <- peerOut{err: err}
			return
		}
		pc := icepkg.NewPacketConn(iceConn)
		sess, err := transport.Dial(pc, pc.RemoteAddr().String(), transport.Profile(cfg.Transport.Profile))
		if err != nil {
			iceConn.Close()
			peerBOut <- peerOut{err: err}
			return
		}
		router := forward.NewAcceptRouter(sess, map[string]int{"echo": echoPort}, log)
		go router.Run()
		peerBOut <- peerOut{
			teardown: func() {
				router.Close()
				sess.Close()
				iceConn.Close()
			},
		}
	}()

	wg.Wait()
	a := <-peerAOut
	b := <-peerBOut
	if a.err != nil {
		t.Fatalf("peer-a: %v", a.err)
	}
	if b.err != nil {
		t.Fatalf("peer-b: %v", b.err)
	}
	defer a.teardown()
	defer b.teardown()

	// 5. Client sends "ping" to peer-a's listener, expects "ping" back.
	port := a.listenerPorts["echo"]
	if port == 0 {
		t.Fatal("no listener port")
	}
	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ping" {
		t.Errorf("echo = %q, want ping", buf)
	}
}

// ensure config.example.json parses — catches schema drift.
func TestConfigExampleParses(t *testing.T) {
	// file is at ../../config.example.json from this dir.
	path := filepath.Join("..", "..", "config.example.json")
	if _, err := os.Stat(path); err != nil {
		t.Skip("config.example.json not found")
	}
	t.Setenv("NLPUNCH_SECRET", "test-secret")
	if _, err := config.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
}
