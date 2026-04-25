package forward

import (
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

// yamuxAdapter makes *yamux.Session satisfy forward.Session (which returns
// net.Conn, not *yamux.Stream).
type yamuxAdapter struct{ s *yamux.Session }

func (a yamuxAdapter) OpenStream() (net.Conn, error)   { return a.s.OpenStream() }
func (a yamuxAdapter) AcceptStream() (net.Conn, error) { return a.s.AcceptStream() }

// yamuxPair returns a pair of yamux sessions connected by an in-memory pipe.
// Lets us test forward logic without depending on kcp-go timing.
func yamuxPair(t *testing.T) (server, client Session) {
	t.Helper()
	c1, c2 := net.Pipe()
	var sv, cl *yamux.Session
	var svErr, clErr error

	done := make(chan struct{})
	go func() {
		sv, svErr = yamux.Server(c1, nil)
		close(done)
	}()
	cl, clErr = yamux.Client(c2, nil)
	<-done

	if svErr != nil || clErr != nil {
		t.Fatalf("yamux: sv=%v cl=%v", svErr, clErr)
	}
	t.Cleanup(func() {
		sv.Close()
		cl.Close()
		c1.Close()
		c2.Close()
	})
	return yamuxAdapter{sv}, yamuxAdapter{cl}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// startEchoTCP starts a TCP echo server on 127.0.0.1:0 and returns its port.
func startEchoTCP(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(c)
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

func TestRoundTrip_tcpListenToDialEcho(t *testing.T) {
	// Topology:
	//   test client  -> TCP 127.0.0.1:Lport  (client-side forwarder listens)
	//                -> yamux stream "echo"
	//                -> server-side forwarder dials 127.0.0.1:echoPort
	//                -> echo server
	svSess, clSess := yamuxPair(t)
	echoPort := startEchoTCP(t)
	log := quietLogger()

	// Server-side: AcceptStream → dial local echo.
	acc := NewAcceptRouter(svSess, map[string]int{"echo": echoPort}, log)
	go acc.Run()
	t.Cleanup(func() { acc.Close() })

	// Client-side: listen on 127.0.0.1:0 → OpenStream("echo").
	lst := NewTCPListener(clSess, []ListenSpec{{Name: "echo", LocalPort: 0, RemotePort: echoPort}}, log)
	if err := lst.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { lst.Close() })

	// Port 0 → pick actual bound port.
	ports := lst.BoundPorts()
	if ports["echo"] == 0 {
		t.Fatal("listener did not bind")
	}
	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(ports["echo"])))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	c.SetDeadline(time.Now().Add(5 * time.Second))
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

func TestRoundTrip_unknownStreamTag(t *testing.T) {
	// If client opens a stream with a tag the server doesn't know,
	// the stream must be closed cleanly; server keeps running.
	svSess, clSess := yamuxPair(t)
	log := quietLogger()

	acc := NewAcceptRouter(svSess, map[string]int{"echo": 9999}, log)
	go acc.Run()
	t.Cleanup(func() { acc.Close() })

	s, err := clSess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeStreamHeader(s, streamHeader{tag: "nonexistent", sessionID: 1, stripeIdx: 0, numStripes: 1}); err != nil {
		t.Fatal(err)
	}
	s.SetDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	n, err := s.Read(buf)
	if err == nil && n > 0 {
		t.Errorf("expected EOF/close, got %d bytes", n)
	}
}

func TestBinaryStream_byteExact(t *testing.T) {
	svSess, clSess := yamuxPair(t)
	log := quietLogger()

	// Server side: deliver every inbound stream's bytes to this chan.
	received := make(chan []byte, 1)
	gotPort := receiveAllServer(t, received)
	acc := NewAcceptRouter(svSess, map[string]int{"blob": gotPort}, log)
	go acc.Run()
	t.Cleanup(func() { acc.Close() })

	lst := NewTCPListener(clSess, []ListenSpec{{Name: "blob", LocalPort: 0, RemotePort: gotPort}}, log)
	if err := lst.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lst.Close() })

	port := lst.BoundPorts()["blob"]
	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	// Send 16 KB of pseudo-random bytes.
	payload := make([]byte, 16*1024)
	for i := range payload {
		payload[i] = byte(i*31 + 7)
	}
	go func() {
		c.Write(payload)
		c.(*net.TCPConn).CloseWrite()
	}()

	select {
	case got := <-received:
		if len(got) != len(payload) {
			t.Fatalf("len = %d, want %d", len(got), len(payload))
		}
		for i := range payload {
			if got[i] != payload[i] {
				t.Fatalf("mismatch at %d: %d vs %d", i, got[i], payload[i])
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for bytes")
	}
}

// receiveAllServer starts a tcp server that collects every connection's
// read stream and sends the bytes to ch when the client closes its write half.
func receiveAllServer(t *testing.T, ch chan<- []byte) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf, _ := io.ReadAll(c)
		ch <- buf
	}()
	return l.Addr().(*net.TCPAddr).Port
}

// itoa is a cheap strconv.Itoa for tests without importing strconv in every file.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// TestBulkServerPush_afterClientHeader exercises the throughput-down shape:
// the client opens a TCP connection, writes a tiny header, then only reads;
// the server reads the header, then writes a large payload and closes.
// Pins that server-initiated bulk bytes on a stream that the *client* opened
// make it all the way back to the client — the symptom we saw in round-E
// was that this direction delivered 0 bytes.
func TestBulkServerPush_afterClientHeader(t *testing.T) {
	svSess, clSess := yamuxPair(t)
	log := quietLogger()

	const wantBytes = 1 << 20 // 1 MiB
	serverPort := startServerPushTCP(t, wantBytes)

	acc := NewAcceptRouter(svSess, map[string]int{"push": serverPort}, log)
	go acc.Run()
	t.Cleanup(func() { acc.Close() })

	lst := NewTCPListener(clSess, []ListenSpec{{Name: "push", LocalPort: 0, RemotePort: serverPort}}, log)
	if err := lst.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lst.Close() })

	port := lst.BoundPorts()["push"]
	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))

	// Tiny 5-byte header (mimics bench-client's modeUpload + 4-byte duration).
	if _, err := c.Write([]byte{'U', 0, 0, 0, 5}); err != nil {
		t.Fatal(err)
	}

	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != wantBytes {
		t.Fatalf("server push delivered %d bytes, want %d", len(got), wantBytes)
	}
}

// startServerPushTCP starts a TCP server that, for every connection, reads a
// 5-byte header and then writes exactly `payloadSize` bytes back and closes.
func startServerPushTCP(t *testing.T, payloadSize int) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var hdr [5]byte
				if _, err := io.ReadFull(c, hdr[:]); err != nil {
					return
				}
				// Write a deterministic blob so size is the only invariant.
				payload := make([]byte, payloadSize)
				for i := range payload {
					payload[i] = byte(i * 13)
				}
				_, _ = c.Write(payload)
			}(c)
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

func TestMain(m *testing.M) {
	// Left intentionally thin: every test wires its own helpers. The
	// zero-value uses of os/sync are just to keep the imports pinned
	// for tests that reference them.
	_ = os.Stderr
	_ = sync.Mutex{}
	os.Exit(m.Run())
}
