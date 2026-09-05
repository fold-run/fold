# Defaults review (v1.0)

Freezing the v1 contract freezes the defaults: changing one after v1.0 is a
breaking change even though no field name moves. This is the pre-1.0 review
of every resolved default — each is a decision on record, not an accident of
implementation order. Verdict for all: **keep**.

The fields themselves — types, shapes, and what each one does — are in
[configuration.md](configuration.md); this file is only the *why* behind
each resolved value.

## Security posture

| Default | Value | Rationale |
|---|---|---|
| `auth.mode` | `disabled` | Paired with the CLI's loopback bind (below): the out-of-the-box gateway is private to the machine, so the quick start works without an IdP. Exposure requires the deliberate act of passing `--host 0.0.0.0`, and production configs set `mode: required`. Secure-by-default network posture instead of mandatory auth. Breaking the pair — auth off *and* a non-loopback bind — logs a warning at startup, because each half is a supported choice and only the combination is an open gateway. |
| `policy` | absent (allow-all) | An absent `policy` block builds an allow-all engine, and `defaultDecision` is only consulted once a block exists. The quick start has to work with one upstream and no policy, and a gateway that refused every call until a policy was written would be a gateway nobody got far enough to configure. The deny-by-default posture README describes is the engine's once a block is present, which is why the deploy checklist's third line exists. |
| `--host` | `127.0.0.1` | The other half of the pair. Never widen this default. |
| `server.allowedHosts` | localhost set (only when unset) | DNS-rebinding protection that matches the loopback default. An explicit allowlist replaces — never extends — the localhost seed. |
| `server.maxBodyBytes` | 1 MiB | Bounds memory per request. Deliberately conservative; workloads shipping large base64 content raise it knowingly. |
| `upstreams[].maxResponseBytes` | 64 MiB | Added after v1.0, so not part of the original review. The peer of `server.maxBodyBytes` in the outbound direction, and the one path that had no bound at all. Set far above any real MCP payload on purpose: this is a backstop against an upstream spending the gateway's memory (decoded *and* cached, and with Redis pushed into shared fleet state), not a size policy — so it should never fire in a healthy federation. Chosen over shipping it disabled because unbounded memory keyed by something the gateway does not choose is a defect rather than a configured choice; negative restores the previous behaviour. |
| `server.icons.enabled` | `true` | Added after v1.0. On by default because a federation whose icons do not load is fold looking like a server whose upstreams have no icons — the invisibility rule broken quietly. It is close to a no-op regardless: with no `server.publicUrl` and no `auth.resource` there is nothing to mint under, so nothing is rewritten and nothing is fetched. Unlike MCP Apps, which ships with no setting at all, this one has a switch because it makes fold fetch a URL an *upstream* supplied, and an operator is entitled to decline that. |
| `server.icons.maxBytes` | 256 KiB | Far above any real icon and far below anything that costs the gateway memory. Refuses rather than truncates, like `maxResponseBytes`. A value below 1 KiB is rejected at startup: every fetch failing does not read as a typo. |
| `server.icons.timeoutMs` | 5000 | The same bound `timeouts.connectMs` takes for an upstream connect. It is a browser-facing fetch, not the proxy path, so it is not under the latency gate. |
| `server.icons.cacheTtlMs` | 3600000 (1h) | Icon bytes change far less often than a tool list does, and eviction is free — a miss is one re-fetch. Per instance rather than shared: image blobs in Redis would make the shared store a CDN and turn a Redis outage into an icon outage, for no correctness gain. |
| `server.sessionIdleTimeoutMs` | 30 min (added in v1.11) | Ending a session with `DELETE` is optional in the protocol, so without expiry every abandoned client leaves a session — and the upstream subscriptions it pins — held forever. 30 minutes is long enough that a thinking human or a slow agent never notices (the SDK counts any request as activity), short enough that leaks stay bounded. Negative opts out, restoring the pre-1.11 behavior. |
| `auth.ema.tokenTtlSec` | 600 | Short-lived minted tokens; refresh is cheap (the ID-JAG is re-presented). |
| `auth.ema.tokenRateLimitPerMinute` | 600 | Anti-amplification on the unauthenticated token endpoint. |
| `auth.ema.allowedAssertionTypes` | absent (no positive typ check) | Added after v1.0. Absent keeps the v1-compatible behaviour of rejecting only access-token types. Set when the IdP stamps a known assertion typ such as `oauth-id-jag+jwt`. |
| `auth.ema.allowedClientIds` | absent (no client_id check) | Added after v1.0. Absent keeps the v1-compatible behaviour of copying `client_id` into the minted token unvalidated. Set to prevent unrelated IdP tokens for the same audience from being exchanged. |
| `server.introspection.enabled` | `false` (added in v1.9) | No new surface unless asked for. `/api/federation` names upstream URLs, owners, and labels; serving it is a deliberate act, not a default. |
| `server.introspection.groups` | unset (added in v1.9; was `server.console.groups` in v1.3–v1.8) | Unset means any valid principal may read `/api/federation` — the documented baseline. The allowlist is opt-in because it only means something once an operator decides which IdP groups map to "platform team". It moved out of `console` in v1.9: the allowlist gates the API, and the API is no longer the console's alone. |
| `server.console.enabled` | `false` (added in v1.2) | No new surface unless asked for: the console's static assets serve unauthenticated by design, so exposing them is an operator's deliberate choice, not a default. Requires `server.introspection.enabled` — the page has no other data source. |
| `server.console.oauth` | unset (added in v1.4) | Sign-in is opt-in because it requires an IdP-side client registration (client id + redirect URI) that fold cannot conjure. Unset, the console falls back to paste-token. `oauth.issuer` defaults to the first direct-mode issuer — the common single-IdP case needs no choice. |
| `policy.serverInitiatedDecision` | `allow` (added in v1.12) | The one default in this table that is not the safest available option, and it is deliberate. Sampling and elicitation flowed ungoverned in every deployment before the check existed, including deny-by-default ones, so folding them under `defaultDecision` would have refused traffic that worked the day before — a changed meaning, which the contract forbids. A separate field with the old behavior is what makes the tightening an operator's act rather than an upgrade's. The deploy checklist says to set `deny`. |
| `upstream.pinDefinitions` | `off` (added in v1.12) | Detection of a rewritten tool definition is new behavior and new audit volume, so it opts in. Recommended for upstreams an operator does not run themselves, where "the tools are still what you approved" is otherwise an assumption. |
| `policyAllow.toolKind` | absent (no annotation gate, added in v1.12) | Absent means names decide, which is the stronger boundary anyway: the gate reads annotations supplied by the server being gated. A default gate would imply a guarantee fold cannot make. |
| `policyRule.deny` / `policyAllow.args` | absent (added in v1.12) | Both are narrowings, so absence must mean "no narrowing". Their absence is also what keeps evaluation on the pre-v1.12 path — the engine checks at construction whether any rule uses them. |
| `issuer.jwksUri` | `{issuer}/.well-known/jwks.json` | Common convention; IdPs that differ (e.g. Okta org servers) set it explicitly. A guess, but a configurable one. |
| `issuer.groupsClaim` | `groups` | Okta's name; Entra/Auth0 set their own. |

