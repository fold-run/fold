// fold console. Read-only dashboard + MCP test console.
//
// The test console is a minimal streamable-HTTP MCP client pointed at the
// gateway's own MCP endpoint: initialize → notifications/initialized →
// tools/list → tools/call. Responses arrive as plain JSON or as an SSE
// stream on the POST response; both are handled below.
//
// Security notes: the token lives in this page's memory only (never
// storage); everything received from the gateway — tool names,
// descriptions, results — is rendered with textContent, never markup,
// because upstream-controlled strings are untrusted here.
"use strict";

const $ = (id) => document.getElementById(id);

const S = {
  token: "",
  mcpPath: "/mcp",
  version: "dev", // replaced by the gateway's stamped version on first state fetch
  sessionId: null,
  protocolVersion: null,
  nextId: 0,
  mode: "tools", // "tools" | "prompts" | "resources"
  lists: { tools: [], prompts: [], resources: [] },
};

const MODES = {
  tools: { list: "tools/list", listKey: "tools", call: "tools/call", verb: "Call tool", args: true },
  prompts: { list: "prompts/list", listKey: "prompts", call: "prompts/get", verb: "Get prompt", args: true },
  resources: { list: "resources/list", listKey: "resources", call: "resources/read", verb: "Read resource", args: false },
};

// ---------- OAuth (Authorization Code + PKCE) ----------
//
// The console is a public client: no secret exists, the PKCE verifier is
// the proof of possession. The access token lives in page memory only; the
// verifier (not a credential by itself) sits in sessionStorage exactly for
// the duration of the redirect round-trip and is removed on return.

const PKCE_KEY = "fold-console-pkce";

function b64url(bytes) {
  return btoa(String.fromCharCode(...bytes)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

async function loadAuthHint() {
  try {
    const res = await fetch("api/auth");
    if (res.ok) S.authHint = await res.json();
  } catch { /* hint is optional; paste-token still works */ }
  const oauth = S.authHint && S.authHint.oauth;
  $("signin").hidden = !oauth || !!S.token;
  if (oauth) $("token").placeholder = "…or paste a Bearer token";
}

// discoverAS resolves the authorization server metadata: OIDC discovery
// first, then RFC 8414's inserted-path form for plain OAuth ASes.
async function discoverAS(issuer) {
  const iss = issuer.replace(/\/$/, "");
  const u = new URL(iss);
  const candidates = [
    iss + "/.well-known/openid-configuration",
    u.origin + "/.well-known/oauth-authorization-server" + u.pathname.replace(/\/$/, ""),
  ];
  for (const c of candidates) {
    try {
      const res = await fetch(c);
      if (!res.ok) continue;
      const meta = await res.json();
      if (meta.authorization_endpoint && meta.token_endpoint) return meta;
    } catch { /* try the next form */ }
  }
  throw new Error("could not discover authorization server metadata for " + issuer);
}

async function signIn() {
  banner("");
  try {
    const oauth = S.authHint.oauth;
    const meta = await discoverAS(oauth.issuer);
    const verifier = b64url(crypto.getRandomValues(new Uint8Array(32)));
    const state = b64url(crypto.getRandomValues(new Uint8Array(16)));
    const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier));
    sessionStorage.setItem(PKCE_KEY, JSON.stringify({ verifier, state, tokenEndpoint: meta.token_endpoint }));
    const authz = new URL(meta.authorization_endpoint);
    authz.searchParams.set("response_type", "code");
    authz.searchParams.set("client_id", oauth.clientId);
    authz.searchParams.set("redirect_uri", location.origin + location.pathname);
    authz.searchParams.set("code_challenge", b64url(new Uint8Array(digest)));
    authz.searchParams.set("code_challenge_method", "S256");
    authz.searchParams.set("state", state);
    if (oauth.scopes && oauth.scopes.length) authz.searchParams.set("scope", oauth.scopes.join(" "));
    if (S.authHint.resource) authz.searchParams.set("resource", S.authHint.resource); // RFC 8707
    location.assign(authz);
  } catch (err) {
    banner("sign-in failed: " + err.message);
  }
}

