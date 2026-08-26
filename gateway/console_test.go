package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fold-run/fold/config"
)

// consoleConfig is a minimal config with the console switched on.
func consoleConfig(upstreams []config.Upstream) *config.Config {
	return &config.Config{
		Upstreams: upstreams,
		Server:    &config.ServerSection{Introspection: &config.Introspection{Enabled: true}, Console: &config.Console{Enabled: true}},
	}
}

// The console is off by default: none of its routes may exist without
// server.console.enabled or server.introspection.enabled — the mux must fall
// through to 404. /api/ is a top-level namespace now, so "nothing is served
// there by default" is the assertion that keeps it from becoming a surface
// nobody asked for.
func TestConsoleDisabledByDefault(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{
		{ID: "u", URL: up.URL},
	}})

	for _, path := range []string{"/console/", "/console", "/api/federation", "/api/auth-hint"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s with console disabled: want 404, got %d", path, resp.StatusCode)
		}
	}
}

// With the console enabled and auth off, the static page serves with its
// CSP and the state API reports the federation.
func TestConsoleEnabledStateAndAssets(t *testing.T) {
	up1, _ := newUpstreamServer(t, "get_weather")
	up2, _ := newUpstreamServer(t, "search")
	cfg := consoleConfig([]config.Upstream{
		{ID: "weather", URL: up1.URL, Namespace: "wx"},
		{ID: "search", URL: up2.URL, Namespace: "search"},
	})
	cfg.Server.MCPPath = "/rpc"
	ts, _ := startGateway(t, cfg)

	// Static assets: 200, CSP pinned to this origin, and it is our page.
	resp, err := http.Get(ts.URL + "/console/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /console/: want 200, got %d", resp.StatusCode)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("CSP = %q, want default-src 'self'", csp)
	}
	if !strings.Contains(string(body), "fold console") {
		t.Errorf("console page does not contain %q", "fold console")
	}

	// Bare /console redirects into the directory.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err = noRedirect.Get(ts.URL + "/console")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("GET /console: want 301, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/console/" {
		t.Errorf("redirect Location = %q, want /console/", loc)
	}

	// State API: version, mcpPath, and the upstream roster.
	resp, err = http.Get(ts.URL + "/api/federation")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/federation: want 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("state Content-Type = %q, want application/json", ct)
	}
	var st struct {
		Version               string   `json:"version"`
		AuthRequired          bool     `json:"authRequired"`
		MCPPath               string   `json:"mcpPath"`
		PolicyDefaultDecision string   `json:"policyDefaultDecision"`
		StaticUpstreams       int      `json:"staticUpstreams"`
		SharedState           bool     `json:"sharedState"`
		NamespaceSeparator    string   `json:"namespaceSeparator"`
		PageSize              int      `json:"pageSize"`
		AuditSinks            []string `json:"auditSinks"`
		Upstreams             []struct {
			ID           string `json:"id"`
			Namespace    string `json:"namespace"`
			Connected    bool   `json:"connected"`
			Source       string `json:"source"`
			AuthStrategy string `json:"authStrategy"`
		} `json:"upstreams"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if st.Version == "" {
		t.Error("state.version is empty")
	}
	if st.AuthRequired {
		t.Error("state.authRequired = true with auth disabled")
	}
	if st.MCPPath != "/rpc" {
		t.Errorf("state.mcpPath = %q, want /rpc", st.MCPPath)
	}
	// No policy section means the engine is allow-all; the console must not
	// mislabel that as deny-by-default.
	if st.PolicyDefaultDecision != "allow (no policy configured)" {
		t.Errorf("state.policyDefaultDecision = %q, want %q", st.PolicyDefaultDecision, "allow (no policy configured)")
	}
	if st.StaticUpstreams != 2 {
		t.Errorf("state.staticUpstreams = %d, want 2", st.StaticUpstreams)
	}
	if len(st.Upstreams) != 2 {
		t.Fatalf("state.upstreams has %d entries, want 2", len(st.Upstreams))
	}
	if st.SharedState {
		t.Error("state.sharedState = true without Redis")
	}
	if st.NamespaceSeparator != "__" {
		t.Errorf("state.namespaceSeparator = %q, want __", st.NamespaceSeparator)
	}
	if st.PageSize != 200 {
		t.Errorf("state.pageSize = %d, want 200", st.PageSize)
	}
	if st.AuditSinks == nil {
		t.Error("state.auditSinks should be [] (never null) with no audit config")
	}
	byID := map[string]string{}
	for _, u := range st.Upstreams {
		byID[u.ID] = u.Namespace
		if u.Source != "static" {
			t.Errorf("upstream %s source = %q, want static", u.ID, u.Source)
		}
		if u.AuthStrategy != "none" {
			t.Errorf("upstream %s authStrategy = %q, want none", u.ID, u.AuthStrategy)
		}
		if !u.Connected {
			t.Errorf("upstream %q not connected", u.ID)
		}
	}
	if byID["weather"] != "wx" || byID["search"] != "search" {
		t.Errorf("upstream id/namespace pairs = %v", byID)
	}
}

// The state API is data and authenticates like /mcp; the static assets are
// the same bytes for everyone and stay open.
func TestConsoleStateRequiresAuth(t *testing.T) {
	iss := newFixtureIssuer(t)
	up, _ := newUpstreamServer(t, "tool")
	// A second, dead upstream: with auth required the console must reduce
	// its raw connect error (which can name env vars / internal hosts) to a
	// category, while topology (URLs) stays visible to a valid principal.
	cfg := authedConfig(iss, []config.Upstream{
		{ID: "u", URL: up.URL, Namespace: "u"},
		{ID: "dead", URL: "http://127.0.0.1:1/mcp", Namespace: "dead"},
	}, nil)
	cfg.Server = &config.ServerSection{Introspection: &config.Introspection{Enabled: true}, Console: &config.Console{Enabled: true}}
	ts, _ := startGateway(t, cfg)

	// No token → 401 on the data plane.
	resp, err := http.Get(ts.URL + "/api/federation")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("state without token: want 401, got %d", resp.StatusCode)
	}

	// Static assets stay open: identical bytes for every caller, no data.
	resp, err = http.Get(ts.URL + "/console/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("assets without token: want 200, got %d", resp.StatusCode)
	}

	// Valid token → 200 with the detailed (URL-bearing) view.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/federation", nil)
	req.Header.Set("Authorization", "Bearer "+iss.mint(t, "alice", "https://gw.example.com", nil))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("state with token: want 200, got %d", resp.StatusCode)
	}
	var st struct {
		AuthRequired bool `json:"authRequired"`
		Upstreams    []struct {
			ID        string `json:"id"`
			URL       string `json:"url"`
			Connected bool   `json:"connected"`
			Error     string `json:"error"`
		} `json:"upstreams"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if !st.AuthRequired {
		t.Error("state.authRequired = false with auth required")
	}
	if len(st.Upstreams) != 2 {
		t.Fatalf("state.upstreams has %d entries, want 2", len(st.Upstreams))
	}
	for _, u := range st.Upstreams {
		switch u.ID {
		case "u":
			if u.URL != up.URL {
				t.Errorf("authenticated state is not detailed: url = %q, want %q", u.URL, up.URL)
			}
		case "dead":
			if u.Connected {
				t.Error("dead upstream reports connected")
			}
			// Raw dial errors would leak "connection refused" + target
			// details; with auth on, the console serves only the category.
			if u.Error != "unreachable — details in gateway logs" {
				t.Errorf("dead upstream error not redacted to category: %q", u.Error)
			}
		default:
			t.Errorf("unexpected upstream %q in state", u.ID)
		}
	}
}

