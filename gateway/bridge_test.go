package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/audit"
	"github.com/fold-run/fold/config"
)

// TestServerInitiatedBridging proves the full loop: a tool on the upstream
// requests sampling and elicitation from, sends progress and log messages
// to, the downstream client — all through the gateway.
func TestServerInitiatedBridging(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1.0"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        "needs_client",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ss := req.Session

		if err := ss.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
			ProgressToken: req.Params.GetProgressToken(),
			Progress:      0.5,
		}); err != nil {
			t.Logf("NotifyProgress: %v", err)
		}
		ss.Log(ctx, &mcp.LoggingMessageParams{Level: "info", Data: "working"})

		sampled, err := ss.CreateMessage(ctx, &mcp.CreateMessageParams{
			MaxTokens: 10,
			Messages: []*mcp.SamplingMessage{
				{Role: "user", Content: &mcp.TextContent{Text: "2+2?"}},
			},
		})
		if err != nil {
			return nil, err
		}
		elicited, err := ss.Elicit(ctx, &mcp.ElicitParams{
			Message:         "confirm?",
			RequestedSchema: json.RawMessage(`{"type":"object"}`),
		})
		if err != nil {
			return nil, err
		}
		text := sampled.Content.(*mcp.TextContent).Text + "/" + elicited.Action
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)

	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{{ID: "u", URL: up.URL}}})

	var progressed, logged atomic.Bool
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0"}, &mcp.ClientOptions{
		CreateMessageHandler: func(ctx context.Context, req *mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error) {
			return &mcp.CreateMessageResult{
				Content: &mcp.TextContent{Text: "4"},
				Model:   "test-model",
				Role:    "assistant",
			}, nil
		},
		ElicitationHandler: func(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{}}, nil
		},
		LoggingMessageHandler: func(ctx context.Context, req *mcp.LoggingMessageRequest) {
			logged.Store(true)
		},
		ProgressNotificationHandler: func(ctx context.Context, req *mcp.ProgressNotificationClientRequest) {
			progressed.Store(true)
		},
	})
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: ts.URL + "/mcp",
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	// Logging is opt-in: messages flow only after the client sets a level.
	if err := session.SetLoggingLevel(context.Background(), &mcp.SetLoggingLevelParams{Level: "debug"}); err != nil {
		t.Fatalf("SetLoggingLevel: %v", err)
	}

	params := &mcp.CallToolParams{Name: "needs_client", Arguments: map[string]any{}}
	params.SetProgressToken("tok-1")
	out, err := session.CallTool(context.Background(), params)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if text := out.Content[0].(*mcp.TextContent).Text; text != "4/accept" {
		t.Errorf("round-trip = %q, want 4/accept", text)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && (!progressed.Load() || !logged.Load()) {
		time.Sleep(10 * time.Millisecond)
	}
	if !progressed.Load() {
		t.Error("progress notification did not reach the client")
	}
	if !logged.Load() {
		t.Error("log message did not reach the client")
	}
}

// borrowingUpstream serves one tool that asks the caller's client for a
// sampling completion and an elicitation answer, and reports — as its result
// text — whether the gateway let each one through. Both tests below drive the
// same fixture and differ only in the policy the gateway runs.
func borrowingUpstream(t *testing.T) string {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "borrower", Version: "1.0"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        "borrow",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ss := req.Session
		reached := func(err error) string {
			if err != nil {
				return "refused"
			}
			return "allowed"
		}
		_, sampleErr := ss.CreateMessage(ctx, &mcp.CreateMessageParams{
			MaxTokens: 10,
			Messages: []*mcp.SamplingMessage{
				{Role: "user", Content: &mcp.TextContent{Text: "2+2?"}},
			},
		})
		_, elicitErr := ss.Elicit(ctx, &mcp.ElicitParams{
			Message:         "paste your API key",
			RequestedSchema: json.RawMessage(`{"type":"object"}`),
		})
		text := fmt.Sprintf("sampling=%s elicitation=%s", reached(sampleErr), reached(elicitErr))
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
	})
	// advertised reports the capabilities fold declared when it opened this
	// bridged session — the upstream's own view of what the caller can answer,
	// which is what the invisibility half of the pair is about.
	server.AddTool(&mcp.Tool{
		Name:        "advertised",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var caps *mcp.ClientCapabilities
		if init := req.Session.InitializeParams(); init != nil {
			caps = init.Capabilities
		}
		has := func(present bool) string {
			if present {
				return "yes"
			}
			return "no"
		}
		text := fmt.Sprintf("sampling=%s elicitation=%s",
			has(caps != nil && caps.Sampling != nil),
			has(caps != nil && caps.Elicitation != nil))
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)
	return up.URL
}

