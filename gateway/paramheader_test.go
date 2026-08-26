package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/audit"
	"github.com/fold-run/fold/config"
)

// The streamable HTTP transport names intermediaries directly: one that does
// not recognize an `Mcp-Param-{Name}` header MUST forward it and otherwise
// ignore it. fold is such an intermediary, and its upstream leg is a fresh SDK
// client session whose headers the SDK generates — so nothing a caller sent
// used to survive the hop.
//
// The forward is the smaller half of what these tests pin. The larger half is
// that the forward stays exactly this wide: fold does not relay arbitrary
// client headers, which is why a caller cannot hand an upstream a credential
// or an identity assertion of its own choosing. Every test below that asserts
// an `Mcp-Param-*` header crossed also asserts that an `Authorization`, a
// `Cookie`, and a plain `X-*` header did not.

// callerOnlyHeaders are headers a caller may set that must stop at the
// gateway. They ride along in every request these tests make, so a relay that
// widened past the one namespace a specification requires fails here.
var callerOnlyHeaders = map[string]string{
	"Authorization": "Bearer caller-token-do-not-relay",
	"Cookie":        "session=caller-cookie-do-not-relay",
	"X-Custom":      "caller-custom-do-not-relay",
}

// paramProbe is a real SDK MCP server that records the HTTP headers of every
// tools/call it is handed. The recording wrapper is the only hand-rolled part;
// the protocol on both sides is the SDK's.
//
// One tool is annotated with `x-mcp-header`. Under the 2026-07-28 era that
// annotation makes the SDK server *require* the matching `Mcp-Param-Tenant`
// header on every call to it, and reject the call without one — which is the
// conformance failure a dropped header actually causes.
type paramProbe struct {
	url string

	mu    sync.Mutex
	calls []http.Header   // headers of every tools/call
	posts []probedRequest // every POST, whatever it carried
}

// probedRequest is one POST the upstream saw: what fold sent and what it sent
// it with, so a test can ask which requests the relay touched.
type probedRequest struct {
	body string
	hdr  http.Header
}

const probeTenantSchema = `{"type":"object","properties":{"tenant":{"type":"string","x-mcp-header":"Tenant"}},"required":["tenant"]}`

// newParamProbe runs the probe in the sessionful era, which is the era fold's
// upstream leg negotiates by default.
func newParamProbe(t *testing.T) *paramProbe {
	t.Helper()
	return newParamProbeWith(t, nil)
}

// newParamProbeStateless runs the probe in the 2026-07-28 era. The SDK serves
// that era only when stateless, and only there does it enforce SEP-2243's
// header/body agreement.
func newParamProbeStateless(t *testing.T) *paramProbe {
	t.Helper()
	return newParamProbeWith(t, &mcp.StreamableHTTPOptions{Stateless: true})
}

func newParamProbeWith(t *testing.T, opts *mcp.StreamableHTTPOptions) *paramProbe {
	t.Helper()
	p := &paramProbe{}
	server := mcp.NewServer(&mcp.Implementation{Name: "param-probe", Version: "1.0"}, nil)
	server.AddTool(&mcp.Tool{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})
	// SEP-2243: this argument is mirrored into Mcp-Param-Tenant, and the SDK
	// server validates the header against the body.
	server.AddTool(&mcp.Tool{Name: "tenant_echo", InputSchema: json.RawMessage(probeTenantSchema)},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args struct {
				Tenant string `json:"tenant"`
			}
			_ = json.Unmarshal(req.Params.Arguments, &args)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "tenant:" + args.Tenant}}}, nil
		})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, opts)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sniff the body rather than trusting Mcp-Method: that header only
		// exists from 2026-07-28 on, and the probe has to see both eras.
		if r.Method == http.MethodPost && r.Body != nil {
			body, err := io.ReadAll(r.Body)
			_ = r.Body.Close()
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			p.mu.Lock()
			p.posts = append(p.posts, probedRequest{body: string(body), hdr: r.Header.Clone()})
			if bytes.Contains(body, []byte(`"tools/call"`)) {
				p.calls = append(p.calls, r.Header.Clone())
			}
			p.mu.Unlock()
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	p.url = ts.URL
	return p
}

