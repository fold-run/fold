package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

// The connection-owned `_meta` keys describe the connection a request arrived
// on. Behind a gateway there are two of those, so a caller that fills them in
// itself is describing a connection the upstream is not on — and the SDK
// believes the per-request values over the session's own handshake. These
// tests pin the strip in both directions, and pin that it stays surgical:
// every other `_meta` key is the caller's to send.

// vendorMetaKey is an ordinary third-party `_meta` key. It rides along in
// every forged payload below so a strip that turned into a wipe fails loudly.
const vendorMetaKey = "example.com/vendor"

// probeResourceURI is the resource the meta probe serves.
const probeResourceURI = "probe:///doc.txt"

// forgedConnectionMeta is what a caller sends when it wants the upstream to
// believe something about the connection that is not true: another client's
// identity and a capability set fold has no session to bridge.
//
// protocolVersion is deliberately absent. The SDK's streamable HTTP server
// refuses any request carrying `_meta.protocolVersion` on a stateful server
// (SEP-2575 header agreement), so that key cannot reach the router over the
// wire fold serves — it is covered in TestSanitizeRequestMeta and
// TestSanitizeRawMeta instead, where the strip is exercised directly.
func forgedConnectionMeta() mcp.Meta {
	return mcp.Meta{
		mcp.MetaKeyClientInfo: map[string]any{"name": "impersonator", "version": "9.9.9"},
		mcp.MetaKeyClientCapabilities: map[string]any{
			"sampling":    map[string]any{},
			"elicitation": map[string]any{},
		},
		vendorMetaKey: "keep",
	}
}

// metaProbe is a real SDK MCP server that records the `_meta` each of its
// handlers was actually handed. Recording is the only thing hand-rolled about
// it: the protocol on both sides is the SDK's.
type metaProbe struct {
	url string

	mu   sync.Mutex
	seen map[string]mcp.Meta
}

func newMetaProbe(t *testing.T) *metaProbe {
	t.Helper()
	p := &metaProbe{seen: map[string]mcp.Meta{}}
	server := mcp.NewServer(&mcp.Implementation{Name: "meta-probe", Version: "1.0"}, &mcp.ServerOptions{
		CompletionHandler: func(_ context.Context, req *mcp.CompleteRequest) (*mcp.CompleteResult, error) {
			p.record("completion/complete", req.Params.Meta)
			return &mcp.CompleteResult{Completion: mcp.CompletionResultDetails{Values: []string{"v"}}}, nil
		},
	})
	server.AddTool(&mcp.Tool{Name: "probe", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			p.record("tools/call", req.Params.Meta)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})
	server.AddPrompt(&mcp.Prompt{Name: "probe"},
		func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			p.record("prompts/get", req.Params.Meta)
			return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{
				{Role: "user", Content: &mcp.TextContent{Text: "hello"}},
			}}, nil
		})
	server.AddResource(&mcp.Resource{URI: probeResourceURI, Name: "doc", MIMEType: "text/plain"},
		func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			p.record("resources/read", req.Params.Meta)
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{
				{URI: req.Params.URI, MIMEType: "text/plain", Text: "body"},
			}}, nil
		})
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(ts.Close)
	p.url = ts.URL
	return p
}

func (p *metaProbe) record(method string, meta mcp.Meta) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen[method] = meta
}

func (p *metaProbe) metaFor(t *testing.T, method string) mcp.Meta {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	meta, ok := p.seen[method]
	if !ok {
		t.Fatalf("upstream never received %s", method)
	}
	return meta
}

// assertConnectionMetaStripped is the whole contract in one place: the
// connection-owned keys are gone, and everything else the caller sent is
// still there.
func assertConnectionMetaStripped(t *testing.T, method string, meta mcp.Meta) {
	t.Helper()
	for _, key := range []string{mcp.MetaKeyProtocolVersion, mcp.MetaKeyClientInfo, mcp.MetaKeyClientCapabilities} {
		if v, ok := meta[key]; ok {
			t.Errorf("%s: upstream received forged %q = %v", method, key, v)
		}
	}
	if got := meta[vendorMetaKey]; got != "keep" {
		t.Errorf("%s: vendor _meta key = %v, want %q — the strip must be surgical, not a wipe", method, got, "keep")
	}
}

