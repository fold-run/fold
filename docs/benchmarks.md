# Benchmarks

Four instruments, all in this repo, all runnable by anyone. Every performance number fold publishes traces to one of them; if a claim doesn't resolve to a table on this page, it's a bug.

- **`bench/latency_test.go`** answers *"how much latency does fold add?"* — sequential calls, direct vs proxied, CI-gated on every merge at added p50 < 5 ms.
- **`tools/perf`** answers *"how many requests per second does one instance sustain, and at what p99?"* — the numbers an enterprise buyer asks for.
- **`gateway/tenant_bench_test.go`** answers *"how many tenants can one config document hold?"* — tenant resolution runs once per authenticated request and its cost is set by how many tenants you declare, not by how big your federation is.
- **`gateway/perfbench_test.go`** answers *"what does federation itself cost?"* — the merge, policy filter, namespace rewrite, and cursor work on `tools/list`, measured gateway-side with warm caches and no client in the picture. This is the only instrument whose cost scales with the size of your federation.

## Methodology

**Latency gate** (`make bench`): the same client calls the same in-process echo upstream directly and through a real gateway; 200 warmup + 2,000 measured sequential calls per side; the gate compares p50s. The 5 ms bound is deliberately loose for shared CI runners — it gates regressions, not records.

**Load test** (`make loadtest`): three processes, so the load generator, the gateway, and the upstream never share a scheduler. The gateway is the real production entry — the built `./cmd/fold` binary with a config file. The driver models real MCP clients: each "connection" is an official-SDK client session (initialize once, then sequential `tools/call`), because fold's client side is session-keyed — a load test should measure what a real client experiences, including the SDK on both ends. Every stage runs DIRECT (driver → upstream) and FOLD (driver → gateway → upstream), so the upstream's own ceiling is visible: fold's RPS can never exceed direct's, and the gap is the honest cost. 3 s warmup, 10 s measured, connection sweep 8 / 64 / 256, client retries disabled (one call, one request).

**Federation size is a variable, not a constant.** Both instruments above default to one fixture upstream exposing one trivial tool, which isolates proxy overhead but exercises none of the work that scales with federation size. `FOLD_LOAD_UPSTREAMS` and `FOLD_LOAD_TOOLS` set the shape; the `tools/list` numbers below are reported per shape, because a single number for them would be meaningless.

**Federation cost** (`go test ./gateway -run '^$' -bench BenchmarkFederatedListTools -benchmem`): one `tools/list` through the gateway's own merge path with the list caches warm — no network, no client SDK. Isolating it this way matters: an end-to-end profile attributes most of its allocations to schema validation inside the *calling* client's SDK, which is not fold's cost to claim or to fix.

**Tenant resolution** (`go test ./gateway -run '^$' -bench BenchmarkResolveTenant -benchmem`): one principal resolved against a snapshot holding 10 to 10,000 tenant declarations, measured by selector shape because the shapes index differently. Each case resolves a principal matching the last declaration — not pessimism about ordering, since resolution visits every candidate regardless, but it keeps the match out of the noise.

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

Zero errors across the sweep. Passthrough mode (single un-namespaced upstream) measures within noise of namespaced.

Headline, stated the way we'd defend it on a stage: **one fold instance sustained ~9,300 req/s of `tools/call` at 64 connections with p99 ≤ 19 ms — 13,400 req/s at 256 connections with p99 ≤ 61 ms — zero errors across the sweep, at 58–64% of the upstream's own direct ceiling.**

### Federation cost (`tools/list`, gateway-side, warm caches)

*Re-measured 2026-08-15 on `main`, after v1.14.0 and including the allocation fix noted below (unreleased at the time of measurement); the `tools/call` tables above are the v1.3.0 run and have not been re-measured since.*

What one merged list costs the gateway itself: fan-out, per-principal policy filter, namespace rewrite, and cursor fingerprint. Median of 5 runs.

