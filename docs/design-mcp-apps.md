# Design: MCP Apps through a federating gateway

Status: **§2 (`ui://` routing) shipped; §1 (capability parity) designed and
unbuilt**, with every premise below verified on the wire 2026-08-14 — see
"What the wire says". Routing went first because it was the defect a
federation hits today, while parity is insurance against a spec SHOULD that no
shipping server takes up yet. This records what the
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

## What the wire says

Everything below was measured on 2026-08-14 rather than reasoned about, with
the real peers: the `basic-server-vanillajs` example from
[ext-apps](https://github.com/modelcontextprotocol/ext-apps) (`@modelcontextprotocol/sdk`
1.29, `@modelcontextprotocol/ext-apps` 1.7.5) run twice as two upstreams,
plus a third stateful server gating on the SDK's own `getUiCapability()`
helper, all three behind fold v1.13.1 with namespaces `alpha`, `beta`,
`gated`.

**The downgrade is real, and it is fold's declaration that causes it.** An
app-aware client declared the extension to fold; the gated upstream logged
`initialize from {"name":"fold-gateway"} extensions=null → TEXT-ONLY
fallback` and fold served the caller a `gated__get-time` with no `_meta.ui` at
all, in the same list as two app-enabled tools from the ungated upstreams. The
host asked for apps, the upstream offered them, and the gateway in between
lost the question.

**Nothing shipping today gates yet.** No example server in the ext-apps
repository calls `getUiCapability`; they register UI tools unconditionally,
which is why apps work through fold at all right now. The gating fixture above
had to be written for this experiment — and writing it surfaced that the
SDK's own documented pattern (register inside `oninitialized`) throws
`registerCapabilities` after connect unless some tool is already registered.
So the failure mode is a live risk on a spec SHOULD, not yet a widespread
outage. That is an argument about urgency, not about correctness.

**The collision is not hypothetical.** The shipped template names its resource
`ui://get-time/mcp-app.html` — no server segment, exactly as the extension
permits. Two upstreams built from it therefore advertise one URI, and
`resources/list` through fold returns the same URI twice with nothing to tell
the entries apart. Worse, ownership is **history-dependent**: with a cold
gateway, `resources/read` of that URI was answered by `alpha` (probe order);
after any client called `resources/list`, the same read was answered by `beta`
(last writer into the ownership map). A host rendering `alpha__get-time` gets
beta's interface, and which one it gets depends on what some other client did
first.

**An app-initiated call fails at the gateway, not at the host.** The reference
`AppBridge` installs `oncalltool` as a verbatim forward of `tools/call` to the
server — no name validation, no visibility check. Replaying what it would
send, `{"name":"get-time"}` through fold returns
`-32043 unknown name "get-time": no upstream owns this namespace`, while
`alpha__get-time` succeeds. So §3 is a real break with a fold-shaped error on
it. Two mitigating facts: the same bridge exposes `tools/list` to the app, so
an app that resolves names dynamically can adapt, and `ui/initialize` hands
the app the full `Tool` — namespaced — for the invocation that opened it.
Neither helps the app that hardcodes a name.

That bridge also does *not* enforce the spec's "reject app calls to tools
without `app` visibility" rule; the reference host filters only the model's
list. Which is the §4 hole seen from the other side: the enforcement the
gateway cannot do is not reliably done above it either.

**One finding outside apps.** The upstream advertises
`"execution":{"taskSupport":"forbidden"}` on its tool; through fold the field
is gone, because `mcp.Tool` in go-sdk v1.7.0 does not model it and the decode/
encode round trip drops what it does not know. That is an invisibility
deviation on every upstream that declares task support, unrelated to this
design, and it belongs in its own report against the SDK.

### Reproducing

```bash
git clone --depth 1 https://github.com/modelcontextprotocol/ext-apps
cd ext-apps/examples/basic-server-vanillajs && npm install --no-workspaces
INPUT=mcp-app.html npx vite build          # builds dist/mcp-app.html
PORT=3001 bun main.ts                      # upstream "alpha"
# copy the directory, mark its dist/mcp-app.html, run on 3002 as "beta"
# fold with both as namespaced upstreams, then compare tools/list and a
# resources/read of ui://get-time/mcp-app.html before and after resources/list
```

---

## 1. fold tells every upstream it cannot render apps

The root session — the shared session that serves every `tools/list`,
`resources/read` and subscription — connects with client options carrying only
notification handlers and no `Capabilities` (`gateway/upstream.go:633`).
Bridged sessions declare `&mcp.ClientCapabilities{}` and add sampling and
elicitation as handlers are installed (`gateway/gateway.go:1128`). The
endpoint health probe declares nothing either (`gateway/upstream.go:592`).

So an upstream that follows the spec's advice registers its text-only
fallbacks and fold serves those, to every client, forever — measured, not
inferred: a gated upstream behind fold logged `extensions=null` and served the
fallback to a host that had declared the extension to fold one hop earlier.
The SDK already models the field (`ClientCapabilities.Extensions`,
`AddExtension`, v1.7.0); this is wiring, not a dependency.

