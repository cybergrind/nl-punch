package signaling

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestServerClient_offerAnswerRoundTrip(t *testing.T) {
	srv := NewServer("")
	ts := startTestServer(t, srv)
	c := NewClient(ts.URL(), "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	offer := Offer{
		Ufrag:      "abc",
		Pwd:        "xyz",
		Candidates: []string{"candidate:1 ..."},
	}
	if err := c.PostOffer(ctx, "sess1", "peer-a", offer); err != nil {
		t.Fatalf("PostOffer: %v", err)
	}

	got, err := c.GetOffer(ctx, "sess1", "peer-a")
	if err != nil {
		t.Fatalf("GetOffer: %v", err)
	}
	if got.Ufrag != "abc" || got.Pwd != "xyz" || len(got.Candidates) != 1 {
		t.Errorf("offer round-trip mismatch: %+v", got)
	}

	answer := Answer{
		Ufrag:      "def",
		Pwd:        "uvw",
		Candidates: []string{"candidate:2 ..."},
	}
	if err := c.PostAnswer(ctx, "sess1", answer); err != nil {
		t.Fatalf("PostAnswer: %v", err)
	}
	gotA, err := c.GetAnswer(ctx, "sess1")
	if err != nil {
		t.Fatalf("GetAnswer: %v", err)
	}
	if gotA.Ufrag != "def" {
		t.Errorf("answer round-trip mismatch: %+v", gotA)
	}
}

func TestServer_sharedSecretEnforced(t *testing.T) {
	srv := NewServer("hunter2")
	ts := startTestServer(t, srv)

	// Wrong secret → 401.
	c := NewClient(ts.URL(), "wrong")
	ctx := context.Background()
	err := c.PostOffer(ctx, "s", "peer-a", Offer{Ufrag: "u", Pwd: "p"})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("expected 401 error, got %v", err)
	}

	// Right secret → ok.
	c2 := NewClient(ts.URL(), "hunter2")
	if err := c2.PostOffer(ctx, "s", "peer-a", Offer{Ufrag: "u", Pwd: "p"}); err != nil {
		t.Errorf("with correct secret: %v", err)
	}
}