| federation | tools in list | ns/op | B/op | allocs/op |
|---|---|---|---|---|
| 1 upstream × 10 tools | 10 | 1,251 | 640 | 16 |
| 5 upstreams × 20 tools | 100 | 5,332 | 3,336 | 31 |
| 20 upstreams × 50 tools | 1,000 | **31,003** | 21,871 | 83 |
| 20 × 50, deny-by-default policy with globs | 1,000 | 54,190 | 21,866 | 83 |

The allocation counts are unchanged from the 2026-08-07 measurement, and for one release they were not: definition pinning (v1.12.0) added a captured variable to the closure each list method hands `cachedList`, which turned a statically-allocated function literal into one allocation per upstream per request — on the warm-cache path the feature's own design record promised to stay off. Restored on `main` after v1.14.0 by passing the upstream as a parameter instead of capturing it — so v1.12.0 through v1.14.0 carry the extra allocation; the regression and its fix are why this table is re-measured per release rather than trusted. The `ns/op` growth since is real and is governance the gateway now does on every list: tenant resolution, the visibility subset, and consumption metering all landed in that window.

Two properties worth stating, because both were once false: a warm list-cache hit is **~55 ns and one allocation regardless of list size** (the parsed form is memoized, not re-decoded per request), and **policy filtering allocates nothing** — the `_policy` row's allocation count is identical to the row above it, so the cost of filtering 1,000 tools per principal is CPU only.

### Tenant resolution (cardinality)

*Measured 2026-08-09 on `main`. Median of 5 runs, `go test ./gateway -run '^$' -bench BenchmarkResolveTenant -benchmem -count=5`.*

