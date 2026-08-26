# Design: per-user upstream credentials

Status: **proposed**. This records the design for a sixth upstream credential
strategy — `vault-oauth`, in which fold completes an OAuth authorization-code
flow with a third party *as the caller* and holds the resulting token — and
settles where the consent happens, where the token lives, and what fold
refuses to become in the process.

## Motivation

fold has five credential strategies and none of them reaches a third-party
SaaS MCP server. `static` sends one key for everybody. `client-credentials`
sends the gateway's own service identity. `token-exchange` (RFC 8693) sends a
token minted by an IdP the upstream must already trust. `passthrough` sends
the caller's own fold-audience bearer. Every one of them assumes the upstream
either accepts a shared secret or federates with the enterprise's identity
provider.

A hosted MCP server run by a SaaS vendor does neither. It accepts its own
OAuth tokens, issued by its own authorization server, scoped to its own user
accounts — and the only way to get one is for the user to sit in front of a
browser and approve it. Today that leaves an organization with two options,
both bad: give every developer their own copy of the credential to keep on
their laptop, or share one service account and lose per-user attribution
entirely. The second is the one fold was built to eliminate, and the first is
the credential sprawl in the README's second bullet.

This is also the gap between fold and the ecosystem's stated direction. The
[roadmap](roadmap.md) has "credential brokering so agents never hold upstream
keys" as a shipped strength, and it is true for every upstream that speaks the
enterprise's identity — which is to say, for the upstreams an organization
runs itself, and none of the ones it buys.

**An adjacent finding, which belongs here because it sharpens the motivation.**
The specification's security guidance forbids the shape `passthrough` takes
against a SaaS upstream: "The MCP server **MUST NOT** use the client's
credentials for the third-party service: That would be token passthrough,
which is forbidden." fold's `passthrough` forwards a token minted *for fold*
to an upstream, which is a deliberate operator choice for an upstream inside
the same trust domain and is gated accordingly ([security
model](security-model.md)). Pointed at a third-party service it is both
forbidden and useless — the audience is wrong and the SaaS will refuse it.
`vault-oauth` is the sanctioned answer for exactly the case `passthrough`
cannot serve.

## What it is, and is not

fold becomes an **OAuth client to the third party, on behalf of one caller**.
It holds the tokens, attaches them on egress, and refreshes them. The caller's
agent never sees an upstream credential, which is the same property every
other strategy already gives — extended to the upstreams that could not have
it before.

It is **not a secrets manager**. fold stores what it minted through a flow it
ran, keyed to an identity it verified. It does not accept a credential an
operator or a user hands it, does not expose a read API for what it holds, and
has no import path. The moment fold takes custody of a secret it did not mint,
it owes the world key rotation, escrow, and an access model, and it becomes a
worse version of a product that already exists.

