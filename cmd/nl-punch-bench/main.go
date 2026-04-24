// Command nl-punch-bench measures the behavior of an established nl-punch
// tunnel end-to-end: RTT, fresh-stream open latency, one-way throughput,
// and short idle-with-burst stability. One binary, two modes: the server
// runs on the peer-b side as a trivial TCP echo/source/sink; the client
// runs on the peer-a side and dials the local nl-punch listener, so every
// byte traverses yamux+kcp+ICE.
//
// Framing — first byte of every stream is a mode byte:
//
//	'P'  ping/echo. Server echoes bytes back until EOF. Client drives
//	     N ping-pongs of a fixed payload and records each RTT.
//	'S'  sink. Client writes for a duration, half-closes its write side,
//	     server reads until EOF then writes a uint64 BE byte-count and
//	     closes. Used for client→server throughput.
//	'U'  upload-from-server. Client writes a uint32 BE duration-ms,
//	     server writes random bytes for that duration then closes.
//	     Used for server→client throughput.
//	'H'  handshake. Server writes one byte back and closes. Used for
//	     fresh-stream open-latency.
//
// Results are emitted as one JSON object per line on stdout ("jsonl")
// so a run script can redirect to a file and later diff runs.
package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"sort"
	"time"
)

const (
	modePing   byte = 'P'
	modeSink   byte = 'S'
	modeUpload byte = 'U'
	modeHello  byte = 'H'

	pingPayloadSize = 16
	ioBufSize       = 64 * 1024
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "server":
		runServer(os.Args[2:])
	case "client":
		runClient(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  nl-punch-bench server --listen HOST:PORT")
	fmt.Fprintln(os.Stderr, "  nl-punch-bench client --target HOST:PORT --test <latency|fresh-stream|throughput-up|throughput-down|idle> [options]")
}

// --------------------- server ---------------------

func runServer(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:9900", "TCP listen address")
	_ = fs.Parse(args)

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		die("listen: %v", err)
	}
	fmt.Fprintf(os.Stderr, "bench server on %s\n", ln.Addr())
	for {
		conn, err := ln.Accept()
		if err != nil {
			fmt.Fprintf(os.Stderr, "accept: %v\n", err)
			continue
		}
		go serve(conn)
	}
}

func serve(c net.Conn) {
	defer c.Close()
	var hdr [1]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return
	}
	switch hdr[0] {
	case modePing:
		// Echo loop. Mirror whatever the client sends until it hangs up.
		buf := make([]byte, ioBufSize)
		for {
			n, err := c.Read(buf)
			if n > 0 {
				if _, werr := c.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}

	case modeSink:
		// Discard bytes until EOF (client half-close), then report count.
		buf := make([]byte, ioBufSize)
		var count uint64
		for {
			n, err := c.Read(buf)
			count += uint64(n)
			if err != nil {
				break
			}
		}
		_ = binary.Write(c, binary.BigEndian, count)

	case modeUpload:
		// Read duration, then flood random data for that duration.
		var durMs uint32
		if err := binary.Read(c, binary.BigEndian, &durMs); err != nil {
			return
		}
		buf := make([]byte, ioBufSize)
		fillRandom(buf)
		deadline := time.Now().Add(time.Duration(durMs) * time.Millisecond)
		for time.Now().Before(deadline) {
			if _, err := c.Write(buf); err != nil {
				return
			}
		}

	case modeHello:
		// Minimal handshake — prove the stream carries a byte both ways.
		_, _ = c.Write([]byte{modeHello})
	}
}

// --------------------- client ---------------------