// lastCall returns the headers of the most recent tools/call the upstream saw.
func (p *paramProbe) lastCall(t *testing.T) http.Header {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) == 0 {
		t.Fatal("upstream never received a tools/call")
	}
	return p.calls[len(p.calls)-1]
}

// allPosts returns every POST the upstream saw, body and headers together.
func (p *paramProbe) allPosts() []probedRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]probedRequest(nil), p.posts...)
}

// callerHeaders is a client-side transport that adds headers verbatim,
// preserving the multi-valued form that connect's map[string]string cannot.
type callerHeaders struct{ hdr http.Header }

func (c callerHeaders) RoundTrip(req *http.Request) (*http.Response, error) {
	for name, values := range c.hdr {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	return http.DefaultTransport.RoundTrip(req)
}

// connectWith opens a real SDK client session that sends hdr on every request.
func connectWith(t *testing.T, endpoint string, hdr http.Header) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "param-client", Version: "1.0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: callerHeaders{hdr: hdr}},
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// paramCallerHeaders is what a caller sends: three Mcp-Param-* headers, one of
// them multi-valued, plus the three headers that must not cross.
func paramCallerHeaders() http.Header {
	hdr := http.Header{}
	hdr.Set("Mcp-Param-Tenant", "acme")
	hdr.Set("Mcp-Param-Region", "eu-west-1")
	hdr.Add("Mcp-Param-Trait", "alpha")
	hdr.Add("Mcp-Param-Trait", "beta")
	for k, v := range callerOnlyHeaders {
		hdr.Set(k, v)
	}
	return hdr
}

// assertCallerOnlyHeadersStopped is the narrow half of the contract, asserted
// everywhere the wide half is. If someone widens the relay, this fails.
func assertCallerOnlyHeadersStopped(t *testing.T, where string, got http.Header) {
	t.Helper()
	for name, sent := range callerOnlyHeaders {
		if v := got.Get(name); v == sent {
			t.Errorf("%s: caller's %s header crossed to the upstream (%q); fold relays only Mcp-Param-*", where, name, v)
		}
	}
}

// TestParamHeadersReachTheUpstream is the forward and its bound in one place:
// the caller's Mcp-Param-* headers arrive with their values intact — including
// the multi-valued one — and the caller's Authorization, Cookie, and X-Custom
// do not arrive at all.
//
// The direct leg first is the control. It proves the probe can see all six
// headers when nothing filters them, so the gateway leg's three absences are
// fold's doing rather than a fixture that cannot observe them.
func TestParamHeadersReachTheUpstream(t *testing.T) {
	probe := newParamProbe(t)
	hdr := paramCallerHeaders()
	ctx := context.Background()

	// Control: straight at the upstream, no gateway in the path.
	direct := connectWith(t, probe.url, hdr)
	if _, err := direct.CallTool(ctx, &mcp.CallToolParams{Name: "echo"}); err != nil {
		t.Fatalf("direct CallTool: %v", err)
	}
	seen := probe.lastCall(t)
	for name := range hdr {
		if seen.Get(name) == "" {
			t.Fatalf("control leg: upstream did not see %s, so this fixture cannot prove anything about relaying", name)
		}
	}

	// Through the gateway.
	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{
		{ID: "p", Namespace: "p", URL: probe.url},
	}})
	session := connectWith(t, ts.URL+"/mcp", hdr)
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "p__echo"}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	got := probe.lastCall(t)

	if v := got.Get("Mcp-Param-Tenant"); v != "acme" {
		t.Errorf("upstream Mcp-Param-Tenant = %q, want %q", v, "acme")
	}
	if v := got.Get("Mcp-Param-Region"); v != "eu-west-1" {
		t.Errorf("upstream Mcp-Param-Region = %q, want %q", v, "eu-west-1")
	}
	if v := got.Values("Mcp-Param-Trait"); strings.Join(v, ",") != "alpha,beta" {
		t.Errorf("upstream Mcp-Param-Trait = %v, want [alpha beta] — the multi-valued form must survive", v)
	}
	assertCallerOnlyHeadersStopped(t, "tools/call", got)
}