It is **not a write control plane**. The
[console](design-console.md#explicitly-out-of-scope) declines writes because
registration is discovery's job and a second registration path would compete
with it. That argument is about *configuration* — which upstreams exist, what
policy governs them — and it is untouched here. A user connecting their own
account writes a credential scoped to themselves; it changes nothing another
caller can see, nothing the federation routes to, and nothing a reload reads.
The boundary this record adds, so the distinction does not erode: **the only
thing a request may write is one credential belonging to the authenticated
caller who made it.** No endpoint here takes an upstream id it did not derive
from a signed state, and none writes anything readable by another principal.

It is **not a UI**. No connected-accounts page, no forms, no console changes.
Which turns out not to be a limitation, because the protocol specifies where
the interaction belongs.

---

## 1. The protocol already specifies this

This is the part that changed the design, and it is worth stating before
anything else: **MCP defines this feature**, normatively, including the attack
it invites and the mitigation it requires. The design below implements a
specification rather than inventing a mechanism.

[Elicitation](https://modelcontextprotocol.io/specification/2026-07-28/client/elicitation)
has two modes. Form mode collects structured data through the client. **URL
mode** — introduced in the `2025-11-25` revision — exists for "sensitive
interactions that must *not* pass through the MCP client", and the
specification's worked example of one is this exact feature: "URL mode
elicitation enables a pattern where MCP servers act as OAuth clients to
third-party resource servers."

Four requirements come with it, and fold satisfies each by construction:

| Specification requirement | How this design meets it |
|---|---|
| "The third-party credentials **MUST NOT** transit through the MCP client" | The token is written by the callback endpoint and read by the egress transport. It never enters a response body. |
| "The MCP server **MUST NOT** use the client's credentials for the third-party service" | The caller's bearer authenticates them *to fold* and is not forwarded; `vault-oauth` attaches only what fold minted with the third party. |
| "The user **MUST** authorize the MCP server directly" | The flow runs in the user's browser against the third party, with fold as the OAuth client. |
| "The MCP server is responsible for tokens ... (in other words, the MCP server must be stateful)" | Section 3. |

And one attack, described in the specification at length: a malicious caller
triggers the elicitation, hands the URL to a victim, the victim completes the
flow, and **the victim's tokens bind to the attacker's identity** — an account
takeover. The mitigation is normative: "the server **MUST** ensure that the
user who started the elicitation request ... is the same user who completes
the authorization flow", and the recommended shape is a fold-hosted *connect*
URL that verifies the browser's identity before redirecting onward, rather
than handing the client the third party's authorize URL directly. Section 4 is
that check.

**The SDK can already do all of it.** `mcp.ElicitParams` carries `Mode`,
`URL`, and `ElicitationID` (go-sdk v1.7.0, `mcp/protocol.go:2125`);
`ElicitationCapabilities` distinguishes `form` from `url`; and
`notifications/elicitation/complete` with a client-side waiter
(`mcp/client.go:829`) is how a blocked call learns the browser flow finished.
fold already calls `ss.Elicit` on the downstream session at
`gateway/gateway.go:1271` — relaying an *upstream's* elicitation to the
caller's client. This originates fold's own on the same path.

Two consequences worth naming. **fold invents no error code and no bespoke
consent UX**, which is the largest thing this record removes from an earlier
sketch. And **this is not era-gated**: unlike `subscriptions/listen`, URL mode
predates the `2026-07-28` MRTR change and rides the legacy server-initiated
path fold already serves, so it works for the clients fold has today and
arrives at MRTR's `InputRequiredResult` unchanged when fold gets there.

---

## 2. Where it sits

`auth.UpstreamCredentials.Apply` (`auth/upstream.go:96`) is the seam — one
switch over strategies, called by the credential transport on every outbound
request. `vault-oauth` is a sixth case:

1. Resolve the caller from the request context (`PrincipalFromContext`),
   requiring `iss` and `sub` exactly as `token-exchange` does.
2. Look up `(upstream, issuer, subject)` in the vault.
3. Present the access token if it is live; refresh it if it is not; and if
   there is no record at all, fail the request with a distinguished sentinel.

The strategy is **caller-derived**, so `PerRequest()` reports true and the
existing consequences apply with no new code: one upstream root session per
`(issuer, subject)`, list caching disabled for that upstream, and
`auth.mode: "required"` enforced at validation. That machinery exists because
`token-exchange` needed it, and it is exactly right here.

**The missing-credential case is where the design earns its shape.** The
router catches the sentinel and, instead of returning an error, sends the
caller a URL-mode elicitation naming the upstream and carrying fold's connect
URL, then blocks the call until the flow completes or the elicitation is
declined. From the agent's side, calling a tool on an unconnected upstream
prompts its user to connect and then succeeds — no configuration, no
out-of-band instructions, no operator in the loop.

Ordering: this sits **after** policy, in the per-upstream guard band alongside
rate limiting and the circuit breaker. A caller who is not authorized for a
tool must never be prompted to connect an account for it — the prompt would
disclose that the tool exists, which is precisely what per-principal
invisibility withholds.

**A client that does not support URL elicitation** gets a minted error instead,
carrying the connect URL in the message and in `data`. The specification is
explicit that "Servers **MUST NOT** send elicitation requests with modes that
are not supported by the client", and fold already normalizes each client's
declared capabilities into a profile (`gateway/uicapability.go`), so the check
is a read of something computed. This is a fallback, not a second mechanism:
the URL is the same one, minted the same way, and the identity check on the
other end is identical.

**Whose elicitation is it?** `policy.serverInitiatedDecision` governs
sampling and elicitation that *upstreams* make of callers — the reverse path
is deny-by-default because an upstream can otherwise put an arbitrary prompt
in front of a caller's human. A fold-originated consent prompt is not that: it
is the gateway asking its own caller for something the caller's own request
requires, and gating it on the reverse-path policy would mean an operator who
locked down upstream elicitation had silently disabled account connection.
So it is exempt from that decision and audited in its own right (section 6).
The record states this explicitly because "the gateway may prompt" is a
capability worth being deliberate about.

---

## 3. The vault

**Key:** `(upstream id, issuer, subject)`. Not the provider-plus-method tuple
a registry would use — in fold's model the upstream *is* the provider, and
`(issuer, subject)` is already the identity key that `token-exchange` caches
under and that per-principal root sessions are keyed by. Three places, one
key shape.

**Storage:** `state.Store` (`internal/state`), the interface that already
holds task ownership and definition pins, with a Redis implementation so a
fleet agrees and an in-memory one for a single instance. No new dependency, no
new failure mode, and the operational story is the one operators already have.

**Encrypted, always.** `state.Store` holds opaque bytes and Redis holds them
in the clear; a refresh token is a long-lived credential and fold's own
[security model](security-model.md) says secrets never appear in the config
document, which would sit oddly beside plaintext credentials in a cache. Each
record is sealed with AES-256-GCM under a key from `secretRef` — the same
convention `ema.signingKeyRef` uses — with **the key tuple as the AEAD
additional data**, so a record cannot be moved between principals or between
upstreams by an attacker with write access to Redis. Fleet-wide decryption
requires the same key in every replica, which is already true of the EMA
signing key.

**What is stored:** the access token, the refresh token when the provider
issues one, the expiry, the granted scopes, and the instant of the grant.
**What is not:** anything about the caller beyond the key tuple, and never the
caller's own fold-audience bearer.

**Lifetime and reload.** Records outlive the upstream's presence in the
config. A reload that retires an upstream does not delete its vault entries,
because a typo in a config document must not destroy every user's connection
to a service — and an upstream re-added under the same id keeps working. They
expire on a long TTL instead (`vault.recordTtl`, defaulting to something like
90 days), which bounds abandoned records without making a rollback
destructive. Deletion is deliberate: the caller disconnecting, or the provider
refusing a refresh (section 5).

**Why not an external secrets manager in v1.** Because the seam is what
matters and `state.Store` is already it. A deployment that requires OpenBao or
AWS Secrets Manager needs a second implementation behind the same interface,
which is the arrangement `state.Provider` already documents for its own two
implementations — "nothing has asked for a third" is a reason to wait, not a
reason to design so it cannot happen. The AEAD envelope means the v1 default
is not a placeholder: it is a defensible answer on its own.

---

## 4. Consent, and the identity check that makes it safe

**One callback for the whole gateway**, not one per upstream:
`{resource}/oauth/connect/callback`. Every third-party OAuth app an operator
registers gets the same redirect URI, so onboarding a new SaaS upstream is a
client id, a secret, and a scope list — never another URL to register. The
upstream is carried in the signed state, not in the path.

**Two endpoints, and the first one is the mitigation:**

- `GET /oauth/connect/{state}` — the *connect URL*, which is what goes in the
  elicitation. It verifies that the browser opening it is the principal the
  elicitation was minted for, and only then redirects to the third party's
  authorize endpoint. This is the specification's prescribed defence against
  the account-takeover attack in section 1, and it is the reason fold hands
  out its own URL rather than the third party's.
- `GET /oauth/connect/callback` — receives the code, verifies the state,
  exchanges, seals, stores, and sends `notifications/elicitation/complete` so
  the blocked call resumes.

**How the browser is identified.** fold has no session cookie: it is an API
gateway whose callers hold bearer tokens. The honest options are to require
`server.console.oauth` (the console's Authorization Code + PKCE sign-in
already establishes an IdP-authenticated browser identity, and comparing its
`sub` against the state's is precisely the specification's worked example), or
to run a short IdP sign-in from the connect endpoint for deployments without
the console. **v1 requires the former**: `vault-oauth` requires console OAuth
to be configured, validated at startup rather than discovered at consent time.
Reusing a sign-in fold already implements beats inventing a second one, and
refusing to start is better than a flow that works until someone tries it.

**The state is AEAD-sealed** and carries the principal, the upstream, a short
expiry, and a nonce — the same list the MRTR pattern requires of
`requestState`, for the same reasons. It is single-use, enforced through
`state.Once`, which is what EMA already uses to make an ID-JAG's `jti`
unredeemable twice. PKCE is used unconditionally, including with confidential
clients.

Both endpoints are **rate-limited like `/oauth/token`**, and neither reveals
whether a principal has a record: an unknown or expired state is one response,
whatever the reason.

---

## 5. Refresh, rotation, and the fleet

Refresh happens **on use**, not on a schedule: a background refresher would
keep tokens alive for callers who have gone, and a token nobody is using is a
token that should be allowed to lapse. The fetch is single-flighted per key,
which `cachedFetch` (`auth/upstream.go:166`) already does for exactly this
reason — a burst of first-time callers for one identity must not become a
burst of grant requests carrying the client secret.

**A provider that refuses a refresh deletes the record.** A revoked or expired
grant is indistinguishable from a wrong one, and the recovery is the same:
drop it, and let the next call elicit consent again. This is what makes
revocation-at-the-provider work without fold being told about it.

**The open problem is rotation across a fleet.** Many providers rotate the
refresh token on every use and invalidate the previous one. Two fold instances
refreshing the same record concurrently will therefore have one of them
holding a token the provider has already killed — and the loser's write can
land *after* the winner's, leaving the vault holding the dead one. Per-instance
single-flight does not help; this needs a fleet-wide lease, and `state.Provider`
has `Once` (a claim that never releases) but no lease primitive.

Adding one is a change to an interface with two implementations, which is the
same weight as the `ttlMs` gap in the README's "Not implemented" — and it is
the piece of this design that is not finished. See the last section.

---

## 6. Audit and metrics

Audit is the single exit door, and a credential minted on a user's behalf is
exactly the kind of event a trail is kept for. A new method `oauth/connect`,
with the outcome carried in `decision` verbatim — `elicited`, `minted`,
`refreshed`, `refresh_failed`, `revoked`, `state_invalid`, `identity_mismatch`
— which is the shape `oauth/token` already uses for EMA, so a SIEM rule
written for one reads the other. `identity_mismatch` is the alertable one: it
is the section 1 attack, observed.

The event carries the principal, the upstream, and the granted scopes. It
never carries the token, the code, the state, or the refresh token. Metrics:
`fold_connect_flows_total{upstream,outcome}` and
`fold_vault_refreshes_total{upstream,outcome}`, both bounded by config —
`upstream` is a label fold already uses and the outcomes are a closed set.

---

## 7. What this declines

- **`vault-pat`** — a per-user personal access token the user pastes in.
  It needs a form to paste into, which means a page, which means the console
  grows a write surface. The specification's URL mode covers it (its own
  worked example is an API-key form), but the page would be fold's to build
  and maintain. Deferred until someone has an upstream that offers no OAuth.
- **Per-user OAuth client registration.** The operator registers one app per
  upstream. Anything else means fold holding client secrets it did not
  configure.
- **A credential import path.** See "not a secrets manager".
- **A read API for what the vault holds.** Not even "is this connected" —
  that answer is available by making a call, and an endpoint that enumerates
  which services a colleague has connected is a disclosure with no
  corresponding need.
- **Refreshing ahead of expiry on a timer.** See section 5.

---

## Config surface

```jsonc
{
  "id": "github",
  "url": "https://api.githubcopilot.com/mcp/",
  "namespace": "github",
  "auth": {
    "strategy": "vault-oauth",
    "authorizationEndpoint": "https://github.com/login/oauth/authorize",
    "tokenEndpoint": "https://github.com/login/oauth/access_token",
    "clientId": "Iv1.0123456789abcdef",
    "clientAuth": { "type": "secret", "secretRef": "FOLD_GITHUB_CLIENT_SECRET" },
    "scopes": ["repo", "read:org"]
  }
}
```

Plus one gateway-wide block, because the vault key and the callback origin are
properties of the deployment rather than of an upstream:

```jsonc
"auth": {
  "vault": {
    "sealKeyRef": "FOLD_VAULT_KEY",   // env var: 32-byte key, base64
    "recordTtlDays": 90
  }
}
```

Validation refuses `vault-oauth` unless `auth.mode` is `"required"`,
`auth.vault.sealKeyRef` is set and resolves, `server.console.oauth` is
configured, and both endpoints are absolute https URLs. Discovery-sourced
upstreams may not select it unless `discovery.allowedAuthStrategies` names it
— it is a credentialed strategy and the existing gate covers it with no new
code, which is the gate working as designed.

## Implementation phases

1. **The vault and the strategy.** `state.Store`-backed sealed records, the
   `vault-oauth` case in `Apply`, config and schema, validation. No consent
   flow: a record placed by a test fixture proves the egress half.
2. **The connect endpoints.** State sealing, the identity check, PKCE, the
   callback, `state.Once` replay protection, rate limiting.
3. **The elicitation.** URL-mode elicitation on the missing-credential path,
   the completion notification, the blocked-call resume, and the minted-error
   fallback for clients without the capability.
4. **Refresh.** On-use refresh, single-flight, delete-on-refusal — and the
   fleet lease, or a documented statement of what happens without one.

Each phase ships with its docs, as the decision hook's did.

## The question this record does not settle

**Whether `state.Provider` grows a lease.** Section 5's rotation race is real,
and the three candidate answers are not equivalent: a lease primitive on the
interface (correct, and a change to a two-implementation interface that every
other feature then inherits); routing all refreshes for one key to one
instance (no interface change, but fold has no request affinity and would be
inventing one); or accepting last-writer-wins and relying on delete-on-refusal
to self-heal (cheapest, and it converts a rare race into a rare re-consent
prompt — which is a bad experience but not a security failure).

The third is very likely right for v1 and the first is very likely right
eventually. Which one ships should be decided by someone holding a provider's
actual rotation behaviour, not in advance — the same reason the decision
hook's record left its demand-versus-decide question open.