func runClient(args []string) {
	fs := flag.NewFlagSet("client", flag.ExitOnError)
	target := fs.String("target", "127.0.0.1:9900", "address of local nl-punch listener")
	test := fs.String("test", "latency", "latency|fresh-stream|throughput-up|throughput-down|idle")
	pingN := fs.Int("ping-n", 200, "latency: number of ping-pongs")
	freshN := fs.Int("fresh-n", 100, "fresh-stream: number of sequential opens")
	throughputSec := fs.Int("throughput-sec", 20, "throughput: duration in seconds")
	idleSec := fs.Int("idle-sec", 300, "idle: hold duration in seconds")
	heartbeatSec := fs.Int("heartbeat-sec", 10, "idle: heartbeat interval in seconds")
	burstSec := fs.Int("burst-sec", 10, "idle: trailing burst duration in seconds")
	label := fs.String("label", "", "free-form label emitted in result records")
	_ = fs.Parse(args)

	emitMeta(*test, *target, *label)

	switch *test {
	case "latency":
		doLatency(*target, *pingN, *label)
	case "fresh-stream":
		doFreshStream(*target, *freshN, *label)
	case "throughput-up":
		doThroughputUp(*target, time.Duration(*throughputSec)*time.Second, *label)
	case "throughput-down":
		doThroughputDown(*target, time.Duration(*throughputSec)*time.Second, *label)
	case "idle":
		doIdle(*target, time.Duration(*idleSec)*time.Second, time.Duration(*heartbeatSec)*time.Second, time.Duration(*burstSec)*time.Second, *label)
	default:
		die("unknown --test %q", *test)
	}
}

func emitMeta(test, target, label string) {
	emit(map[string]any{
		"event":  "meta",
		"test":   test,
		"target": target,
		"label":  label,
		"ts":     time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// --- latency ---

func doLatency(addr string, n int, label string) {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		dieResult("latency", label, err)
	}
	defer c.Close()
	if _, err := c.Write([]byte{modePing}); err != nil {
		dieResult("latency", label, err)
	}
	buf := make([]byte, pingPayloadSize)
	rtts := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		binary.BigEndian.PutUint64(buf[0:8], uint64(i))
		binary.BigEndian.PutUint64(buf[8:16], uint64(time.Now().UnixNano()))
		t0 := time.Now()
		if _, err := c.Write(buf); err != nil {
			dieResult("latency", label, err)
		}
		if _, err := io.ReadFull(c, buf); err != nil {
			dieResult("latency", label, err)
		}
		rtts = append(rtts, time.Since(t0))
	}
	emitDurationStats("latency", label, rtts)
}

// --- fresh-stream ---

func doFreshStream(addr string, n int, label string) {
	opens := make([]time.Duration, 0, n)
	var errs int
	for i := 0; i < n; i++ {
		t0 := time.Now()
		c, err := net.Dial("tcp", addr)
		if err != nil {
			errs++
			emit(map[string]any{"event": "error", "test": "fresh-stream", "i": i, "err": err.Error()})
			continue
		}
		if _, err := c.Write([]byte{modeHello}); err != nil {
			errs++
			c.Close()
			emit(map[string]any{"event": "error", "test": "fresh-stream", "i": i, "err": err.Error()})
			continue
		}
		var b [1]byte
		if _, err := io.ReadFull(c, b[:]); err != nil {
			errs++
			c.Close()
			emit(map[string]any{"event": "error", "test": "fresh-stream", "i": i, "err": err.Error()})
			continue
		}
		opens = append(opens, time.Since(t0))
		c.Close()
	}
	emitDurationStats("fresh-stream", label, opens)
	emit(map[string]any{"event": "summary", "test": "fresh-stream", "label": label, "errors": errs, "samples": len(opens)})
}

// --- throughput up (client → server) ---
//
// Emits a `result` event with `ok=true` on clean completion, or a
// `partial` event with whatever we managed to push before the connection
// broke. The latter is what's useful when the tunnel collapses under bulk
// write — we still want to see the measured rate.
func doThroughputUp(addr string, dur time.Duration, label string) {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		dieResult("throughput-up", label, err)
	}
	defer c.Close()
	if _, err := c.Write([]byte{modeSink}); err != nil {
		dieResult("throughput-up", label, err)
	}
	buf := make([]byte, ioBufSize)
	fillRandom(buf)
	t0 := time.Now()
	// Hard wall-clock deadline. c.Write can block for a long time on a
	// backed-up yamux stream, so relying only on a for-loop time check
	// can leave us writing long past `dur`. SetWriteDeadline forces an
	// i/o timeout error at exactly t0+dur.
	_ = c.SetWriteDeadline(t0.Add(dur))
	var sent uint64
	for {
		n, err := c.Write(buf)
		sent += uint64(n)
		if err != nil {
			elapsed := time.Since(t0)
			// Normal end-of-window is an i/o timeout — emit as "result".
			evKind := "partial"
			if ne, ok := err.(net.Error); ok && ne.Timeout() && elapsed >= dur {
				evKind = "result"
			}
			emit(map[string]any{
				"event":      evKind,
				"test":       "throughput-up",
				"label":      label,
				"bytes_sent": sent,
				"send_sec":   elapsed.Seconds(),
				"mbps_sent":  float64(sent*8) / elapsed.Seconds() / 1e6,
				"err":        err.Error(),
			})
			return
		}
	}
}