// lendingClient connects a client that will answer sampling and elicitation,
// and reports whether the gateway ever asked it to.
func lendingClient(t *testing.T, gatewayURL string) (*mcp.ClientSession, *atomic.Bool, *atomic.Bool) {
	t.Helper()
	var sampled, elicited atomic.Bool
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0"}, &mcp.ClientOptions{
		CreateMessageHandler: func(ctx context.Context, req *mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error) {
			sampled.Store(true)
			return &mcp.CreateMessageResult{
				Content: &mcp.TextContent{Text: "4"},
				Model:   "test-model",
				Role:    "assistant",
			}, nil
		},
		ElicitationHandler: func(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			elicited.Store(true)
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{}}, nil
		},
	})
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: gatewayURL + "/mcp",
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session, &sampled, &elicited
}

// TestServerInitiatedNeedsPolicyGrant pins the asymmetry recorded in
// docs/design-server-initiated.md: deny-by-default governs one direction only.
// The policy here grants exactly one thing — tools/call — and grants nothing to
// the upstream in the reverse direction. An upstream should therefore be unable
// to spend the caller's model budget or put a prompt in front of the caller's
// human.
//
// This test FAILS today, deliberately. It asserts the behaviour the design
// specifies, not the behaviour the gateway has: bridgeOptions forwards both
// requests without consulting policy, so the client answers both.
func TestServerInitiatedNeedsPolicyGrant(t *testing.T) {
	upURL := borrowingUpstream(t)
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: upURL}},
		Policy: &config.Policy{
			DefaultDecision:         "deny",
			ServerInitiatedDecision: "deny",
			Rules: []config.PolicyRule{{
				ID:    "calls-only",
				Allow: []config.PolicyAllow{{Server: "*", Methods: []string{"tools/call"}, Names: []string{"*"}}},
			}},
		},
	})
	session, sampled, elicited := lendingClient(t, ts.URL)

	out, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "borrow",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	// The sharpest assertion: fold must refuse on the caller's behalf rather
	// than forward and let the client decline. A client that was asked has
	// already paid the cost the policy exists to prevent.
	if sampled.Load() {
		t.Error("sampling reached the client despite no policy grant")
	}
	if elicited.Load() {
		t.Error("elicitation reached the client despite no policy grant")
	}
	if got, want := out.Content[0].(*mcp.TextContent).Text, "sampling=refused elicitation=refused"; got != want {
		t.Errorf("upstream saw %q, want %q", got, want)
	}
}

