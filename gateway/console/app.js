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
  sessionId: null,
  protocolVersion: null,
  nextId: 0,
  tools: [],
};

// ---------- dashboard ----------

function authHeaders() {
  const h = {};
  if (S.token) h["Authorization"] = "Bearer " + S.token;
  return h;
}

function card(label, value, cls) {
  const div = document.createElement("div");
  div.className = "card";
  const b = document.createElement("b");
  b.textContent = value;
  div.append(label + ": ", b);
  if (cls) b.className = cls;
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
  if (!res.ok) {
    banner("state fetch failed: HTTP " + res.status);
    return;
  }
  const st = await res.json();
  S.mcpPath = st.mcpPath || "/mcp";
  $("version").textContent = /^\d/.test(st.version) ? "v" + st.version : st.version;

  const summary = $("summary");
  summary.replaceChildren(
    card("auth", st.authRequired ? "required" : "disabled"),
    card("policy", st.policyDefaultDecision + " by default, " + st.policyRules + " rule(s)"),
    card("upstreams", String(st.staticUpstreams) + " static + " + String(st.discoveredUpstreams) + " discovered"),
  );
  if (st.passthrough) summary.append(card("mode", "passthrough"));
  if (st.globalRequestsPerMinute) summary.append(card("global limit", st.globalRequestsPerMinute + "/min"));

  const tbody = $("upstreams").querySelector("tbody");
  tbody.replaceChildren();
  for (const u of st.upstreams || []) {
    const tr = document.createElement("tr");
    const cells = [
      u.id,
      u.namespace || "—",
      u.connected ? "yes" : "no",
      u.breaker || "",
      u.connected ? u.latencyMs + " ms" : (u.error || "—"),
      (u.endpoints || []).map((e) => (e.url || "endpoint") + (e.healthy ? " ✓" : " ✗")).join(", ") || (u.url || "—"),
      u.owner ? [u.owner.org, u.owner.team].filter(Boolean).join(" / ") : "—",
    ];
    cells.forEach((v, i) => {
      const td = document.createElement("td");
      td.textContent = String(v);
      if (i === 2) td.className = u.connected ? "ok" : "bad";
      tr.append(td);
    });
    tbody.append(tr);
  }

  const disc = $("discovery");
  disc.replaceChildren();
  if (st.discovery) {
    disc.append(card("discovery", st.discovery.url));
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

async function connect() {
  S.sessionId = null;
  S.protocolVersion = null;
  S.nextId = 0;
  $("mcpstatus").textContent = "connecting…";
  try {
    const init = await rpc("initialize", {
      protocolVersion: "2025-06-18",
      capabilities: {},
      clientInfo: { name: "fold-console", version: "1" },
    });
    S.protocolVersion = init.protocolVersion;
    await rpc("notifications/initialized", undefined, { notification: true });
    const listed = await rpc("tools/list", {});
    S.tools = listed.tools || [];
    const sel = $("tools");
    sel.replaceChildren();
    for (const t of S.tools) {
      const opt = document.createElement("option");
      opt.value = t.name;
      opt.textContent = t.name;
      sel.append(opt);
    }
    sel.disabled = S.tools.length === 0;
    $("args").disabled = S.tools.length === 0;
    $("call").disabled = S.tools.length === 0;
    $("mcpstatus").textContent = S.tools.length + " tool(s) visible to this principal";
    showToolDesc();
  } catch (err) {
    $("mcpstatus").textContent = "";
    banner("MCP connect failed: " + err.message);
  }
}

function showToolDesc() {
  const t = S.tools.find((x) => x.name === $("tools").value);
  $("tooldesc").textContent = t && t.description ? t.description : "";
}

async function callTool() {
  banner("");
  $("result").textContent = "";
  let args = {};
  const raw = $("args").value.trim();
  if (raw) {
    try { args = JSON.parse(raw); } catch (err) {
      banner("arguments are not valid JSON: " + err.message);
      return;
    }
  }
  $("call").disabled = true;
  try {
    const result = await rpc("tools/call", { name: $("tools").value, arguments: args });
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
$("tools").addEventListener("change", showToolDesc);
$("call").addEventListener("click", callTool);

refreshState();
startPolling();
