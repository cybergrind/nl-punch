// Package config loads and validates the nl-punch JSON config.
package config

import (
	"encoding/json"
	"fmt"
	"os"
)

type Config struct {
	SessionID string          `json:"session_id"`
	Signaling SignalingConfig `json:"signaling"`
	ICE       ICEConfig       `json:"ice"`
	Transport TransportConfig `json:"transport"`
	Peers     map[string]Peer `json:"peers"`
}

type SignalingConfig struct {
	URL          string `json:"url"`
	SharedSecret string `json:"shared_secret"`
}

type ICEConfig struct {
	STUN []string     `json:"stun"`
	TURN []TURNServer `json:"turn"`
}

type TURNServer struct {
	URI  string `json:"uri"`
	User string `json:"user"`
	Pass string `json:"pass"`
}

type TransportConfig struct {
	Profile string `json:"profile"`
}

type Peer struct {
	Listen []ListenSpec `json:"listen"`
	Dial   []DialSpec   `json:"dial"`
}

type ListenSpec struct {
	LocalPort  int    `json:"local_port"`
	RemotePort int    `json:"remote_port"`
	Name       string `json:"name"`
}

type DialSpec struct {
	LocalPort int    `json:"local_port"`
	Name      string `json:"name"`
}

const (
	RolePeerA = "peer-a"
	RolePeerB = "peer-b"

	ProfileLowLatency = "low-latency"
	ProfileBalanced   = "balanced"
	ProfileBandwidth  = "bandwidth"
)

var validProfiles = map[string]bool{
	ProfileLowLatency: true,
	ProfileBalanced:   true,
	ProfileBandwidth:  true,
}

// Other returns the peer role that is NOT role.
func Other(role string) string {
	if role == RolePeerA {
		return RolePeerB
	}
	return RolePeerA
}

// Load reads, parses, env-expands, defaults, and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	expanded := os.ExpandEnv(string(raw))

	var cfg Config
	if err := json.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	applyDefaults(&cfg)
	if err := validate(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Transport.Profile == "" {
		cfg.Transport.Profile = ProfileLowLatency
	}
	if len(cfg.ICE.STUN) == 0 {
		// Mix providers: single-provider outages are real (e.g. some ISPs
		// block UDP to stun.l.google.com specifically), and ICE gathering
		// fails silently when no srflx is discovered. Cloudflare fronts
		// first because it answers on 3478 (a registered STUN port) and
		// has broader reachability than Google's alt ports.
		cfg.ICE.STUN = []string{
			"stun.cloudflare.com:3478",
			"stun.l.google.com:19302",
			"stun.nextcloud.com:443",
		}
	}
}

func validate(cfg *Config) error {
	if cfg.SessionID == "" {
		return fmt.Errorf("session_id is required")
	}
	if cfg.Signaling.URL == "" {
		return fmt.Errorf("signaling.url is required")
	}
	if !validProfiles[cfg.Transport.Profile] {
		return fmt.Errorf("transport.profile %q invalid (want low-latency|balanced|bandwidth)", cfg.Transport.Profile)
	}

	for role := range cfg.Peers {
		if role != RolePeerA && role != RolePeerB {
			return fmt.Errorf("unknown peer role %q (must be peer-a or peer-b)", role)
		}
	}
	if _, ok := cfg.Peers[RolePeerA]; !ok {
		return fmt.Errorf("peer-a missing from peers")
	}
	if _, ok := cfg.Peers[RolePeerB]; !ok {
		return fmt.Errorf("peer-b missing from peers")
	}

	// Stream-name uniqueness per peer (listen and dial share a namespace).
	for role, peer := range cfg.Peers {
		seen := map[string]bool{}
		for _, l := range peer.Listen {
			if l.Name == "" {
				return fmt.Errorf("%s: listen entry with empty name", role)
			}
			if seen[l.Name] {
				return fmt.Errorf("%s: duplicate stream name %q", role, l.Name)
			}
			seen[l.Name] = true
		}
		for _, d := range peer.Dial {
			if d.Name == "" {
				return fmt.Errorf("%s: dial entry with empty name", role)
			}
			if seen[d.Name] {
				return fmt.Errorf("%s: duplicate stream name %q", role, d.Name)
			}
			seen[d.Name] = true
		}
	}
	return nil
}
