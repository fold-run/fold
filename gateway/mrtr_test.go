package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

// A multi-round-trip request (SEP-2322) that needs more input is not blocked
// on: the upstream answers with the questions it wants answered and an opaque
// `requestState`, and the client replies by retrying the *same* request with
// `inputResponses` filled in and that state echoed back. The specification
// requires the client to "echo back the exact value" of the field and forbids
// it from inspecting, parsing, modifying, or assuming anything about the
// contents.
//
// fold is on both ends of that sentence — the server receiving the retry and
// the client forwarding it — and the three methods the pattern applies to
// (`tools/call`, `prompts/get`, `resources/read`) are the three where fold
// builds fresh params rather than forwarding what it was handed. A retry that
// arrives upstream without its continuation looks like a first attempt, so the
// upstream asks the same questions again, and the exchange loops until the
// client's retry ceiling — re-prompting a human on every pass.
//
// These tests assert what the upstream *observed*, which is the only place the
// loss is visible: every field is optional on the wire, so dropping one leaves
// a well-formed request that simply means something else.

// mrtrRequestState is an upstream's continuation token. Deliberately not a
// tidy identifier: fold must not normalise, trim, re-encode or otherwise
// improve a value it is forbidden to look at.
const mrtrRequestState = `step=2;cursor=eyJvZmZzZXQiOjF9`

// mrtrCall is what a probe upstream saw of one request: the name it was asked
// for after fold's rewriting, and the continuation that should have travelled
// with it untouched.
type mrtrCall struct {
	name      string
	state     string
	responses mcp.InputResponseMap
	meta      mcp.Meta
}

// mrtrProbe is a real SDK MCP server exposing the three multi-round-trip
// methods. Recording the params each handler was handed is the only
// hand-rolled thing about it; the protocol on both sides is the SDK's.
type mrtrProbe struct {
	id  string
	url string
	// uiURI is the interface resource, published at the URI the MCP Apps
	// starter templates ship — so two probes collide on it and fold mints
	// both, which is what puts a read on the minted branch of readResource.
	uiURI string
	// plainURI is an ordinary resource, whose URI fold never rewrites. Reads
	// of it take the affinity branch instead.
	plainURI string

	mu    sync.Mutex
	calls map[string]mrtrCall
	total int
}

func newMRTRProbe(t *testing.T, id string) *mrtrProbe {
	t.Helper()
	p := &mrtrProbe{
		id:       id,
		uiURI:    collidingUIURI,
		plainURI: "probe:///" + id + "/doc.txt",
		calls:    map[string]mrtrCall{},
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "mrtr-probe-" + id, Version: "1.0"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        "echo",
		Description: "records the continuation it was called with",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		p.record("tools/call", req.Params.Name, mrtrCall{
			name:      req.Params.Name,
			state:     req.Params.RequestState,
			responses: req.Params.InputResponses,
			meta:      req.Params.Meta,
		})
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done:" + id}}}, nil
	})
	server.AddPrompt(&mcp.Prompt{Name: "confirm"},
		func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			p.record("prompts/get", req.Params.Name, mrtrCall{
				name:      req.Params.Name,
				state:     req.Params.RequestState,
				responses: req.Params.InputResponses,
				meta:      req.Params.Meta,
			})
			return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{
				{Role: "user", Content: &mcp.TextContent{Text: "done:" + id}},
			}}, nil
		})
	readHandler := func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		p.record("resources/read", req.Params.URI, mrtrCall{
			name:      req.Params.URI,
			state:     req.Params.RequestState,
			responses: req.Params.InputResponses,
			meta:      req.Params.Meta,
		})
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{
			{URI: req.Params.URI, MIMEType: "text/plain", Text: "done:" + id},
		}}, nil
	}
	server.AddResource(&mcp.Resource{URI: p.uiURI, Name: "interface", MIMEType: "text/html;profile=mcp-app"}, readHandler)
	server.AddResource(&mcp.Resource{URI: p.plainURI, Name: "doc", MIMEType: "text/plain"}, readHandler)

	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(ts.Close)
	p.url = ts.URL
	return p
}

func (p *mrtrProbe) record(method, name string, c mrtrCall) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls[method+" "+name] = c
	p.total++
}

// call returns what the probe saw of the last request for method and name,
// failing when it saw none — a retry that never arrived is the same bug as one
// that arrived empty.
func (p *mrtrProbe) call(t *testing.T, method, name string) mrtrCall {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.calls[method+" "+name]
	if !ok {
		t.Fatalf("upstream %q never received %s %q (saw %d requests)", p.id, method, name, p.total)
	}
	return c
}

func (p *mrtrProbe) requests() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.total
}

