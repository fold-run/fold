package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

// The subscription table is keyed by a URI the caller chose, and under
// passthrough any URI resolves to the one upstream, so one long-lived session
// could grow it without bound. A bounded map would be the wrong tool — evicting
// an entry orphans the upstream subscription behind it — so the bound is a
// refusal at the cap, lifted as soon as the session lets something go.
func TestSubscriptionsPerSessionAreCapped(t *testing.T) {
	prev := maxSubscriptionsPerSession
	maxSubscriptionsPerSession = 3
	t.Cleanup(func() { maxSubscriptionsPerSession = prev })

	server := mcp.NewServer(&mcp.Implementation{Name: "subs", Version: "1.0"}, &mcp.ServerOptions{
		Capabilities:       &mcp.ServerCapabilities{Resources: &mcp.ResourceCapabilities{Subscribe: true}},
		SubscribeHandler:   func(context.Context, *mcp.SubscribeRequest) error { return nil },
		UnsubscribeHandler: func(context.Context, *mcp.UnsubscribeRequest) error { return nil },
	})
	uris := []string{"file:///a", "file:///b", "file:///c", "file:///d"}
	for _, uri := range uris {
		server.AddResource(&mcp.Resource{URI: uri, Name: uri},
			func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return &mcp.ReadResourceResult{}, nil
			})
	}
	up := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(up.Close)

	// Passthrough: a single un-namespaced upstream, so every URI resolves.
	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{{ID: "u", URL: up.URL}}})
	session := connect(t, ts.URL, nil)
	ctx := context.Background()

	for _, uri := range uris[:3] {
		if err := session.Subscribe(ctx, &mcp.SubscribeParams{URI: uri}); err != nil {
			t.Fatalf("subscribe %s under the cap: %v", uri, err)
		}
	}
	// Re-subscribing to a held URI is not a new subscription and is not refused.
	if err := session.Subscribe(ctx, &mcp.SubscribeParams{URI: uris[0]}); err != nil {
		t.Fatalf("re-subscribe to a held URI refused: %v", err)
	}
	err := session.Subscribe(ctx, &mcp.SubscribeParams{URI: uris[3]})
	if err == nil || !strings.Contains(err.Error(), "subscription limit") {
		t.Fatalf("fourth subscription under a cap of 3: err=%v, want a subscription-limit refusal", err)
	}
	if err := session.Unsubscribe(ctx, &mcp.UnsubscribeParams{URI: uris[0]}); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}
	if err := session.Subscribe(ctx, &mcp.SubscribeParams{URI: uris[3]}); err != nil {
		t.Fatalf("subscribe after freeing a slot: %v", err)
	}
}