**The answer is to proxy the declaration, not to configure it.** fold's job
here is the one it does everywhere else — carry what the client said to the
upstream and carry the answer back. The only reason that is not a one-line
change is the shape of the sessions.

**Calls are already per-client, so they are already answerable.** A named
invocation rides the bridged session (`gateway/router.go:324`), and
`bridgeOptions` already mirrors what the downstream client declared —
selectively, one capability at a time. Carrying the client's `extensions`
across is a few lines there and needs no design.

**Lists are the hard part, and only because the root session is shared.** One
root session per upstream serves every client's `tools/list`, and capabilities
are negotiated once per session, at initialize. There is no per-client answer
available to proxy: whatever fold declares on that session is a claim made on
behalf of every caller at once. An earlier draft of this document resolved that
with a per-upstream config field and compensating egress filters — declare
apps upward for everyone, then strip `_meta.ui` and app-only tools back out for
clients that had not asked. That works, and it is the wrong shape: it makes an
operator answer a question the protocol already answers, and it spends
egress-path work undoing a claim fold chose to make.

**Key the root session by a capability profile instead.** Normalize each
downstream client's declared extensions into a profile, and keep one root
session — and one list cache entry — per profile per upstream. Today the
profile space has two members, app-aware and plain, and every client falls into
one of them. A federation whose clients are all one kind pays exactly what it
pays now: one session, one cached list. A mixed one pays two, and each client
gets the list the upstream would have handed it directly.

That is the property worth the machinery. Parity stops being something fold
arranges and becomes something it cannot avoid: fold no longer needs to strip
`_meta.ui`, or to filter app-only tools out of a naive host's list, because a
client that did not declare the extension is talking to a session that did not
declare it either, and the upstream itself decided what to register. The
invisibility rule is satisfied by construction rather than by compensation.

Two constraints decide whether it holds:

- **The profile is fold's, not the client's.** It must be computed from the
  extension identifiers fold recognizes — today exactly one — and never from
  the raw map the client sent. Keying on client-supplied data would let one
  caller declaring a thousand invented extension ids mint a thousand sessions
  and cache entries per upstream: the failure `internal/bounded` exists to
  prevent, in a place where the blast radius is upstream connections rather
  than memory.
- **The profile belongs in the `ListCache` key.** That cache is Redis-backed
  and shared across a fleet, so a profile-blind key would let one pod's
  app-aware list be served to another pod's plain client — the same bug as the
  local one, arriving from a different instance.

**Cost, and where the risk moved.** An upstream's `session` field becomes a
small keyed set, so everything that touches root-session lifecycle has to
follow: endpoint pinning and failover (`gateway/endpoints.go`), the drain on
reload, and the subscription state that today lives on the one session.
Subscriptions are per-upstream rather than per-client, so they should stay on
the first profile session opened rather than being duplicated — a rule worth
writing into the code, since the alternative is duplicate
`notifications/resources/updated` fan-out. This is more implementation surface
than a config field, and that is the honest trade: the knob was cheaper because
it was willing to be wrong in two directions.

**No config field.** Not even an opt-out. An operator who does not want an
upstream's app variants in the federation is expressing a policy, and fold has
a policy engine; adding a second, weaker way to say it — one that works by
lying to the upstream about what the client is — would be a control with the
wrong name. If the demand turns out to be real, it arrives as a policy
predicate, not as a handshake setting.

## 2. `ui://` URIs are unique per server, and fold indexes them per URI

`resourceOwner` is a flat URI→upstream map (`gateway/gateway.go:145`), which is
correct for real resource URIs: they carry a scheme and an authority, and
collisions across upstreams are a curiosity. `ui://` is different. The spec
requires uniqueness only within a server, and the shipped template does not
even follow the `ui://{server-name}/{view}` convention the published examples
describe — it names its resource `ui://get-time/mcp-app.html`, so two teams
starting from that template collide on first contact. The loser is silent: the
last upstream to list the URI wins the map (`gateway/router.go:408`), and the
host renders one vendor's interface for the other vendor's tool. Reproduced
above, with the additional wrinkle that the winner depends on request history
— a cold gateway answers from the probe order, a warmed one from the list.

**Shipped: mint the URI** — `gateway/uiresource.go`. An upstream's `ui://X`
is republished to clients as `ui://fold/{namespace}/X`, and resolved back to
`(upstream, X)` on the way in. The rewrite covers `_meta.ui.resourceUri` and
the deprecated flat `_meta["ui/resourceUri"]` on `tools/list`, the entries in
`resources/list`, the URIs a `resources/read` answers with, and
`resources/updated` for a subscribed interface. Reads resolve from the URI
itself, so the collision is gone, the probe is gone, and the answer no longer
depends on which lists a client fetched first.

An earlier draft of this section proposed a weaker fix first — harvest
ownership from the tool metadata, keep last-writer-wins, and emit a conflict
event — with minting held back as the "later, if collisions prove real"
option. Two things retired that ordering. Collisions *are* real, on the
published template, which the experiment above demonstrated rather than
predicted. And harvesting does not fix the defect it exposes: a read still
arrives carrying nothing but the URI, so fold would still have to pick one of
the two upstreams, and one of the two apps would still render the wrong
interface — logged this time, which is not the same as working.