// consoleWire mirrors the console's JS client (gateway/console/app.js): a
// plain HTTP JSON-RPC exchange against /mcp — Accept json+SSE, session id
// and protocol version echoed as headers, responses parsed from either a
// JSON body or an SSE stream. No SDK client: the point is the browser path.
type consoleWire struct {
	t         *testing.T
	endpoint  string
	token     string
	sessionID string
	protocol  string
	nextID    int
}

type consoleRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *consoleWire) rpc(method string, params any, notification bool) (json.RawMessage, *consoleRPCError) {
	c.t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	var id int
	if !notification {
		c.nextID++
		id = c.nextID
		msg["id"] = id
	}
	body, err := json.Marshal(msg)
	if err != nil {
		c.t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.token)
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	if c.protocol != "" {
		req.Header.Set("MCP-Protocol-Version", c.protocol)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.sessionID = sid
	}
	if notification {
		if resp.StatusCode != http.StatusAccepted {
			c.t.Fatalf("%s: want 202, got %d", method, resp.StatusCode)
		}
		return nil, nil
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("%s: read body: %v", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("%s: HTTP %d: %s", method, resp.StatusCode, truncate(string(raw), 300))
	}

	var payloads [][]byte
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		// Events are blank-line separated; a payload is its data: lines.
		for _, chunk := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n\n") {
			var data []string
			for _, line := range strings.Split(chunk, "\n") {
				if rest, ok := strings.CutPrefix(line, "data:"); ok {
					data = append(data, strings.TrimPrefix(rest, " "))
				}
			}
			if len(data) > 0 {
				payloads = append(payloads, []byte(strings.Join(data, "\n")))
			}
		}
	} else {
		payloads = [][]byte{raw}
	}
	for _, p := range payloads {
		var reply struct {
			ID     int              `json:"id"`
			Result json.RawMessage  `json:"result"`
			Error  *consoleRPCError `json:"error"`
		}
		if err := json.Unmarshal(p, &reply); err != nil || reply.ID != id {
			continue
		}
		return reply.Result, reply.Error
	}
	c.t.Fatalf("%s: no response for id %d on the stream", method, id)
	return nil, nil
}

