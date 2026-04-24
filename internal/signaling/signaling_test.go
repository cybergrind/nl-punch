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
