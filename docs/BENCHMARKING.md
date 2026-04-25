# Benchmarking — further work

The Round-A→F sweeps in this repo gave us the first ground truth for how a
studio-to-scheduler nl-punch tunnel actually performs. That sweep is
useful, but it is narrow: one tuning, one pair of machines, one network
path, five-minute-class tests. This document collects concrete next
experiments worth running before we rely on nl-punch for production
NativeLink traffic.

The harness (`cmd/nl-punch-bench`) is already structured so that adding a
test is adding one Go func plus one entry in `bench-runs/run.sh`. Prefer
to extend the harness rather than bolt on ad-hoc scripts.

## Open problems the numbers should close

### 1. Server-push bulk delivery (ROUND F blocker)

Status: unsolved. `throughput-down` delivers 0 bytes through the tunnel
and ≥256 KB (full yamux initial window) through in-memory pipes.

Experiments, in order of cost:

- **Shrink `MaxStreamWindowSize`** to 16 KB in `internal/transport/transport.go`
  via `yamuxConfig`. If the bug is "exactly one window's worth arrives
  then stall," a smaller window exposes it as a repeating sawtooth;
  otherwise the test throughput crawls forward at one window per RTT.
  Either outcome is diagnostic.
- **Expose kcp-go SNMP counters** (`kcp.DefaultSnmp.Copy()`) on both
  peers and log them with the stats ticker. Compare
  `InPkts/OutPkts/RetransSegs/FastRetransSegs/LostSegs` per direction —
  if one direction has massive `RetransSegs`, the underlying UDP path
  is asymmetric.
- **Local pion/ICE reproducer.** Set up two `pionice.Agent` instances
  in-process with host candidates only and run the existing
  `TestSession_continuousServerPush` against that transport. If it
  reproduces in-process, debug pion. If it doesn't, the bug requires
  real NAT traversal to manifest and we need to instrument on the Macs.
- **Reverse the stream-open direction.** `forward.TCPListener.handle`
  always calls `OpenStream`; switch to `AcceptStream` on the bulk-read
  side (NL worker fetching from CAS on the scheduler) so the bulk side
  is the stream opener. Tests whether the bug is asymmetric between
  yamux opener/accepter.
- **Application-level keepalive on a dedicated stream.** If yamux Ping
  isn't enough (Round F falsified it), try: bidirectional 1-byte
  heartbeat over a permanently-open stream named `keepalive`. Same
  mechanism as yamux Ping but goes through a real data stream — forces
  the yamux flow-control state machine to update receive windows.

### 2. Sustained throughput without tunnel death

Currently the throughput-up direction dies around 90 s of sustained
write. NL builds run for hours. We need a test that matches that.

- Add `--throughput-sec` values of 30, 60, 90, 180 with a hard
  requirement that the tunnel survives afterwards (i.e., a post-test
  `latency` sample succeeds). Turn this into a red/green gate in CI
  once the fundamentals work.
- Introduce a **rate-limit** knob in the bench's `modeSink` / `modeUpload`
  handlers so we can measure "survival at N Mbps" — then sweep N from
  1 Mbps upward, picking the highest rate that doesn't drop the tunnel
  across 10 minutes.

### 3. Real-world workload replay

Today's bench uses uniform random bytes and steady-state rates. NL
traffic is bursty (scheduler dispatches an action, worker fetches a
few MB of inputs, runs the action, uploads ≤ tens of MB of outputs,
repeats). A synthetic replay that matches this shape would catch
regressions the steady-state bench misses.

- Capture bytes-per-second timeseries from a real 10-minute NL build
  via `iptables NFLOG` or a sidecar that reads `lsof`-style counters.
- Add a `replay` mode to the bench that reads a small TSV
  (`timestamp_ms,direction,bytes`) and reproduces the pattern.

### 4. Multi-stream scaling

The prod symptom that started this work was "13 active streams → yamux
keepalive fail." Our bench only ever opens 1 stream at a time.

- Extend `latency` and `throughput-up` to accept `--concurrency N` and
  run N streams in parallel. Report combined p50/p95 RTT and aggregate
  Mbps.
- A specific data point NL needs: 12 concurrent CAS reads of ~10 MB
  blobs (one per remote worker slot). That's the "scheduler feeds 12
  workers at once" case.

### 5. Long-idle survival

Round D idle=5 min already passes. The original prod bug manifested at
~25 min. Worth confirming at 30, 60, and 120 min with a trailing burst.

- Reuse the existing `idle` test; add `--idle-sec 3600`. Run it twice
  a week in CI against the real .132/.166 pair to catch regressions
  from ISP-level or upstream changes.

### 6. TURN fallback

`cfg.ICE.TURN` is plumbed through but never exercised. When studio's
STUN path is blocked (observed in the original Round-A incident
report), only TURN gets a tunnel up at all.

- Stand up a coturn instance on yy or a cheap VPS. Add its config to
  a new `config.turn.json` and add a sweep run under
  `bench-runs/turn/`. Key question: does TURN cap throughput lower
  than the 4 Mbps direct path we measured?

## Infrastructure work

### Reliable measurement

- **Record the live `sysctl kern.ipc.maxsockbuf` / TCP buffer sizes
  alongside each sweep** — the 4 Mbps upload number we recorded may
  be partly bounded by the local kernel, not the tunnel.
- **Rotate the bench results dir** once per day so we don't lose the
  historical series under the accidental `mv` in `deploy-and-bench.sh`.
- **Hash-check binaries before each run** and embed the git SHA into
  the JSONL `meta` event. Right now the label is a free-form string
  and reruns are hard to compare.

### Diffing runs

- A tiny Go tool `cmd/nl-punch-bench-diff` (or even a `jq` snippet)
  that reads two result dirs and emits a table of deltas. Would make
  "did this change regress anything?" a one-liner.

### Continuous nl-punch benchmarks

- Trigger the sweep from the controller on a cron (daily 05:00 local)
  against a standing bench tunnel. Gate on the idle test staying
  `broken=false`; failing runs ping a Slack webhook.

## Scope explicitly NOT in this doc

- Payload encryption (orthogonal to performance — when we add it,
  measure overhead separately).
- TURN *performance* across multiple regions (wait until TURN is a
  supported path).
- Protocol switching to QUIC or other multiplexers (big lift, not
  motivated by current data).