This is a deliberate exception to "resource URIs are never rewritten", and it
is argued rather than assumed. The rule protects identifiers *clients persist*;
a `ui://` pointer is republished on every `tools/list`, so nothing a client
stored goes stale. The exception is kept narrow by construction: only the
`ui://` scheme is touched, the minted form derives from the namespace the
operator already chose (so it is stable across restarts, reloads, and
instances), policy and audit still see the URI the upstream published, and a
passthrough upstream is not rewritten at all.

What it deliberately does not chase is a `ui://` URI embedded in a *tool
result*. Following it there means inspecting and rewriting response bodies,
which fold does not do; the MVP fetches an interface from `_meta.ui` rather
than from result content, so this costs nothing today and is written down in
case that changes. Resource *templates* are left alone for the same reason —
no template in the wild is a `ui://` one, and the read path has no template
form to resolve back.

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

The reference host does not rescue it. `AppBridge` forwards an app's
`tools/call` to the server verbatim, so the failure surfaces as fold's
`-32043` rather than as a host-side rejection — measured above.

**Recommendation: still do not build a local fix.** The obvious one — accept
an unambiguous bare name as a fallback resolution — weakens the namespace
contract for every caller to rescue apps that hardcode names, and it would
hand an app whatever tool happens to own that name across the whole
federation, which is the cross-upstream reach §4 is already uncomfortable
about. The answer belongs in the extension: a way for an app to learn the name
its host knows the tool by. A federating gateway is a case the MVP did not
consider, and the report is worth more than a patch. What fold can do
meanwhile is make the failure legible — a `-32043` whose message says the name
looks unnamespaced, which is a message change rather than a routing change.

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
close it alone. The reference host is the reason to file it rather than assume
the layer above holds: its bridge forwards an app's `tools/call` without
checking `visibility` at all, so today the rule the gateway cannot enforce is
not being enforced above the gateway either.

## What stays out

- **Rendering, hosting, or inspecting app HTML.** fold proxies bytes. The
  sandbox is the host's, and reading the HTML would be content inspection,
  which is [declined](roadmap.md#non-goals).
- **Enforcing CSP or permissions from `_meta.ui`.** Host-side controls;
  a gateway that policed them would be enforcing half a boundary.
- **The `ui/*` postMessage dialect.** Not on the wire fold speaks.
- **Rewriting `ui://` URIs inside tool results.** §2.
- **A bare-name fallback for app-initiated calls.** §3.
- **A config field for any of it**, opt-out included. §1.
- **Egress filtering of `_meta.ui` and app-only tools.** Needed only by the
  design §1 replaced; with profile-keyed sessions there is nothing to undo.

## Compatibility

Additive, and — unusually for a feature this size — with no config surface at
all: no new field, so no schema, example, defaults, or README table to keep in
lockstep, no new default to freeze, and no new error code or audit method.
Nothing new on the proxy path: §2's rewrite happens where the list cache
refills, memoized like the namespaced views beside it, and §1's profile is
resolved once per session rather than per request. The conformance suite is
unaffected — the extension is not part of the core specification it checks, no
fixture in it publishes a `ui://` resource, and a single-profile federation
makes exactly the connections it makes today.

The one wire-visible change from §2: a client that persisted a `ui://` URI
fold minted under a *different namespace* for the same upstream — an operator
renamed it — must re-read the tool list, exactly as it must for the tool name
that moved with it.

The one behaviour change to state plainly: an upstream may now see two client
sessions where it saw one, and may be asked for its tool list twice. Upstreams
that gate on capabilities are the reason, and they are the only ones that will
answer differently.

## Implementation phases

0. **Verification** — **done**, 2026-08-14; results in "What the wire says",
   and they moved three of the sections above.
1. **Parity** — the bridged-session half first, since it is self-contained:
   carry the client's declared extensions onto the per-client session. Then the
   root session becomes profile-keyed, with the profile in the `ListCache` key,
   subscriptions pinned to the first session opened, and endpoint pinning and
   reload drain following the set rather than the single field. Integration
   tests with a real SDK upstream that registers UI-enabled tools only when the
   client declares the extension, and two downstream clients — one app-aware,
   one not — that must each get their own answer from it, concurrently and
   across a reload. The Go SDK peers the tests already use can gate directly on
   `InitializeParams().Capabilities.Extensions`, so the fixture does not
   inherit the TypeScript SDK's registration-order trap. This phase is the
   feature; the rest is refinement.
2. **Routing** — **shipped** ahead of phase 1, because it was the live defect
   and phase 1 is insurance. Minting on every egress surface, resolution on
   read, subscribe, unsubscribe and the orphan sweep, and integration tests
   with two SDK upstreams that collide on one `ui://` URI. Verified against
   the two TypeScript app servers from phase 0 as well as the Go fixtures:
   each tool's interface now reads from its own upstream, cold or warm.
3. **Visibility** — `_meta.ui.visibility` surfaced in `/api/federation` and the
   console.
4. **Upstream** — the two spec reports from §3 and §4, and whatever they
   return with. Neither is a fold feature until the protocol has an answer.
