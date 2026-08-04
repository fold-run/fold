---
name: conformance
description: Run, debug, or bump the pinned MCP conformance suite for fold. Use when conformance fails locally or in CI, when the scheduled drift workflow flags upstream movement, or when deliberately bumping CONFORMANCE_COMMIT / CONFORMANCE_PKG.
---

# fold conformance suite

`make conformance` runs the official MCP conformance suite through the
gateway fronting the conformance repo's everything-server in passthrough
mode. CI gates merges on **40/40 checks**. Requires go, node/npm/npx, git.

The pin lives in `scripts/conformance.sh`:
- `CONFORMANCE_COMMIT` — conformance repo commit (server fixture)
- `CONFORMANCE_PKG` — npm package version (the checker)

The scheduled drift workflow overrides these to `main`/`@latest` to catch
upstream movement early; the pin itself is bumped deliberately.

## Debugging a failure

1. Reproduce locally: `make conformance`. Note *which* check fails.
2. First question: **did the gateway stop being invisible?** Run the same
   scenario against the upstream directly (the script starts it on
   `UPSTREAM_PORT`, default 3901; gateway on 3902) and diff the behavior.
   - Differs through the gateway only → a fold bug. Usual suspects:
     response buffering/rewriting that federation doesn't require,
     namespacing applied in passthrough mode (it must not be), SSE
     stream handling, or a minted error replacing a pass-through error.
   - Same failure hitting the upstream directly → the suite or fixture
     moved; this is a pin/drift issue, not a fold bug.
3. Environment failures (ports busy, npx cache, node missing) look like
   hangs — check `wait_for` timeouts in the script output before
   suspecting the gateway.

## Bumping the pin (deliberate, never drive-by)

1. Test the target first without editing anything:
   `CONFORMANCE_COMMIT=<sha> CONFORMANCE_PKG=@modelcontextprotocol/conformance@<ver> make conformance`
2. If the check count changed (e.g. new checks beyond 40), read the new
   checks and decide: does fold pass them, or does a gap need closing
   first? A gap that stays open belongs in README "Not implemented".
3. Edit both pin variables in `scripts/conformance.sh` together, update
   the 40/40 count in CLAUDE.md/CI docs if it changed, and run the full
   suite once more on the edited script.
4. The bump gets its own commit with the reason (drift workflow finding,
   new spec feature, etc.) — per-step approval before committing.
