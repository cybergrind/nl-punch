package signaling

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

// Server is an in-memory rendezvous for offers/answers. Not persisted;
// restarts drop in-flight sessions (they're short-lived so this is fine).
type Server struct {
	secret string

	mu      sync.Mutex
	offers  map[string]Offer  // key: sessionID + "|" + role
	answers map[string]Answer // key: sessionID
}

func NewServer(secret string) *Server {
	return &Server{
		secret:  secret,
		offers:  map[string]Offer{},
		answers: map[string]Answer{},
	}
}

const secretHeader = "X-Nl-Punch-Secret"

// Handler returns an http.Handler that serves /punch/offer and /punch/answer.
// Mount it wherever; Client defaults to path "/punch".
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/punch/offer", s.handleOffer)
	mux.HandleFunc("/punch/answer", s.handleAnswer)
	// Bare root for tiny health probe; harmless.
	mux.HandleFunc("/punch/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	return s.authMiddleware(mux)
}

func (s *Server) authMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.secret != "" && r.Header.Get(secretHeader) != s.secret {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (s *Server) handleOffer(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var body struct {
			SessionID string `json:"session_id"`
			Role      string `json:"role"`
			Offer     Offer  `json:"offer"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if body.SessionID == "" || body.Role == "" {
			http.Error(w, "session_id and role required", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.offers[body.SessionID+"|"+body.Role] = body.Offer
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	case http.MethodGet:
		sess := r.URL.Query().Get("session_id")
		role := r.URL.Query().Get("role")
		if sess == "" || role == "" {
			http.Error(w, "session_id and role required", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		off, ok := s.offers[sess+"|"+role]
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, off)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var body struct {
			SessionID string `json:"session_id"`
			Answer    Answer `json:"answer"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if body.SessionID == "" {
			http.Error(w, "session_id required", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.answers[body.SessionID] = body.Answer
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	case http.MethodGet:
		sess := r.URL.Query().Get("session_id")
		if sess == "" {
			http.Error(w, "session_id required", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		ans, ok := s.answers[sess]
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, ans)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// Clear drops a session's offer/answer state. Called by the active peer
// after a successful ICE handshake so stale candidates don't linger.
func (s *Server) Clear(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.offers {
		if strings.HasPrefix(k, sessionID+"|") {
			delete(s.offers, k)
		}
	}
	delete(s.answers, sessionID)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
