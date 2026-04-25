package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleConfig = `{
  "session_id": "test-session",
  "signaling": {
    "url": "http://signaling.example.com:8901/punch",
    "shared_secret": "${TEST_SECRET}"
  },
  "ice": {
    "stun": ["stun.l.google.com:19302"],
    "turn": []
  },
  "peers": {
    "peer-a": {
      "listen": [
        {"local_port": 8080, "remote_port": 8080, "name": "http"},
        {"local_port": 5432, "remote_port": 5432, "name": "pg"}
      ]
    },
    "peer-b": {
      "dial": [
        {"local_port": 8080, "name": "http"},
        {"local_port": 5432, "name": "pg"}
      ]
    }
  }
}`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_parsesFullConfig(t *testing.T) {
	t.Setenv("TEST_SECRET", "hunter2")
	p := writeConfig(t, sampleConfig)

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.SessionID != "test-session" {
		t.Errorf("SessionID = %q, want test-session", cfg.SessionID)
	}
	if cfg.Signaling.SharedSecret != "hunter2" {
		t.Errorf("shared_secret not expanded: %q", cfg.Signaling.SharedSecret)
	}
	if len(cfg.ICE.STUN) != 1 || cfg.ICE.STUN[0] != "stun.l.google.com:19302" {
		t.Errorf("STUN = %v", cfg.ICE.STUN)
	}
	a, ok := cfg.Peers["peer-a"]
	if !ok {
		t.Fatal("peer-a missing")
	}
	if len(a.Listen) != 2 {
		t.Fatalf("peer-a listen = %d, want 2", len(a.Listen))
	}
	if a.Listen[0].Name != "http" || a.Listen[0].LocalPort != 8080 || a.Listen[0].RemotePort != 8080 {
		t.Errorf("listen[0] = %+v", a.Listen[0])
	}
}

func TestLoad_defaultSTUNHasDiverseProviders(t *testing.T) {
	// Some networks block UDP to specific STUN providers (we've hit ISPs
	// that silently drop stun.l.google.com). If all default STUNs share a
	// single operator, one outage leaves a peer with host-only candidates
	// and ICE can't reach Connected. Keep defaults diverse.
	body := `{
      "session_id": "s",
      "signaling": {"url": "http://x"},
      "peers": {"peer-a": {}, "peer-b": {}}
    }`
	p := writeConfig(t, body)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ICE.STUN) < 2 {
		t.Fatalf("default STUN has %d entries, want >= 2", len(cfg.ICE.STUN))
	}
	providers := map[string]bool{}
	for _, host := range cfg.ICE.STUN {
		// host is "domain:port"; strip port, then take the registrable
		// domain (last two labels).
		d := host
		if i := strings.LastIndex(d, ":"); i >= 0 {
			d = d[:i]
		}
		labels := strings.Split(d, ".")
		if len(labels) >= 2 {
			d = labels[len(labels)-2] + "." + labels[len(labels)-1]
		}
		providers[d] = true
	}
	if len(providers) < 2 {
		t.Errorf("default STUN providers = %v, want >= 2 distinct", providers)
	}
}

func TestLoad_validateStreamNameUniqueness(t *testing.T) {
	body := `{
      "session_id": "s",
      "signaling": {"url": "http://x"},
      "peers": {
        "peer-a": {"listen": [
          {"local_port": 1, "remote_port": 1, "name": "dup"},
          {"local_port": 2, "remote_port": 2, "name": "dup"}
        ]},
        "peer-b": {}
      }
    }`
	p := writeConfig(t, body)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for duplicate stream name, got nil")
	}
}

func TestLoad_validateRoleStructure(t *testing.T) {
	// Peer keys must be peer-a and peer-b.
	body := `{
      "session_id": "s",
      "signaling": {"url": "http://x"},
      "peers": {"peer-a": {}, "peer-z": {}}
    }`
	p := writeConfig(t, body)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for unknown peer role")
	}
}

func TestRoleOther(t *testing.T) {
	if Other("peer-a") != "peer-b" {
		t.Errorf("Other(peer-a) = %q", Other("peer-a"))
	}
	if Other("peer-b") != "peer-a" {
		t.Errorf("Other(peer-b) = %q", Other("peer-b"))
	}
}

func TestLoad_missingFile(t *testing.T) {
	_, err := Load("/nonexistent/path.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}