// mrtrInputResponses is a filled-in answer set: the two response shapes the
// SDK can carry, one of them nested and one of them a list, so a forwarding
// that flattened or dropped part of the map fails rather than passing on the
// key count.
func mrtrInputResponses() mcp.InputResponseMap {
	return mcp.InputResponseMap{
		"confirm": &mcp.ElicitResult{
			Action: "accept",
			Content: map[string]any{
				"approve": true,
				"count":   float64(3),
				"note":    "ship it",
			},
		},
		"roots": &mcp.ListRootsResult{Roots: []*mcp.Root{
			{URI: "file:///work", Name: "work"},
			{URI: "file:///tmp"},
		}},
	}
}

// mrtrJSON renders an answer set the way the wire carries it, so "the upstream
// received what the caller sent" can be compared as bytes rather than through
// the interface types the SDK reconstructs on each hop.
func mrtrJSON(t *testing.T, m mcp.InputResponseMap) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal inputResponses: %v", err)
	}
	return string(b)
}

// assertContinuationForwarded is the whole contract in one place: the
// continuation the caller sent is the continuation the upstream saw.
func assertContinuationForwarded(t *testing.T, method string, got mrtrCall, wantState string) {
	t.Helper()
	if got.state != wantState {
		t.Errorf("%s: upstream saw requestState %q, want %q — a retry stripped of its state is a first attempt, and the exchange loops",
			method, got.state, wantState)
	}
	want := mrtrJSON(t, mrtrInputResponses())
	if have := mrtrJSON(t, got.responses); have != want {
		t.Errorf("%s: upstream saw inputResponses %s, want %s", method, have, want)
	}
}

// mrtrGateway federates two probes, both publishing the same ui:// URI, so
// every test here has a namespace to strip and a collision to mint through.
func mrtrGateway(t *testing.T) (*mcp.ClientSession, *mrtrProbe, *mrtrProbe) {
	t.Helper()
	app := newMRTRProbe(t, "app")
	other := newMRTRProbe(t, "other")
	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{
		{ID: "app", URL: app.url, Namespace: "app"},
		{ID: "other", URL: other.url, Namespace: "other"},
	}})
	return connect(t, ts.URL, nil), app, other
}

// tools/call. The retry has to arrive at the upstream the namespace named,
// under the bare tool name, carrying the continuation — all three at once. A
// fix that preserved the state while breaking the rewrite, or routed correctly
// while rebuilding the params empty, is not a fix.
func TestMultiRoundTripRetryReachesToolUpstream(t *testing.T) {
	session, app, other := mrtrGateway(t)

	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:           "app__echo",
		Arguments:      map[string]any{"x": 1},
		InputResponses: mrtrInputResponses(),
		RequestState:   mrtrRequestState,
	}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	got := app.call(t, "tools/call", "echo")
	if got.name != "echo" {
		t.Errorf("upstream saw tool %q, want the bare %q", got.name, "echo")
	}
	assertContinuationForwarded(t, "tools/call", got, mrtrRequestState)
	if n := other.requests(); n != 0 {
		t.Errorf("the other upstream received %d requests, want 0 — the retry went to the wrong server", n)
	}
}

// prompts/get, the same shape.
func TestMultiRoundTripRetryReachesPromptUpstream(t *testing.T) {
	session, app, other := mrtrGateway(t)

	if _, err := session.GetPrompt(context.Background(), &mcp.GetPromptParams{
		Name:           "app__confirm",
		InputResponses: mrtrInputResponses(),
		RequestState:   mrtrRequestState,
	}); err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}

	got := app.call(t, "prompts/get", "confirm")
	if got.name != "confirm" {
		t.Errorf("upstream saw prompt %q, want the bare %q", got.name, "confirm")
	}
	assertContinuationForwarded(t, "prompts/get", got, mrtrRequestState)
	if n := other.requests(); n != 0 {
		t.Errorf("the other upstream received %d requests, want 0 — the retry went to the wrong server", n)
	}
}

