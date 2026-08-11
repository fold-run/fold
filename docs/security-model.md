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
| `auth.issuers[].issuer` / `jwksUri` | The inbound identity root: forging a principal only requires substituting the key set. Issuers are allowlisted and checked before any network I/O; JWKS fetches are single-flighted, size-bounded, and timeout-bounded so unknown-`kid` floods cannot be amplified against the IdP. |
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
single-use (recorded fleet-wide via Redis when configured), the endpoint is
rate limited against amplification, and issuers marked `mode: "exchange"`
are never accepted as direct bearer issuers.

## Enforcement: the invisibility pair

Policy is deny-by-default and enforced twice: named invocations
(`tools/call`, `prompts/get`, `resources/read`, and the completions and
subscriptions derived from them) are denied, **and** list results are
filtered per principal — a caller never sees a tool it cannot call. Rules
match subjects, groups, issuers, and verified token claims (ABAC); subjects,
groups, and claim names are only meaningful within an issuer, so rules
should pin `issuers` whenever more than one IdP is trusted.

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
  month. Exhaustion is `-32044`, distinct from a rate limit because the
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
per-instance enforcement rather than going down, and says so via
`fold_budget_degraded_total`.

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
be steered to, so it needs no Host allowlist, and the public port keeps its
checks and stops carrying the disclosure at all. What guards the telemetry
listener is network scope; bind it accordingly, and do not route it from
outside.

## The console has no privileged path

The optional read-only console (`server.console.enabled`, default off) adds
two kinds of surface, each with a deliberate trust story:

- **Static assets** (`/console/`) are the same bytes for every caller and
  carry no data, so they serve unauthenticated. A strict CSP
  (`default-src 'self'`) pins every fetch the page makes to the gateway's
  own origin.
- **The state API** (`/console/api/state`) is data, so it authenticates
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
  `server.console.groups`: the state API then answers 403 to any
  principal not carrying an allowlisted group, and every such denial
  exits through the audit sink like any other authorization decision.
  The allowlist requires `auth.mode: "required"` (validation enforces
  it), and the usual multi-issuer caveat applies: group names are only
  unique within an issuer, so keep the list meaningful across every
  issuer you trust.
- **The test console** is a plain MCP client running in the browser,
  pointed at `/mcp`. Its traffic is indistinguishable from any other
  client's: policy filters what it lists, denials answer `-32042`, rate
  limits apply, and every call lands in the audit trail. The console cannot
  bypass governance because there is nothing to bypass with — it holds no
  credential of its own (the user's token lives in page memory only,
  never storage) and reaches no endpoint a client couldn't.
- **Sign-in** (`server.console.oauth`) uses Authorization Code + PKCE: the
  console is a public client, so no secret exists to protect — the PKCE
  verifier is the proof, held in `sessionStorage` only for the redirect
  round-trip and removed on return (it is not a credential by itself; the
  access token never touches storage). The unauthenticated
  `/console/api/auth` hint carries only public SPA configuration — a
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