// --- throughput down (server → client) ---
//
// Whatever we managed to read before EOF or error gets emitted as a
// result — if the read errored (non-EOF) we tag the event as `partial`.
func doThroughputDown(addr string, dur time.Duration, label string) {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		dieResult("throughput-down", label, err)
	}
	defer c.Close()
	if _, err := c.Write([]byte{modeUpload}); err != nil {
		dieResult("throughput-down", label, err)
	}
	if err := binary.Write(c, binary.BigEndian, uint32(dur.Milliseconds())); err != nil {
		dieResult("throughput-down", label, err)
	}
	// Give the server `dur` to write + 5 s slack for final drain.
	_ = c.SetReadDeadline(time.Now().Add(dur + 5*time.Second))
	buf := make([]byte, ioBufSize)
	t0 := time.Now()
	var got uint64
	var readErr error
	for {
		n, err := c.Read(buf)
		got += uint64(n)
		if err != nil {
			if err != io.EOF {
				readErr = err
			}
			break
		}
	}
	elapsed := time.Since(t0)
	ev := map[string]any{
		"test":  "throughput-down",
		"label": label,
		"bytes": got,
		"sec":   elapsed.Seconds(),
		"mbps":  float64(got*8) / elapsed.Seconds() / 1e6,
	}
	if readErr != nil {
		ev["event"] = "partial"
		ev["err"] = readErr.Error()
	} else {
		ev["event"] = "result"
	}
	emit(ev)
}

// --- idle with trailing burst ---
//
// Holds one echo-stream open for idle. Sends a 16-byte ping every
// heartbeatInterval and records per-beat RTT. After idle elapses, runs a
// back-to-back ping loop for burstDur and records those RTTs too. Emits
// per-beat events plus a final summary so we can see whether the tunnel
// survived the idle window and how the burst behaves after.
func doIdle(addr string, idleDur, heartbeatInterval, burstDur time.Duration, label string) {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		dieResult("idle", label, err)
	}
	defer c.Close()
	if _, err := c.Write([]byte{modePing}); err != nil {
		dieResult("idle", label, err)
	}
	buf := make([]byte, pingPayloadSize)

	oneBeat := func(kind string, seq int) (time.Duration, error) {
		binary.BigEndian.PutUint64(buf[0:8], uint64(seq))
		binary.BigEndian.PutUint64(buf[8:16], uint64(time.Now().UnixNano()))
		t0 := time.Now()
		if err := c.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
			return 0, err
		}
		if _, err := c.Write(buf); err != nil {
			return 0, err
		}
		if _, err := io.ReadFull(c, buf); err != nil {
			return 0, err
		}
		return time.Since(t0), nil
	}

	start := time.Now()
	idleRTTs := make([]time.Duration, 0)
	seq := 0
	tick := time.NewTicker(heartbeatInterval)
	defer tick.Stop()

	// Opening beat (t=0) so we have a zero-elapsed baseline.
	if rtt, err := oneBeat("idle-heartbeat", seq); err == nil {
		emit(map[string]any{
			"event":         "heartbeat",
			"test":          "idle",
			"label":         label,
			"seq":           seq,
			"elapsed_sec":   0.0,
			"rtt_ms":        rtt.Seconds() * 1000,
		})
		idleRTTs = append(idleRTTs, rtt)
	} else {
		emit(map[string]any{
			"event":       "heartbeat_error",
			"test":        "idle",
			"label":       label,
			"seq":         seq,
			"elapsed_sec": 0.0,
			"err":         err.Error(),
		})
		emitSummary(label, idleRTTs, nil, err, time.Since(start))
		return
	}

	for {
		select {
		case <-tick.C:
			if time.Since(start) >= idleDur {
				goto burstPhase
			}
			seq++
			elapsed := time.Since(start)
			rtt, err := oneBeat("idle-heartbeat", seq)
			if err != nil {
				emit(map[string]any{
					"event":       "heartbeat_error",
					"test":        "idle",
					"label":       label,
					"seq":         seq,
					"elapsed_sec": elapsed.Seconds(),
					"err":         err.Error(),
				})
				emitSummary(label, idleRTTs, nil, err, time.Since(start))
				return
			}
			idleRTTs = append(idleRTTs, rtt)
			emit(map[string]any{
				"event":       "heartbeat",
				"test":        "idle",
				"label":       label,
				"seq":         seq,
				"elapsed_sec": elapsed.Seconds(),
				"rtt_ms":      rtt.Seconds() * 1000,
			})
		}
	}