// A caller cannot describe fold's upstream connection for it. The forged keys
// stop at the gateway; the vendor key in the same `_meta` goes through.
func TestForgedConnectionMetaStopsAtTheGateway(t *testing.T) {
	probe := newMetaProbe(t)
	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{
		{ID: "p", Namespace: "p", URL: probe.url},
	}})
	session := connect(t, ts.URL, nil)
	ctx := context.Background()

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "p__probe",
		Meta: forgedConnectionMeta(),
	}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	assertConnectionMetaStripped(t, "tools/call", probe.metaFor(t, "tools/call"))

	if _, err := session.GetPrompt(ctx, &mcp.GetPromptParams{
		Name: "p__probe",
		Meta: forgedConnectionMeta(),
	}); err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	assertConnectionMetaStripped(t, "prompts/get", probe.metaFor(t, "prompts/get"))

	// Resource URIs are opaque, so read the one the upstream listed.
	if _, err := session.ListResources(ctx, nil); err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if _, err := session.ReadResource(ctx, &mcp.ReadResourceParams{
		URI:  probeResourceURI,
		Meta: forgedConnectionMeta(),
	}); err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	assertConnectionMetaStripped(t, "resources/read", probe.metaFor(t, "resources/read"))

	if _, err := session.Complete(ctx, &mcp.CompleteParams{
		Ref:      &mcp.CompleteReference{Type: "ref/prompt", Name: "p__probe"},
		Argument: mcp.CompleteParamsArgument{Name: "x", Value: ""},
		Meta:     forgedConnectionMeta(),
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	assertConnectionMetaStripped(t, "completion/complete", probe.metaFor(t, "completion/complete"))
}

// The task methods forward the caller's bytes, and the SDK models none of
// them — so this path is the only one where nothing but fold can drop the
// forged keys. taskId has to survive the rewrite, or the strip would break
// routing.
func TestForgedConnectionMetaStopsOnTheTaskPath(t *testing.T) {
	var seen struct {
		mu  sync.Mutex
		raw json.RawMessage
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "task-meta-probe", Version: "1.0"}, nil)
	mcp.AddReceivingCustomMethod(server, "tasks/get",
		func(_ context.Context, _ *mcp.ServerSession, p *rawParams) (*rawResult, error) {
			seen.mu.Lock()
			seen.raw = append(json.RawMessage(nil), p.raw...)
			seen.mu.Unlock()
			id := extractTaskID(p.raw)
			return &rawResult{raw: json.RawMessage(`{"task":{"taskId":"` + id + `","status":"working"}}`)}, nil
		})
	up := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(up.Close)

	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{{ID: "t", URL: up.URL}}})
	session := taskClient(t, ts.URL, nil)

	if _, err := callTaskMethod(t, session, "tasks/get", map[string]any{
		"taskId": "task-1",
		"_meta": map[string]any{
			mcp.MetaKeyClientInfo:         map[string]any{"name": "impersonator", "version": "9.9.9"},
			mcp.MetaKeyClientCapabilities: map[string]any{"sampling": map[string]any{}},
			vendorMetaKey:                 "keep",
		},
	}); err != nil {
		t.Fatalf("tasks/get: %v", err)
	}

	seen.mu.Lock()
	raw := seen.raw
	seen.mu.Unlock()
	if len(raw) == 0 {
		t.Fatal("upstream never received tasks/get")
	}
	var got struct {
		TaskID string         `json:"taskId"`
		Meta   map[string]any `json:"_meta"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("upstream params %s: %v", raw, err)
	}
	if got.TaskID != "task-1" {
		t.Errorf("taskId = %q, want task-1 — the sanitizer must not disturb routing", got.TaskID)
	}
	for _, key := range []string{mcp.MetaKeyClientInfo, mcp.MetaKeyClientCapabilities} {
		if v, ok := got.Meta[key]; ok {
			t.Errorf("upstream received forged %q = %v on the task path", key, v)
		}
	}
	if got.Meta[vendorMetaKey] != "keep" {
		t.Errorf("vendor _meta key = %v, want %q", got.Meta[vendorMetaKey], "keep")
	}
}

// The reverse direction: an upstream identifying itself on its own connection
// must not reach the caller, who connected to fold. fold's own tag replaces
// it, and the upstream's other result meta is untouched.
func TestUpstreamServerInfoIsNotRelayed(t *testing.T) {
	stamped := func() mcp.Meta {
		return mcp.Meta{
			mcp.MetaKeyServerInfo: map[string]any{"name": "upstream-identity", "version": "2.0"},
			vendorMetaKey:         "keep",
		}
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "stamper", Version: "1.0"}, &mcp.ServerOptions{
		CompletionHandler: func(_ context.Context, _ *mcp.CompleteRequest) (*mcp.CompleteResult, error) {
			return &mcp.CompleteResult{
				Completion: mcp.CompletionResultDetails{Values: []string{"v"}},
				Meta:       stamped(),
			}, nil
		},
	})
	server.AddPrompt(&mcp.Prompt{Name: "probe"},
		func(_ context.Context, _ *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{
				{Role: "user", Content: &mcp.TextContent{Text: "hello"}},
			}}, nil
		})
	server.AddTool(&mcp.Tool{Name: "probe", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "ok"}},
				Meta:    stamped(),
			}, nil
		})
	up := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(up.Close)

	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{
		{ID: "s", Namespace: "s", URL: up.URL},
	}})
	session := connect(t, ts.URL, nil)

	out, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "s__probe"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if v, ok := out.Meta[mcp.MetaKeyServerInfo]; ok {
		t.Errorf("caller saw the upstream's %q = %v", mcp.MetaKeyServerInfo, v)
	}
	if out.Meta[metaUpstream] != "s" {
		t.Errorf("%s = %v, want %q", metaUpstream, out.Meta[metaUpstream], "s")
	}
	if out.Meta[vendorMetaKey] != "keep" {
		t.Errorf("vendor result meta = %v, want %q", out.Meta[vendorMetaKey], "keep")
	}

	// completion/complete returns the upstream's result without fold's own
	// tag, so it is the one named path where the strip cannot ride along with
	// the tagging — and the SDK fills fold's identity in only when the key is
	// absent, so a relayed one would also crowd fold's out.
	comp, err := session.Complete(context.Background(), &mcp.CompleteParams{
		Ref:      &mcp.CompleteReference{Type: "ref/prompt", Name: "s__probe"},
		Argument: mcp.CompleteParamsArgument{Name: "x", Value: ""},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if v, ok := comp.Meta[mcp.MetaKeyServerInfo]; ok {
		t.Errorf("caller saw the upstream's %q on a completion result = %v", mcp.MetaKeyServerInfo, v)
	}
	if comp.Meta[vendorMetaKey] != "keep" {
		t.Errorf("vendor completion meta = %v, want %q", comp.Meta[vendorMetaKey], "keep")
	}
}

// sameMeta reports whether two maps are the same map, not merely equal ones.
func sameMeta(a, b mcp.Meta) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

// The fast path is the contract as much as the strip is: a request carrying
// none of these keys — every request from a conforming client today — must
// come back as the very same map, because the proxy path's allocation profile
// is gated in CI. And the caller's map is never mutated: audit and the
// decision hook read it after the call.
func TestSanitizeRequestMeta(t *testing.T) {
	tests := []struct {
		name     string
		in       mcp.Meta
		identity bool
		want     mcp.Meta
	}{
		{name: "nil", in: nil, identity: true, want: nil},
		{name: "empty", in: mcp.Meta{}, identity: true, want: mcp.Meta{}},
		{
			name:     "no connection keys",
			in:       mcp.Meta{vendorMetaKey: "keep", "progressToken": "p1"},
			identity: true,
			want:     mcp.Meta{vendorMetaKey: "keep", "progressToken": "p1"},
		},
		{
			name: "protocol version",
			in:   mcp.Meta{mcp.MetaKeyProtocolVersion: "2026-07-28", vendorMetaKey: "keep"},
			want: mcp.Meta{vendorMetaKey: "keep"},
		},
		{
			name: "client info",
			in:   mcp.Meta{mcp.MetaKeyClientInfo: map[string]any{"name": "x"}, vendorMetaKey: "keep"},
			want: mcp.Meta{vendorMetaKey: "keep"},
		},
		{
			name: "client capabilities",
			in:   mcp.Meta{mcp.MetaKeyClientCapabilities: map[string]any{"sampling": map[string]any{}}, vendorMetaKey: "keep"},
			want: mcp.Meta{vendorMetaKey: "keep"},
		},
		{
			name: "all three",
			in: mcp.Meta{
				mcp.MetaKeyProtocolVersion:    "2026-07-28",
				mcp.MetaKeyClientInfo:         map[string]any{"name": "x"},
				mcp.MetaKeyClientCapabilities: map[string]any{},
				vendorMetaKey:                 "keep",
			},
			want: mcp.Meta{vendorMetaKey: "keep"},
		},
		{
			name: "only connection keys",
			in:   mcp.Meta{mcp.MetaKeyClientInfo: map[string]any{"name": "x"}},
			want: mcp.Meta{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := len(tc.in)
			got := sanitizeRequestMeta(tc.in)
			if same := sameMeta(tc.in, got); same != tc.identity {
				if tc.identity {
					t.Fatalf("sanitizeRequestMeta copied a map with nothing to strip — the allocation-free path is gated by the bench")
				}
				t.Fatalf("sanitizeRequestMeta returned the caller's own map after stripping it")
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("sanitizeRequestMeta = %v, want %v", got, tc.want)
			}
			if len(tc.in) != before {
				t.Errorf("input was mutated: %v", tc.in)
			}
		})
	}
}

// The raw-bytes twin, on the path where fold forwards the caller's JSON
// unread. Its fast paths matter for the same reason, and its failure mode has
// to be "hand the bytes back": refusing a task call over an unparsable `_meta`
// would be a new failure on a path whose contract is that fold does not
// interpret it.
func TestSanitizeRawMeta(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		want      string // "" means: unchanged, and the same backing bytes
		unchanged bool
	}{
		{
			name: "connection keys stripped, vendor and taskId survive",
			in:   `{"taskId":"t-1","_meta":{"io.modelcontextprotocol/clientInfo":{"name":"x"},"io.modelcontextprotocol/protocolVersion":"2026-07-28","example.com/vendor":"keep"}}`,
			want: `{"_meta":{"example.com/vendor":"keep"},"taskId":"t-1"}`,
		},
		{
			name:      "vendor keys only",
			in:        `{"taskId":"t-1","_meta":{"example.com/vendor":"keep"}}`,
			unchanged: true,
		},
		{
			name:      "no _meta at all",
			in:        `{"taskId":"t-1"}`,
			unchanged: true,
		},
		{
			// The namespace appears in a value, not as a key: the byte scan
			// admits it, the parse must then leave it alone.
			name:      "namespace mentioned outside _meta",
			in:        `{"taskId":"t-1","note":"io.modelcontextprotocol/clientInfo"}`,
			unchanged: true,
		},
		{
			// Same, but with a `_meta` present that holds none of the keys.
			// This one is re-encoded rather than handed back: once the blob is
			// on the parse path, what goes out has to be what fold inspected,
			// which is the duplicate-member case below.
			name: "namespace mentioned inside an unrelated _meta value",
			in:   `{"taskId":"t-1","_meta":{"example.com/vendor":"io.modelcontextprotocol/clientInfo"}}`,
			want: `{"_meta":{"example.com/vendor":"io.modelcontextprotocol/clientInfo"},"taskId":"t-1"}`,
		},
		{
			// A key spelled with an escape decodes to the key fold removes
			// while sharing none of its bytes, so the byte scan alone would
			// wave it through — one escaped letter would be the whole bypass.
			name: "connection key spelled with an escape",
			in:   `{"taskId":"t-1","_meta":{"\u0069o.modelcontextprotocol/clientInfo":{"name":"x"},"example.com/vendor":"keep"}}`,
			want: `{"_meta":{"example.com/vendor":"keep"},"taskId":"t-1"}`,
		},
		{
			// Duplicate members: Go's decoder keeps the last, so the check
			// reads a `_meta` with no connection key — while an upstream whose
			// parser keeps the first would read the one fold thought it had
			// removed. What leaves fold must be the single member fold
			// actually inspected.
			name: "duplicate _meta members",
			in:   `{"taskId":"t-1","_meta":{"io.modelcontextprotocol/clientInfo":{"name":"x"}},"_meta":{"example.com/vendor":"keep"}}`,
			want: `{"_meta":{"example.com/vendor":"keep"},"taskId":"t-1"}`,
		},
		{
			name:      "invalid JSON",
			in:        `{"taskId":"t-1","_meta":{"io.modelcontextprotocol/clientInfo":`,
			unchanged: true,
		},
		{
			name:      "unparsable _meta",
			in:        `{"taskId":"t-1","_meta":"io.modelcontextprotocol/clientInfo"}`,
			unchanged: true,
		},
		{
			name: "emptied _meta is removed entirely",
			in:   `{"taskId":"t-1","_meta":{"io.modelcontextprotocol/clientCapabilities":{}}}`,
			want: `{"taskId":"t-1"}`,
		},
		{
			name:      "empty params",
			in:        ``,
			unchanged: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := json.RawMessage(tc.in)
			got := sanitizeRawMeta(in)
			if tc.unchanged {
				if string(got) != tc.in {
					t.Fatalf("sanitizeRawMeta rewrote %s to %s", tc.in, got)
				}
				if len(in) > 0 && &got[0] != &in[0] {
					t.Errorf("sanitizeRawMeta copied bytes it had nothing to strip from")
				}
				return
			}
			if string(got) != tc.want {
				t.Errorf("sanitizeRawMeta(%s) = %s, want %s", tc.in, got, tc.want)
			}
			if string(in) != tc.in {
				t.Errorf("input bytes were mutated: %s", in)
			}
		})
	}
}

// The sanitizer runs on every task method, and a task id it cannot find is
// still routed (by probe) rather than rejected — so a params object whose
// `_meta` fold rewrote must still parse as the caller wrote it.
func TestSanitizeRawMetaKeepsParamsParseable(t *testing.T) {
	in := json.RawMessage(`{"taskId":"t-9","cursor":"abc","_meta":{"io.modelcontextprotocol/clientInfo":{"name":"x"},"progressToken":7}}`)
	out := sanitizeRawMeta(in)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("sanitized params no longer parse: %v (%s)", err, out)
	}
	if got["taskId"] != "t-9" || got["cursor"] != "abc" {
		t.Errorf("sanitized params lost fields: %s", out)
	}
	meta, _ := got["_meta"].(map[string]any)
	if _, ok := meta[mcp.MetaKeyClientInfo]; ok {
		t.Errorf("clientInfo survived: %s", out)
	}
	if meta["progressToken"] != float64(7) {
		t.Errorf("progressToken did not survive: %s", out)
	}
	if strings.Contains(string(out), metaKeyPrefix) {
		t.Errorf("a connection-namespaced key survived: %s", out)
	}
}