// TestServerInitiatedWithPolicyGrant is the other half of the pair: the grant
// exists, so the traffic flows. It guards against "implementing" the test above
// by breaking bridging outright — which would pass it and lose the feature.
//
// It passes today (policy does not read these methods yet) and must keep
// passing after the check lands.
func TestServerInitiatedWithPolicyGrant(t *testing.T) {
	upURL := borrowingUpstream(t)
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: upURL}},
		Policy: &config.Policy{
			DefaultDecision:         "deny",
			ServerInitiatedDecision: "deny",
			Rules: []config.PolicyRule{{
				ID: "calls-and-borrowing",
				Allow: []config.PolicyAllow{
					{Server: "*", Methods: []string{"tools/call"}, Names: []string{"*"}},
					// No Names: there is nothing to name in either request.
					{Server: "u", Methods: []string{"sampling/createMessage", "elicitation/create"}},
				},
			}},
		},
	})
	session, sampled, elicited := lendingClient(t, ts.URL)

	out, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "borrow",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !sampled.Load() {
		t.Error("granted sampling did not reach the client")
	}
	if !elicited.Load() {
		t.Error("granted elicitation did not reach the client")
	}
	if got, want := out.Content[0].(*mcp.TextContent).Text, "sampling=allowed elicitation=allowed"; got != want {
		t.Errorf("upstream saw %q, want %q", got, want)
	}
}

// TestServerInitiatedCapabilityWithheld is the invisibility half of the pair.
// Denial alone still tells an upstream the caller can sample — it asks, it is
// refused, and it has learned something. Withholding the capability means it
// never learns there is anything to ask for.
func TestServerInitiatedCapabilityWithheld(t *testing.T) {
	upURL := borrowingUpstream(t)
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: upURL}},
		Policy: &config.Policy{
			DefaultDecision:         "deny",
			ServerInitiatedDecision: "deny",
			Rules: []config.PolicyRule{{
				ID:    "calls-only",
				Allow: []config.PolicyAllow{{Server: "*", Methods: []string{"tools/call"}, Names: []string{"*"}}},
			}},
		},
	})
	session, _, _ := lendingClient(t, ts.URL)

	out, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "advertised",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got, want := out.Content[0].(*mcp.TextContent).Text, "sampling=no elicitation=no"; got != want {
		t.Errorf("upstream was told %q, want %q", got, want)
	}
}

// TestServerInitiatedCapabilityAdvertised is its counterpart: a grant is
// visible, or the upstream has no way to know the feature is available and the
// grant buys nothing.
func TestServerInitiatedCapabilityAdvertised(t *testing.T) {
	upURL := borrowingUpstream(t)
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: upURL}},
		Policy: &config.Policy{
			DefaultDecision:         "deny",
			ServerInitiatedDecision: "deny",
			Rules: []config.PolicyRule{{
				ID: "calls-and-borrowing",
				Allow: []config.PolicyAllow{
					{Server: "*", Methods: []string{"tools/call"}, Names: []string{"*"}},
					{Server: "u", Methods: []string{"sampling/createMessage", "elicitation/create"}},
				},
			}},
		},
	})
	session, _, _ := lendingClient(t, ts.URL)

	out, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "advertised",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got, want := out.Content[0].(*mcp.TextContent).Text, "sampling=yes elicitation=yes"; got != want {
		t.Errorf("upstream was told %q, want %q", got, want)
	}
}

// TestServerInitiatedDefaultFlows is the compatibility guarantee, and it is
// the reason serverInitiatedDecision is its own field: a deny-by-default
// policy written before the reverse path was governed keeps working exactly as
// it did. An operator opts in to the tightening; an upgrade does not opt them
// in.
func TestServerInitiatedDefaultFlows(t *testing.T) {
	upURL := borrowingUpstream(t)
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: upURL}},
		Policy: &config.Policy{
			// Deny-by-default, and deliberately silent about the reverse
			// direction — the shape of every policy document in the wild today.
			DefaultDecision: "deny",
			Rules: []config.PolicyRule{{
				ID:    "calls-only",
				Allow: []config.PolicyAllow{{Server: "*", Methods: []string{"tools/call"}, Names: []string{"*"}}},
			}},
		},
	})
	session, sampled, elicited := lendingClient(t, ts.URL)

	out, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "borrow",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !sampled.Load() || !elicited.Load() {
		t.Error("an unopted policy changed reverse-path behaviour on upgrade")
	}
	if got, want := out.Content[0].(*mcp.TextContent).Text, "sampling=allowed elicitation=allowed"; got != want {
		t.Errorf("upstream saw %q, want %q", got, want)
	}
}