// resources/read on a minted ui:// URI. This is the branch that rebuilt its
// params: the other two read branches forward what they were given and were
// never at risk, so a test that reads any old resource passes whether the
// branch is fixed or not.
//
// The URI the upstream saw is what pins the branch. Only the minted path
// rewrites it back to the upstream's own `ui://widget/main`; affinity and
// probe would have forwarded `ui://fold/app/...` unchanged.
func TestMultiRoundTripRetryReachesMintedUIResource(t *testing.T) {
	session, app, other := mrtrGateway(t)
	minted := "ui://fold/app/widget/main"

	res, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{
		URI:            minted,
		InputResponses: mrtrInputResponses(),
		RequestState:   mrtrRequestState,
	})
	if err != nil {
		t.Fatalf("ReadResource %s: %v", minted, err)
	}
	if got := res.Contents[0].Text; got != "done:app" {
		t.Fatalf("read answered %q, want the app upstream's %q", got, "done:app")
	}

	got := app.call(t, "resources/read", collidingUIURI)
	if got.name != collidingUIURI {
		t.Errorf("upstream saw URI %q, want its own %q — this read did not take the minted branch", got.name, collidingUIURI)
	}
	assertContinuationForwarded(t, "resources/read", got, mrtrRequestState)
	if n := other.requests(); n != 0 {
		t.Errorf("the other upstream received %d requests, want 0 — the minted URI names its owner", n)
	}
}

// The affinity branch, for contrast and against regression: an ordinary URI is
// forwarded whole, so its continuation was never dropped. Reading it through
// the same gateway as the minted case is what makes the pair meaningful —
// before the fix these two disagreed.
func TestMultiRoundTripRetryReachesPlainResource(t *testing.T) {
	session, app, _ := mrtrGateway(t)
	ctx := context.Background()

	// List first so the read takes the affinity branch rather than the probe.
	if _, err := session.ListResources(ctx, nil); err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if _, err := session.ReadResource(ctx, &mcp.ReadResourceParams{
		URI:            app.plainURI,
		InputResponses: mrtrInputResponses(),
		RequestState:   mrtrRequestState,
	}); err != nil {
		t.Fatalf("ReadResource %s: %v", app.plainURI, err)
	}

	assertContinuationForwarded(t, "resources/read", app.call(t, "resources/read", app.plainURI), mrtrRequestState)
}

// The continuation and the `_meta` strip run over the same params object, and
// each has broken the other before: the strip copies the params, the rebuild
// replaced them. A caller sending a forged connection key alongside a retry
// must have the key removed and the retry left alone — both, on every one of
// the three methods.
func TestMultiRoundTripContinuationSurvivesConnectionMetaStrip(t *testing.T) {
	session, app, _ := mrtrGateway(t)
	ctx := context.Background()

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:           "app__echo",
		Meta:           forgedConnectionMeta(),
		InputResponses: mrtrInputResponses(),
		RequestState:   mrtrRequestState,
	}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	toolCall := app.call(t, "tools/call", "echo")
	assertConnectionMetaStripped(t, "tools/call", toolCall.meta)
	assertContinuationForwarded(t, "tools/call", toolCall, mrtrRequestState)

	if _, err := session.GetPrompt(ctx, &mcp.GetPromptParams{
		Name:           "app__confirm",
		Meta:           forgedConnectionMeta(),
		InputResponses: mrtrInputResponses(),
		RequestState:   mrtrRequestState,
	}); err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	promptCall := app.call(t, "prompts/get", "confirm")
	assertConnectionMetaStripped(t, "prompts/get", promptCall.meta)
	assertContinuationForwarded(t, "prompts/get", promptCall, mrtrRequestState)

	// The minted branch again: the one that both copies for the strip and
	// rebuilds for the mint.
	if _, err := session.ReadResource(ctx, &mcp.ReadResourceParams{
		URI:            "ui://fold/app/widget/main",
		Meta:           forgedConnectionMeta(),
		InputResponses: mrtrInputResponses(),
		RequestState:   mrtrRequestState,
	}); err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	readCall := app.call(t, "resources/read", collidingUIURI)
	assertConnectionMetaStripped(t, "resources/read", readCall.meta)
	assertContinuationForwarded(t, "resources/read", readCall, mrtrRequestState)
}

