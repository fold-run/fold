---
name: reloadable-state
description: Checklist for adding state or config to fold that must survive hot reload — snapshot placement, Reload semantics, discovery merge, schema lockstep, and the reload/churn test matrix. Use when adding a config field, per-upstream setting, or any state the gateway reads per-request.
---

# Adding reloadable state to fold

fold's reloadable state is **one atomic snapshot** (`routes` in
`gateway/gateway.go`). Every request loads the snapshot once and routes
against it. Getting a new field wrong here breaks hot reload, discovery
merges, or fleet consistency in ways unit tests won't catch — follow the
checklist.

## 1. Decide where the state lives

- **Read per-request, changeable at runtime** → a field on the `routes`
  snapshot, populated at snapshot build. Never a fresh field on `Gateway`.
- **Shared across a gateway fleet** (windows, breakers, caches) → behind
  `state.Provider` (`internal/state`), with both providers implemented:
  in-memory and Redis. Redis operations fail open with the 500 ms bound.
- **Construction-only** (`auth`, `server`, `routing`, `audit`, `tracing`
  sections) → wire at `gateway.New` **and** make sure `Reload` still
  rejects changes to it. If your field is in one of these sections,
  that's the whole point: verify the rejection path has a test.

## 2. Config plumbing (lockstep, enforced by tests)

- Add the field to `config/config.go` with validation in the same place
  neighboring fields validate.
- Mirror it in `config/fold.config.schema.json` — the schema drift test
  fails otherwise.
- It falls under the v1 compatibility contract (README "API stability"):
  new fields must be optional with a safe default.
- Document: `docs/configuration.md` (state the default and unit — `Ms`
  suffix convention), `docs/defaults.md`, and
  `fold.config.example.json` if it's mainstream. A whole new top-level
  section also needs a row in the README's config summary table.
- If the field is construction-wired, say so in its `docs/configuration.md`
  row **and** check README "Configuration hot-reloads", which enumerates the
  non-reloadable sections — the two must agree.

## 3. Reload semantics

- Snapshot build must read the field from the merged document: base
  config + discovery-sourced upstreams. Base reloads and discovery syncs
  each preserve the other's contribution — if your field can come from
  either side, test both directions.
- Validation runs on the **whole merged document before any swap** — a
  bad value must reject the reload, leaving the old snapshot serving.
- Upstream identity: if your field is part of upstream config, changing
  it must retire-and-drain that upstream on reload; leaving it identical
  must **reuse** the upstream (sessions survive). Check the
  config-comparison logic includes your field.

## 4. Test matrix (`gateway/reload_test.go`, `churn_test.go`)

Cover, with real SDK peers:
- [ ] Field takes effect after `Reload` without gateway restart
- [ ] Invalid value rejects the reload; old snapshot still serves
- [ ] Config-identical reload preserves sessions
- [ ] Discovery sync preserves a base-config value (and vice versa) if
      the field can appear in discovery-sourced upstreams
- [ ] Construction-wired: `Reload` rejects the change (if §1 applies)
- [ ] Redis + in-memory parity if behind `state.Provider` (miniredis)
- [ ] Churn: reload under concurrent traffic races clean (`make race`)

## 5. Before done

`make check`; `make bench` if the field is read on the proxy path (read it
from the snapshot, precomputed — no per-request parsing).
