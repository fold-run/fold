# Configuration

fold is configured with one JSON document, validated on startup
(`fold --validate`) and loaded from `--config <path>` or `FOLD_CONFIG`,
which accepts either a file path or the JSON itself inline.

A JSON Schema for the document ships with fold —
[`config/fold.config.schema.json`](../config/fold.config.schema.json),
printed by `fold --schema`, included in release archives — for editor
completion and CI linting. The schema is the structural contract (fields,
types, enums, required properties) and is kept in lockstep with the code by
test; cross-field rules (namespace requirements, https mandates) remain
`fold --validate`'s job.

Every default below is frozen by the v1 compatibility contract
([API stability](../README.md#api-stability)) and reviewed as a deliberate
decision in [defaults.md](defaults.md). A full working document is
[`fold.config.example.json`](../fold.config.example.json).

## `upstreams`

**Required.** The federation itself — one entry per upstream MCP server.

| Field | Default | Notes |
|---|---|---|
| `id` | — | Lowercase alphanumeric + hyphens. Used in policy, audit, health. |
| `url` | — | The upstream's MCP endpoint. Exactly one of `url` / `urls`. |
| `urls` | — | Multiple equivalent replicas of this upstream. New sessions balance round-robin across them with connect failover; a failed endpoint rests for the breaker's `halfOpenAfterMs`. |
| `namespace` | none | Tool/prompt name prefix (`{namespace}__{name}`). Omitted → passthrough; only valid with a single upstream. |
| `protocol` | `session` | `"session"` negotiates the sessionful handshake, required to bridge server-initiated traffic (sampling, elicitation, logging, progress, resource updates). `"auto"`/`"2026-07-28"` lets the SDK prefer the stateless 2026 protocol, which cannot carry server-initiated requests. |
| `owner` | none | `{ org, team, contact }` — surfaces in audit and health. |
| `labels` | none | Free-form string map for reporting. |
| `auth` | `{"strategy":"none"}` | Upstream credential strategy — see below. |
| `timeouts` | `5s/60s/off` | `connectMs`, `requestMs`, `streamIdleMs`. The last, when set, bounds silence on the upstream's standalone SSE stream: a server that accepts the stream and then wedges — TCP alive, no bytes — is cut and reconnected instead of holding a dead notification channel open forever. Off by default, and set it only for upstreams that emit notifications or SSE keepalives: reconnects only count as progress when events arrive, so an idle bound against a legitimately quiet upstream would eventually exhaust the client's retry budget and kill the session. In-flight calls are governed by `requestMs`, not this. |
| `circuitBreaker` | `5 / 30s` | `failureThreshold` consecutive failures open the circuit; a probe is admitted after `halfOpenAfterMs`. |
| `rateLimit` | none | `{ requestsPerMinute }` for this upstream only. |
| `budget` | none | `{ period, upstreamCalls }` — a consumption allowance that **accumulates** until the calendar period rolls over, unlike `rateLimit`, which smooths a burst and forgets it. `period` is `hour`, `day`, or `month` (default), aligned to UTC. Requires shared state to mean anything across a fleet; without it each instance enforces its own allowance and the gateway warns at startup. |
| `healthCheck` | none | `{ intervalMs }` — actively probe every endpoint (full MCP connect) on this interval, ejecting dead replicas before client traffic hits them and restoring recovered ones immediately. Absent → passive health (connect failures eject, cooldown restores). |
| `cacheTtlMs` | `30000` | TTL for cached list results. Negative disables caching. |
| `maxResponseBytes` | `67108864` (64 MiB) | Bounds a single response from this upstream. An upstream that exceeds it is reported unavailable (`-31041`) rather than truncated — a shortened body would be a response the upstream never sent. On an event stream the bound applies per event, not per connection. Negative disables it. See below. |
| `pinDefinitions` | `off` | `"warn"` records the digest of every tool and prompt definition this upstream advertises and reports a change — a description, schema, or annotation rewritten after the federation was approved. See below. |

**Definition pinning.** A tool's description and input schema are the instruction set a model acts on, and its annotations are what policy decides with; fold re-reads them from the upstream on every cache refill. With `pinDefinitions: "warn"`, it also remembers them: the digest of each definition goes to shared state, a difference emits the `upstream/definitionChanged` audit event and increments `fold_definition_drift_total{upstream,kind}`, and the new definition becomes the baseline — so a change is one alert, not one per refill. Nothing is withheld; this reports.

It is a comparison, not an inspection. fold never asks whether a description is malicious, only whether it is the same one it served before, which is why this is not the inline scanning the roadmap [declines](roadmap.md#non-goals). Two honest limits: trust-on-first-use cannot vouch for a definition fold has never seen, and a legitimate improvement to a description trips it exactly like a rewrite would — the event says something changed, not that something is wrong. The cost lands on the list-refill path, never on the proxy path. Reasoning, and why blocking is deliberately unbuilt: [design-definition-pinning.md](design-definition-pinning.md).

List freshness works end to end: when an upstream emits a `list_changed` notification, the gateway invalidates its cache **and re-emits the notification to every connected client**, so clients refetch and see the change immediately. TTLs remain the backstop when no notification arrives.

**Stdio servers.** `url` is always an HTTP endpoint — the gateway never runs a process. To federate an MCP server that speaks stdio (which is most of them), put [`fold-stdio`](stdio.md) in front of it: it runs the server and exposes it over streamable HTTP, so the upstream entry is an ordinary `url` and every strategy, guard, and policy rule above applies unchanged. The command is fixed at the shim's argv and never travels over the network — which is why stdio is not a field here. See [design-stdio.md](design-stdio.md) for why the process supervision lives in a sidecar rather than in the gateway.

## Upstream auth strategies

| Strategy | Fields | When |
|---|---|---|
| `none` | — | Trusted network, no upstream auth. |
| `static` | `secretRef`, `header?`, `scheme?` | API-key upstreams. `secretRef` names an environment variable. |
| `passthrough` | — | Forwards the client's Bearer token as-is. Upstreams doing strict RFC 8707 audience checks will reject it — prefer token-exchange. |
| `client-credentials` | `tokenEndpoint`, `clientId`, `clientAuth`, `scopes?`, `resource?` | Service identity per upstream. Tokens cached until 60s before expiry. |
| `token-exchange` | `tokenEndpoint`, `clientId`, `clientAuth`, `audience`, `scopes?` | RFC 8693 — exchanges the caller's token for an upstream-audience token, preserving user identity end-to-end. **Recommended enterprise default.** Cached per (upstream, subject). |

`passthrough` and `token-exchange` derive per-principal credentials, so they require `auth.mode: "required"` — without a verified caller identity there is no subject to exchange for, and passthrough would forward whatever header an anonymous caller supplied.

`clientAuth`: `{ "type": "client_secret_post" | "client_secret_basic", "secretRef": "..." }`. Token endpoints must use `https` (loopback exempt). Upstream credentials are attached per request and bound to the configured upstream host: the gateway refuses cross-host redirects and never re-attaches a credential to another host, so a hostile upstream cannot capture the API key (or a passthrough caller's token) with a 3xx. Exchanged tokens are cached per `(upstream, issuer, subject)`. List results are not cached for `passthrough`/`token-exchange` upstreams, since those may be per-user, and such upstreams hold **one MCP session per principal** rather than the single session an upstream with a gateway-configured credential shares: the session is established by whoever opens it and keeps that identity for its whole life, so sharing it would present one caller to the upstream while carrying another's token. Those per-caller sessions age out on the same idle sweep as bridged sessions, and `healthCheck` is ignored for them — a probe holds no caller credential, so it could only ever fail.

### What fold tells clients about caching

Separate from `cacheTtlMs`, which governs what *fold* caches from an upstream:
every list fold serves carries the MCP caching hints (`ttlMs`, `cacheScope`)
the [specification](https://modelcontextprotocol.io/specification/2026-07-28/server/utilities/caching)
requires on a complete list result.

`cacheScope` is computed from the configuration, once per snapshot. It is
`"private"` whenever two callers can legitimately receive different lists —
any `policy` rule, any `tenants` entry, or any upstream whose credential is
caller-derived — and `"public"` only for a federation that genuinely serves
one list to everyone.

`defaultDecision` deliberately does not affect it: a default with no rules
serves *every* caller the same list — the whole federation under `allow`,
nothing at all under `deny` — so neither varies by who is asking. The spec names exactly
this case: *"private" is appropriate for [...] filtered list results that vary
per user*, and fold's per-principal filtering is the whole point of the
enforcement pair.

**One thing the scope cannot express.** A federated list can also vary by what
the *client* declared it can render: an upstream may register a different tool
set for a client that supports [MCP Apps](https://modelcontextprotocol.io/extensions/apps/overview),
which is why fold keys root sessions and list-cache entries by capability
profile. Two callers in the *same* authorization context can therefore receive
different `tools/list` bytes. That is a content-negotiation axis — an HTTP
cache would express it with `Vary` — and the MCP caching section defines no
equivalent, so `cacheScope` cannot say it: `"private"` would be the wrong
instrument, since it bounds sharing across authorization contexts rather than
across client capabilities. What contains it in practice is `ttlMs: 0`, which
tells every conforming cache not to retain the list at all. A cache that
ignored the TTL and honoured the scope could serve an app-aware list to a plain
client; nothing fold can put in these two fields would prevent that.

`ttlMs` is `0`, the spec's "immediately stale". fold does not advertise a
lifetime for a list, because a reload, a discovery sync, or a policy change can
alter what a caller is entitled to see at any moment. fold emits `list_changed`
on all three — which is the invalidation signal the spec pairs with a TTL — but
picking a number here would pick it for every deployment at once, and the
conservative value is the one that cannot serve a stale entitlement.

### Bounding what an upstream may return

`maxResponseBytes` is the outbound peer of `server.maxBodyBytes`. The inbound
direction has been bounded since v1, and everything else the gateway fetches —
tokens, JWKS, the discovery document, the decision hook — carries its own
limit; the upstream data path was the one place with none. Without it, an
upstream that returns a runaway `tools/list` spends the gateway's memory
twice: once decoding it, and again storing it in the list cache, which with
`server.redisUrl` set pushes it into shared fleet state. It is the same rule
`internal/bounded` applies to keys, applied to bytes.

The bound **refuses rather than truncates**. A shortened body would be a
response the upstream never sent, which is the response rewriting fold
declines everywhere else, so an upstream that exceeds it is reported
unavailable (`-31041`) and nothing partial is cached or served. The refusal is
counted by `fold_upstream_response_capped_total{upstream}` and has its own
alert in the shipped rules.

On a `text/event-stream` response the bound applies to **each event**, not to
the connection. A subscription that stays open for a day is legitimately
unbounded in total, so a connection-wide cap would cut healthy traffic at an
arbitrary hour; what the bound is actually protecting against is a single
oversized payload.

The default is 64 MiB — far above any real MCP payload, because this is a
backstop against an upstream spending the gateway's memory rather than a size
policy. A federation that legitimately serves more raises it; one that wants
the pre-v1.15 unbounded behaviour sets it negative.

### Icons survive federation

MCP lets a server hang an icon off a tool, prompt, or resource, and it tells
the *client* how to handle one: fetch only `https` or `data:`, reject unsafe
schemes and cross-origin redirects, fetch without credentials, sniff the bytes
rather than trust the declared type — and **verify that icon URIs are from the
same origin as the server**.

That last rule is what makes an icon a federation problem rather than a field.
An upstream's icon points at the upstream's origin, which behind a gateway is
neither the origin the client connected to nor, for the cluster-internal
upstream that is the ordinary enterprise case, an origin the client can reach
at all. So a conforming client rejects every federated icon and a lenient one
fails to load it, and fold looks like a server whose upstreams have no icons —
the invisibility rule broken in the direction nobody reports.

fold mints its own URL for each one, `{publicUrl}/icons/{namespace}/{digest}`,
and serves the bytes. It is the same move [MCP Apps](#) `ui://` minting makes
and it is safe for the same reason: an icon `src` is not an identifier a client
persists, it is republished on every list.

Four bounds, and they are why this is not an open proxy:

- **Only an icon on one of that upstream's own configured endpoint hosts is
  minted.** That is the same host bound credential attachment uses — nothing
  rides to a host the upstream did not configure — checked when the URL is
  minted and again immediately before the fetch, so a hand-built digest cannot
  smuggle a request somewhere else. An icon on any other host is republished
  exactly as sent: it is already a public URL, and withholding it would only
  take an icon away from a client lenient enough to have rendered it.
- **Unsafe schemes are dropped** — `javascript:`, `file:`, `ftp:`, `ws:`. A
  client MUST reject these anyway, so dropping costs a conforming client
  nothing; what it avoids is fold putting its own name on one. This is the only
  place the feature removes data.
- **`data:` URIs pass through untouched.** Already same-origin-safe, already
  reachable; proxying would re-serve bytes fold is holding. `maxResponseBytes`
  is the bound on an oversized one, as it is for everything else an upstream
  sends.
- **Passthrough mints nothing**, exactly like `ui://`. A single un-namespaced
  upstream is not a federation.

The fetch carries no credentials — not by policy but by construction, since the
client used for it has a plain transport with no path to upstream credentials
at all — refuses redirects outright, is bounded by `maxBytes` and refuses
rather than truncates, and identifies the image from its magic bytes rather
than the declared type. The allowlist is `image/png`, `image/jpeg`,
`image/webp`, and `image/gif`.

**`image/svg+xml` is refused**, and the refusal is worth stating rather than
discovering. Two arguments, either sufficient. fold would be serving the SVG
from *fold's own origin*, where an SVG's embedded script runs same-origin with
`/console/`, `/api/federation`, and `/oauth/token` — strictly worse than the
upstream serving it, because the upstream's origin holds nothing of fold's.
And structurally: SVG is XML and has no magic bytes, so the strict magic-byte
allowlist the specification asks a consumer to maintain cannot admit it at all;
doing so would mean trusting the declared content type or parsing the XML, and
the second is the inline content inspection the roadmap
[declines](roadmap.md#non-goals). An upstream whose only icon is an SVG renders
as no icon.

The endpoint is **unauthenticated**, and that is forced rather than chosen: the
specification has clients fetch icons without credentials, and a browser
`<img>` carries no bearer token, so an authenticated endpoint would serve
nothing to the clients it exists for. What that discloses is bounded to match —
the path names no tool, prompt, or resource, only a namespace and a digest, and
the bytes are branding artwork identical for every caller, the same class as
the console's static assets. Note what it is *not*: policy filtering is
preserved here by unguessability rather than by authentication. A principal who
cannot see a tool is never handed its icon URL, but one who guesses a URL gets
the bytes — which is why the endpoint serves images and nothing else. An
operator who does not accept that sets `icons.enabled: false`. The reasoning in
full is in [security-model.md](security-model.md).

`identity.icons` are fold's own and are **not** proxied: an operator sets a URL
they own. A client enforcing the same-origin rule will render only a `data:`
icon or one hosted at the gateway's own origin, so **prefer `data:`** — it
needs no fetch, no cache, and no machinery.

### Keeping a stream alive

MCP's own expectation is that a *client* pings if it wants a session kept
alive. That is reasonable advice to a server and awkward advice to a gateway:
the idle timeout that cuts the stream usually belongs to a load balancer the
operator configured, and the gateway is frequently the only thing in that path
they control. A cut stream is survivable — both ends reconnect, with backoff
and `Last-Event-ID` — but it is reconnect churn plus a window in which
notifications are not being delivered.

`keepAliveMs` closes that by sending the pings from fold's side. Three things
to know before turning it on; all of them were measured rather than assumed,
and two of them will change a working deployment.

**It disconnects clients that decline the standalone stream.** A server-to-client
ping needs somewhere to land, and that somewhere is the standalone `GET` SSE
stream — which the transport makes a MAY, not a MUST. A client that speaks only
`POST` is healthy, fully functional, and completely correct; with `keepAliveMs`
set it is also unreachable by a ping, so every tick fails immediately and its
session is closed once the threshold is reached. Measured: a client that
declines the stream is disconnected after three ticks, and its next call is
answered `session not found`. If any of your clients might decline it, this
setting is not for you.

**The interval must clear twice your slowest round trip, not once.** Each ping
is given half the interval to be answered. A legitimate client slower than that
burns a failure on every tick and is closed on the third, so the number to
reason from is `2 × the slowest legitimate round trip`, floored well above it —
not merely something smaller than the balancer's timeout.

**It supersedes `sessionIdleTimeoutMs` for any client that answers.** Under
streamable HTTP a client answers a ping with a `POST`, and a `POST` is exactly
what resets the idle timer — so fold's own keepalive keeps the session it is
pinging from ever idling out. Measured: with a 250 ms idle timeout and a 50 ms
keepalive, a client that sent nothing after connecting was still alive and
being pinged after 1.8 seconds. What changes is the criterion rather than the
setting: reclamation moves from *activity* to *liveness*. A client that has
genuinely gone away is now reclaimed **faster** — three missed pings instead of
the whole idle timeout — while one that is present but idle is not reclaimed at
all. If `sessionIdleTimeoutMs` was bounding session-state growth against
long-lived idle clients, it stops doing that.

fold tolerates three consecutive misses before closing, rather than the one the
SDK defaults to, because a gateway's clients sit behind whatever network an
operator has and the specification's guidance is that *multiple* failed pings
may trigger a reset.

Note also that `ping` is a method the `2026-07-28` revision removed. This is
therefore another thing that is correct for the era fold serves and has no
counterpart in the next one; see README "Not implemented".

## `auth`

Gateway authentication — who may call fold at all, and on whose token.

```jsonc
{
  "mode": "required",                     // "disabled" (default) | "required"
  "resource": "https://gw.example.com",   // canonical resource URI = required token audience (RFC 8707)
  "issuers": [
    {
      "issuer": "https://acme.okta.com",
      "jwksUri": "https://acme.okta.com/oauth2/v1/keys",  // default: {issuer}/.well-known/jwks.json
      "groupsClaim": "groups"             // Okta "groups", Entra "roles", Auth0 custom-namespaced
    }
  ]
}
```

The groups claim must be a JSON **array** of strings; any other shape (a
single string, a comma-joined value) reads as no groups — fail closed, so a
misconfigured claim denies rather than grants. Configure the IdP to emit an
array.

**What a client discovers before it can connect.** An MCP client that meets a
`401` follows the `WWW-Authenticate` challenge to
`/.well-known/oauth-protected-resource`, reads `authorization_servers`, and
goes to that issuer to obtain a token. fold publishes `scopes_supported` in
that document — every scope named in `policy.rules[].subjects.scopes` — so a
client can request the right scopes in its authorization request instead of
discovering them from a `-31042` denial after it has already connected. It is
a hint rather than a contract: holding every scope listed entitles a caller to
nothing on its own, because scopes are one gate among several and a rule can
also require an identity, an issuer, or a claim. The list tracks policy
reloads.

Scopes used in [`tenants`](#tenants) selectors are deliberately **not**
published. This endpoint is unauthenticated and a tenant scope is usually a
customer's name, so advertising them would put a customer roster behind a
well-known URL — a different class of secret from a capability name like
`docs:write`, and one fold does not otherwise disclose. A tenant scope is also
an identity assertion rather than a permission, so an authorization server
asked for one will not mint it: naming it would leak something real in return
for advice a client cannot act on.

**The one thing fold cannot supply for you.** The authorization server in that
chain is your IdP, not fold, and mainstream MCP clients register themselves
dynamically ([RFC 7591](https://www.rfc-editor.org/rfc/rfc7591)). An IdP that
supports dynamic client registration — Auth0, WorkOS, Descope, Keycloak and
others do — completes the chain with no further work. An IdP that does not
(Okta and Entra typically require an administrator to pre-register each
application) leaves the client unable to obtain credentials on its own, and
the deployment needs either pre-registered client ids distributed out of band
or an authorization server in front that does support registration. fold is a
resource server: it validates tokens and tells clients where to get them. See
[roadmap.md](roadmap.md) for where a DCR-capable front sits in fold's plans.

With `mode: "required"`, every `/mcp` request needs a valid Bearer token: trusted issuer (checked before any network I/O), verified signature via cached JWKS, exact audience match, a non-empty `sub`, asymmetric algorithms only (RS/ES/EdDSA). Failures answer 401 with a `WWW-Authenticate` challenge pointing at `/.well-known/oauth-protected-resource` (RFC 9728), which the gateway publishes. Issuer and JWKS URLs must use `https` (loopback exempt) — they are the inbound trust anchor. The JWKS fetch is single-flighted, size-bounded, and timeout-bounded so an unauthenticated flood of unknown-`kid` tokens cannot be amplified into requests against the IdP. A cached key set is refreshed every 5 minutes even when the `kid` is known — that is what lets a key the IdP has revoked stop verifying tokens — and a refresh that fails keeps serving the last good set, retried at most every 30 seconds and counted in `fold_jwks_fetches_total{outcome="stale"}`, so an IdP outage degrades to stale keys rather than to refusing every caller.

### `auth.ema`

Enterprise-Managed Authorization. fold can embed a deliberately
one-grant-wide MCP Authorization Server: `POST /oauth/token` exchanges an enterprise-IdP-issued **ID-JAG** (Identity Assertion JWT Authorization Grant, RFC 7523 `jwt-bearer`) for a short-lived fold-signed access token. Everything the gateway then accepts has `aud` = fold, which keeps upstream token exchange coherent.

```jsonc
{
  "mode": "required",
  "resource": "https://gw.example.com",
  "issuers": [
    { "issuer": "https://acme.okta.com", "mode": "exchange" }  // ID-JAGs only — never accepted directly
  ],
  "ema": {
    "idpIssuer": "https://acme.okta.com",
    "idpJwksUri": "https://acme.okta.com/oauth2/v1/keys",  // default: {idpIssuer}/.well-known/jwks.json
    "signingKeyRef": "FOLD_EMA_KEY",   // env var: ES256 private key, PKCS#8 PEM
    "tokenTtlSec": 600,                // minted-token lifetime (default 600)
    "tokenRateLimitPerMinute": 600,    // cap on the unauthenticated /oauth/token endpoint (default 600)
    "allowedAssertionTypes": ["oauth-id-jag+jwt"],  // optional positive typ allowlist
    "allowedClientIds": ["fold-client"]             // optional client_id allowlist
  }
}
```

An assertion must be issued by `idpIssuer` for the `resource` audience and carry `exp` and `jti`; each `jti` is single-use until it expires (recorded fleet-wide via Redis when configured), so a captured ID-JAG cannot be redeemed twice. fold publishes RFC 8414 authorization-server metadata at `/.well-known/oauth-authorization-server` whenever EMA is enabled, so a client that follows the `authorization_servers` pointer in the protected-resource document can discover the token endpoint and key set rather than being configured with them out of band. The document is narrow on purpose: it describes the one grant this server implements and claims nothing else. Issuers with `mode: "exchange"` are excluded from direct token presentation and from the advertised `authorization_servers` — fold itself is the authorization server for those, publishing its minting key at `/.well-known/jwks.json` and announcing the `io.modelcontextprotocol/enterprise-managed-authorization` extension in the protected-resource metadata. The token endpoint is unauthenticated by design (the assertion is the credential) and rate-limited against amplification. Generate a key with `openssl ecparam -genkey -name prime256v1 | openssl pkcs8 -topk8 -nocrypt`.

**Hardening the ID-JAG exchange.** By default fold rejects only access-token typ values (`at+jwt`) and otherwise accepts any IdP-signed JWT that matches issuer, audience, subject, `jti`, and `exp`. That leaves a residual trust-model gap: an ID token or an application token minted for the gateway's resource audience satisfies every check. Two optional allowlists close it:

- `allowedAssertionTypes` is a positive list of JOSE `typ` values the assertion must carry. SEP-990 §5.1 requires `oauth-id-jag+jwt`; set it when your IdP stamps that value.
- `allowedClientIds` restricts which `client_id` claims fold will mint a token for. The claim is copied into the minted access token but otherwise unvalidated; an allowlist prevents an unrelated IdP-issued token for the same audience from being exchanged.

Either list may be used alone; both empty preserves the v1-compatible behaviour — and logs a warning at startup, because a token endpoint that will exchange any correctly addressed IdP-signed JWT is a posture worth opting into rather than inheriting. Set `allowedAssertionTypes` in production.

## `policy`

```jsonc
{
  "defaultDecision": "deny",
  "serverInitiatedDecision": "deny",             // see "the reverse direction" below
  "rules": [
    {
      "id": "eng-github",
      "subjects": { "groups": ["engineering"] }, // and/or "subs"; omit → any principal
      "allow": [
        { "server": "github", "methods": ["tools/call"], "names": ["get_*", "create_pr"] },
        { "server": "search" },                  // all methods/names on that upstream
        { "server": "deploy", "names": ["deploy"], "args": { "target.environment": "staging" } },
        { "server": "*", "methods": ["tools/call"], "toolKind": "readOnly" }
      ],
      "deny": [ { "server": "billing", "names": ["refund_*"] } ],
      "maxItems": 40
    }
  ]
}
```

**How a decision is reached.** With no `policy` block at all, the engine is **allow-all** — every principal may invoke every tool. Once a block is present, any matching `deny` refuses, whatever the rule order; otherwise the first matching `allow` grants; otherwise `defaultDecision`. Policy governs named invocations (`tools/call`, `prompts/get`, `resources/read`), the completions and subscriptions derived from them (`completion/complete` is gated behind the prompt/resource it completes; `resources/subscribe` behind the resource), and it **filters list results per principal** — callers never see tools, prompts, or resources they cannot reach. Protocol plumbing (ping, the lists themselves) is not policy-gated; invisibility plus call-denial is the enforcement pair.

A rule may carry `allow`, `deny`, or both, and at least one of them.

### Deny wins, globally

An explicit deny is not overridable by an allow, as in IAM and in firewalls, and **it does not matter where the rule sits in the document**. With first-match precedence, appending a broad allow at the bottom would silently widen access, and a rule whose correctness depends on where it was pasted will eventually be pasted in the wrong place. A deny names its rule in the audit event's `ruleId`, which the default decision cannot.

Deny is not a second enforcement mechanism: it participates in the same decision, so a carved-out name is invisible in lists *and* uncallable, together. Documents that use no deny evaluate on the first-match short-circuit they always have — the engine settles that at construction.

### Constraining arguments

`"args"` is a map of dotted JSON path to required value, all of which must match — the difference between "may call `deploy`" and "may call `deploy` against staging". Values compare **type-exactly**, like subject claims: `"1"` and `1` are different, because a lenient comparison silently widens a grant. Paths have no wildcards and no array indexing; a matcher that starts narrow can widen later, whereas one that starts expressive cannot be narrowed.

**A constrained tool is visible but conditionally callable.** There are no arguments at list time, so this cannot filter a list: the tool appears, and a call with non-matching arguments is denied. That is a genuine weakening of the invisibility pair — its guarantee becomes *a caller never sees a tool they cannot call at all* rather than *never sees anything they might be refused* — and an operator who needs the stronger property grants by name. The mirror case is a `deny` carrying `args`, which for the same reason does **not** hide the tool: "no deploys to production" must not remove `deploy` entirely.

The denial names the constraint path that failed and never the value the rule wanted, because that value is the operator's configuration and a refusal is a poor place to disclose a policy document one field at a time.

fold forwards `arguments` as raw JSON and never parses them, so this is conditional: a document without `args` parses nothing and evaluates on the path it always has.

### Gating on what a tool says it does

`"toolKind"` matches the MCP annotations an upstream publishes: `readOnly` requires `readOnlyHint`, `nonDestructive` additionally admits tools whose `destructiveHint` is false. That is how one rule says "read anything, write nothing" without naming a tool. Unlike `args`, annotations arrive *with* the list, so this filters lists as well as denying calls.

**It is a hygiene control, not a security boundary — and the difference is not a nuance.** The annotations are declared by the very server being gated, so an upstream that labels `delete_everything` as `readOnlyHint: true` is believed. Use it for federations your organization operates, where the risk is an operator forgetting to update an allowlist. Against an upstream you do not control — a vendor's server, anything arriving through discovery — **the boundary is `names`**, and no amount of `toolKind` substitutes for it.

Two behaviours follow from taking the MCP spec at its word. `readOnlyHint` defaults to false and `destructiveHint` to true, so **an unannotated tool is neither read-only nor non-destructive** and fails both gates; fold does not "improve" those defaults, because the improvement would be admitting every unannotated tool. And **unknown annotations deny**: if fold cannot establish a tool's annotations from the list it holds, the call is refused. That includes upstreams whose list caching is disabled because their credential is caller-derived, so `toolKind` and `passthrough`/`token-exchange` do not compose — an operator wanting both grants by name.

### Bounding a list

`"maxItems": 40` bounds how many list items one rule may make visible in a response — a guardrail against handing an agent a thousand tools, which is context paid for on every turn. It is a bound, not a curation: fold drops whatever falls past the cap in merge order, because it has no notion of which tools matter, and acquiring one would be the semantic tool selection this project [declines](roadmap.md#non-goals). A truncated list says so in the result's `_meta["run.fold/truncated"]`, in the audit event's `itemsCapped`, and in `fold_list_items_total{stage="capped"}` — a cap that hid capability silently would be worse than no cap. **The cap bounds visibility, not authority**: a name it withheld from a list is still callable if policy allows it.

### The reverse direction

`"serverInitiatedDecision": "deny"` extends policy to the `sampling/createMessage` and `elicitation/create` requests an upstream makes of the caller's client over a bridged session. Those spend something of the caller's — model budget, or a human's attention — so a rule grants them explicitly: `{ "server": "corpus", "methods": ["sampling/createMessage"] }`, server-and-method only, since neither request has a name.

Enforcement is the invisibility pair pointed the other way: an upstream that may not ask is never told the caller can answer, so it does not ask — and a request arriving on a session whose grant a reload removed is refused with `-31042`, which a client is entitled to say. Those refusals, and every allowed exchange, carry `direction: "server_initiated"` in the audit trail; a capability withheld outright has no event, because nothing was asked.

It is a separate knob from `defaultDecision`, and it defaults to `"allow"`, for compatibility rather than conviction: this traffic flowed ungoverned before the check existed, so folding it under the existing field would have broken working installs on upgrade. **Production deployments should set it to `"deny"`.** Content-level questions — refusing an elicitation that asks for a password — stay out of the gateway; that is the external decision hook's job. Reasoning: [design-server-initiated.md](design-server-initiated.md).

### Matching principals

Scope a rule to specific token issuers with `"subjects": { "issuers": ["https://corp.okta.com"], "groups": [...] }`. Subjects and group names are only unique within an issuer, so **when more than one issuer is trusted, pin rules to an issuer** — otherwise a lower-assurance IdP could mint a principal that matches a rule written for another.

Attribute-based rules match on verified token claims: `"subjects": { "claims": { "dept": "eng", "mfa": true } }`. Every listed claim must match — the token claim equals the value, or, when the token carries an array (like an entitlements list), contains it. Values are JSON scalars (string, number, bool). Claims gate like issuers: they combine with `subs`/`groups` as an additional requirement, or stand alone as the whole subject. The same issuer-pinning caveat applies — claim names mean whatever each IdP says they mean, so pin claim-based rules to an issuer when more than one is trusted. Richer conditions (device posture, network location) belong in the IdP, surfaced to fold as claims — that is what token claims are for.

Scope-based rules match on the OAuth scopes the token carries: `"subjects": { "scopes": ["reports:write"] }`. Every listed scope must be held — **scopes are conjunctive**, where `subs` and `groups` are alternatives. That asymmetry is the point: `subs` and `groups` answer *who is this*, and one identity is enough, while a scope answers *what were they granted*, and a list of those is a set of requirements rather than a choice. Like claims, scopes gate — they combine with `subs`/`groups` as an additional requirement, or stand alone as the whole subject.

Scopes are their own field rather than a `claims` entry because the standard spelling cannot be matched as a claim: [RFC 6749](https://www.rfc-editor.org/rfc/rfc6749#section-3.3) makes `scope` a *space-delimited string*, so `"claims": { "scope": "write" }` does not match a token carrying `scope: "read write"` — it compares whole values. fold reads `scope` first and `scp` only if that yields nothing, and accepts either as a space-delimited string or a JSON array, so a rule is written once against the concept rather than against an issuer's spelling. The two are never merged: an issuer sending both is sending the same set twice, and merging would silently union two claims that disagreed.

**A scope denial says what would fix it.** When a caller is refused and the *only* thing standing between them and a rule was scopes, the `-31042` error names them — in the message, and in `data.missingScopes` for a client that reads structured errors — and the audit event carries `missingScopes`. An agent can then re-authorize for exactly what it lacks instead of retrying blind.

Two rules bound that disclosure, and they matter more than the convenience:

- **Only scopes the caller lacks are named**, never the full requirement. Re-authorizing accumulates permissions rather than replacing them, so a caller holding `read` who needs `read` and `write` is told to obtain `write` alone — asking for the pair risks an IdP issuing a token that drops what they already had.
- **A rule contributes only when scopes were the sole obstacle and it targets that exact invocation.** If a non-scope condition rejected the caller first, or the rule was about a different tool, the denial names nothing — disclosing a scope requirement guarding something the caller could not reach anyway would leak that the thing exists. This is the same discipline `args` already follows, which reports *which* argument path failed and never the value the rule wanted.

Scopes work in [`tenants`](#tenants) selectors too, with the same semantics.

## `hook`

The external decision endpoint — fold's one out-of-process seam, and its answer to the plugin runtime it [declines](roadmap.md#non-goals). Absent by default; nothing on the request path changes without it.

```jsonc
{
  "url": "https://inspector.internal/decide",
  "timeoutMs": 300,            // required — no default
  "onError": "deny",           // required — "allow" or "deny", no default
  "stages": ["ingress", "egress", "serverInitiated"],  // nothing runs unless a stage is named
  "methods": ["tools/call"],   // omit → every inspectable invocation
  "bearerSecretRef": "HOOK_TOKEN"
}
```

fold `POST`s a versioned JSON envelope per inspected invocation — stage, method, bare name, upstream, principal, tenant, and the caller's arguments verbatim — and expects `{"decision":"allow"|"deny","reason":"..."}`. The `reason` is returned to the caller, deliberately: unlike a policy denial, where the expected value is the operator's configuration, a hook denial is usually about the caller's own content, and a refusal nobody can act on becomes a support ticket instead of a fix.

**The hook decides; it cannot rewrite.** No redacted arguments, no filtered results. A hook that could edit traffic would be the [transformation non-goal](roadmap.md#non-goals) with an extra hop — fold buffering and mutating bodies, behavior through the gateway no longer matching the upstream. A hook that wants content changed refuses the call and says why.

**Ingress and egress are not interchangeable.** Ingress inspects the invocation and its arguments before the upstream sees them. Egress inspects the result — and by then **the upstream has already acted**. A denial at egress withholds the disclosure, not the effect: the row is deleted, the message is sent, and the caller is told the result was withheld. Egress is a data-loss control; stopping an action means refusing it at ingress. fold says this in the error message too, so a caller does not read "denied" as "did not happen".

A result too large to inspect (1 MiB) is not truncated — a partial body is exactly the blind spot an inspector must not be handed — so it takes the `onError` path, which under `"deny"` means refusing results nobody could have inspected.

**The third stage inspects the reverse direction** — what an upstream asks of the caller's client. Policy decides whether an upstream may sample or elicit at all; `serverInitiated` decides whether *this* request is acceptable, which is where "refuse an elicitation that asks the human for an API key" lives. Unlike egress, refusing here prevents the thing: the client is never asked, so no model tokens are spent and no human sees the prompt.

**It runs after policy.** Its allow is necessary but never sufficient, and its operator is never handed traffic the gateway has already refused. An organization that turns the hook off is still governed by everything above.

**Both bounds are required, with no defaults.** A hook without a timeout is a gateway without one. And fold will not guess between fail-open and fail-closed: compliance deployments want traffic to stop when inspection stops, availability-first deployments want the gateway to keep serving, and guessing would be wrong half the time in a direction an operator discovers during an incident. A hook that times out is abandoned at the bound rather than waited on — a *slow* hook is more dangerous than a broken one, because failing open turns it into an invisible bypass.

**Sending arguments to a second endpoint is a data-egress decision.** The traffic fold otherwise proxies to exactly one upstream now also reaches your inspector, principal claims included. There is deliberately no redaction knob: a partial body is a scanner's blind spot, and choosing what to strip is the content judgement being delegated. An organization that cannot send a body to its own hook should not enable the stage.

Denials get their own audit outcome (`hook_denied`) so an operator can tell their policy from their inspector, and every inspected request carries `hookOutcome`. When `onError: "allow"`, an error there means the call proceeded **uninspected** — `fold_hook_decisions_total{outcome="error"}` and the packaged `FoldHookErrors` alert exist for exactly that. Cost is `fold_hook_duration_seconds`: measured at ~42 µs per decision against a local no-op endpoint, which is the floor, not an estimate of your inspector. Reasoning and the phases still to come: [design-decision-hook.md](design-decision-hook.md).

## `audit`

```jsonc
{
  "sinks": [
    { "type": "stdout" },
    { "type": "file", "path": "/var/log/fold/audit.jsonl", "maxSizeMb": 100, "maxFiles": 5 },
    { "type": "otlp-logs", "url": "http://otel-collector:4318" },
    {
      "type": "webhook",
      "url": "https://siem.example.com/ingest",
      "headers": { "x-api-key": "..." },
      "bearerSecretRef": "FOLD_AUDIT_TOKEN",
      "retry": { "maxAttempts": 4, "initialBackoffMs": 500, "maxBackoffMs": 30000 },
      "deadLetterPath": "/var/log/fold/audit-dead.jsonl"
    }
  ],
  "scrub": {
    "redactUsageKeys": ["promptTokens", "secret"],
    "maxErrorLength": 1024
  },
  "requireDurable": false
}
```

One JSON event per terminal response — including 401s, 403-equivalents, and 429s — with principal, upstream, tenant, authz decision + rule id, outcome, and latency. The `stdout`, `webhook`, and `otlp-logs` sinks deliver asynchronously from a bounded buffer (1024 events) so audit never adds request latency; a full buffer drops and counts rather than blocking, and a closing sink drains for up to three seconds and counts what it could not place. The `file` sink is the exception and is synchronous on purpose: it is the durability anchor `requireDurable` points at, and a buffered file would make that promise conditional on a clean shutdown.

| Sink | Fields | Notes |
|---|---|---|
| `stdout` | — | One JSON line per event. |
| `file` | `path`, `maxSizeMb` (100), `maxFiles` (5) | One JSON line per event, rotated by size. Rotation renames in place (`audit.jsonl` → `.1` → `.2`), so a tail or log shipper watching the live name survives it, and the file count is bounded — a gateway that fills its disk with its own audit trail has found a novel way to stop serving. |
| `webhook` | `url`, `headers`, `bearerSecretRef`, `retry`, `deadLetterPath` | Batched POSTs. Redirects are refused outright: the request carries the sink's token and records naming principals and tools. `bearerSecretRef` names an environment variable holding a bearer token, the same convention `discovery` and the decision hook use — `headers` takes static values, so without it a receiver that authenticates forces the credential into the config document, which is then the one part of a fold config that cannot be checked in, logged, or handed to somebody debugging a federation. Setting both it and an `Authorization` header is rejected rather than merged, and a variable that is empty at startup fails the sink loudly instead of delivering unauthenticated: a receiver would refuse the batch, fold would retry it, and the trail would look delivered while landing in the dead-letter file. |
| `otlp-logs` | `url`, `headers`, `retry`, `deadLetterPath` | OpenTelemetry log records over OTLP/HTTP, via the OTel SDK's own exporter — the wire format is not hand-rolled. A bare base URL (`http://collector:4318`) gets OTLP's `/v1/logs` path, matching the `tracing` section's convention. The event's fields become attributes under the same names fold's spans use (`mcp.method`, `fold.upstream`, `fold.tenant`, `enduser.id`), so a trace and its audit record join on the same keys; the body is a short summary, and the outcome sets severity — a refusal is `WARN`, not `ERROR`, because policy denying a call is the gateway working. |

**Delivery is retried, and what it cannot deliver is kept.** A failing POST is retried with exponential backoff and equal jitter — `maxAttempts` 4, `initialBackoffMs` 500, `maxBackoffMs` 30000 by default, so a receiver restarting costs nothing. Retry is on without configuration, because the alternative is losing exactly the events someone will later go looking for. A `4xx` other than `429` is not retried: a receiver that rejects the payload will reject it identically four times. When attempts run out, events are appended to `deadLetterPath` for replay; without one they are counted and gone.

**Losses are visible.** `fold_audit_events_total{sink,outcome}` counts `delivered`, `retried`, `dead_lettered`, and `dropped` — the audit trail cannot report its own gaps, so this is where a gap shows up. The packaged alert `FoldAuditEventsLost` fires on either kind of loss ([observability](operations.md#dashboards-alerts-and-slos)).

**`requireDurable` refuses to start without a trail that survives.** Off by default, because shipping `stdout` to a collector is a legitimate production choice and fold cannot see the far end of it — so this is an assertion the operator makes about their own deployment rather than a default fold is entitled to impose. Set it, and at least one sink must keep what fold could not deliver: a `file` sink qualifies on its own, a `webhook` or `otlp-logs` sink once it has a `deadLetterPath`, and `stdout` never does. A document declaring none is rejected by `fold --validate`; a declared durable sink whose path will not open fails at startup instead, which is the case validation cannot see. It promises that every *abandoned delivery* has somewhere on disk to land — not that nothing is ever lost: a sink whose buffer fills while its receiver is down still drops, because writing to disk from the request path is the latency audit must never add.

**Scrubbing upstream-provided fields.** The optional `scrub` object transforms events before they are written to sinks: `redactUsageKeys` deletes the named keys from the `usage` map an upstream returned in its result `_meta`, and `maxErrorLength` truncates the `error` field. This is useful when a misbehaving upstream can return secrets, PII, or verbose stack traces in either place — `scrub` removes or bounds them without dropping the rest of the event. It applies after upstream calls are counted and after outcomes are decided, so metrics and decisions are unaffected. It edits the record only: the result the client receives is untouched, because the `usage` map on the event is the upstream's own result `_meta` and fold scrubs a copy of it.

**Every record names the replica that wrote it.** The `instance` field is `FOLD_INSTANCE_ID` when set, otherwise the hostname — which Docker sets per container and Kubernetes per pod, so a fleet is attributable with no configuration. It is deliberately an environment variable rather than a config field: the value has to differ per replica, and the config document is the one thing every replica shares. On the `otlp-logs` sink it rides the resource as `service.instance.id`, the attribute OTel backends already group by, rather than being repeated on every record.

## `server`

| Field | Default | Notes |
|---|---|---|
| `mcpPath` | `/mcp` | Path the gateway serves MCP on. |
| `allowedHosts` | localhost set | DNS-rebinding protection: allowed Host/Origin hostnames. Set to your public hostname(s) in production, or `["*"]` only behind a trusted proxy. |
| `rateLimit` | none | Global `{ requestsPerMinute }` across all upstreams, plus optional `perPrincipalPerMinute` capping each authenticated principal on its own bucket, so one caller's flood cannot 429 the others. For a bucket shared by a *team* rather than one per person, see [`tenants`](#tenants). |
| `budget` | none | `{ period, upstreamCalls }` — a consumption allowance across every upstream, accumulating until the calendar period rolls over. Like the rest of this section it is construction-wired: a reload rejects a change to it, so an allowance cannot be widened under a running gateway. |
| `maxBodyBytes` | 1 MiB | Request body cap; larger bodies are answered `413` (chunked bodies are cut off at the cap). |
| `keepAliveMs` | off | Pings each connected client on this interval, so a long-lived stream keeps carrying bytes past an intermediary's idle timeout. Off by default, and **read the notes below before enabling** — it disconnects clients that decline the standalone SSE stream, and it supersedes `sessionIdleTimeoutMs` for any client that answers. Construction-wired. |
| `sessionIdleTimeoutMs` | 30 min | Closes a downstream MCP session after this long without a request from its client. Ending a session with `DELETE` is optional in the protocol and clients routinely reconnect without it; unexpired, each abandoned session would hold gateway state — and the upstream subscriptions it pins — forever. Negative disables expiry. Construction-wired; watch the population via `fold_downstream_sessions`. |
| `redisUrl` | `REDIS_URL` env | `redis://` URL sharing cache, rate-limit, and breaker state across gateway instances. Absent → in-process state. Redis outages fail open (bounded 500 ms per operation). |
| `metricsAddr` | unset | Moves `/metrics` (and `/health`) to their own listener, e.g. `":9090"`. Absent, they stay on the main port behind `allowedHosts` — which is why a scraper arriving as a pod IP or a service name gets `403` and reads as "target down". A separate listener is the arrangement to prefer whenever something other than the gateway's own host scrapes it: it is not an origin a browser can be steered to, so it needs no Host allowlist, while the public port stops exposing upstream ids, namespaces, tenant ids, and endpoint URLs to a rebinding attempt. **Bind it to an internal interface** — network scope is what protects it. Construction-wired; the Helm chart sets it for you via `metrics.listener.enabled`. |
| `metricsAllowedHosts` | unset | Optional Host allowlist for the separate `metricsAddr` listener. When set, requests whose Host does not match are refused with 403; empty preserves the previous unguarded behaviour. Follows the same rules as `allowedHosts`: exact hostnames or `"*.suffix"` patterns; ports are ignored. Use it when the metrics listener must sit on an interface reachable from a broader network than you would let scrape it unbounded. Construction-wired. |
| `introspection` | disabled | `{ "enabled": true }` serves the read-only APIs: `GET /api/federation` (the federation snapshot — health, breaker and endpoint state, upstream source — static vs discovered — and credential-strategy names, discovery status, shared-state/audit/tracing facts, and the viewer's tenant governance) and `GET /api/auth-hint` (the unauthenticated sign-in hint). With auth enabled, `/api/federation` requires the same Bearer token as `/mcp` and shares its rate budgets; add `"groups": ["platform-ops"]` to further restrict reading to principals carrying one of those groups (403 otherwise, audited) — the fix for deployments where any valid token holder is too wide an audience. (A viewer who resolves to a tenant sees that tenant's federation rather than the whole one; see [`tenants`](#tenants).) |
| `console` | disabled | `{ "enabled": true }` serves the read-only fold console page at `/console`: an observability dashboard plus an MCP test console for tools, prompts, and resources that talks to the gateway's own `/mcp` endpoint — console traffic is governed and audited like any other client's. The dashboard renders what `/api/federation` reports, so it requires `introspection.enabled`. Add `"oauth": { "clientId": "fold-console" }` and the console signs users in with Authorization Code + PKCE against a trusted issuer (register `{origin}/console/` as the redirect URI at the IdP; `issuer` picks among multiple trusted issuers, `scopes` adds authorization scopes) instead of a pasted token. The page's assets are maintained in [fold-run/fold-console](https://github.com/fold-run/fold-console) and vendored here at a pinned commit. |
| `publicUrl` | falls back to `auth.resource` | The absolute base URL clients reach this gateway at, e.g. `https://mcp.acme.example`. fold has no other way to know it — it terminates no TLS of its own and reads no forwarded-proto header — and a `Host` taken from the request cannot stand in, because `allowedHosts: ["*"]` is a supported posture behind a trusted proxy and a caller-supplied `Host` would then be injected into every URL fold mints for that caller. Today one feature needs it: federated icons, below. Must use `https` (loopback exempt). Construction-wired. |
| `icons` | enabled | `{ enabled, maxBytes, timeoutMs, cacheTtlMs }` — how fold serves the icons its upstreams advertise. See below. Default on, which is a no-op until there is a `publicUrl` to mint under. |
| `identity` | none | `{ websiteUrl, icons }` — what fold says about *itself* at `initialize`, alongside the name, title, and version it always reports. Each icon is `{ src, mimeType?, sizes?, theme? }`; `src` must be `https:` or `data:`. Construction-wired. |

## `routing`

| Field | Default | Notes |
|---|---|---|
| `namespaceSeparator` | `__` | Separator between namespace and bare name in public tool/prompt names. Must not contain lowercase letters, digits, or hyphens (the namespace alphabet). |
| `pageSize` | `200` | Per-page bound on federated list results (tools, prompts, resources, templates, tasks). Fold merges and policy-filters every upstream's full list, then serves it in pages; cursors are opaque, bound to the calling principal, and expire when the underlying snapshot changes (the client receives `-32602` and restarts the list — `list_changed` notifications already prompt refetches). Negative disables pagination (single merged page). Lists merge in **configuration order**, each upstream's own order preserved — which is what makes an offset cursor meaningful, and what the `2026-07-28` revision asks for when it says a server SHOULD return tools in a deterministic order. |

## `discovery`

```jsonc
{
  "url": "https://registry.internal/fold-upstreams.json",  // serves {"upstreams":[...]} — same schema as the static section
  "intervalMs": 30000,                                     // poll interval (default 30s); syncs once immediately at startup
  "bearerSecretRef": "FOLD_REGISTRY_TOKEN",                // optional: env var sent as a Bearer token on the poll
  "allowedAuthStrategies": ["static"],                     // optional: credential strategies discovered upstreams may carry (absent → unrestricted)
  "allowedSecretRefs": ["ML_SEARCH_API_KEY"],              // optional: env vars discovered upstreams may name in secretRef (absent → unrestricted)
  "allowedCredentialHosts": ["*.svc.cluster.local"],       // optional: where a credentialed discovered upstream may send secrets (url + tokenEndpoint hosts)
  "minHealthCheckIntervalMs": 1000                         // floor on discovered healthCheck.intervalMs (default 1000)
}
```

These are the gateway-side backstop for a partially trusted registry. Whoever controls the discovery source controls an upstream's `secretRef` names, its `tokenEndpoint`, **and** its destination URL — so naming a secret is only half the exposure. `allowedSecretRefs` bounds which secrets a discovered upstream may reference; `allowedCredentialHosts` bounds where a credentialed upstream may send them (both its endpoint hosts and its token endpoint). Patterns are exact hostnames or `*.suffix`, which matches subdomains only — list the apex separately if you mean it. Because the two halves are only meaningful together, **`allowedCredentialHosts` is required whenever `allowedAuthStrategies` or `allowedSecretRefs` permits credentials**; config validation rejects the half-configured combination rather than leaving the destination open. Any violation rejects the document whole and the last good set keeps serving. When there is no last good set — a gateway with no static upstreams whose source has never been applied — `/health` answers `503` until one is, so a pod that booted while its registry was down is held out of rotation rather than serving an empty federation as if that were the answer. Set all three whenever the people who can register upstreams are not the people who operate the gateway (see [security-model.md](security-model.md)).

The URL decides where traffic routes and where upstream credentials attach, so it must use `https` (loopback exempt). Back it with whatever produces the document — on Kubernetes, [`fold-discovery`](discovery-controller.md) does it out of the box: label a Service `fold.run/upstream: "true"` and it joins the federation. Any other producer works too — a service registry, a script writing to object storage. Each gateway instance polls independently; a consistent source keeps a fleet consistent.

## `tenants`

Groups principals for governance. A tenant is a label on identity, resolved from claims the IdP already asserts — it never travels alongside a token and is never a trust anchor. **A tenant groups principals; it does not authenticate them**, and policy remains the authority on what may be invoked. Reloadable, unlike `server.budget`: tenants change when a customer signs up.

```jsonc
"tenants": [
  {
    "id": "acme",
    "subjects": { "claims": { "org_id": "acme-prod" } },  // same shape policy rules use
    "budget": { "period": "month", "upstreamCalls": 500000 },
    "rateLimit": { "requestsPerMinute": 2000 },           // one bucket for the whole tenant
    "upstreams": ["billing", "crm"]                        // optional: all upstreams if omitted
  }
]
```

| Field | Default | Notes |
|---|---|---|
| `id` | — | Lowercase alphanumeric + hyphens. Appears in every audit event the tenant's principals produce, and as the `tenant` label on `fold_tenant_*` metrics. |
| `subjects` | — | Required. Which principals belong, using the same shape policy rules use (`groups`, `subs`, `issuers`, `claims`). A tenant with no selector would capture every caller, so it is rejected. |
| `budget` | none | `{ period, upstreamCalls }` for the tenant as a whole — the dimension a per-upstream or server-wide budget cannot express. Charged in upstream invocations like every other budget, only for calls that reach an upstream; exhaustion mints `-31044` naming the tenant. |
| `rateLimit` | none | `{ requestsPerMinute }`, one bucket shared by the tenant's principals; over it, `429` with `Retry-After`. Distinct from `server.rateLimit.perPrincipalPerMinute`, which gives each *person* a bucket: ten agents on one team get ten allowances there and one here. |
| `upstreams` | all | Optional visibility subset by upstream id, evaluated before policy. |

**How the four pieces behave.** The budget and the server's are charged narrowest-first (upstream → tenant → server), so a refusal never spends a wider allowance. The rate limit is enforced with its siblings before routing, widest-first (global → tenant → per-principal). The visibility subset filters the *fan-out*: an upstream outside it is never asked, so it costs no request, no budget, and no partial-failure entry when it is down — and a named invocation against it is refused before the policy engine sees it, with `-31042` (`tasks/*` answer "no upstream owns that id" instead, because there a refusal must not reveal existence). A viewer's console shows their tenant's federation, not the operator's.

A principal belongs to at most one tenant. Overlap that validation cannot decide statically — two selectors that only collide for some principals — is caught at request time and **refused**, not guessed: assigning a caller by precedence would hand them another tenant's allowance and visibility. An unmatched principal has no tenant and is governed exactly as before tenancy existed, so an existing deployment behaves identically until it declares one.

Resolution is a map lookup for the two selector shapes a large document repeats — one claim equalling one value, or one group — so a document with ten thousand tenants resolves as fast as one with ten ([benchmarks](benchmarks.md#tenant-resolution-cardinality)); compound selectors still scan, so keep those in the tens. Design record: [design-tenancy.md](design-tenancy.md).

## `tracing`

```jsonc
{
  "otlpEndpoint": "http://otel-collector:4318",  // OTLP/HTTP collector; a bare base URL gets the standard /v1/traces path
  "serviceName": "fold",                         // resource service.name (default "fold")
  "sampleRatio": 1.0,                            // sampling for traces fold roots itself; parent-based, so sampled callers stay sampled
  "recordPrincipal": false                       // opt-in: stamp the principal's subject on server spans as enduser.id
}
```

Absent, fold only propagates the caller's W3C trace context (see Observability). Present, fold emits its own spans: one server span per MCP request — carrying method, tool/prompt name, routed upstream, policy decision + rule id, and outcome, the same fields as the audit event — and one client span per upstream call with its guard outcome (ok, rate-limited, circuit open, error). The principal's subject is stamped only with `recordPrincipal: true`: it is personal data, trace backends commonly have broader access than the audit trail, and the audit event already carries the same identity. Spans export through a batching processor, so the request path never waits on the collector.