// Console traffic is governed like any other client's: the browser-shaped
// HTTP sequence (initialize → notifications/initialized → tools/list →
// tools/call) sees policy-filtered lists, gets -31042 for a disallowed
// call, and the denial lands in the audit sink under the right principal.
func TestConsoleClientGovernance(t *testing.T) {
	events := make(chan []byte, 64)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		select {
		case events <- b:
		default:
		}
	}))
	t.Cleanup(sink.Close)

	iss := newFixtureIssuer(t)
	up, _ := newUpstreamServer(t, "get_thing", "delete_thing")
	cfg := authedConfig(iss,
		[]config.Upstream{{ID: "things", URL: up.URL, Namespace: "things"}},
		&config.Policy{
			DefaultDecision: "deny",
			Rules: []config.PolicyRule{{
				ID:       "alice-read",
				Subjects: &config.PolicySubjects{Subs: []string{"alice"}},
				Allow:    []config.PolicyAllow{{Server: "things", Names: []string{"get_*"}}},
			}},
		})
	cfg.Server = &config.ServerSection{Introspection: &config.Introspection{Enabled: true}, Console: &config.Console{Enabled: true}}
	cfg.Audit = &config.Audit{Sinks: []config.AuditSink{{Type: "webhook", URL: sink.URL}}}
	ts, _ := startGateway(t, cfg)

	c := &consoleWire{
		t:        t,
		endpoint: ts.URL + "/mcp",
		token:    iss.mint(t, "alice", "https://gw.example.com", nil),
	}

	// initialize → notifications/initialized, exactly as app.js does.
	initRaw, rpcErr := c.rpc("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "fold-console", "version": "1"},
	}, false)
	if rpcErr != nil {
		t.Fatalf("initialize: JSON-RPC %d: %s", rpcErr.Code, rpcErr.Message)
	}
	var initRes struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(initRaw, &initRes); err != nil {
		t.Fatalf("initialize result: %v", err)
	}
	if c.sessionID == "" {
		t.Fatal("initialize did not assign a session id")
	}
	c.protocol = initRes.ProtocolVersion
	c.rpc("notifications/initialized", nil, true)

	// tools/list: only the allowed tool is visible.
	listRaw, rpcErr := c.rpc("tools/list", map[string]any{}, false)
	if rpcErr != nil {
		t.Fatalf("tools/list: JSON-RPC %d: %s", rpcErr.Code, rpcErr.Message)
	}
	var listRes struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(listRaw, &listRes); err != nil {
		t.Fatalf("tools/list result: %v", err)
	}
	var names []string
	for _, tool := range listRes.Tools {
		names = append(names, tool.Name)
	}
	if got := strings.Join(names, ","); got != "things__get_thing" {
		t.Errorf("filtered list = %q, want only things__get_thing", got)
	}

	// tools/call of the invisible tool: the gateway's policy error code.
	_, rpcErr = c.rpc("tools/call", map[string]any{
		"name":      "things__delete_thing",
		"arguments": map[string]any{},
	}, false)
	if rpcErr == nil {
		t.Fatal("expected policy denial for things__delete_thing")
	}
	if rpcErr.Code != -31042 {
		t.Errorf("denial code = %d, want -31042", rpcErr.Code)
	}

	// The allowed call still works over the same wire.
	if _, rpcErr := c.rpc("tools/call", map[string]any{
		"name":      "things__get_thing",
		"arguments": map[string]any{},
	}, false); rpcErr != nil {
		t.Errorf("allowed call failed: JSON-RPC %d: %s", rpcErr.Code, rpcErr.Message)
	}

	// With a policy section present, the state API reports the deny default.
	stateReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/federation", nil)
	stateReq.Header.Set("Authorization", "Bearer "+c.token)
	stateResp, err := http.DefaultClient.Do(stateReq)
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		PolicyDefaultDecision string `json:"policyDefaultDecision"`
		PolicyRules           int    `json:"policyRules"`
	}
	if err := json.NewDecoder(stateResp.Body).Decode(&st); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	stateResp.Body.Close()
	if st.PolicyDefaultDecision != "deny" {
		t.Errorf("state.policyDefaultDecision = %q, want deny", st.PolicyDefaultDecision)
	}
	if st.PolicyRules != 1 {
		t.Errorf("state.policyRules = %d, want 1", st.PolicyRules)
	}

	// The denial exits through audit, attributed to the caller.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case b := <-events:
			var batch []struct {
				Principal string `json:"principal"`
				Method    string `json:"method"`
				Name      string `json:"name"`
				Outcome   string `json:"outcome"`
			}
			if err := json.Unmarshal(b, &batch); err != nil {
				t.Fatalf("audit batch: %v: %s", err, b)
			}
			for _, ev := range batch {
				if ev.Method == "tools/call" && ev.Outcome == "denied" {
					if ev.Principal != "alice" {
						t.Errorf("denied call audited under %q, want alice", ev.Principal)
					}
					if ev.Name != "things__delete_thing" {
						t.Errorf("denied call audited for %q, want things__delete_thing", ev.Name)
					}
					return
				}
			}
		case <-deadline:
			t.Fatal("denied tools/call was not audited")
		}
	}
}