burstPhase:
	burstRTTs := make([]time.Duration, 0)
	burstStart := time.Now()
	for time.Since(burstStart) < burstDur {
		seq++
		rtt, err := oneBeat("idle-burst", seq)
		if err != nil {
			emit(map[string]any{
				"event":       "burst_error",
				"test":        "idle",
				"label":       label,
				"seq":         seq,
				"burst_sec":   time.Since(burstStart).Seconds(),
				"err":         err.Error(),
			})
			emitSummary(label, idleRTTs, burstRTTs, err, time.Since(start))
			return
		}
		burstRTTs = append(burstRTTs, rtt)
	}
	emitSummary(label, idleRTTs, burstRTTs, nil, time.Since(start))
}

func emitSummary(label string, idle, burst []time.Duration, breakErr error, totalElapsed time.Duration) {
	sum := map[string]any{
		"event":         "summary",
		"test":          "idle",
		"label":         label,
		"idle_samples":  len(idle),
		"burst_samples": len(burst),
		"total_sec":     totalElapsed.Seconds(),
	}
	if breakErr != nil {
		sum["broken"] = true
		sum["err"] = breakErr.Error()
	} else {
		sum["broken"] = false
	}
	if len(idle) > 0 {
		sum["idle_rtt_ms"] = stats(idle)
	}
	if len(burst) > 0 {
		sum["burst_rtt_ms"] = stats(burst)
	}
	emit(sum)
}

// --------------------- helpers ---------------------

func emit(v any) {
	_ = json.NewEncoder(os.Stdout).Encode(v)
}

func emitDurationStats(test, label string, samples []time.Duration) {
	emit(map[string]any{
		"event":   "result",
		"test":    test,
		"label":   label,
		"samples": len(samples),
		"rtt_ms":  stats(samples),
	})
}

func stats(s []time.Duration) map[string]float64 {
	if len(s) == 0 {
		return nil
	}
	sorted := make([]time.Duration, len(s))
	copy(sorted, s)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	q := func(p float64) float64 {
		idx := int(float64(len(sorted)-1) * p)
		return sorted[idx].Seconds() * 1000
	}
	var sum time.Duration
	for _, d := range sorted {
		sum += d
	}
	return map[string]float64{
		"min":  sorted[0].Seconds() * 1000,
		"p50":  q(0.50),
		"p90":  q(0.90),
		"p95":  q(0.95),
		"p99":  q(0.99),
		"max":  sorted[len(sorted)-1].Seconds() * 1000,
		"mean": float64(sum) / float64(len(sorted)) / float64(time.Millisecond),
	}
}

func fillRandom(b []byte) {
	// math/rand is fine; we're just feeding the pipe, not proving anything
	// crypto-wise.
	var r [8]byte
	for i := 0; i < len(b); i += 8 {
		binary.LittleEndian.PutUint64(r[:], rand.Uint64())
		copy(b[i:], r[:])
	}
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "nl-punch-bench: "+format+"\n", a...)
	os.Exit(1)
}

func dieResult(test, label string, err error) {
	emit(map[string]any{
		"event": "fatal",
		"test":  test,
		"label": label,
		"err":   err.Error(),
	})
	// Propagate as non-zero exit so shell scripts notice.
	if errors.Is(err, io.EOF) {
		os.Exit(3)
	}
	os.Exit(4)
}
