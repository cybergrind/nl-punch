package transport

// KCPProfile bundles the tuning knobs passed to kcp-go's SetNoDelay and
// window-size calls. We expose named presets so operators pick a mode
// instead of fiddling with individual numbers.
type KCPProfile struct {
	// SetNoDelay(nodelay, interval_ms, resend, nc).
	NoDelay  int
	Interval int // milliseconds
	Resend   int
	NC       int // 1 = no congestion control (low-latency)

	// SetWindowSize(send, recv). Values in packets, not bytes.
	SendWindow int
	RecvWindow int

	// SetMtu. 0 → library default.
	MTU int
}

// Profile returns the preset for a named profile.
// Defaults to LowLatency on unknown input; validation happens in config.
func Profile(name string) KCPProfile {
	switch name {
	case "balanced":
		return KCPProfile{
			NoDelay: 1, Interval: 20, Resend: 2, NC: 1,
			SendWindow: 512, RecvWindow: 512,
		}
	case "bandwidth":
		return KCPProfile{
			NoDelay: 0, Interval: 40, Resend: 0, NC: 0,
			SendWindow: 1024, RecvWindow: 1024,
		}
	case "low-latency":
		fallthrough
	default:
		// Aggressive on rexmit (10 ms tick, fast resend after 2 dup ACKs).
		// Congestion control ENABLED (NC=0) — the original NC=1 flooded
		// asymmetric home-ISP uplinks, starved return-path ACKs and yamux
		// keepalives, and killed the tunnel within ~15 s of sustained
		// writes. Window trimmed to 128/256 pkts so that even a brief
		// oversend can't saturate the uplink beyond what cwnd allows.
		return KCPProfile{
			NoDelay: 1, Interval: 10, Resend: 2, NC: 0,
			SendWindow: 128, RecvWindow: 256,
		}
	}
}