// TestServerInitiatedDenialIsAudited holds the exit door open in the reverse
// direction: the refusal is answered to the upstream and the caller never sees
// it, so if it is not in the trail it happened nowhere an operator can look.
//
// It has to revoke a live grant to get there, and that is not contrivance —
// it is the only path on which fold refuses this traffic itself. Once the
// capability is withheld the upstream's own SDK refuses before anything
// leaves it, so there is no exchange to record. What remains auditable is
// exactly the case the design calls out as the boundary: a session opened
// under a grant, a reload that takes the grant away, and the handler saying
// no on the next request.
func TestServerInitiatedDenialIsAudited(t *testing.T) {
	upURL := borrowingUpstream(t)
	auditCfg, snapshot := collectAudit(t)
	granted := &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: upURL}},
		Audit:     auditCfg,
		Policy: &config.Policy{
			DefaultDecision:         "deny",
			ServerInitiatedDecision: "deny",
			Rules: []config.PolicyRule{{
				ID: "calls-and-borrowing",
				Allow: []config.PolicyAllow{
					{Server: "*", Methods: []string{"tools/call"}, Names: []string{"*"}},
					{Server: "u", Methods: []string{"sampling/createMessage", "elicitation/create"}},
				},
			}},
		},
	}
	ts, gw := startGateway(t, granted)
	session, _, _ := lendingClient(t, ts.URL)

	// Open the bridged session while the grant stands, so the capability is
	// advertised and the handlers are installed.
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "borrow",
		Arguments: map[string]any{},
	}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	revoked := *granted
	revoked.Policy = &config.Policy{
		DefaultDecision:         "deny",
		ServerInitiatedDecision: "deny",
		Rules: []config.PolicyRule{{
			ID:    "calls-only",
			Allow: []config.PolicyAllow{{Server: "*", Methods: []string{"tools/call"}, Names: []string{"*"}}},
		}},
	}
	if err := gw.Reload(&revoked); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	// Same session, same advertised capability — and now refused, because the
	// handler asks policy again on every request.
	out, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "borrow",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool after revoke: %v", err)
	}
	if got, want := out.Content[0].(*mcp.TextContent).Text, "sampling=refused elicitation=refused"; got != want {
		t.Errorf("after revoke upstream saw %q, want %q", got, want)
	}

	// Both calls are in the trail, so match the refusal rather than the first
	// event for the method: the allowed exchange from before the reload is
	// also there, and is also the record it should be.
	evt := awaitEvent(t, snapshot, func(e audit.Event) bool {
		return e.Method == "sampling/createMessage" && e.Decision == "deny"
	})
	if evt.Direction != "server_initiated" {
		t.Errorf("direction = %q, want server_initiated", evt.Direction)
	}
	if evt.Outcome != audit.OutcomeDenied {
		t.Errorf("outcome = %q, want denied", evt.Outcome)
	}
	if evt.Upstream != "u" {
		t.Errorf("upstream = %q, want u", evt.Upstream)
	}
	// The elicitation is a second exchange and earns its own record.
	if e := awaitEvent(t, snapshot, func(e audit.Event) bool {
		return e.Method == "elicitation/create" && e.Decision == "deny"
	}); e.Outcome != audit.OutcomeDenied {
		t.Errorf("elicitation outcome = %q, want denied", e.Outcome)
	}
	// And the grant that stood before the reload is a record too — an allowed
	// borrow is an exchange, not a non-event.
	if e := awaitEvent(t, snapshot, func(e audit.Event) bool {
		return e.Method == "sampling/createMessage" && e.Decision == "allow"
	}); e.Outcome != audit.OutcomeOK {
		t.Errorf("allowed sampling outcome = %q, want ok", e.Outcome)
	}
}
