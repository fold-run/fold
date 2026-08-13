# Design: MCP Apps through a federating gateway

Status: **proposed, nothing shipped.** This records what the
[MCP Apps extension](https://modelcontextprotocol.io/extensions/apps/overview)
(SEP-1865, specification dated 2026-01-26) asks of an intermediary, which of
its assumptions federation breaks, and how much of the gap is fold's to close.

## Motivation

An app-enabled upstream advertises tools that carry a pointer to an HTML
interface; the host fetches that interface, sandboxes it in an iframe, and
renders it in the conversation instead of a wall of text. Claude, Claude
Desktop, VS Code Copilot, Microsoft 365 Copilot and Goose ship support today,
so this is not a future the gateway can wait out.

fold has no awareness of any of it. `grep -rn 'ui://\|_meta.ui\|io.modelcontextprotocol/ui'`
finds nothing. What works today works by accident, because an app rides on
primitives fold already proxies — and what does not work fails silently, in
the direction fold's central rule forbids: the same upstream renders an
interface when a host connects to it directly and returns plain text when the
host connects through fold. The gateway is supposed to be invisible. Here it
is visible as a downgrade, and nothing in the trail says so.

## What the extension actually is, on the wire

Two halves, and only one of them is fold's problem.

The **host↔app half** is a JSON-RPC dialect (`ui/initialize`,
`ui/notifications/tool-result`, `ui/open-link`, …) spoken over `postMessage`
between the host page and the sandboxed iframe. It never touches the gateway,
and nothing below concerns it.

The **client↔server half** is ordinary MCP, and it is entirely metadata:

| Surface | Carries |
|---|---|
| `initialize` client capabilities | `extensions["io.modelcontextprotocol/ui"] = {mimeTypes: ["text/html;profile=mcp-app"]}` |
| tool `_meta.ui` | `resourceUri` (a `ui://` URI), `visibility` (subset of `["model","app"]`, default both) |
| `resources/read` of that URI | the HTML, `mimeType: "text/html;profile=mcp-app"`, plus `_meta.ui` with `csp`, `permissions`, `domain` |

Three properties of that half matter here, all of them stated by the spec:

1. **It is client-declared.** Servers SHOULD check client capabilities before
   registering UI-enabled tools, and SHOULD offer a text-only fallback. There
   is no reciprocal server capability — a server that hears nothing assumes no.
2. **UI resources need not be listed.** Servers MAY omit them from
   `resources/list`; discovery is meant to happen through tool metadata, and
   the URI need only be unique *within one server*.
3. **`visibility` is enforced by the host.** A tool without `"model"` must be
   kept out of the agent's tool list; a tool without `"app"` must not be
   callable by an app; and cross-server calls are blocked for app-only tools.

## What already works, unintentionally

- `_meta.ui` survives namespacing. `namespacedTools` shallow-copies the tool
  and rewrites only `Name` (`gateway/upstream.go:1040`), so `resourceUri` and
  `visibility` reach the host intact. Tool results pass through verbatim.
- `ui://` reads resolve, by the slow path. Because UI resources may be
  unlisted, `resourceOwner` usually has no record of them, and `readResource`
  falls through to the probe loop that tries every policy-allowed upstream in
  turn (`gateway/router.go:509`). It finds the right one. It costs one round
  trip per upstream, on the render path, the first time each app is opened.
- Passthrough mode (one upstream, no namespace) is fine but for §1 below.

That is the whole of it, and none of it was designed.

---

## 1. fold tells every upstream it cannot render apps

The root session — the shared session that serves every `tools/list`,
`resources/read` and subscription — connects with client options carrying only
notification handlers and no `Capabilities` (`gateway/upstream.go:633`).
Bridged sessions declare `&mcp.ClientCapabilities{}` and add sampling and
elicitation as handlers are installed (`gateway/gateway.go:1128`). The
endpoint health probe declares nothing either (`gateway/upstream.go:592`).

So an upstream that follows the spec's advice registers its text-only
fallbacks and fold serves those, to every client, forever. The SDK already
models the field (`ClientCapabilities.Extensions`, `AddExtension`, v1.7.0);
this is wiring, not a dependency.

**The tension is that the declaration is per-session and the session is
shared.** One root session per upstream backs every downstream client, so
whatever fold declares upward is a statement about the federation, not about
the caller in hand. That is not a reason to declare nothing — it is a reason to
split the problem in two:

**Upward: declare, per upstream, on by default.** A new field on
`config.Upstream` alongside `protocol` (`config/config.go:167`), which is the
existing home for "what era of handshake this connection negotiates":

```json
{ "id": "analytics", "url": "...", "apps": "on" }
```

`"on"` (default) declares `io.modelcontextprotocol/ui` with the MVP mime type;
`"off"` suppresses it for an upstream whose UI variant an operator does not
want in the federation at all. Default-on is the unusual choice for this repo,
where defaults are frozen and features arrive off — and it is right here
because this is not a new control. It restores what a direct connection would
have produced, and the invisibility rule is the one thing fold does not make
opt-in.

**Downward: shape the list for the client in hand.** For a downstream client
whose `initialize` did *not* declare the extension (reachable from
`ss.InitializeParams()`, as the bridge already does at `gateway/gateway.go:1141`):

- drop tools whose `_meta.ui.visibility` omits `"model"` — an app-only tool
  handed to a host that has never heard of the rule ends up in the model's
  tool list, which is precisely what the spec forbids and what that host
  cannot know to prevent;
- strip `_meta.ui` from the rest, so the client sees the tool it would have
  seen had it asked the upstream itself.

That pairing is what makes default-on safe: fold asks for the richer surface
and then hands each caller only as much of it as that caller declared it can
handle. It also means fold enforces the one `visibility` rule it is positioned
to enforce, for the hosts least equipped to.

**Cost.** Two memoized public views per upstream instead of one — app-aware
and plain — both built when the list cache refills, keyed the same way
`publicView` already is (`gateway/upstream.go:1040`). No per-request copying,
nothing new on the proxy path, and the shared-and-read-only invariant on
cached items is preserved because both variants are built at fill time.

## 2. `ui://` URIs are unique per server, and fold indexes them per URI

`resourceOwner` is a flat URI→upstream map (`gateway/gateway.go:145`), which is
correct for real resource URIs: they carry a scheme and an authority, and
collisions across upstreams are a curiosity. `ui://` is different. The spec
requires uniqueness only within a server, the convention in the published
examples is `ui://{server-name}/{view}`, and that convention is a naming habit
rather than a rule. Two upstreams shipping `ui://app/main` — a plausible
result of two teams starting from the same template — collide, and the loser
is silent: the last upstream to list the URI wins the map
(`gateway/router.go:408`), and the host renders one vendor's interface for the
other vendor's tool.

Two fixes, at different prices.

**Now: harvest ownership from the tool metadata.** When `listTools` merges a
list, every `_meta.ui.resourceUri` it passes is an upstream telling fold
exactly which upstream owns that URI. Record it the way `listResources`
records ownership, including for items policy filtered out, since the read
path re-checks policy and tenancy anyway. This costs nothing on the request
path, removes the N-probe first render from §"What already works", and makes
the collision *visible*: two different upstreams claiming one URI in a single
merge is a fact fold now holds, and it becomes an audit event
(`upstream/uiResourceConflict`) plus a log line rather than a silent
overwrite. Last-writer-wins remains the behaviour; the operator learns that it
happened.

**Later, only if collisions prove real: mint the URI.** Rewrite
`_meta.ui.resourceUri` to a fold-scoped form on egress and reverse it on
`resources/read`. This is exact, and it is a deliberate exception to "resource
URIs are never rewritten" that has to be argued rather than assumed: the rule
protects opaque identifiers that *clients persist*, and a `ui://` URI in tool
metadata is regenerated on every list. The reason it does not ship first is
reach — the same URI can appear in embedded resources inside tool results, and
chasing it there means inspecting and rewriting response bodies, which is the
thing fold does not do. Ship the index, watch for the conflict event, and let
evidence decide.

## 3. An app that hardcodes a tool name cannot call it through fold

An app calls its server's tools by name. Through fold the tool is
`{namespace}__{name}`, and `resolve` answers a bare name with `-32043`
(`gateway/router.go:177`). There is nothing to rewrite: the name lives inside
the app's HTML bundle, which fold serves as opaque bytes.

The severity depends on how the app learned the name, and the spec makes both
kinds possible. `ui/initialize` returns `hostContext.toolInfo.tool` — the full
`Tool` object for the invocation that opened the app, which through fold
carries the namespaced name — so an app that calls `toolInfo.tool.name` works
unmodified. An app with `callTool("get_data")` written into it does not.

It also depends on host behaviour that is not specified: a host that validates
an app's requested tool name against the tool list it holds will reject the
call before fold ever sees it, and a host that forwards it verbatim produces a
`-32043` in fold's audit trail. Those are different bug reports.

**Recommendation: do not build a fix blind.** The obvious one — accept an
unambiguous bare name as a fallback resolution — weakens the namespace
contract for every caller, not just apps, and buys nothing if the host rejects
the call first. Stand a real app-enabled upstream behind fold, point Claude
Desktop at it, and read what actually crosses the wire. Then either document
the limitation or take it to the ext-apps repository, where the shape of the
answer is a way for an app to learn the name its host knows the tool by. A
federating gateway is a case the MVP did not consider, and the report is worth
more than a local patch.

## 4. Federation collapses the host's cross-server app boundary

The spec has the host block cross-server tool calls for app-only tools, so an
app from vendor A cannot reach vendor B's app-only surface. Behind fold, A and
B are one server connection. That boundary is gone, and fold cannot rebuild
it: the spec defines no marker on `tools/call` saying a call came from an app
rather than from the model, so the gateway sees two identical requests and has
nothing to decide with.

What remains is what fold already has, and it is not nothing: policy is
deny-by-default per principal and per upstream, and an app calling across
upstreams is only as dangerous as the grants that principal holds. What fold
should add is visibility rather than a new control — `_meta.ui.visibility`
recorded on the tool in `/api/federation` and in the console, so an operator
can see which tools in the federation are app-only and reason about who can
reach them.

The real fix is an origin marker in the spec. That is the second thing worth
filing upstream, and it is the more consequential of the two: every gateway,
proxy and aggregator in the ecosystem has this hole, and none of them can
close it alone.

## What stays out

- **Rendering, hosting, or inspecting app HTML.** fold proxies bytes. The
  sandbox is the host's, and reading the HTML would be content inspection,
  which is [declined](roadmap.md#non-goals).
- **Enforcing CSP or permissions from `_meta.ui`.** Host-side controls;
  a gateway that policed them would be enforcing half a boundary.
- **The `ui/*` postMessage dialect.** Not on the wire fold speaks.
- **Rewriting `ui://` URIs inside tool results.** §2.
- **A bare-name fallback for app-initiated calls.** §3, pending evidence.

## Compatibility

Additive. One new per-upstream enum field, which `Reload` already treats as a
rebuild trigger because upstream reuse compares the whole config struct
(`gateway/gateway.go:452`); one new audit method string, which the wire-surface
freeze permits; no change to any existing default other than the deliberate
default-on in §1, whose effect on a client that did not ask for apps is
neutralised by the egress shaping in the same section. Nothing new on the
proxy path. The conformance suite is unaffected — the extension is not part of
the core specification it checks.

Schema lockstep applies: `config/fold.config.schema.json`, `fold.config.example.json`, the
README config table, and `docs/defaults.md` move with the field.

## Implementation phases

1. **Parity** — declare the extension upward per upstream, shape tools
   downward by the client's declared capabilities, both public views built at
   list-fill time. Integration tests with a real SDK upstream that registers
   UI-enabled tools only when the client declares the extension, and a second
   downstream client that declares nothing and must see neither `_meta.ui` nor
   the app-only tool. This phase is the feature; the rest is refinement.
2. **Routing** — harvest `resourceUri` ownership during `listTools`, conflict
   event and metric, a test with two upstreams claiming one `ui://` URI, and a
   test that an unlisted UI resource reads in one round trip after a list.
3. **Visibility** — `_meta.ui.visibility` surfaced in `/api/federation` and the
   console.
4. **Upstream** — the two spec reports from §3 and §4, and whatever they
   return with. Neither is a fold feature until the protocol has an answer.
