package signaling

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrNotFound is returned when the peer's offer/answer hasn't been posted yet.
var ErrNotFound = errors.New("signaling: not found")

// IsNotFound reports whether an error (possibly wrapped) is ErrNotFound.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// Client talks to a signaling Server. baseURL may be with or without
// trailing /punch; both are accepted.
type Client struct {
	base   string
	secret string
	http   *http.Client
}

func NewClient(baseURL, secret string) *Client {
	base := strings.TrimRight(baseURL, "/")
	// Caller may have given the top-level URL (http://host:port) or the
	// /punch path directly (http://host:port/punch). Normalize to path form.
	if !strings.HasSuffix(base, "/punch") {
		base = base + "/punch"
	}
	return &Client{
		base:   base,
		secret: secret,
		http:   &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		reqBody = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reqBody)
	if err != nil {
		return err
	}
	if c.secret != "" {
		req.Header.Set(secretHeader, c.secret)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent:
		if out != nil && resp.StatusCode == http.StatusOK {
			return json.NewDecoder(resp.Body).Decode(out)
		}
		return nil
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusUnauthorized:
		return fmt.Errorf("signaling: %s (401)", strings.TrimSpace(readBody(resp)))
	default:
		return fmt.Errorf("signaling %s %s: %d %s",
			method, path, resp.StatusCode, strings.TrimSpace(readBody(resp)))
	}
}

func readBody(r *http.Response) string {
	buf, _ := io.ReadAll(io.LimitReader(r.Body, 1024))
	return string(buf)
}

// PostOffer uploads this peer's offer for (sessionID, role).
func (c *Client) PostOffer(ctx context.Context, sessionID, role string, o Offer) error {
	return c.do(ctx, http.MethodPost, "/offer", map[string]any{
		"session_id": sessionID,
		"role":       role,
		"offer":      o,
	}, nil)
}

// GetOffer reads the offer posted for (sessionID, role). ErrNotFound if none.
func (c *Client) GetOffer(ctx context.Context, sessionID, role string) (Offer, error) {
	var o Offer
	q := url.Values{}
	q.Set("session_id", sessionID)
	q.Set("role", role)
	err := c.do(ctx, http.MethodGet, "/offer?"+q.Encode(), nil, &o)
	return o, err
}

// PostAnswer uploads this peer's answer for sessionID.
func (c *Client) PostAnswer(ctx context.Context, sessionID string, a Answer) error {
	return c.do(ctx, http.MethodPost, "/answer", map[string]any{
		"session_id": sessionID,
		"answer":     a,
	}, nil)
}

// GetAnswer reads the answer posted for sessionID. ErrNotFound if none.
func (c *Client) GetAnswer(ctx context.Context, sessionID string) (Answer, error) {
	var a Answer
	q := url.Values{}
	q.Set("session_id", sessionID)
	err := c.do(ctx, http.MethodGet, "/answer?"+q.Encode(), nil, &a)
	return a, err
}

// AwaitOffer polls GetOffer until it returns a non-ErrNotFound result or
// ctx is cancelled. Use for the side that's waiting on its peer.
func (c *Client) AwaitOffer(ctx context.Context, sessionID, role string, every time.Duration) (Offer, error) {
	for {
		o, err := c.GetOffer(ctx, sessionID, role)
		if err == nil {
			return o, nil
		}
		if !IsNotFound(err) {
			return Offer{}, err
		}
		select {
		case <-time.After(every):
		case <-ctx.Done():
			return Offer{}, ctx.Err()
		}
	}
}

// AwaitAnswer mirrors AwaitOffer.
func (c *Client) AwaitAnswer(ctx context.Context, sessionID string, every time.Duration) (Answer, error) {
	return c.AwaitAnswerMatching(ctx, sessionID, every, "")
}

// AwaitAnswerMatching polls GetAnswer until a non-ErrNotFound answer is
// available whose Gen equals wantGen (when wantGen is non-empty). An
// empty wantGen accepts any answer (back-compat behavior). A non-matching
// answer is treated the same as "not found" — keep polling — so a stale
// leftover from a prior attempt or a concurrent peer-b cycle doesn't
// short-circuit the handshake.
func (c *Client) AwaitAnswerMatching(ctx context.Context, sessionID string, every time.Duration, wantGen string) (Answer, error) {
	for {
		a, err := c.GetAnswer(ctx, sessionID)
		if err == nil {
			if wantGen == "" || a.Gen == wantGen {
				return a, nil
			}
			// Mismatched gen: stale answer. Keep polling.
		} else if !IsNotFound(err) {
			return Answer{}, err
		}
		select {
		case <-time.After(every):
		case <-ctx.Done():
			return Answer{}, ctx.Err()
		}
	}
}