// The state is opaque, and opaque means fold does not improve it. Whitespace
// is not trimmed, JSON-ish text is not re-encoded, and non-ASCII is not
// normalised — the specification forbids a client from inspecting, parsing,
// modifying or assuming anything about the contents, and the only way to obey
// a rule like that is to carry the bytes.
func TestMultiRoundTripRequestStateIsOpaque(t *testing.T) {
	session, app, _ := mrtrGateway(t)
	ctx := context.Background()

	for _, tc := range []struct{ name, state string }{
		{"leading and trailing whitespace", "  step=2\t\n"},
		{"json object", `{"step":2,"nonce":"a b","nested":{"k":[1,2]}}`},
		{"json array", `[1,"two",{"three":3}]`},
		{"quotes and backslashes", `he said "\\resume" \ "` + "\x00" + `"`},
		{"non-ascii", "état-2 ✓ 日本語 🙂"},
		{"combining marks not normalised", "état"},
		{"looks like a namespaced name", "app__echo"},
		{"looks like a minted uri", "ui://fold/other/widget/main"},
		{"longer than a frame", longRequestState()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := session.CallTool(ctx, &mcp.CallToolParams{
				Name:           "app__echo",
				InputResponses: mrtrInputResponses(),
				RequestState:   tc.state,
			}); err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			assertContinuationForwarded(t, "tools/call", app.call(t, "tools/call", "echo"), tc.state)

			if _, err := session.GetPrompt(ctx, &mcp.GetPromptParams{
				Name:           "app__confirm",
				InputResponses: mrtrInputResponses(),
				RequestState:   tc.state,
			}); err != nil {
				t.Fatalf("GetPrompt: %v", err)
			}
			assertContinuationForwarded(t, "prompts/get", app.call(t, "prompts/get", "confirm"), tc.state)

			if _, err := session.ReadResource(ctx, &mcp.ReadResourceParams{
				URI:            "ui://fold/app/widget/main",
				InputResponses: mrtrInputResponses(),
				RequestState:   tc.state,
			}); err != nil {
				t.Fatalf("ReadResource: %v", err)
			}
			assertContinuationForwarded(t, "resources/read", app.call(t, "resources/read", collidingUIURI), tc.state)
		})
	}
}

// longRequestState is larger than any framing boundary the transports use, so
// a truncation shows up as a mismatch rather than as a passing prefix.
func longRequestState() string {
	b := make([]byte, 16*1024)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return string(b)
}

// The whole exchange, end to end, with an upstream that really does ask for
// more input. The failure this pins is the one dropping the continuation
// produces: the upstream cannot tell a retry from a first attempt, so it asks
// the same question again and the exchange runs to the SDK's retry ceiling —
// ten round trips upstream and ten prompts at the human.
//
// The counts are the assertion. Two attempts and one prompt is the pattern
// working; anything more is the loop. It holds whichever leg runs the retry,
// so it survives a change of mind about where fold answers input requests.
func TestMultiRoundTripCompletesInOneRetry(t *testing.T) {
	var mu sync.Mutex
	var attempts []mrtrCall

	upstream := mcp.NewServer(&mcp.Implementation{Name: "mrtr-loop", Version: "1.0"}, nil)
	upstream.AddTool(&mcp.Tool{
		Name:        "deploy",
		Description: "asks before it acts",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		mu.Lock()
		attempts = append(attempts, mrtrCall{
			name:      req.Params.Name,
			state:     req.Params.RequestState,
			responses: req.Params.InputResponses,
		})
		mu.Unlock()
		// The upstream's own test for "have I been answered yet". It is the
		// test every MRTR server makes, and the reason a dropped continuation
		// is an infinite regress rather than a wrong answer.
		if len(req.Params.InputResponses) == 0 {
			return &mcp.CallToolResult{
				InputRequests: mcp.InputRequestMap{
					"confirm": &mcp.ElicitParams{Message: "deploy to production?"},
				},
				RequestState: "deployment-123",
			}, nil
		}
		answer, ok := req.Params.InputResponses["confirm"].(*mcp.ElicitResult)
		if !ok || answer.Action != "accept" {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{
				&mcp.TextContent{Text: "unusable answer"},
			}}, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: "deployed with state " + req.Params.RequestState},
		}}, nil
	})
	up := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return upstream }, nil))
	t.Cleanup(up.Close)

	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{
		{ID: "app", URL: up.URL, Namespace: "app"},
	}})

	var prompts int
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0"}, &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			mu.Lock()
			prompts++
			mu.Unlock()
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": true}}, nil
		},
	})
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "app__deploy"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got := res.Content[0].(*mcp.TextContent).Text; got != "deployed with state deployment-123" {
		t.Errorf("call answered %q, want the completed deployment", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 2 {
		t.Fatalf("upstream saw %d attempts, want 2 — an unanswered upstream asks again until the retry ceiling", len(attempts))
	}
	if attempts[0].state != "" || len(attempts[0].responses) != 0 {
		t.Errorf("first attempt already carried a continuation: state=%q responses=%d",
			attempts[0].state, len(attempts[0].responses))
	}
	if attempts[1].state != "deployment-123" {
		t.Errorf("retry carried requestState %q, want the upstream's own %q", attempts[1].state, "deployment-123")
	}
	if answer, ok := attempts[1].responses["confirm"].(*mcp.ElicitResult); !ok || answer.Action != "accept" {
		t.Errorf("retry carried inputResponses %#v, want the accepted answer", attempts[1].responses)
	}
	if prompts != 1 {
		t.Errorf("the human was asked %d times, want 1 — every extra round trip is another prompt", prompts)
	}
}
