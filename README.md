# nl-punch

A symmetric UDP hole-punching TCP forwarder. Two instances — one per peer —
establish a single ICE/UDP association, layer KCP + yamux on top, and pump
configured TCP ports over multiplexed streams.

Think of it as a drop-in replacement for a chain of SSH `-L` / `-R` tunnels
between two NATed hosts: zero root, zero kernel modules, a static binary
on each side, and a tiny HTTP signaling endpoint somewhere both peers can
reach.

- No root / no TUN / no kernel extensions — runs as an unprivileged user process.
- Static binary, CGO off. ~8 MB on `darwin/arm64`, ~9 MB on `linux/amd64`.
- Multiplexed: many concurrent TCP connections share one UDP tunnel.
- Tunable latency/bandwidth tradeoff via named KCP profiles.

## Status

Early / experimental. Works between two cone-NAT peers (the common home
ISP case). TURN is plumbed through as a config field but not
field-validated. No payload encryption — see _Limitations_.

---

## Architecture

```
  peer-a                                            peer-b
  ──────                                            ──────
  your app  ── tcp ──►  nl-punch :PORT                   nl-punch ──► 127.0.0.1:PORT (your service)
                         │                                  ▲
                         │    ICE (UDP + STUN + optional    │
                         │    TURN) — one UDP association   │
                         │    carrying KCP reliable frames  │
                         ▼                                  │
                     ┌─────────────────────────────────────────┐
                     │  yamux streams, each tagged with a      │
                     │  stream name from config                │
                     └─────────────────────────────────────────┘

  Signaling (offer/answer rendezvous, out of band):
    both peers ──► http://<signaling-host>/punch
                   (run nl-punch-signaling, or any HTTP server
                    that implements the 4 endpoints below)
```

Each TCP connection on a peer's local listener opens a fresh yamux stream,
tagged with the configured stream name. The peer's accept router reads
the tag and dials the matching local port on its own side. Bytes are
pumped bidirectionally until either side closes.

**Role convention.** `peer-a` is the passive (ICE controlling, yamux
server) side; `peer-b` is the active (ICE controlled, yamux client) side.
This is just a naming convention — ICE itself is symmetric — but the
roles must be opposite on the two peers.

---

## Quick start — local two-process test

Build:

```bash
make test        # run unit + e2e tests
make build       # cross-compile into dist/ for darwin/arm64 and linux/amd64
```

Run the signaling server (once, anywhere reachable from both peers; for
the local test below, your own machine is fine):

```bash
./dist/nl-punch-signaling-linux-amd64 --listen :8901
```

Run peer-a (listener side — exposes `127.0.0.1:9000` that tunnels to peer-b's
`127.0.0.1:8000`):

```bash
cat > /tmp/a.json <<'EOF'
{
  "session_id": "test",
  "signaling": {"url": "http://127.0.0.1:8901/punch"},
  "transport": {"profile": "low-latency"},
  "peers": {
    "peer-a": {"listen": [{"local_port": 9000, "remote_port": 8000, "name": "http"}]},
    "peer-b": {"dial":   [{"local_port": 8000, "name": "http"}]}
  }
}
EOF
./dist/nl-punch-linux-amd64 --config /tmp/a.json --role peer-a
```

Run peer-b (dialer side — whatever listens on `127.0.0.1:8000` becomes reachable):

```bash
./dist/nl-punch-linux-amd64 --config /tmp/a.json --role peer-b
```

Now `curl http://127.0.0.1:9000` on the peer-a machine hits whatever is
listening on `127.0.0.1:8000` on the peer-b machine. For a quick test,
`python3 -m http.server 8000` on peer-b's side.

---

## Command-line flags

### `nl-punch`

| Flag | Required | Description |
|------|----------|-------------|
| `--config PATH` | yes | JSON config file (schema below) |
| `--role ROLE` | yes | `peer-a` or `peer-b`; must match a key under `peers` |
| `--session ID` | no | overrides `session_id` from config — useful when reusing one config across sessions |
| `--signaling URL` | no | overrides `signaling.url` from config |
| `--secret S` | no | overrides `signaling.shared_secret`; also read from `$NLPUNCH_SECRET` |
| `--log-level L` | no | `debug`, `info` (default), `warn`, `error` |

### `nl-punch-signaling`

| Flag | Default | Description |
|------|---------|-------------|
| `--listen ADDR` | `:8901` | HTTP listen address |
| `--secret S` | `$NLPUNCH_SECRET` | shared secret sent in `X-Nl-Punch-Secret`; empty = no auth |

---

## Config schema

There is no shipped default that points at a specific server or opens
real ports — `config.example.json` intentionally uses placeholder values
you have to replace. A full schema walk-through:

