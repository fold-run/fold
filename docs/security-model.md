# Security model

What fold trusts, what it enforces, and how the pieces compose. The
[SECURITY.md](../SECURITY.md) at the repo root covers reporting and
supported versions; this document is the architecture.

## Trust anchors

Four configuration values are trust anchors — compromising any of them
compromises the gateway — and validation forces each onto `https` (loopback
exempt for development):

| Anchor | Why it is one |
|---|---|
| `auth.issuers[].issuer` / `jwksUri` | The inbound identity root: forging a principal only requires substituting the key set. Issuers are allowlisted and checked before any network I/O; JWKS fetches are single-flighted, size-bounded, and timeout-bounded so unknown-`kid` floods cannot be amplified against the IdP. A cached set is re-read every 5 minutes even when every `kid` is known, so a key the IdP withdraws stops verifying tokens within that window rather than for the life of the process; a refresh that fails keeps the last good set serving (an IdP outage must not reject every valid caller), is retried at most every 30 s, and is counted in `fold_jwks_fetches_total` so the outage is visible as the IdP's rather than as bad clients. |
| `auth.ema.idpIssuer` / `idpJwksUri` | The same, for the ID-JAG exchange path. |
| Upstream `tokenEndpoint`s | Carry client secrets (client-credentials, token-exchange). |
| `discovery.url` | Decides where traffic routes and where credentials attach: whoever controls the document can add upstreams. Documents are strictly parsed, size-capped (4 MiB), and validated whole against the running config — a collision with a static upstream rejects the entire document. |

## The inbound chain