// TestParamHeadersRideEveryUpstreamRequestOfTheCall records the relay's actual
// reach, which is wider than "the tools/call POST": the headers are lifted onto
// the request context, so every upstream request fold makes under that context
// carries them — including the bridged session's own handshake, issued lazily
// inside the first named invocation.
//
// That is harmless as the specification is written (an intermediary forwards
// and ignores; a server validates a param header only for a tools/call naming
// a tool that annotates it) and it is what makes the relay allocation-free on
// the path that matters. It is recorded here because it is not obvious from
// the call site, and because a future upstream that reads these headers on a
// handshake would be reading a value scoped to a call it has not seen yet.
func TestParamHeadersRideEveryUpstreamRequestOfTheCall(t *testing.T) {
	probe := newParamProbe(t)
	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{
		{ID: "p", Namespace: "p", URL: probe.url},
	}})
	// A fresh gateway, so the first call is what opens the bridged session.
	session := connectWith(t, ts.URL+"/mcp", paramCallerHeaders())
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "p__echo"}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	var withParam, withoutParam []string
	for _, post := range probe.allPosts() {
		kind := "other"
		for _, m := range []string{"initialize", "tools/call", "tools/list", "notifications/initialized"} {
			if strings.Contains(post.body, `"`+m+`"`) {
				kind = m
				break
			}
		}
		if post.hdr.Get("Mcp-Param-Tenant") == "acme" {
			withParam = append(withParam, kind)
		} else {
			withoutParam = append(withoutParam, kind)
		}
		assertCallerOnlyHeadersStopped(t, kind, post.hdr)
	}
	if !slices.Contains(withParam, "tools/call") {
		t.Errorf("tools/call did not carry the param header; carried=%v bare=%v", withParam, withoutParam)
	}
	t.Logf("upstream POSTs carrying Mcp-Param-Tenant: %v; without: %v", withParam, withoutParam)
}

// TestParamHeaderRoundTripSEP2243 is the conformance failure in its natural
// form, and the reason this is a correctness fix rather than a courtesy. The
// upstream annotates a tool argument with `x-mcp-header`; under 2026-07-28 the
// SDK server then *requires* the matching Mcp-Param-Tenant header on every
// call to that tool and rejects the call without it. A dropped header does not
// merely lose a routing hint — it turns a valid call into a protocol error.
//
// The upstream leg is the era-carrying one here: fold serves its own clients
// sessionfully (see TestModernEraIsRefusedByTheTransport), so a caller reaches
// fold below 2026-07-28 and mints nothing itself — it sets the header, as any
// non-SDK client may. `protocol: "auto"` lets the upstream leg negotiate the
// era where the header is enforced.
func TestParamHeaderRoundTripSEP2243(t *testing.T) {
	probe := newParamProbeStateless(t)
	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{
		{ID: "p", Namespace: "p", URL: probe.url, Protocol: "auto"},
	}})

	hdr := http.Header{}
	hdr.Set("Mcp-Param-Tenant", "acme")
	for k, v := range callerOnlyHeaders {
		hdr.Set(k, v)
	}
	session := connectWith(t, ts.URL+"/mcp", hdr)
	ctx := context.Background()

	// The annotation has to survive federation, or nothing downstream of fold
	// could mint the header in the first place.
	list, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var annotated bool
	for _, tool := range list.Tools {
		if tool.Name == "p__tenant_echo" {
			raw, _ := json.Marshal(tool.InputSchema)
			annotated = strings.Contains(string(raw), `"x-mcp-header"`)
		}
	}
	if !annotated {
		t.Error("federated tool lost its x-mcp-header annotation; nothing downstream could mint the header")
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "p__tenant_echo",
		Arguments: map[string]any{"tenant": "acme"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v (the upstream rejects this call when the param header is dropped)", err)
	}
	if res.IsError {
		t.Fatalf("CallTool returned an error result: %v", res.Content)
	}
	got := probe.lastCall(t)
	if v := got.Get("Mcp-Param-Tenant"); v != "acme" {
		t.Errorf("upstream Mcp-Param-Tenant = %q, want %q", v, "acme")
	}
	assertCallerOnlyHeadersStopped(t, "sep-2243 round trip", got)
}