```jsonc
{
  // Session name — both peers must agree. Used as the signaling key.
  // Pick something unique enough that it won't collide with another
  // deployment sharing the same signaling server.
  "session_id": "CHANGE-ME-to-a-unique-session-id",

  "signaling": {
    // Base URL of the rendezvous endpoint. "/punch" suffix added if absent.
    "url": "http://signaling.example.com:8901/punch",
    // Optional. ${ENV_VAR} is expanded from the process environment.
    "shared_secret": "${NLPUNCH_SECRET}"
  },

  "ice": {
    // STUN servers in host:port form. If this field is omitted the
    // defaults below are used.
    "stun": [
      "stun.cloudflare.com:3478",
      "stun.l.google.com:19302",
      "stun.nextcloud.com:443"
    ],
    // Optional TURN fallback. Used only if the direct path fails. URI
    // must be a full stun/turn URI, e.g. "turn:turn.example.com:3478?transport=udp".
    "turn": [
      // {"uri": "turn:...", "user": "...", "pass": "..."}
    ]
  },

  "transport": {
    // low-latency (default) | balanced | bandwidth — see "Transport profiles".
    "profile": "low-latency"
  },

  "peers": {
    // peer-a: the listener side. TCP clients connect to local_port;
    // bytes are tunneled to peer-b, which dials local_port on its side.
    "peer-a": {
      "listen": [
        {"local_port": 8080, "remote_port": 8080, "name": "http"}
      ]
    },

    // peer-b: the dialer side. When a stream tagged "http" arrives,
    // peer-b dials 127.0.0.1:8080 locally.
    "peer-b": {
      "dial": [
        {"local_port": 8080, "name": "http"}
      ]
    }
  }
}
```

### Field reference

| Field | Type | Meaning |
|-------|------|---------|
| `session_id` | string, required | Rendezvous key; both peers must use the same value |
| `signaling.url` | string, required | Base URL of the signaling endpoint |
| `signaling.shared_secret` | string | Optional. Sent as `X-Nl-Punch-Secret`; `${VAR}` expansion supported |
| `ice.stun[]` | `["host:port", ...]` | STUN servers; defaults to Cloudflare + Google + Nextcloud |
| `ice.turn[]` | `[{uri,user,pass}, ...]` | Optional TURN fallback |
| `transport.profile` | string | `low-latency` \| `balanced` \| `bandwidth` (default: `low-latency`) |
| `peers.<role>.listen[].name` | string | Stream tag; must be unique per peer and match the opposite side's tag |
| `peers.<role>.listen[].local_port` | int | TCP port this peer binds on `127.0.0.1` |
| `peers.<role>.listen[].remote_port` | int | Port the other peer dials when this stream arrives |
| `peers.<role>.dial[].name` | string | Stream tag this peer handles on the accepting side |
| `peers.<role>.dial[].local_port` | int | Port on `127.0.0.1` to dial when that stream arrives |

### Notes on asymmetric configs

The schema above uses one symmetric config with both peers declared. You
can equivalently give each peer a config that only contains its own half:
`--role peer-a` only reads `peers.peer-a`, etc. Stream names on `listen`
entries must match `dial` entries on the other peer; otherwise inbound
streams with unknown tags are dropped (with a warning).

Each peer may have both `listen` and `dial` blocks — nothing stops you
from exposing ports in both directions on the same tunnel. Stream names
share one namespace per peer, so keep them unique across both blocks.

---

## Transport profiles

KCP parameters are exposed as three named presets, picked with
`transport.profile`. All three multiplex many streams over one UDP
association; they differ in how aggressively KCP retransmits and paces
its writes.

| Profile | KCP `NoDelay(...)` | Window | Use case |
|---------|---------------------|--------|----------|
| `low-latency` (default) | `(1, 10, 2, 1)` | 1024 / 1024 pkts | Small chatty RPCs on a clean link. Fastest reaction to loss; highest bandwidth overhead |
| `balanced`     | `(1, 20, 2, 1)` | 512 / 512 pkts  | Middle ground |
| `bandwidth`    | `(0, 40, 0, 0)` | 1024 / 1024 pkts | Bulk transfers on a lossy or metered link. Congestion control enabled, slower recovery |

The tuple is KCP's `(nodelay, interval_ms, resend, nc)`:
- `nodelay=1` switches on KCP's fast-ack mode.
- `interval` is the internal update tick (smaller = lower latency, more CPU and packets).
- `resend` — retransmit after this many dup ACKs (0 disables fast-resend).
- `nc=1` disables congestion control (lowest latency, no fairness).

Stream-mode and ACK-no-delay are on for `low-latency` and `balanced`;
off for `bandwidth`.

---

## Signaling

ICE needs the two peers to exchange credentials + candidates before the
UDP path can be established. `nl-punch` does this over a tiny HTTP
rendezvous. You have two options:

**A. Standalone `nl-punch-signaling`** — ship the binary somewhere both
peers can reach, no persistent state:

```bash
./nl-punch-signaling-linux-amd64 --listen :8901 --secret "$NLPUNCH_SECRET"
```

**B. Piggy-back on an existing HTTP server** — add the four endpoints below
to whatever web service you already operate. The full reference
implementation is in `internal/signaling/server.go` (a drop-in
`http.Handler` available via `signaling.NewServer(secret).Handler()`).