// handleCallback finishes the flow when the page loads with ?code=…: the
// code and state are stripped from the URL and history before anything
// else, the saved verifier is single-use, and the token never leaves memory.
async function handleCallback() {
  const q = new URLSearchParams(location.search);
  if (!q.has("code") && !q.has("error")) return;
  const savedRaw = sessionStorage.getItem(PKCE_KEY);
  sessionStorage.removeItem(PKCE_KEY);
  history.replaceState(null, "", location.pathname);
  if (q.has("error")) {
    banner("sign-in failed: " + q.get("error") + (q.get("error_description") ? ": " + q.get("error_description") : ""));
    return;
  }
  let saved = null;
  try { saved = JSON.parse(savedRaw); } catch { /* treated as missing */ }
  if (!saved || !saved.state || saved.state !== q.get("state")) {
    banner("sign-in failed: state mismatch — start again from this page.");
    return;
  }
  const body = new URLSearchParams({
    grant_type: "authorization_code",
    code: q.get("code"),
    redirect_uri: location.origin + location.pathname,
    client_id: S.authHint.oauth.clientId,
    code_verifier: saved.verifier,
  });
  if (S.authHint.resource) body.set("resource", S.authHint.resource);
  try {
    const res = await fetch(saved.tokenEndpoint, {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body,
    });
    const tok = await res.json();
    if (!res.ok || !tok.access_token) {
      banner("token exchange failed: " + (tok.error || "HTTP " + res.status) + (tok.error_description ? ": " + tok.error_description : ""));
      return;
    }
    S.token = tok.access_token;
    $("signin").hidden = false;
    $("signin").textContent = "Signed in ✓";
    $("signin").disabled = true;
    $("token").hidden = true;
  } catch (err) {
    banner("token exchange failed: " + err.message);
  }
}

// ---------- dashboard ----------

function authHeaders() {
  const h = {};
  if (S.token) h["Authorization"] = "Bearer " + S.token;
  return h;
}

function card(label, value, cls) {
  const div = document.createElement("div");
  div.className = "card";
  const l = document.createElement("span");
  l.className = "label";
  l.textContent = label;
  const b = document.createElement("b");
  b.textContent = value;
  if (cls) b.className = cls;
  div.append(l, b);
  return div;
}

function banner(msg) {
  const el = $("banner");
  el.hidden = !msg;
  el.textContent = msg || "";
}

async function refreshState() {
  banner("");
  let res;
  try {
    // Relative to this page's base (/console/), so it survives any prefix
    // a fronting proxy adds.
    res = await fetch("api/state", { headers: authHeaders() });
  } catch (err) {
    banner("state fetch failed: " + err.message);
    return;
  }
  if (res.status === 401) {
    banner("Unauthorized — paste a valid Bearer token above (the same token /mcp accepts).");
    // Stop the poll loop: retrying without a token every 15 s only
    // generates 401 audit events. The token change handler restarts it.
    stopPolling();
    return;
  }
  if (res.status === 403) {
    banner("This principal is not in the console viewer allowlist (server.console.groups). The denial was audited.");
    stopPolling();
    return;
  }
  if (!res.ok) {
    banner("state fetch failed: HTTP " + res.status);
    return;
  }
  const st = await res.json();
  S.mcpPath = st.mcpPath || "/mcp";
  S.version = st.version || "dev";
  $("version").textContent = /^\d/.test(st.version) ? "v" + st.version : st.version;

  // Auth off means no token will ever be needed; hide the input instead of
  // leaving a field that does nothing. Signed-in sessions don't need it
  // either.
  $("token").hidden = !st.authRequired || $("signin").disabled;

  const summary = $("summary");
  summary.replaceChildren(
    card("endpoint", S.mcpPath),
    card("auth", (st.authRequired ? "required" : "disabled") + (st.emaEnabled ? " + EMA" : "")),
    card("policy", st.policyDefaultDecision + ", " + st.policyRules + " rule(s)"),
    card("upstreams", String(st.staticUpstreams) + " static + " + String(st.discoveredUpstreams) + " discovered"),
    card("state", st.sharedState ? "shared (Redis)" : "in-process"),
    card("audit", (st.auditSinks || []).join(" + ") || "none"),
    card("routing", "sep " + st.namespaceSeparator + " · " + (st.pageSize ? "page size " + st.pageSize : "no pagination")),
  );
  if (st.tracingEnabled) summary.append(card("tracing", "OTLP"));
  if (st.viewerGroups && st.viewerGroups.length) summary.append(card("viewers", st.viewerGroups.join(", ")));
  if (st.passthrough) summary.append(card("mode", "passthrough"));
  if (st.globalRequestsPerMinute) {
    const per = st.perPrincipalRequestsPerMinute ? " · " + st.perPrincipalRequestsPerMinute + "/min per principal" : "";
    summary.append(card("rate limit", st.globalRequestsPerMinute + "/min global" + per));
  }

  const tbody = $("upstreams").querySelector("tbody");
  tbody.replaceChildren();
  for (const u of st.upstreams || []) {
    const tr = document.createElement("tr");

    const breakerCls = { closed: "ok", open: "bad" }[u.breaker] || "warn";
    const cells = [
      { v: u.id },
      { v: u.namespace || "—" },
      { v: u.source || "—" },
      { v: u.authStrategy || "—" },
      { v: u.connected ? "yes" : "no", cls: u.connected ? "ok" : "bad" },
      { v: u.breaker || "—", cls: u.breaker === "closed" ? "" : breakerCls },
      { v: u.connected ? u.latencyMs + " ms" : (u.error || "—"), cls: u.connected ? "" : "err" },
      { v: (u.endpoints || []).map((e) => (e.url || "endpoint") + (e.healthy ? " ✓" : " ✗")).join(", ") || (u.url || "—") },
      { v: u.owner ? [u.owner.org, u.owner.team].filter(Boolean).join(" / ") : "—" },
    ];
    for (const c of cells) {
      const td = document.createElement("td");
      td.textContent = String(c.v);
      if (c.cls) td.className = c.cls;
      tr.append(td);
    }
    const labelTd = document.createElement("td");
    const labels = Object.entries(u.labels || {});
    if (labels.length === 0) labelTd.textContent = "—";
    for (const [k, v] of labels) {
      const chip = document.createElement("span");
      chip.className = "chip";
      chip.textContent = k + "=" + v;
      labelTd.append(chip);
    }
    tr.append(labelTd);
    tbody.append(tr);
  }
  if (!(st.upstreams || []).length) {
    const tr = document.createElement("tr");
    const td = document.createElement("td");
    td.colSpan = 10;
    td.textContent = "No upstreams in the federation.";
    td.className = "err";
    tr.append(td);
    tbody.append(tr);
  }

  const disc = $("discovery");
  disc.replaceChildren();
  if (st.discovery) {
    const interval = st.discovery.intervalMs ? (st.discovery.intervalMs / 1000) + " s" : "30 s (default)";
    disc.append(card("discovery", st.discovery.url + " · every " + interval));
    if (st.discovery.lastOutcome) {
      disc.append(card(
        "last sync",
        st.discovery.lastOutcome + " at " + st.discovery.lastSyncAt,
        st.discovery.lastOutcome === "applied" || st.discovery.lastOutcome === "unchanged" ? "ok" : "bad",
      ));
    }
  }
}

