# Benchmarks

Two instruments, both in this repo, both runnable by anyone. Every performance number fold publishes traces to one of them; if a claim doesn't resolve to a table on this page, it's a bug.

- **`bench/latency_test.go`** answers *"how much latency does fold add?"* — sequential calls, direct vs proxied, CI-gated on every merge at added p50 < 5 ms.
- **`tools/perf`** answers *"how many requests per second does one instance sustain, and at what p99?"* — the numbers an enterprise buyer asks for.

## Methodology

**Latency gate** (`make bench`): the same client calls the same in-process echo upstream directly and through a real gateway; 200 warmup + 2,000 measured sequential calls per side; the gate compares p50s. The 5 ms bound is deliberately loose for shared CI runners — it gates regressions, not records.

**Load test** (`make loadtest`): three processes, so the load generator, the gateway, and the upstream never share a scheduler. The gateway is the real production entry — the built `./cmd/fold` binary with a config file. The driver models real MCP clients: each "connection" is an official-SDK client session (initialize once, then sequential `tools/call`), because fold's client side is session-keyed — a load test should measure what a real client experiences, including the SDK on both ends. Every stage runs DIRECT (driver → upstream) and FOLD (driver → gateway → upstream), so the upstream's own ceiling is visible: fold's RPS can never exceed direct's, and the gap is the honest cost. 3 s warmup, 10 s measured, connection sweep 8 / 64 / 256, client retries disabled (one call, one request).

**Hardware** for the numbers below: Apple M4 Pro (14 cores, 48 GB), Go 1.26, macOS, loopback. Raw results: [`launch/loadtest.json`](../launch/loadtest.json), [`launch/loadtest-passthrough.json`](../launch/loadtest-passthrough.json).

## The numbers (v1.3.0, 2026-08-05)

### Added latency (the gate's instrument, quiet machine)

| | p50 |
|---|---|
| Added p50, gateway vs direct | **~0.20 ms** (gateway p99 ≈ 0.57 ms) |

### Throughput (`tools/call`, namespaced mode)

| conns | direct req/s | fold req/s | fold p50 | fold p99 | retention |
|---|---|---|---|---|---|
| 8 | 12,796 | **8,038** | 0.9 ms | 2.0 ms | 63% |
| 64 | 14,551 | **9,263** | 6.2 ms | 19.0 ms | 64% |
| 256 | 22,878 | **13,379** | 16.7 ms | 60.1 ms | 58% |

Zero errors across the sweep. Passthrough mode (single un-namespaced upstream) measures within noise of namespaced. `tools/list` through fold sustains ~42,000 req/s at parity with direct — that's the [list cache](../README.md#configuration) absorbing reads inside its TTL, not proxy throughput; quote `tools/call`, not `tools/list`.

Headline, stated the way we'd defend it on a stage: **one fold instance sustained ~9,300 req/s of `tools/call` at 64 connections with p99 ≤ 19 ms — 13,400 req/s at 256 connections with p99 ≤ 61 ms — zero errors across the sweep, at 58–64% of the upstream's own direct ceiling.**

## Reading the numbers honestly

- **Loopback and a trivial echo upstream.** The instrument isolates fold's added work (routing, policy, audit accounting, proxying via the SDK) from everything it can't control — your network and your upstream's real latency. Your end-to-end numbers are your upstream's numbers plus roughly the table's gap.
- **One untuned instance.** Default config, no `GOMAXPROCS` pinning, no OS tuning, the driver competing for the same physical machine. Treat these as a floor for dedicated hardware, not a ceiling.
- **Retention (58–64%) is the honest framing.** The direct column is an in-process SDK server with near-zero work per call; fold adds a full second hop through the same SDK. Against a real upstream doing real work, the relative overhead shrinks toward the latency gate's ~0.2 ms.
- **Sessions are the unit of concurrency.** Each connection is a full SDK session, and fold holds a per-client upstream session behind it — 256 connections means 256 live sessions end to end. That is the deployment-realistic shape, not an artificial socket storm.
- **Don't benchmark demo.fold.run.** It's rate-limited (300 req/min), containerized on quarter-vCPU hardware, and fronted by Cloudflare — it demonstrates federation, not fold's ceiling. Run the harness instead; it's one command.

The harness earns its keep: its first run caught fold's upstream transport riding Go's default idle pool (2 conns/host), which forced a TCP handshake on most proxied requests above 2 concurrent calls — fixed in the same change that added the harness. In production, the continuous version of these measurements is the `fold_request_duration_seconds` / `fold_upstream_request_duration_seconds` histogram pair.

## Reproduce

```bash
make bench                                    # latency gate (what CI runs)
make loadtest                                 # namespaced sweep
FOLD_LOAD_MODE=passthrough make loadtest      # single-upstream mode
```

Knobs: `FOLD_LOAD_CONNECTIONS` (default `8,64,256`), `FOLD_LOAD_DURATION` (s, default 10), `FOLD_LOAD_WARMUP` (s, default 3), `FOLD_LOAD_SCENARIOS` (`tools/call,tools/list`), `FOLD_LOAD_JSON` (write raw results), and `FOLD_LOAD_FOLD_URL`/`FOLD_LOAD_DIRECT_URL` to drive an already-running deployment instead of the loopback topology. The load test is deliberately **not** in CI: shared runners make throughput numbers noise; the latency gate is the merge-time guard.