| Method | Path | Body / Query |
|--------|------|--------------|
| POST | `/punch/offer`  | `{session_id, role, offer: {ufrag, pwd, candidates[]}}` |
| GET  | `/punch/offer`  | `?session_id=…&role=…` → `{ufrag, pwd, candidates[]}` |
| POST | `/punch/answer` | `{session_id, answer: {...}}` |
| GET  | `/punch/answer` | `?session_id=…` → `{...}` |

All requests carry `X-Nl-Punch-Secret: <secret>` when `--secret` is set on
the server.

---

## Deployment notes

The binary is self-contained (CGO off, statically linked on Linux). On
macOS a supervisor shell loop is the simplest way to keep it running:

```bash
#!/usr/bin/env bash
while true; do
  nl-punch \
      --config ~/nl-punch.json \
      --role peer-a \
      2>&1 | tee -a ~/nl-punch.log
  echo "nl-punch exited, restart in 2s"
  sleep 2
done
```

On Linux, a minimal systemd unit works just as well:

```ini
[Service]
Restart=always
RestartSec=2
Environment=NLPUNCH_SECRET=...
ExecStart=/usr/local/bin/nl-punch --config /etc/nl-punch.json --role peer-a
```

Once both peers are running, you should see log lines like
`ice state state=Connected` and then `transport up` on both sides, plus
`tcp listener bound` lines for each configured port on the listener side.

---

## Operational logs

`nl-punch` writes structured text logs to stderr. Notable lines:

- `ice state state=Connected role=peer-a` — ICE handshake succeeded.
- `transport up profile=low-latency role=peer-a` — KCP+yamux ready.
- `tcp listener bound name=<tag> addr=127.0.0.1:<port> remote_port=<port>` — one line per configured listener.
- `stats uptime=… streams_active=… bytes_in=… bytes_out=… mode=direct` — emitted every 30 s.
- `session ended err=… uptime=…` → followed by `reconnecting wait=…` — full ICE restart with exponential backoff (1 s → 60 s, ±20% jitter).

---

## Development

### Layout

```
nl-punch/
├── cmd/
│   ├── nl-punch/              # main binary (incl. e2e test)
│   └── nl-punch-signaling/    # standalone rendezvous server
├── internal/
│   ├── config/                # JSON loader + validation
│   ├── forward/               # TCP ↔ stream pump, tag framing
│   ├── ice/                   # pion/ice wrapper + PacketConnAdapter
│   ├── signaling/             # HTTP client + in-memory server
│   └── transport/             # KCP + yamux session wrapper, profiles
├── config.example.json
├── Makefile
└── go.mod
```

### Tests

Every package has tests; the suite runs in under 90 seconds end-to-end.

```bash
make test        # all packages
make test-v      # verbose
go test ./internal/forward/...   # one package
```

Package-by-package:

| Package | What's covered |
|---------|---------------|
| `config`    | parse, env-expansion, default profile, role validation, name uniqueness, diverse-STUN defaults |
| `signaling` | offer/answer round-trip, secret auth, poll-and-await, 404 semantics |
| `transport` | open/accept stream, concurrent streams, profile presets distinct |
| `forward`   | listen→dial echo, unknown-tag rejection, 16 KB byte-exactness |
| `ice`       | loopback handshake via in-memory signaling, timeout with no peer |
| `cmd/nl-punch` | full pipeline: ICE → KCP → yamux → TCP echo |

### Wire format (between peers)

Each yamux stream carries one TCP connection. The first byte of the
stream is a tag length N (1–255); the next N bytes are the ASCII tag;
everything after is raw TCP bytes.

```
  byte 0         : tag length (uint8)
  bytes 1..N     : tag (ASCII, e.g. "http")
  bytes N+1..    : opaque TCP payload
```

### Cross-compile

```bash
make build-darwin-arm64    # → dist/nl-punch-darwin-arm64
make build-linux-amd64     # → dist/nl-punch-linux-amd64
make build                 # both, plus both signaling binaries
```

Output is CGO-free, stripped (`-ldflags '-s -w'`), built with `-trimpath`.

---

## Limitations / out of scope

- **No TURN testing.** TURN config is plumbed through but hasn't been
  validated against a live relay. If the direct path fails on your NAT
  pair, test TURN before relying on it.
- **No automatic relay fallback.** If ICE fails repeatedly, `nl-punch`
  keeps retrying with exponential backoff; there's no built-in fallback
  to a different transport. Running a parallel SSH tunnel as a backstop
  is one option.
- **No encryption on the KCP payload.** The assumption is that the
  tunneled traffic is already authenticated at a higher layer (TLS,
  mTLS, SSH, …) and that the signaling shared secret is the gate on
  who can establish a session. If you need on-wire confidentiality of
  plaintext payloads, terminate TLS end-to-end or swap KCP's
  `BlockCrypt` slot (wired through, pass a non-nil cipher).
- **Symmetric NAT on both sides** will block the direct path. Configure
  TURN in that case.

---

## License

MIT. See [LICENSE](LICENSE).