// The state API reports the discovery poller: once a valid document
// applies, discovery.lastOutcome/lastSyncAt and discoveredUpstreams follow.
func TestConsoleDiscoveryStatus(t *testing.T) {
	up, _ := newUpstreamServer(t, "alpha_tool")
	upB, _ := newUpstreamServer(t, "beta_tool")
	registry, doc := discoveryRegistry(t, "")

	cfg := consoleConfig([]config.Upstream{{ID: "a", URL: up.URL, Namespace: "a"}})
	cfg.Discovery = &config.Discovery{URL: registry.URL, IntervalMs: 50}
	ts, _ := startGateway(t, cfg)

	doc.Store(fmt.Sprintf(`{"upstreams":[{"id":"b","url":%q,"namespace":"b"}]}`, upB.URL))

	type stateDoc struct {
		StaticUpstreams     int `json:"staticUpstreams"`
		DiscoveredUpstreams int `json:"discoveredUpstreams"`
		Upstreams           []struct {
			ID     string `json:"id"`
			Source string `json:"source"`
		} `json:"upstreams"`
		Discovery *struct {
			URL         string `json:"url"`
			IntervalMs  int    `json:"intervalMs"`
			LastOutcome string `json:"lastOutcome"`
			LastSyncAt  string `json:"lastSyncAt"`
		} `json:"discovery"`
	}
	fetchState := func() stateDoc {
		t.Helper()
		resp, err := http.Get(ts.URL + "/api/federation")
		if err != nil {
			t.Fatalf("GET state: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET state: want 200, got %d", resp.StatusCode)
		}
		var st stateDoc
		if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
			t.Fatalf("decode state: %v", err)
		}
		return st
	}

	waitFor(t, 5*time.Second, func() bool {
		st := fetchState()
		return st.Discovery != nil && st.Discovery.LastOutcome == "applied" && st.DiscoveredUpstreams == 1
	}, "console state never reported the applied discovery sync")

	st := fetchState()
	if st.Discovery.URL != registry.URL {
		t.Errorf("discovery.url = %q, want %q", st.Discovery.URL, registry.URL)
	}
	if st.Discovery.IntervalMs != 50 {
		t.Errorf("discovery.intervalMs = %d, want 50", st.Discovery.IntervalMs)
	}
	if st.Discovery.LastSyncAt == "" {
		t.Error("discovery.lastSyncAt is empty after an applied sync")
	} else if _, err := time.Parse(time.RFC3339, st.Discovery.LastSyncAt); err != nil {
		t.Errorf("discovery.lastSyncAt = %q is not RFC 3339: %v", st.Discovery.LastSyncAt, err)
	}
	if st.StaticUpstreams != 1 {
		t.Errorf("staticUpstreams = %d, want 1", st.StaticUpstreams)
	}
	// Source annotation distinguishes how each upstream joined the
	// federation: the registry-sourced one is "discovered", the config
	// one "static".
	sources := map[string]string{}
	for _, u := range st.Upstreams {
		sources[u.ID] = u.Source
	}
	if sources["b"] != "discovered" {
		t.Errorf(`upstream b source = %q, want "discovered"`, sources["b"])
	}
	if sources["a"] != "static" {
		t.Errorf(`upstream a source = %q, want "static"`, sources["a"])
	}
	if st.DiscoveredUpstreams != 1 {
		t.Errorf("discoveredUpstreams = %d, want 1 (the document lists one)", st.DiscoveredUpstreams)
	}
}