// TestForgedParamHeaderIsTheUpstreamsRefusalToMake is the failure path the
// relay opens, and where it lands. fold forwards these headers without reading
// them, so a caller can send one that disagrees with the body it sent — and the
// party that catches that is the upstream, not fold. The upstream's refusal
// then travels the ordinary route: passed through verbatim, because fold mints
// no error of its own for someone else's protocol complaint, and audited like
// every other terminal response.
//
// The forgery costs the caller its own call and nothing else, which is the
// property that makes "forward and otherwise ignore" safe for fold to do. It
// is *not* a property fold enforces: an upstream that routes on these headers
// without checking them against the body it was handed is trusting a value
// fold's policy never saw.
func TestForgedParamHeaderIsTheUpstreamsRefusalToMake(t *testing.T) {
	probe := newParamProbeStateless(t)
	auditPath := t.TempDir() + "/audit.jsonl"
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "p", Namespace: "p", URL: probe.url, Protocol: "auto"}},
		Audit:     &config.Audit{Sinks: []config.AuditSink{{Type: "file", Path: auditPath}}},
	})

	hdr := http.Header{}
	hdr.Set("Mcp-Param-Tenant", "not-the-tenant-in-the-body")
	session := connectWith(t, ts.URL+"/mcp", hdr)

	_, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "p__tenant_echo",
		Arguments: map[string]any{"tenant": "acme"},
	})
	if err == nil {
		t.Fatal("a param header disagreeing with the body was accepted")
	}
	// The upstream's own words, not one of fold's six error codes.
	if !strings.Contains(err.Error(), "header mismatch") {
		t.Errorf("error = %v, want the upstream's header-mismatch complaint passed through verbatim", err)
	}
	for _, minted := range []string{"policy denied", "unknown namespace", "unavailable"} {
		if strings.Contains(err.Error(), minted) {
			t.Errorf("fold minted %q for an upstream's protocol complaint: %v", minted, err)
		}
	}

	events := readAuditEvents(t, auditPath, "tools/call")
	if len(events) != 1 {
		t.Fatalf("tools/call audit events = %d, want 1: %+v", len(events), events)
	}
	if events[0].Outcome != audit.OutcomeError {
		t.Errorf("audit outcome = %q, want %q: %+v", events[0].Outcome, audit.OutcomeError, events[0])
	}
	if events[0].Upstream != "p" || events[0].Name != "p__tenant_echo" {
		t.Errorf("audit event = %+v, want upstream p / name p__tenant_echo", events[0])
	}
	// fold allowed the call; the upstream refused it. The audit event says so,
	// which is the distinction an operator needs to tell a policy problem from
	// a peer's complaint.
	if events[0].Decision != "allow" {
		t.Errorf("audit decision = %q, want allow — fold did not deny this", events[0].Decision)
	}
	if !strings.Contains(events[0].Error, "header mismatch") {
		t.Errorf("audit error = %q, want the upstream's complaint recorded", events[0].Error)
	}
}

