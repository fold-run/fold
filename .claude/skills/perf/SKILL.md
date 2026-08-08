---
name: perf
description: Investigate or fix fold proxy-path performance — run the added-latency gate, profile with the bench-profiler agent, and apply the move-to-snapshot-build fix pattern. Use when make bench fails, when a change touches the hot path, or when the user asks about gateway latency or allocations.
---

# fold proxy-path performance

The merge gate: added p50 through the gateway < 5 ms
(`bench/latency_test.go` `TestAddedLatencyGate`, skipped unless
`FOLD_BENCH=1`). Performance here is a test, not a vibe — every claim
needs a `BENCH_RESULT` number behind it.

**The gate has a blind spot, by design.** Its fixture is one upstream with
one trivial tool, so it measures the proxy hop and nothing that scales with
federation size. For anything touching the list path — merge, policy
filtering, namespacing, cursors — the instrument is
`go test ./gateway -run '^$' -bench BenchmarkFederatedListTools -benchmem`,
and allocations per request are the signal to watch (they are stable across
machines in a way ns/op is not). `FOLD_LOAD_UPSTREAMS` / `FOLD_LOAD_TOOLS`
give `tools/perf` the same federated shape.

## Workflow

1. **Measure**: launch the **bench-profiler** agent (yellow) to run the
   gate, get a clean baseline via stash-bisect if the working diff touched
   the proxy path, and profile if the gate is blown. It returns numbers
   and top contributors with file:line.

2. **Fix** using the repo's pattern — per-request work moves to
   snapshot-build time:
   - Anything derivable from config alone (compiled matchers, key
     prefixes, indexes, namespace tables) is computed once when the
     `routes` snapshot is built and read per-request from the snapshot.
   - Hot-path keys: preallocate/intern rather than `fmt.Sprintf` per
     request.
   - Never fix latency by buffering less *correctly* — streaming
     pass-through is an invisibility requirement, not an optimization.
   - Never fix by loosening the gate threshold or skipping the bench.

3. **Verify**: re-run `make bench` 2–3 times, then `make race` (snapshot
   precomputation moves state — races follow), then `make check`.

4. **Report** the before/after `BENCH_RESULT` lines verbatim.

## When the gate fails in CI but passes locally

CI runners are slower and noisier. Compare the CI run's `BENCH_RESULT`
against recent green runs (`gh run view --log` on both) before concluding
regression vs flake. A genuine regression shows in added_p50 relative to
direct_p50, not in absolute numbers.
