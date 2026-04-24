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
		// Aggressive: 10ms tick, fast resend after 2 dup ACKs, no cwnd.
		return KCPProfile{
			NoDelay: 1, Interval: 10, Resend: 2, NC: 1,
			SendWindow: 1024, RecvWindow: 1024,
		}
	}
}