## Protocol and federation

| Default | Value | Rationale |
|---|---|---|
| `upstream.protocol` | `session` | The deliberate divergence from the SDK's own preference: only sessionful connections carry server-initiated traffic (sampling, elicitation, logging, progress) back through the gateway. `auto` is the opt-out, not the default. |
| `routing.namespaceSeparator` | `__` | Survives every namespace character; validation rejects ambiguous separators. |
| `routing.pageSize` | 200 | Large enough that most federations are one page; small enough to bound response size. Negative opts out. |
| `upstream.cacheTtlMs` | 30000 | A backstop only — `list_changed` notifications invalidate immediately; the TTL covers upstreams that never send them. |

## Resilience

| Default | Value | Rationale |
|---|---|---|
| `timeouts` | connect 5 s, request 60 s, streamIdle off | Request 60 s accommodates slow tools; connect 5 s fails over quickly (multi-endpoint upstreams try the next replica within the same attempt, bounded at three candidates per attempt). streamIdle was documented as "120 s" before v1.11 but implemented nowhere; v1.11 implements it as **opt-in**, which preserves every existing deployment's actual behavior. It is not defaulted on because the SDK client's reconnect budget only replenishes when events arrive — a default idle cut against an upstream that legitimately never notifies would cycle and eventually kill its session. |
| `circuitBreaker` | 5 failures / 30 s half-open | Conventional values; also the endpoint pool's cooldown, by design (one "retry the unhealthy thing after" knob). |
| `rateLimit` (global, per-upstream) | none | The gateway must never throttle by surprise; limits are an operator's policy, opted into. |
| `budget` (server, per-upstream) | absent (no budget) | Added in v1.7. Same reasoning as `rateLimit`, and stronger: a default allowance is a default outage waiting for a busy month, and there is no number that is right for every federation. Opt in. |
| `budget.period` | `month` | The period an allowance is usually negotiated over. Boundaries are UTC so a fleet spanning zones agrees on which month it is — a local-time month would also move under a DST transition. |
| `tenants` | absent (no tenants) | Added in v1.8. Tenancy is additive by construction: with none declared, every principal resolves to no tenant and is governed exactly as before the feature existed. There is no "default tenant" — one would silently capture callers an operator never placed, which is the same footgun a tenant without a selector would be (validation rejects that too). |
| `tenant.budget` / `tenant.rateLimit` | absent (unlimited) | Same reasoning as every other limit: fold must never throttle by surprise. A tenant declared only to appear in the audit record and the metrics is a legitimate and common use — naming a customer is useful before capping them. |
| `tenant.upstreams` | absent (every upstream) | The subset is a narrowing, so its absence has to mean "no narrowing". Defaulting to none would make declaring a tenant an outage. |
| `healthCheck` | absent (passive) | Active probing costs a connect per endpoint per interval; passive ejection + cooldown is free and correct. Opt in. |
| `discovery.intervalMs` | 30000 | Registry churn is minutes-scale; 30 s balances freshness against load on the source. |
| `discovery.allowedAuthStrategies` / `allowedSecretRefs` | absent (unrestricted) | Compatibility with pre-hardening discovery deployments; restricting by default post-v1.0 would break them. The producer (`fold-discovery`) is the inverse — default-deny — because it shipped with the hardening. Set the gateway allowlists whenever the discovery source is not operated by the gateway's operators. |
| `audit.sinks[].retry` | on (4 attempts, 500 ms → 30 s) | Added in v1.9, and deliberately not opt-in: audit is the single exit door, so a receiver's thirty-second restart quietly costing every event in that window was a defect rather than a default. The values are conventional; what matters is that redelivery happens without anyone having configured it. |
| `audit.sinks[].deadLetterPath` | unset (exhausted events are counted, not kept) | Opt-in because it is a file fold will write to indefinitely, and where that lives is an operator's decision. Unset, exhausted events increment `fold_audit_events_total{outcome="dropped"}`; set, they are appended for replay. |
| `audit.scrub` | absent (no redaction) | Added after v1.0. Absent preserves the verbatim event contract. Opt in when upstream-provided `usage` keys or `error` text need to be withheld from sinks. |
| `audit.requireDurable` | `off` | Whether a trail is durable depends on where it lands, and for `stdout` that is a collector fold cannot see — so a default of "on" would refuse to start perfectly sound deployments, and a default that counted stdout would vouch for something fold has no way to check. Opting in is the operator asserting a property of their own deployment; fold then proves it at startup, when the fix is cheap, instead of at audit time. |
| `audit` file sink | `maxSizeMb` 100, `maxFiles` 5 | Bounded by default in both directions. An audit sink that fills the disk takes the gateway down, which is a worse outcome than the delivery problem it was recording. |
| `server.metricsAddr` | unset (metrics on the main port) | Added in v1.9. Absent keeps every endpoint on one listener, which is the simplest thing to reason about and correct for the loopback default: `/metrics` is then covered by DNS-rebinding protection like everything else. It is opt-in rather than on, because a second listening socket is a deployment decision — where to bind it and what may reach it — that fold cannot make for an operator. Turn it on whenever something other than the gateway's own host scrapes. |
| `server.metricsAllowedHosts` | absent (no host validation on the metrics listener) | Added after v1.0. The metrics listener is meant to be network-scoped; the allowlist is opt-in for deployments where the listener must sit on a broader interface. |
| `server.redisUrl` | unset (in-process state) | Single instances need no infrastructure; fleets opt in. Redis outages fail open, bounded 500 ms per operation. |

## Observability

| Default | Value | Rationale |
|---|---|---|
| `tracing` | absent (propagation-only) | First-party spans are opt-in; W3C trace propagation is always on and free. |
| `tracing.sampleRatio` | 1.0 | An operator who configures tracing wants the traces; parent-based, so callers' sampling decisions are honored either way. |
| `tracing.recordPrincipal` | `false` (added in v1.11) | Before v1.11 the principal's subject was always stamped on server spans as `enduser.id`. Now opt-in — a deliberate default change, made for privacy: the subject is personal data, trace backends commonly have broader access than the audit trail, and the audit event carries the same identity under audit-grade access. Dashboards keyed on span `enduser.id` set this to `true` knowingly. |
| `tracing.serviceName` | `fold` | — |
| `--log-level` / `--log-format` | `info` / `text` | Human-first on a terminal; `json` for collectors. |

Non-configurable behaviors reviewed alongside (bridged-session idle sweep at
5 minutes, SSE-header hang timeout 3 s, discovery document cap 4 MiB, JWKS
fetch bounds) are implementation details, not contract — they may be tuned
or made configurable in any release.