// Console routes accept only the read verbs they document.
func TestConsoleMethodDiscipline(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	ts, _ := startGateway(t, consoleConfig([]config.Upstream{{ID: "u", URL: up.URL}}))

	resp, err := http.Post(ts.URL+"/api/federation", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/federation: want 405, got %d", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); allow != http.MethodGet {
		t.Errorf("state Allow = %q, want GET", allow)
	}

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/console/", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /console/: want 405, got %d", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); allow != "GET, HEAD" {
		t.Errorf("assets Allow = %q, want GET, HEAD", allow)
	}

	// HEAD is a read verb the assets advertise — it must answer 200.
	resp, err = http.Head(ts.URL + "/console/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("HEAD /console/: want 200, got %d", resp.StatusCode)
	}
}

// TestIntrospectionViewerAllowlist: server.introspection.groups narrows who may
// read /api/federation. A principal carrying an allowlisted group gets the state;
// one without gets 403 and the denial exits through the audit sink. Static
// assets stay open either way — they carry no data.
func TestIntrospectionViewerAllowlist(t *testing.T) {
	events := make(chan []byte, 64)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		select {
		case events <- b:
		default:
		}
	}))
	t.Cleanup(sink.Close)

	iss := newFixtureIssuer(t)
	up, _ := newUpstreamServer(t, "tool")
	cfg := authedConfig(iss, []config.Upstream{{ID: "u", URL: up.URL}}, nil)
	cfg.Server = &config.ServerSection{Introspection: &config.Introspection{Enabled: true, Groups: []string{"ops"}}, Console: &config.Console{Enabled: true}}
	cfg.Audit = &config.Audit{Sinks: []config.AuditSink{{Type: "webhook", URL: sink.URL}}}
	ts, _ := startGateway(t, cfg)

	get := func(token string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/federation", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Allowlisted group → 200, and the state reports its own allowlist.
	resp := get(iss.mint(t, "alice", "https://gw.example.com", []string{"ops", "eng"}))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("allowlisted viewer: want 200, got %d", resp.StatusCode)
	}
	var st struct {
		ViewerGroups []string `json:"viewerGroups"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	resp.Body.Close()
	if len(st.ViewerGroups) != 1 || st.ViewerGroups[0] != "ops" {
		t.Errorf("state.viewerGroups = %v, want [ops]", st.ViewerGroups)
	}

	// Valid token, no allowlisted group → 403.
	resp = get(iss.mint(t, "bob", "https://gw.example.com", []string{"eng"}))
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-allowlisted viewer: want 403, got %d", resp.StatusCode)
	}

	// The 403 exits through audit, attributed to bob.
	deadline := time.After(5 * time.Second)
	for {
		var batch []struct {
			Principal string `json:"principal"`
			Outcome   string `json:"outcome"`
			Decision  string `json:"decision"`
			Error     string `json:"error"`
		}
		select {
		case b := <-events:
			if err := json.Unmarshal(b, &batch); err != nil {
				t.Fatalf("audit batch: %v: %s", err, b)
			}
			for _, ev := range batch {
				if ev.Outcome == "denied" && strings.Contains(ev.Error, "viewer allowlist") {
					if ev.Principal != "bob" {
						t.Errorf("console denial audited under %q, want bob", ev.Principal)
					}
					if ev.Decision != "deny" {
						t.Errorf("console denial decision = %q, want deny", ev.Decision)
					}
					goto done
				}
			}
		case <-deadline:
			t.Fatal("console viewer denial was not audited")
		}
	}
done:

	// Static assets are ungated regardless of the allowlist.
	resp, err := http.Get(ts.URL + "/console/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("assets with allowlist configured: want 200, got %d", resp.StatusCode)
	}
}

// TestConsoleAuthHint: /api/auth-hint is the deliberately
// unauthenticated login hint — everything in it is public SPA config.
func TestConsoleAuthHint(t *testing.T) {
	iss := newFixtureIssuer(t)
	up, _ := newUpstreamServer(t, "tool")
	cfg := authedConfig(iss, []config.Upstream{{ID: "u", URL: up.URL}}, nil)
	cfg.Server = &config.ServerSection{
		Introspection: &config.Introspection{Enabled: true},
		Console: &config.Console{
			Enabled: true,
			OAuth:   &config.ConsoleOAuth{ClientID: "fold-console", Scopes: []string{"openid"}},
		},
	}
	ts, _ := startGateway(t, cfg)

	// No token needed: the page reads this before it can possibly have one.
	resp, err := http.Get(ts.URL + "/api/auth-hint")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("auth hint without token: want 200, got %d", resp.StatusCode)
	}
	var hint struct {
		AuthRequired bool   `json:"authRequired"`
		Resource     string `json:"resource"`
		OAuth        *struct {
			Issuer   string   `json:"issuer"`
			ClientID string   `json:"clientId"`
			Scopes   []string `json:"scopes"`
		} `json:"oauth"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&hint); err != nil {
		t.Fatalf("decode hint: %v", err)
	}
	if !hint.AuthRequired {
		t.Error("hint.authRequired = false with auth required")
	}
	if hint.Resource != "https://gw.example.com" {
		t.Errorf("hint.resource = %q", hint.Resource)
	}
	if hint.OAuth == nil {
		t.Fatal("hint.oauth missing with console.oauth configured")
	}
	if hint.OAuth.Issuer != iss.server.URL {
		t.Errorf("hint.oauth.issuer = %q, want the fixture issuer %q", hint.OAuth.Issuer, iss.server.URL)
	}
	if hint.OAuth.ClientID != "fold-console" || len(hint.OAuth.Scopes) != 1 {
		t.Errorf("hint.oauth = %+v", hint.OAuth)
	}

	// The asset CSP admits the issuer origin in connect-src (metadata
	// fetch + code exchange), and nothing else beyond 'self'.
	resp, err = http.Get(ts.URL + "/console/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "connect-src 'self' "+iss.server.URL) {
		t.Errorf("CSP %q does not admit the issuer origin in connect-src", csp)
	}

	// Method discipline holds on the hint too.
	postResp, err := http.Post(ts.URL+"/api/auth-hint", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	postResp.Body.Close()
	if postResp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST auth hint: want 405, got %d", postResp.StatusCode)
	}
}

// Without console.oauth the hint still serves (authRequired + resource are
// what the page needs to fall back to paste-token UX) and the CSP stays
// pinned to 'self' alone.
func TestConsoleAuthHintWithoutOAuth(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	ts, _ := startGateway(t, consoleConfig([]config.Upstream{{ID: "u", URL: up.URL}}))

	resp, err := http.Get(ts.URL + "/api/auth-hint")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var hint struct {
		AuthRequired bool            `json:"authRequired"`
		OAuth        json.RawMessage `json:"oauth"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&hint); err != nil {
		t.Fatalf("decode hint: %v", err)
	}
	if hint.AuthRequired || hint.OAuth != nil {
		t.Errorf("hint = %+v, want authRequired=false and no oauth", hint)
	}

	assets, err := http.Get(ts.URL + "/console/")
	if err != nil {
		t.Fatal(err)
	}
	assets.Body.Close()
	if csp := assets.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'self';") {
		t.Errorf("CSP %q should pin connect-src to 'self' alone without oauth", csp)
	}
}