func TestClient_awaitOfferPolls(t *testing.T) {
	srv := NewServer("")
	ts := startTestServer(t, srv)
	c := NewClient(ts.URL(), "")

	// Post the offer after a short delay; AwaitOffer should succeed once it arrives.
	go func() {
		time.Sleep(100 * time.Millisecond)
		c.PostOffer(context.Background(), "late", "peer-a", Offer{Ufrag: "u"})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := c.AwaitOffer(ctx, "late", "peer-a", 50*time.Millisecond)
	if err != nil {
		t.Fatalf("AwaitOffer: %v", err)
	}
	if got.Ufrag != "u" {
		t.Errorf("got %+v", got)
	}
}

func TestClient_awaitOfferTimesOut(t *testing.T) {
	srv := NewServer("")
	ts := startTestServer(t, srv)
	c := NewClient(ts.URL(), "")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := c.AwaitOffer(ctx, "never", "peer-a", 30*time.Millisecond)
	if err == nil {
		t.Fatal("expected context deadline error")
	}
}

// TestPostOffer_clearsStaleAnswer pins the reconnect-correctness contract:
// when a peer posts a new offer for a session, any answer from the *previous*
// session must be invalidated. Without this, peer-a on reconnect reads back
// the peer's old ufrag/pwd/candidates and wastes an ICE check cycle (~2 min)
// before peer-b catches up and overwrites the answer.
func TestPostOffer_clearsStaleAnswer(t *testing.T) {
	srv := NewServer("")
	ts := startTestServer(t, srv)
	c := NewClient(ts.URL(), "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Round 1: peer-b posts an answer (simulating a prior successful handshake).
	if err := c.PostAnswer(ctx, "s1", Answer{Ufrag: "stale", Pwd: "stale"}); err != nil {
		t.Fatalf("PostAnswer (stale): %v", err)
	}

	// Round 2: peer-a posts a fresh offer for the same session.
	if err := c.PostOffer(ctx, "s1", "peer-a", Offer{Ufrag: "fresh", Pwd: "fresh"}); err != nil {
		t.Fatalf("PostOffer (fresh): %v", err)
	}

	// GetAnswer must now return NotFound — no stale answer should leak.
	_, err := c.GetAnswer(ctx, "s1")
	if err == nil {
		t.Fatal("stale answer still present after new offer; expected NotFound")
	}
	if !IsNotFound(err) {
		t.Errorf("expected IsNotFound, got %v", err)
	}
}

// TestPostOffer_unrelatedSessionAnswerUntouched guards against over-aggressive
// clearing: a new offer for session A must not affect session B's answer.
func TestPostOffer_unrelatedSessionAnswerUntouched(t *testing.T) {
	srv := NewServer("")
	ts := startTestServer(t, srv)
	c := NewClient(ts.URL(), "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.PostAnswer(ctx, "sA", Answer{Ufrag: "ansA"}); err != nil {
		t.Fatalf("PostAnswer sA: %v", err)
	}
	if err := c.PostOffer(ctx, "sB", "peer-a", Offer{Ufrag: "offB"}); err != nil {
		t.Fatalf("PostOffer sB: %v", err)
	}

	got, err := c.GetAnswer(ctx, "sA")
	if err != nil {
		t.Fatalf("GetAnswer sA: %v", err)
	}
	if got.Ufrag != "ansA" {
		t.Errorf("session A's answer got clobbered: %+v", got)
	}
}

// TestAwaitAnswer_reconnectWaitsForFresh models the real reconnect path:
// peer-a posts a new offer, then AwaitAnswer. It must block until peer-b
// posts a fresh answer — not fire immediately on a leftover one.
func TestAwaitAnswer_reconnectWaitsForFresh(t *testing.T) {
	srv := NewServer("")
	ts := startTestServer(t, srv)
	c := NewClient(ts.URL(), "")

	// Simulate the previous session's leftover answer still sitting on the server.
	if err := c.PostAnswer(context.Background(), "recon", Answer{Ufrag: "stale", Pwd: "stale"}); err != nil {
		t.Fatalf("seed PostAnswer: %v", err)
	}

	// Peer-a reconnects: posts a fresh offer, then polls for an answer.
	// A fresh answer from peer-b arrives a bit later.
	if err := c.PostOffer(context.Background(), "recon", "peer-a", Offer{Ufrag: "freshO"}); err != nil {
		t.Fatalf("PostOffer: %v", err)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = c.PostAnswer(context.Background(), "recon", Answer{Ufrag: "freshA", Pwd: "freshA"})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := c.AwaitAnswer(ctx, "recon", 20*time.Millisecond)
	if err != nil {
		t.Fatalf("AwaitAnswer: %v", err)
	}
	if got.Ufrag != "freshA" {
		t.Fatalf("AwaitAnswer returned stale data: %+v", got)
	}
}

// TestAwaitAnswerMatching_skipsMismatchedGen verifies that a stale answer
// whose Gen doesn't match the caller's current offer-gen is ignored.
// This is the peer-b-read-stale-offer race guard: without it, peer-a
// would grab an answer that targets a previous offer and its ICE checks
// would fail with mismatched credentials.
func TestAwaitAnswerMatching_skipsMismatchedGen(t *testing.T) {
	srv := NewServer("")
	ts := startTestServer(t, srv)
	c := NewClient(ts.URL(), "")

	// Seed a stale answer tagged with an old gen.
	if err := c.PostAnswer(context.Background(), "s", Answer{Ufrag: "stale", Gen: "old-gen"}); err != nil {
		t.Fatalf("seed PostAnswer: %v", err)
	}

	// A fresh answer with the matching gen arrives shortly.
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = c.PostAnswer(context.Background(), "s", Answer{Ufrag: "fresh", Gen: "new-gen"})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := c.AwaitAnswerMatching(ctx, "s", 20*time.Millisecond, "new-gen")
	if err != nil {
		t.Fatalf("AwaitAnswerMatching: %v", err)
	}
	if got.Ufrag != "fresh" || got.Gen != "new-gen" {
		t.Fatalf("got stale answer: %+v", got)
	}
}

// TestAwaitAnswerMatching_emptyWantAcceptsAny preserves the back-compat
// AwaitAnswer behavior.
func TestAwaitAnswerMatching_emptyWantAcceptsAny(t *testing.T) {
	srv := NewServer("")
	ts := startTestServer(t, srv)
	c := NewClient(ts.URL(), "")

	if err := c.PostAnswer(context.Background(), "s", Answer{Ufrag: "any", Gen: "whatever"}); err != nil {
		t.Fatalf("seed PostAnswer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	got, err := c.AwaitAnswerMatching(ctx, "s", 20*time.Millisecond, "")
	if err != nil {
		t.Fatalf("AwaitAnswerMatching: %v", err)
	}
	if got.Ufrag != "any" {
		t.Fatalf("got %+v", got)
	}
}

func TestServer_missingOfferReturns404(t *testing.T) {
	srv := NewServer("")
	ts := startTestServer(t, srv)
	c := NewClient(ts.URL(), "")

	_, err := c.GetOffer(context.Background(), "nope", "peer-a")
	if err == nil {
		t.Fatal("expected not found error")
	}
	if !IsNotFound(err) {
		t.Errorf("expected IsNotFound, got %v", err)
	}
}