Every authenticated request resolves its principal to at most one tenant, and it cannot stop at the first match — refusing an ambiguous match means seeing every match. So the question [design-tenancy.md](design-tenancy.md#the-cardinality-problem--settled) left open was how many tenants a document can hold before that shows up in the latency gate. The answer, by shape of the tenant's selector:

| tenants declared | one claim = one value | one group | compound (issuer + claim + group) |
|---|---|---|---|
| 10 | 106 ns | 39 ns | 387 ns |
| 100 | 97 ns | 42 ns | 3,756 ns |
| 1,000 | 96 ns | 41 ns | 43,142 ns |
| 10,000 | **97 ns** | **41 ns** | **392,680 ns** |

Zero allocations in every cell, at every size.

The two single-dimension shapes are **flat in the number of tenants** — a document with ten thousand of them resolves as fast as one with ten — because each is a map lookup: the claim index is keyed by what the tenant requires and probed with the principal's value, the group index keyed by what the principal holds. Before that index existed, the same 10,000-tenant claim document measured **450 µs per request**, which against a gateway whose whole added p50 is ~200 µs is not a rounding error but the dominant cost of the request.

The third column is what stays linear, at ~39 ns per declaration, and it is stated rather than buried: a selector combining conditions (an issuer *and* a claim, groups *and* claims) is matched one at a time. Keep those in the tens. Per-customer tenancy wants one claim or one group, which is what an IdP asserts for a customer anyway. A realistic mixed document — seven single-claim tenants for every compound one — measures 61 µs at 10,000, all of it the compound eighth.

Two things the table does not show, both by construction: the index **narrows but never decides** — every candidate it produces is still put through the same `policy.MatchSubjects` a scan would use, so it can only ever be an accelerator — and a principal matching no tenant pays the same lookups and resolves to nothing, which is the steady state for a deployment that declares tenants for some callers and not others (58 µs at 10,000 mixed).

### Throughput (`tools/list`, by federation shape)

`tools/list` throughput is **payload-bound, not gateway-bound** — the numbers move with how many tools are in the response, not with fold's routing work. At 64 connections:

| federation | tools returned | direct req/s | fold req/s | fold p50 | fold p99 |
|---|---|---|---|---|---|
| 1 × 1 | 1 | 19,500 | **24,083** | 2.2 ms | 9.1 ms |
| 5 × 20 | 100 | 5,246 | **1,830** | 31.5 ms | 91.6 ms |
| 20 × 50 | 200 (one page of 1,000) | 2,749 | **1,069** | 54.8 ms | 143.5 ms |

Read this table with care. The direct column serves *one* upstream's tools while fold serves the merged page, so the two columns encode different payloads and their ratio is not a retention figure — at 20 × 50, fold is returning a 200-tool page against direct's 50. The gateway's own share of that work is the 25 µs in the table above; the rest is JSON encoding and transport on both sides. The one honest cross-shape statement: fold's `tools/list` throughput falls ~22× as the federation grows from 1 tool to 1,000, and it is the response size doing that.

Fold exceeding direct in the 1 × 1 row is the [list cache](configuration.md#upstreams) absorbing reads inside its TTL, not proxy throughput. Earlier versions of this page quoted a single `tools/list` figure measured at that shape; it never generalized, and it has been replaced by the table.

## Reading the numbers honestly

- **Loopback and a trivial echo upstream.** The instrument isolates fold's added work (routing, policy, audit accounting, proxying via the SDK) from everything it can't control — your network and your upstream's real latency. Your end-to-end numbers are your upstream's numbers plus roughly the table's gap.
- **One untuned instance.** Default config, no `GOMAXPROCS` pinning, no OS tuning, the driver competing for the same physical machine. Treat these as a floor for dedicated hardware, not a ceiling. The 20 × 50 rows run 20 fixture upstream processes alongside the driver and the gateway on one machine, so they carry more scheduling contention than the smaller shapes.
- **A `tools/call` number does not imply a `tools/list` number.** They scale on different axes: `tools/call` routes one name and proxies one request whatever the federation's size, while `tools/list` touches every upstream and returns a payload proportional to the whole catalog. Quote them separately, and quote `tools/list` with its federation shape attached.
- **Retention (58–64%) is the honest framing.** The direct column is an in-process SDK server with near-zero work per call; fold adds a full second hop through the same SDK. Against a real upstream doing real work, the relative overhead shrinks toward the latency gate's ~0.2 ms.
- **Sessions are the unit of concurrency.** Each connection is a full SDK session, and fold holds a per-client upstream session behind it — 256 connections means 256 live sessions end to end. That is the deployment-realistic shape, not an artificial socket storm.
- **Don't benchmark demo.fold.run.** It's rate-limited (300 req/min), containerized on quarter-vCPU hardware, and fronted by Cloudflare — it demonstrates federation, not fold's ceiling. Run the harness instead; it's one command.

The harness earns its keep: its first run caught fold's upstream transport riding Go's default idle pool (2 conns/host), which forced a TCP handshake on most proxied requests above 2 concurrent calls — fixed in the same change that added the harness. In production, the continuous version of these measurements is the `fold_request_duration_seconds` / `fold_upstream_request_duration_seconds` histogram pair.

## Reproduce

```bash
make bench                                    # latency gate (what CI runs)
make loadtest                                 # namespaced sweep
FOLD_LOAD_MODE=passthrough make loadtest      # single-upstream mode

# Federation cost, gateway-side:
go test ./gateway -run '^$' -bench BenchmarkFederatedListTools -benchmem

# Tenant resolution, by selector shape and tenant count:
go test ./gateway -run '^$' -bench BenchmarkResolveTenant -benchmem

# Throughput against a 20 × 50 federation:
FOLD_LOAD_UPSTREAMS=20 FOLD_LOAD_TOOLS=50 make loadtest
```

Knobs: `FOLD_LOAD_UPSTREAMS` (default 1), `FOLD_LOAD_TOOLS` (per upstream, default 1), `FOLD_LOAD_CONNECTIONS` (default `8,64,256`), `FOLD_LOAD_DURATION` (s, default 10), `FOLD_LOAD_WARMUP` (s, default 3), `FOLD_LOAD_SCENARIOS` (`tools/call,tools/list`), `FOLD_LOAD_JSON` (write raw results), and `FOLD_LOAD_FOLD_URL`/`FOLD_LOAD_DIRECT_URL` to drive an already-running deployment instead of the loopback topology. The load test is deliberately **not** in CI: shared runners make throughput numbers noise; the latency gate is the merge-time guard.