// TestParamHeadersFollowASameHostRedirect pins the injection's host bound from
// the allowed side. fold follows same-host redirects, and each leg of one is a
// separate RoundTrip — so the relay has to happen per leg, not once per call.
func TestParamHeadersFollowASameHostRedirect(t *testing.T) {
	probe := newParamProbe(t)
	// A same-host front door: everything posted to /entry is redirected to the
	// real endpoint on the same origin.
	target, err := url.Parse(probe.url)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/entry") {
			http.Redirect(w, r, "/mcp", http.StatusTemporaryRedirect)
			return
		}
		proxy := *r.URL
		proxy.Scheme, proxy.Host = target.Scheme, target.Host
		proxy.Path = "/"
		out, err := http.NewRequestWithContext(r.Context(), r.Method, proxy.String(), r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out.Header = r.Header.Clone()
		resp, err := http.DefaultTransport.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		flusher, _ := w.(http.Flusher)
		buf := make([]byte, 4096)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				_, _ = w.Write(buf[:n])
				if flusher != nil {
					flusher.Flush()
				}
			}
			if rerr != nil {
				return
			}
		}
	}))
	t.Cleanup(front.Close)

	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{
		{ID: "p", Namespace: "p", URL: front.URL + "/entry"},
	}})
	session := connectWith(t, ts.URL+"/mcp", paramCallerHeaders())
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "p__echo"}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	got := probe.lastCall(t)
	if v := got.Get("Mcp-Param-Tenant"); v != "acme" {
		t.Errorf("after a same-host redirect, upstream Mcp-Param-Tenant = %q, want %q", v, "acme")
	}
	assertCallerOnlyHeadersStopped(t, "same-host redirect", got)
}

