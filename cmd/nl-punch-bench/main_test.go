package main

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// Spin up the server handler in-process and hit it over a real TCP loopback
// connection. Verifies the on-the-wire protocol actually works end-to-end
// without needing the whole nl-punch stack.
func TestServerClientRoundTrip(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	var wg sync.WaitGroup
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				serve(c)
			}(c)
		}
	}()

	addr := ln.Addr().String()

	t.Run("ping echoes back", func(t *testing.T) {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer c.Close()
		if _, err := c.Write([]byte{modePing}); err != nil {
			t.Fatalf("write mode: %v", err)
		}
		out := make([]byte, pingPayloadSize)
		for i := 0; i < 16; i++ {
			binary.BigEndian.PutUint64(out[0:8], uint64(i))
			binary.BigEndian.PutUint64(out[8:16], uint64(time.Now().UnixNano()))
			if _, err := c.Write(out); err != nil {
				t.Fatalf("write ping %d: %v", i, err)
			}
			in := make([]byte, pingPayloadSize)
			if _, err := io.ReadFull(c, in); err != nil {
				t.Fatalf("read ping %d: %v", i, err)
			}
			if string(in) != string(out) {
				t.Fatalf("ping %d echoed wrong bytes", i)
			}
		}
	})

	t.Run("hello handshake", func(t *testing.T) {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer c.Close()
		if _, err := c.Write([]byte{modeHello}); err != nil {
			t.Fatalf("write mode: %v", err)
		}
		var b [1]byte
		if _, err := io.ReadFull(c, b[:]); err != nil {
			t.Fatalf("read hello: %v", err)
		}
		if b[0] != modeHello {
			t.Fatalf("expected %q, got %q", modeHello, b[0])
		}
	})

	t.Run("sink reports byte count", func(t *testing.T) {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if _, err := c.Write([]byte{modeSink}); err != nil {
			t.Fatalf("write mode: %v", err)
		}
		payload := make([]byte, 32*1024)
		fillRandom(payload)
		if _, err := c.Write(payload); err != nil {
			t.Fatalf("write sink: %v", err)
		}
		// Half-close so the server sees EOF and responds.
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		var got uint64
		if err := binary.Read(c, binary.BigEndian, &got); err != nil {
			t.Fatalf("read count: %v", err)
		}
		if got != uint64(len(payload)) {
			t.Fatalf("sink count mismatch: want %d got %d", len(payload), got)
		}
		c.Close()
	})

	t.Run("upload from server respects duration", func(t *testing.T) {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer c.Close()
		if _, err := c.Write([]byte{modeUpload}); err != nil {
			t.Fatalf("write mode: %v", err)
		}
		if err := binary.Write(c, binary.BigEndian, uint32(200)); err != nil {
			t.Fatalf("write dur: %v", err)
		}
		start := time.Now()
		buf := make([]byte, 64*1024)
		var got uint64
		for {
			n, err := c.Read(buf)
			got += uint64(n)
			if err != nil {
				break
			}
		}
		elapsed := time.Since(start)
		if elapsed < 150*time.Millisecond || elapsed > 2*time.Second {
			t.Fatalf("unexpected duration: %v", elapsed)
		}
		if got == 0 {
			t.Fatalf("expected some bytes from upload")
		}
	})
}