// ---------- MCP client ----------

function wireLog(direction, obj) {
  const el = $("wire");
  el.textContent += direction + " " + JSON.stringify(obj, null, 2) + "\n\n";
  el.scrollTop = el.scrollHeight;
}

// One SSE-or-JSON POST exchange. Returns the response message matching id
// (null for notifications); other messages on the stream (server-initiated
// notifications, e.g. progress) go to the wire log.
async function rpc(method, params, { notification = false } = {}) {
  const msg = { jsonrpc: "2.0", method };
  if (params !== undefined) msg.params = params;
  if (!notification) msg.id = ++S.nextId;
  wireLog("→", msg);

  const headers = {
    "Content-Type": "application/json",
    "Accept": "application/json, text/event-stream",
    ...authHeaders(),
  };
  if (S.sessionId) headers["Mcp-Session-Id"] = S.sessionId;
  if (S.protocolVersion) headers["MCP-Protocol-Version"] = S.protocolVersion;

  const res = await fetch(S.mcpPath, { method: "POST", headers, body: JSON.stringify(msg) });
  const sid = res.headers.get("mcp-session-id");
  if (sid) S.sessionId = sid;

  if (res.status === 202) return null; // notification accepted
  if (!res.ok) {
    const body = await res.text();
    throw new Error("HTTP " + res.status + (body ? ": " + body.slice(0, 300) : ""));
  }

  const ct = res.headers.get("content-type") || "";
  let reply = null;
  if (ct.includes("text/event-stream")) {
    // The SDK ends the stream once the response is sent, so reading to the
    // end terminates. Events are blank-line separated; an event's payload
    // is its concatenated `data:` lines.
    const text = await res.text();
    for (const chunk of text.split(/\r?\n\r?\n/)) {
      const data = chunk
        .split(/\r?\n/)
        .filter((l) => l.startsWith("data:"))
        .map((l) => l.slice(5).replace(/^ /, ""))
        .join("\n");
      if (!data) continue;
      let m;
      try { m = JSON.parse(data); } catch { continue; }
      wireLog("←", m);
      if (!notification && m.id === msg.id) reply = m;
    }
  } else {
    reply = await res.json();
    wireLog("←", reply);
  }

  if (notification) return null;
  if (!reply) throw new Error("no response for request " + msg.id + " on the stream");
  if (reply.error) throw new Error("JSON-RPC " + reply.error.code + ": " + reply.error.message);
  return reply.result;
}