Every `/mcp` request passes, in order: host/origin allowlist (DNS-rebinding
protection) → body-size cap → Bearer verification (trusted issuer, JWKS
signature, exact audience per RFC 8707, non-empty `sub`, asymmetric
algorithms only — RS/ES/EdDSA) → global, per-tenant, and per-principal rate
limits → routing (which resolves the caller's tenant) → the tenant's
visibility subset → policy → per-upstream guards, including the per-upstream,
per-tenant, and server budgets → the upstream. Every terminal
response — including the refusals — produces exactly one audit event; audit
is the single exit door.

With EMA, fold additionally acts as a deliberately one-grant-wide
authorization server: `POST /oauth/token` exchanges an enterprise-IdP ID-JAG
(RFC 7523) for a short-lived fold-signed token. Each assertion's `jti` is
single-use (recorded fleet-wide via Redis when configured; the in-process
recorder is size-bounded at 100k live records, a ceiling only reachable by
signing that many valid assertions inside one assertion's lifetime), the
endpoint is rate limited against amplification, and issuers marked
`mode: "exchange"` are never accepted as direct bearer issuers. The exchange
obeys the same single-exit-door rule as `/mcp`: every terminal response —
the mint, an invalid assertion, the rate-limited 429, and above all a
**detected replay** — emits one audit event (`method: "oauth/token"`) whose
`decision` field carries the exchange outcome verbatim (`minted`,
`replayed`, `invalid_grant`, ...), so a replay is alertable on a structured
field and an attacker probing the endpoint under the rate limit is visible
to the trail rather than only to the rate limiter.

## Enforcement: the invisibility pair

Policy is deny-by-default and enforced twice: named invocations
(`tools/call`, `prompts/get`, `resources/read`, and the completions and
subscriptions derived from them) are denied, **and** list results are
filtered per principal — a caller never sees a tool it cannot call. Rules
match subjects, groups, issuers, and verified token claims (ABAC); subjects,
groups, and claim names are only meaningful within an issuer, so rules
should pin `issuers` whenever more than one IdP is trusted.

Two of the newer matchers weaken the pair in ways worth knowing before relying
on them. An **argument constraint** cannot filter a list, because there are no
arguments at list time: a constrained tool is visible and only conditionally
callable, and the pair's guarantee narrows to *a caller never sees a tool they
cannot call at all*. A **`toolKind` gate** does filter lists, but it reads
annotations supplied by the very upstream being gated — it is a hygiene
control for federations you operate, and against a server you do not control
the boundary is naming the tools. Unknown annotations deny, so the gate fails
in the direction the rest of the engine fails.

Task ownership follows the same principle: a task minted through the
gateway is bound to the minting principal, and another caller's requests
for it answer exactly like an unknown id — no existence leak.

Because that is an authorization record rather than a routing hint, it
lives behind `state.Provider` and not in process memory: with Redis
configured the whole fleet reads the same ownership, so a caller cannot
reach another principal's task by landing on an instance that did not serve
the mint, and the binding survives a rolling restart. Records are keyed by
a digest of the task id and hold a digest of the owning principal — neither
the id a caller names nor the subject claims go into shared state verbatim.
A Redis outage falls back to the records that instance mirrored locally, so
it degrades to per-instance enforcement rather than to none. Two limits are
worth knowing: records expire after 24 hours, and a gateway *without* Redis
is still per-instance — in both cases the task falls through to the
locate-by-probe path and becomes reachable by any caller, exactly as it
does for a task fold never saw minted.

## The decision hook is a trust boundary and a data-egress path

`hook` sends each inspected invocation — the caller's arguments verbatim, the
principal's subject, issuer, and groups, and the tenant — to an endpoint an
operator configures. Two consequences deserve to be stated rather than
inferred.

**It is a disclosure.** Traffic fold otherwise proxies to exactly one upstream
now reaches a second destination. That is the operator's decision to make, and
fold makes it explicit rather than offering a redaction knob: a partial body is
a scanner's blind spot, and choosing what to strip is the content judgement
being delegated. The endpoint is held to the same posture as every other
outbound trust path — bounded timeout, redirects refused, a dedicated client —
because a hook that can redirect fold's decision request can be repointed by
whoever answers it.

**It cannot widen a grant.** The hook runs after policy and can only refuse,
so its compromise costs availability rather than authorization: a hostile hook
denies traffic it should have allowed, and cannot admit anything policy has
already refused. That asymmetry is why ingress sits where it does.

**Egress stops disclosure, not action.** By the time the result stage runs the
upstream has done whatever it was going to do. A denial there withholds the
answer from the caller and records it; it does not roll anything back. An
organization deploying egress inspection as a control on *what its agents can
do* has misread it — that control is ingress. Egress is what keeps data from
leaving.

The failure posture is the operator's explicit choice (`onError`), and
fold refuses to start without it. Under `allow`, a hook outage means calls
proceed **uninspected** — recorded per request as `hookOutcome: "error"` and
counted in `fold_hook_decisions_total`, because a fail-open bypass that left
no trace would be the worst of both designs.

## What an upstream advertises can change after you approve it

The trust decision that matters most in a federation is made by a human
reading a tool list, and until `pinDefinitions` it was pinned to nothing.
Definitions are re-read from the upstream on every cache refill, so a tool's
description, schema, or annotations can be rewritten afterwards and be served
under the same namespace, the same policy grant, and the same audit trail.

`pinDefinitions: "warn"` records the digest of each definition and reports a
difference — `upstream/definitionChanged` in the trail,
`fold_definition_drift_total` in the metrics, `FoldDefinitionDrift` in the
packaged alerts. The baseline lives in shared state so a fleet agrees and a
rolling restart cannot silently re-pin, which matters because a restart would
otherwise be the moment to choose.

Three limits, stated rather than implied. Trust-on-first-use cannot vouch for
a definition fold has never seen — this detects change, not badness. A
legitimate improvement trips it identically to a hostile rewrite, because they
are the same bytes; the event says *changed*, and a human decides which kind.
And it reports rather than blocks: prevention needs a way to adopt a new
definition, every candidate costs either a write path into running state or a
hand-copying chore, and that trade is
[unresolved on purpose](design-definition-pinning.md).

## An icon is bytes fold fetches on an upstream's say-so

Federated icons are the one place fold makes an outbound request to a URL an
*upstream* supplied, so the bound on where that request may go is the whole
control. It is the bound credential attachment already uses: the icon's host
must be one of that upstream's own configured endpoint hosts, checked when the
URL is minted and again immediately before the fetch. An icon anywhere else is
never minted, so a hand-built digest for it resolves to nothing — the doubled
check is what stops a digest being a way to aim the gateway at a metadata
service or an internal admin port.

Three more properties, each structural rather than procedural. The fetch
carries no credentials, because the client that performs it has a plain
transport with no path to `auth.UpstreamCredentials` at all — the
specification's "fetch icons without credentials" is a type-level fact here
rather than a rule someone has to keep. Redirects are refused outright rather
than followed within a host: a redirect is never a legitimate step in fetching
an icon, and allowing even a same-host one reopens the origin question. And the
body is bounded and **refused rather than truncated**, because half an image is
a response the upstream never sent.

The image is identified from its magic bytes; the declared type is advisory, as
the specification says to treat it. `image/svg+xml` is refused, and that is the
security decision here. fold would be serving the SVG from *fold's own origin*,
where an SVG's embedded script runs same-origin with `/console/`,
`/api/federation`, and `/oauth/token` — strictly worse than the upstream
serving it, since the upstream's origin holds nothing of fold's. Independently:
SVG is XML and has no magic bytes, so a strict magic-byte allowlist cannot
admit it without either trusting the declared type or parsing the XML, and the
second is the content inspection fold declines.

**The endpoint is unauthenticated, by necessity.** Clients are told to fetch
icons without credentials and a browser `<img>` carries no bearer token, so an
authenticated endpoint would serve nothing to the clients it exists for. What
it discloses is bounded to suit: the path names no tool, prompt, or resource —
only a namespace and a digest — and the bytes are branding artwork identical
for every caller. fold treats them as public data, the same class as the
console's static assets and the `.well-known` documents.

State the limit rather than imply it: **policy filtering is preserved here by
unguessability, not by authentication.** A principal who cannot see a tool is
never handed its icon URL, but a principal who guesses one gets the bytes.
That is the accepted trade, and it is why the endpoint serves only images and
never a name, a description, or a schema. Making the URL a true capability —
`HMAC(k, upstreamID‖src)` rather than a plain digest — needs a fleet-stable
secret that most deployments do not configure, so it is named here as the
upgrade path for a threat model that demands it rather than built
speculatively. An operator who does not accept the trade sets
`server.icons.enabled: false`.

## The reverse direction is governed separately, and opts in

Bridged sessions let an upstream reach the caller's client: sampling borrows
the caller's model, elicitation asks the caller's human a question. Both are
legitimate protocol features and both spend something that belongs to the
caller, so `policy.serverInitiatedDecision: "deny"` puts them behind the same
engine — matched on server and method, decided against the principal whose
in-flight call the request arrived under, and recorded with
`direction: "server_initiated"` in the audit trail.

Enforcement is the invisibility pair again, pointed the other way: an upstream
policy will never let ask is not told the caller can answer, so it does not
ask, and a request that arrives anyway on a session whose grant was revoked is
refused on the spot. The decision is re-read per request, so a reload that
*removes* a grant bites immediately; one that *adds* a grant waits for a new
bridged session.

Two properties to hold in mind. **It defaults to allow**, because this traffic
flowed before the check existed and tightening it silently on upgrade would
break working installs; a hardened deployment sets it explicitly, and the
[production checklist](deploy.md#production-checklist) says so. And a withheld
capability is refused by the upstream's own SDK before a request is sent,
which means **there is no audit event for it** — nothing was asked. The trail
records the exchanges that reach fold: those it allowed, and those it refused
on a session whose grant had been taken away. An operator reading no
sampling events under a deny posture is reading success, not silence.

What the policy check does not do: fold does not read the sampling messages or
the elicitation prompt. An upstream allowed to elicit can ask the human
anything, including for a secret — structurally, that grant is all-or-nothing.

The content question has an answer now, and it is the one this model always
pointed at: the decision hook's `serverInitiated` stage, which sees what the
upstream is asking for and can refuse *this* request while allowing the next.
Refusing there prevents rather than withholds — the client is never asked — and
it is still not a scanner in the gateway, which is the point.

## Credentials never travel further than configured

Upstream credentials (API keys, exchanged tokens, passthrough bearers)
attach per outgoing request and only to requests bound for a configured
endpoint host of that upstream. Two layers enforce it: the HTTP client
refuses cross-host redirects outright, and the transport re-checks the
destination host before attaching anything — a hostile upstream answering
3xx cannot capture a credential. The token-endpoint client refuses
redirects entirely (not just cross-host): Go replays POST bodies on
307/308, and those requests carry the client secret and — under
token-exchange — the caller's own bearer token as `subject_token`, so a
redirecting token endpoint would otherwise hand both to the host it names.
Its response is size-bounded like every other body fold reads from a remote
party, and concurrent first-time callers for one identity share a single
grant request rather than becoming a burst of them against the IdP.
Exchanged tokens cache per `(upstream, issuer, subject)` under a bound —
evicting one costs a re-exchange, never correctness; per-caller strategies
(passthrough, token-exchange) require `auth.mode: "required"` and disable
list caching so one caller's per-user list can never serve another.

**A caller-derived credential also partitions the upstream session, not just
the request.** Attaching credentials per request is necessary but not
sufficient: the MCP session itself is established by whichever caller opened
it, and that identity is durable — the SDK detaches the connection context
but preserves its values, so the `initialize`, the standalone SSE stream, and
every reconnect of that stream carry the opening caller's credential for as
long as the connection lives. A shared session would therefore present one
caller's identity to the upstream while carrying another's bearer on the
request, which an upstream that scopes anything to the session it minted is
entitled to resolve either way — and it would keep re-minting a departed
caller's exchanged token indefinitely. So `passthrough` and `token-exchange`
upstreams hold one root session per `(issuer, subject)`, the same key their
token cache uses. Those sessions age out on the idle sweep that already
bounds bridged sessions, because they are keyed by an identifier the gateway
does not choose. Upstreams whose credential is the gateway's own keep the
single shared session: their identity is the same whoever asks.

The same redirect rule covers fold's other two credentialed outbound
clients. The discovery poller refuses every redirect: the document decides
where traffic routes, and Go only strips a bearer credential when a
redirect leaves the *domain*, so a sibling host — or a plain-http same-host
target — would otherwise receive both the credential and the authority to
register upstreams. The audit webhook refuses them too: its POST carries
the sink's configured headers and a batch of records naming principals and
tools.

Secrets never appear in the config document — `secretRef` fields name
environment variables — and `/health` withholds URLs, owners, and error
text (which can name env vars or internal hosts) unless auth is disabled,
i.e. on deployments already private by posture.

## Discovery moves an authorization boundary — treat it that way

With dynamic discovery, *who can register an upstream* becomes a security
decision made outside fold: on Kubernetes with `fold-discovery`, it is
whoever can create or label a Service in a watched namespace. Three
consequences, each with a control:

- **Credential references are the sharp edge.** A registered upstream
  chooses both its `secretRef` names and its destination URL — ungated,
  that is an exfiltration path for any gateway-held secret, and
  `passthrough` would forward caller tokens to a URL of the registrant's
  choosing. Two independent gates close it: the producer refuses
  credentialed strategies and secret references by default
  (`--allow-auth-strategies`, `--allow-secret-refs`), and the gateway
  enforces its own `discovery.allowedAuthStrategies` /
  `allowedSecretRefs` allowlists as a backstop, rejecting a violating
  document whole. Set the gateway-side allowlists whenever the discovery
  source is not operated by the gateway's operators.
- **Identity claims need bounds.** A registration colliding with a static
  upstream id makes the gateway reject every future document (fail-safe,
  but a freeze an attacker can cause — alert on
  `fold_discovery_syncs_total{outcome="rejected"}`); producer-side
  `--reserved-ids` prevents publishing the collision at all. Among
  discovered entries, namespace prefixing (on by default, `--allow-unprefixed-ids`
  to disable) requires both the upstream id **and** the MCP namespace — the
  identity clients actually route on — to carry the registering Kubernetes
  namespace's prefix, with hyphens escaped so the prefix is unambiguous.
  Contested claims drop every claimant, so list order cannot hand an
  identity to whoever sorts earlier.
- **Policy is the exposure gate, and wildcards defeat it.** A discovered
  upstream is plumbing until a policy rule grants its tools — unless rules
  use `"server": "*"`, which makes every future registration instantly
  callable. With discovery enabled, name servers in allow rules.

## Tenant isolation

A tenant is a **named set of principals**, resolved from claims the IdP
already asserts (`tenants[]`, see the README). The line that governs
everything below: **a tenant groups principals; it does not authenticate
them.** It is derived from the verified `auth.Principal`, never presented
alongside a token, so a caller can no more assert a tenant than they can
assert a group — and tenancy adds no trust anchor, no second authorization
engine, and no parallel allow path. A principal matching two tenant
definitions is **refused** rather than assigned to one: picking would hand a
caller another customer's allowance and visibility, which is precisely the
failure the boundary exists to prevent.

What isolation a tenant actually buys:

- **Visibility.** `tenants[].upstreams` bounds which upstreams exist for that
  tenant, evaluated *before* policy. It filters the fan-out rather than the
  result, so an upstream outside the subset is never contacted on that
  caller's behalf — no request, no session, no budget charge. Named
  invocations against it are refused before the policy engine sees them, and
  the console's federation view is the viewer's, so a topology listing cannot
  undo the cut. Policy remains the authority on what may be invoked among
  what is left; the subset is the coarser cut, not a replacement.
- **Blast radius.** `tenants[].rateLimit` is one bucket for the whole tenant.
  This is the thing `perPrincipalPerMinute` cannot express: that gives each
  *person* a bucket, so ten agents on one team hold ten allowances, while a
  tenant shares one. "Team A cannot flood team B" means the second.
- **Consumption.** `tenants[].budget` is an accumulating, calendar-aligned
  allowance charged in upstream invocations — and charged only for calls that
  reach an upstream, so an outage or a rate limit never spends a customer's
  month. Exhaustion is `-31044`, distinct from a rate limit because the
  remedies differ.
- **Attribution.** Every audit event a tenant's principals produce carries
  `tenant`, denials and rejections included, and `fold_tenant_requests_total`
  / `fold_tenant_upstream_calls_total` carry it as a label. Isolation you
  cannot see the edges of is not something you can operate.

Underneath, the gateway-wide protections still apply and are unchanged: the
global rate limit protects the gateway, per-upstream limits and circuit
breakers protect fragile backends. With Redis, all of this state — tenant
buckets and budgets included, plus EMA replay protection — is fleet-wide, so
a customer's allowance is one allowance rather than one per replica. Redis
outages fail open (bounded 500 ms per operation): the gateway degrades to
per-instance enforcement rather than going down — every primitive keeps a
local mirror, including (since v1.11) the rate-limit windows and breaker
state, whose mirrors are fed on every healthy decision so an outage starts
warm — and says so via `fold_budget_degraded_total` and
`fold_state_degraded_total`.

**What tenancy is not.** It is not a private federation per customer: the
subset is a visibility filter over shared upstreams, not a routing table of
per-tenant backends, and genuinely isolated upstream sets are separate
gateway deployments — cheaper and more auditable than one router pretending
they are isolated. It carries no credentials or issuers, deliberately: that
would make a tenant a trust anchor. And it is not a substitute for policy,
which stays deny-by-default and stays the thing that decides invocations.

## Telemetry is a disclosure surface

A `/metrics` scrape names upstream ids, namespaces, tenant ids, and the
endpoint URLs of multi-endpoint upstreams — a description of the federation's
shape and its internal hostnames. By default it is served on the main port and
covered by the same DNS-rebinding protection as everything else, which is the
right posture for the loopback-bound default: without it, any page an operator
visits could read that from a gateway running on their machine. Origin
checking alone would not do: in a rebinding attack the page and the target
share the attacker's hostname, so the browser treats the request as same-origin
and sends no `Origin` header at all. The Host allowlist is the check that
catches it.

The cost is that a scraper arriving under any other name — a pod IP, a service
name — is answered `403`. `server.metricsAddr` resolves that by moving
`/metrics` and `/health` to their own listener rather than by exempting the
paths: a listener bound to an internal interface is not an origin a browser can
be steered to, and the public port keeps its checks and stops carrying the
disclosure at all. What guards the telemetry listener is network scope; bind
it accordingly, and do not route it from outside. When the listener must sit
on an interface reachable from a broader network, `server.metricsAllowedHosts`
adds an optional Host allowlist so an unlisted scraper is still refused.

## The console has no privileged path

Two optional surfaces, configured separately and off by default: the read-only
APIs (`server.introspection.enabled`) and the console page that renders them
(`server.console.enabled`). Splitting them changes no trust boundary — the page
was always an ordinary client of the API — but it makes the boundary visible in
the config, and it means an operator can serve the data without serving a
browser page. Each has a deliberate trust story:

- **Static assets** (`/console/`) are the same bytes for every caller and
  carry no data, so they serve unauthenticated. A strict CSP
  (`default-src 'self'`) pins every fetch the page makes to the gateway's
  own origin. Their source is maintained out of tree in `fold-run/fold-console`
  and vendored at a pinned commit that CI verifies, so what ships is a reviewed
  artifact rather than whatever upstream happens to hold — an embed manifest
  test additionally bounds the file set that can enter the binary.
- **The federation API** (`/api/federation`) is data, so it authenticates
  exactly like `/mcp` — with `auth.mode: "required"` it demands a valid
  Bearer token through the same verifier — and it shares `/mcp`'s global
  and per-principal rate budgets. It never carries secret material:
  `secretRef` *names* are config, values never appear. Its disclosure rule
  is broader than `/health`'s, and deliberately so: **any authenticated
  principal, regardless of policy grants, sees the federation topology**
  (upstream URLs, owners, labels, endpoint rotation, each upstream's
  source — static vs discovered — and its credential-strategy *name*, plus
  deployment facts: shared-state backend on/off, audit sink types, tracing
  and EMA enablement) — the console exists to show it. One boundary does
  narrow it: a viewer whose principal resolves to a tenant carrying an
  `upstreams` subset sees that subset's topology and nothing else, counts
  included. The subset is a visibility boundary on the MCP path, and a
  dashboard that ignored it would be the one place a tenant could read
  another's upstream URLs. Strategy names and
  sink types are configuration shape, not credential material; the Redis
  URL is never included, since it can embed credentials. Raw connect
  errors are the exception: they can name secret
  env vars or internal hosts, so when auth is on they are reduced to a
  category and the full text stays in gateway logs. If "any valid
  principal" is too wide for a multi-tenant deployment, set
  `server.introspection.groups`: the state API then answers 403 to any
  principal not carrying an allowlisted group, and every such denial
  exits through the audit sink like any other authorization decision.
  The allowlist requires `auth.mode: "required"` (validation enforces
  it), and the usual multi-issuer caveat applies: group names are only
  unique within an issuer, so keep the list meaningful across every
  issuer you trust.
- **The test console** is a plain MCP client running in the browser,
  pointed at `/mcp`. Its traffic is indistinguishable from any other
  client's: policy filters what it lists, denials answer `-31042`, rate
  limits apply, and every call lands in the audit trail. The console cannot
  bypass governance because there is nothing to bypass with — it holds no
  credential of its own (the user's token lives in page memory only,
  never storage) and reaches no endpoint a client couldn't.
- **Sign-in** (`server.console.oauth`) uses Authorization Code + PKCE: the
  console is a public client, so no secret exists to protect — the PKCE
  verifier is the proof, held in `sessionStorage` only for the redirect
  round-trip and removed on return (it is not a credential by itself; the
  access token never touches storage). The unauthenticated
  `/api/auth-hint` hint carries only public SPA configuration — a
  client id ships in every browser app, the issuer is already advertised
  in the RFC 9728 metadata. The asset CSP admits exactly the configured
  issuer's origin in `connect-src` for the metadata fetch and code
  exchange — config-derived, never a wildcard. Tokens are requested with
  the gateway as RFC 8707 resource, so what the flow mints is precisely
  what `/mcp` audits and verifies. The issuer must be a trusted
  direct-mode issuer (validation enforces it); EMA deployments keep the
  paste-token path, since ID-JAGs are not browser-presentable.

## What fold deliberately does not do

Content inspection (DLP, PII filtering, prompt-injection detection) is out
of scope by design — see the README's "Not implemented" section for the
reasoning. fold's security model is structural: allowlists, invisibility,
credential brokering, and a complete audit trail feeding the SIEM that does
the detecting. Report anything that breaks the guarantees above via the
process in [SECURITY.md](../SECURITY.md).