// TestParamHeadersDoNotRideACrossHostRedirect is the refused side of the same
// bound, and the reason the injection sits inside the upstreamHosts check
// beside credential attachment: these headers carry caller-supplied values,
// and nothing caller-supplied may ride a request to a host the upstream did
// not configure.
func TestParamHeadersDoNotRideACrossHostRedirect(t *testing.T) {
	var attackerSaw atomic.Value
	attackerSaw.Store(http.Header{})
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerSaw.Store(r.Header.Clone())
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(attacker.Close)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(upstream.Close)

	gw, err := New(&config.Config{Upstreams: []config.Upstream{{ID: "u", URL: upstream.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Close)

	// Drive the upstream client with a context that carries the caller's
	// param headers, exactly as the middleware would have prepared it. The
	// connect fails — the redirect is refused — but the point is what the
	// off-host target saw.
	ctx := withParamHeaders(context.Background(), paramCallerHeaders())
	u := gw.rt().upstreams[0]
	_, _ = u.connect(ctx, &mcp.ClientOptions{})

	saw := attackerSaw.Load().(http.Header)
	for name := range saw {
		if strings.HasPrefix(name, mcpParamPrefix) {
			t.Fatalf("param header %s=%q leaked to a cross-host redirect target", name, saw.Get(name))
		}
	}
	assertCallerOnlyHeadersStopped(t, "cross-host redirect", saw)
}

// TestParamHeaderInjectionIsHostBound pins the check itself, without a
// redirect: the transport relays only to a host the upstream configured.
// CheckRedirect keeps the case above from reaching RoundTrip at all, so this
// is the only place the defense-in-depth branch is exercised directly.
func TestParamHeaderInjectionIsHostBound(t *testing.T) {
	hosts := map[string]bool{"upstream.example:443": true}
	tr := newCredentialTransport(nil, false, hosts)
	tr.base = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    req,
		}, nil
	})
	ctx := withParamHeaders(context.Background(), paramCallerHeaders())

	for _, tc := range []struct {
		name   string
		target string
		want   string
	}{
		{"configured host", "https://upstream.example:443/mcp", "acme"},
		{"some other host", "https://elsewhere.example/mcp", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, tc.target, strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			var sent http.Header
			tr.base = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				sent = r.Header.Clone()
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{},
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    r,
				}, nil
			})
			resp, err := tr.RoundTrip(req)
			if err != nil {
				t.Fatalf("RoundTrip: %v", err)
			}
			_ = resp.Body.Close()
			if got := sent.Get("Mcp-Param-Tenant"); got != tc.want {
				t.Errorf("Mcp-Param-Tenant sent to %s = %q, want %q", tc.target, got, tc.want)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// TestInjectParamHeadersNeverOverwrites pins the precedence. When fold's own
// upstream SDK client has minted an Mcp-Param-* header from the upstream
// tool's schema, that value describes the call fold is actually making; the
// caller's copy describes the one fold received. The client's wins.
//
// This is a unit test because the generation happens on the client session
// that issues the call, and fold issues named invocations on a bridged
// session, which never lists tools and so has no schema to mint from. If that
// ever changes, the integration path exercises the same branch — this test
// keeps the branch pinned meanwhile.
func TestInjectParamHeadersNeverOverwrites(t *testing.T) {
	ctx := withParamHeaders(context.Background(), paramCallerHeaders())

	hdr := http.Header{}
	hdr.Set("Mcp-Param-Tenant", "from-the-sdk-client")
	injectParamHeaders(ctx, hdr)

	if got := hdr.Get("Mcp-Param-Tenant"); got != "from-the-sdk-client" {
		t.Errorf("Mcp-Param-Tenant = %q, want the SDK client's own value", got)
	}
	if got := hdr.Values("Mcp-Param-Tenant"); len(got) != 1 {
		t.Errorf("Mcp-Param-Tenant = %v, want exactly one value — the caller's must not be appended", got)
	}
	// Everything the client did not set is still relayed.
	if got := hdr.Get("Mcp-Param-Region"); got != "eu-west-1" {
		t.Errorf("Mcp-Param-Region = %q, want eu-west-1", got)
	}
	if got := strings.Join(hdr.Values("Mcp-Param-Trait"), ","); got != "alpha,beta" {
		t.Errorf("Mcp-Param-Trait = %q, want alpha,beta", got)
	}
	assertCallerOnlyHeadersStopped(t, "injectParamHeaders", hdr)
}

// TestInjectParamHeadersWithoutContext is the every-request case: no caller
// param headers means the outgoing headers are untouched.
func TestInjectParamHeadersWithoutContext(t *testing.T) {
	hdr := http.Header{"Content-Type": []string{"application/json"}}
	injectParamHeaders(context.Background(), hdr)
	if len(hdr) != 1 || hdr.Get("Content-Type") != "application/json" {
		t.Errorf("headers = %v, want them untouched", hdr)
	}
}

func TestWithParamHeaders(t *testing.T) {
	for _, tc := range []struct {
		name string
		hdr  http.Header
		want map[string][]string // nil means "the fast path: same ctx, nothing lifted"
	}{
		{name: "nil header map", hdr: nil},
		{name: "empty header map", hdr: http.Header{}},
		{
			name: "no matching headers",
			hdr:  http.Header{"Authorization": {"Bearer x"}, "Cookie": {"a=b"}, "X-Custom": {"y"}},
		},
		{
			name: "one match",
			hdr:  http.Header{"Mcp-Param-Tenant": {"acme"}, "Authorization": {"Bearer x"}},
			want: map[string][]string{"Mcp-Param-Tenant": {"acme"}},
		},
		{
			name: "several matches, one multi-valued",
			hdr: http.Header{
				"Mcp-Param-Tenant": {"acme"},
				"Mcp-Param-Region": {"eu-west-1"},
				"Mcp-Param-Trait":  {"alpha", "beta"},
				"X-Custom":         {"y"},
			},
			want: map[string][]string{
				"Mcp-Param-Tenant": {"acme"},
				"Mcp-Param-Region": {"eu-west-1"},
				"Mcp-Param-Trait":  {"alpha", "beta"},
			},
		},
		{
			// A hand-built map (or a non-SDK peer) need not be canonicalized;
			// the value is kept under the canonical key either way.
			name: "non-canonical key",
			hdr:  http.Header{"mcp-param-tenant": {"acme"}},
			want: map[string][]string{"Mcp-Param-Tenant": {"acme"}},
		},
		{
			name: "near misses are not the namespace",
			hdr: http.Header{
				"Mcp-Params-X":     {"no"}, // plural
				"Mcp-Param":        {"no"}, // no suffix
				"Mcp-Paramx-Y":     {"no"},
				"X-Mcp-Param-Ten":  {"no"}, // prefixed, so not at the head
				"Mcpparam-Tenant":  {"no"},
				"Mcp-Session-Id":   {"no"},
				"Mcp-Param-Suffix": {"yes"}, // the one real match, to prove the loop ran
			},
			want: map[string][]string{"Mcp-Param-Suffix": {"yes"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := context.Background()
			got := withParamHeaders(base, tc.hdr)

			if tc.want == nil {
				// The allocation-free path: nothing to carry, so nothing is
				// wrapped. Identity is the assertion — a new ctx here would
				// mean a per-request allocation on every request fold serves.
				if got != base {
					t.Fatalf("withParamHeaders returned a new context for %v; the no-match path must not wrap", tc.hdr)
				}
				return
			}
			if got == base {
				t.Fatalf("withParamHeaders did not carry %v", tc.hdr)
			}
			kept, ok := got.Value(paramHeaderKey{}).(http.Header)
			if !ok {
				t.Fatal("context carries no param headers")
			}
			if len(kept) != len(tc.want) {
				t.Fatalf("lifted %v, want %v", kept, tc.want)
			}
			for name, want := range tc.want {
				if got := strings.Join(kept.Values(name), ","); got != strings.Join(want, ",") {
					t.Errorf("%s = %q, want %q", name, got, strings.Join(want, ","))
				}
			}
		})
	}
}

// TestWithParamHeadersIsAllocationFreeWhenThereAreNone guards the common case:
// a request with no param headers — which is every request from a client that
// does not use the mechanism — must not pay for the feature. withParamHeaders
// runs on the proxy path for every request fold serves.
func TestWithParamHeadersIsAllocationFree(t *testing.T) {
	// A realistic inbound header set with nothing in the namespace.
	hdr := http.Header{
		"Accept":               {"application/json, text/event-stream"},
		"Authorization":        {"Bearer token"},
		"Content-Type":         {"application/json"},
		"Mcp-Method":           {"tools/call"},
		"Mcp-Name":             {"p__echo"},
		"Mcp-Protocol-Version": {"2026-07-28"},
		"Mcp-Session-Id":       {"abcdef"},
		"User-Agent":           {"param-client/1.0"},
	}
	ctx := context.Background()
	if got := testing.AllocsPerRun(100, func() {
		sinkCtx = withParamHeaders(ctx, hdr)
	}); got != 0 {
		t.Errorf("withParamHeaders allocated %v times per call with no param headers, want 0", got)
	}
	if sinkCtx != ctx {
		t.Error("withParamHeaders wrapped a context it had nothing to put in")
	}
}

var sinkCtx context.Context

// BenchmarkParamHeaders separates the two costs the relay adds to the proxy
// path: what every request pays (a scan of the inbound header map that finds
// nothing) from what only a request using the mechanism pays.
//
//	go test ./gateway -run '^$' -bench BenchmarkParamHeaders -benchmem
func BenchmarkParamHeaders(b *testing.B) {
	none := http.Header{
		"Accept":               {"application/json, text/event-stream"},
		"Authorization":        {"Bearer token"},
		"Content-Type":         {"application/json"},
		"Mcp-Method":           {"tools/call"},
		"Mcp-Name":             {"p__echo"},
		"Mcp-Protocol-Version": {"2026-07-28"},
		"Mcp-Session-Id":       {"abcdef"},
		"User-Agent":           {"param-client/1.0"},
	}
	some := none.Clone()
	some.Set("Mcp-Param-Tenant", "acme")
	some.Set("Mcp-Param-Region", "eu-west-1")
	ctx := context.Background()

	b.Run("lift/none", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			sinkCtx = withParamHeaders(ctx, none)
		}
	})
	b.Run("lift/some", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			sinkCtx = withParamHeaders(ctx, some)
		}
	})
	b.Run("inject/none", func(b *testing.B) {
		b.ReportAllocs()
		out := http.Header{}
		for b.Loop() {
			injectParamHeaders(ctx, out)
		}
	})
	b.Run("inject/some", func(b *testing.B) {
		b.ReportAllocs()
		carried := withParamHeaders(ctx, some)
		for b.Loop() {
			injectParamHeaders(carried, http.Header{})
		}
	})
}
