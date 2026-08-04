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
algorithms only — RS/ES/EdDSA) → global and per-principal rate limits →
routing → policy → per-upstream guards → the upstream. Every terminal
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

## Credentials never travel further than configured

Upstream credentials (API keys, exchanged tokens, passthrough bearers)
attach per outgoing request and only to requests bound for a configured
endpoint host of that upstream. Two layers enforce it: the HTTP client
refuses cross-host redirects outright, and the transport re-checks the
destination host before attaching anything — a hostile upstream answering
3xx cannot capture a credential. Exchanged tokens cache per
`(upstream, issuer, subject)`; per-caller strategies (passthrough,
token-exchange) require `auth.mode: "required"` and disable list caching so
one caller's per-user list can never serve another.

Secrets never appear in the config document — `secretRef` fields name
environment variables — and `/healthz` withholds URLs, owners, and error
text (which can name env vars or internal hosts) unless auth is disabled,
i.e. on deployments already private by posture.

## Tenant isolation under load

The global rate limit protects the gateway; `perPrincipalPerMinute` gives
each authenticated principal its own bucket so one tenant's flood cannot
429 the rest. Per-upstream limits and circuit breakers protect fragile
backends; with Redis, all of this state — plus EMA replay protection — is
fleet-wide. Redis outages fail open (bounded 500 ms per operation): the
gateway degrades to per-instance enforcement rather than going down.

## What fold deliberately does not do

Content inspection (DLP, PII filtering, prompt-injection detection) is out
of scope by design — see the README's "Not implemented" section for the
reasoning. fold's security model is structural: allowlists, invisibility,
credential brokering, and a complete audit trail feeding the SIEM that does
the detecting. Report anything that breaks the guarantees above via the
process in [SECURITY.md](../SECURITY.md).
