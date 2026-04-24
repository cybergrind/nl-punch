package signaling

import (
	"net/http/httptest"
	"testing"
)

type testServer struct {
	ts *httptest.Server
}

func (t *testServer) URL() string { return t.ts.URL }

func startTestServer(t *testing.T, s *Server) *testServer {
	t.Helper()
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &testServer{ts: ts}
}