// fetchList pages through a list method until the cursor runs dry (bounded,
// so a misbehaving server cannot loop the page forever). A server without
// the capability answers method-not-found; treat that as an empty list.
async function fetchList(mode) {
  const m = MODES[mode];
  const items = [];
  let cursor;
  for (let page = 0; page < 25; page++) {
    let res;
    try {
      res = await rpc(m.list, cursor ? { cursor } : {});
    } catch (err) {
      if (/-32601/.test(err.message)) return [];
      throw err;
    }
    items.push(...(res[m.listKey] || []));
    cursor = res.nextCursor;
    if (!cursor) break;
  }
  return items;
}

async function connect() {
  S.sessionId = null;
  S.protocolVersion = null;
  S.nextId = 0;
  $("connect").disabled = true;
  $("mcpstatus").textContent = "connecting…";
  try {
    const init = await rpc("initialize", {
      protocolVersion: "2025-06-18",
      capabilities: {},
      clientInfo: { name: "fold-console", version: S.version || "dev" },
    });
    S.protocolVersion = init.protocolVersion;
    await rpc("notifications/initialized", undefined, { notification: true });
    for (const mode of Object.keys(MODES)) S.lists[mode] = await fetchList(mode);
    $("mcpstatus").textContent =
      S.lists.tools.length + " tools · " +
      S.lists.prompts.length + " prompts · " +
      S.lists.resources.length + " resources visible to this principal";
    for (const mode of Object.keys(MODES)) $("mode-" + mode).disabled = false;
    renderPicker();
  } catch (err) {
    $("mcpstatus").textContent = "";
    banner("MCP connect failed: " + err.message);
  } finally {
    $("connect").disabled = false;
  }
}

// itemKey returns the invocable identity: name for tools/prompts, uri for
// resources (URIs are opaque and never rewritten by the gateway).
function itemKey(mode, item) {
  return mode === "resources" ? item.uri : item.name;
}

function renderPicker() {
  const m = MODES[S.mode];
  const items = S.lists[S.mode];
  const sel = $("picker");
  sel.replaceChildren();
  for (const it of items) {
    const opt = document.createElement("option");
    opt.value = itemKey(S.mode, it);
    opt.textContent = itemKey(S.mode, it) + (S.mode === "resources" && it.name ? " (" + it.name + ")" : "");
    sel.append(opt);
  }
  const empty = items.length === 0;
  sel.disabled = empty;
  $("args").disabled = empty || !m.args;
  $("args").placeholder = m.args ? '{"argument": "value"}' : "resources/read takes no arguments — the URI is the identity";
  if (!m.args) $("args").value = "";
  $("call").disabled = empty;
  $("call").textContent = m.verb;
  showItemDesc();
}

function setMode(mode) {
  S.mode = mode;
  for (const m of Object.keys(MODES)) $("mode-" + m).classList.toggle("active", m === mode);
  renderPicker();
}

function showItemDesc() {
  const it = S.lists[S.mode].find((x) => itemKey(S.mode, x) === $("picker").value);
  $("itemdesc").textContent = it && it.description ? it.description : "";
}

async function invoke() {
  banner("");
  $("result").textContent = "";
  const m = MODES[S.mode];
  const key = $("picker").value;
  let params;
  if (S.mode === "resources") {
    params = { uri: key };
  } else {
    let args = {};
    const raw = $("args").value.trim();
    if (raw) {
      try { args = JSON.parse(raw); } catch (err) {
        banner("arguments are not valid JSON: " + err.message);
        return;
      }
    }
    params = { name: key, arguments: args };
  }
  $("call").disabled = true;
  try {
    const result = await rpc(m.call, params);
    $("result").textContent = JSON.stringify(result, null, 2);
  } catch (err) {
    $("result").textContent = err.message;
  } finally {
    $("call").disabled = false;
  }
}

// ---------- wiring ----------

let pollTimer = null;

function startPolling() {
  if (!pollTimer) pollTimer = setInterval(refreshState, 15000);
}

function stopPolling() {
  if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
}

$("token").addEventListener("change", () => {
  S.token = $("token").value.trim();
  startPolling();
  refreshState();
});
$("refresh").addEventListener("click", () => { startPolling(); refreshState(); });
$("connect").addEventListener("click", connect);
$("picker").addEventListener("change", showItemDesc);
$("call").addEventListener("click", invoke);
$("signin").addEventListener("click", signIn);
for (const mode of ["tools", "prompts", "resources"]) {
  $("mode-" + mode).addEventListener("click", () => setMode(mode));
}

(async () => {
  await loadAuthHint();
  await handleCallback();
  refreshState();
  startPolling();
})();
