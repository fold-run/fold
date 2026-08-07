package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

func TestAuthorityHostRejectsMalformedPort(t *testing.T) {
	for _, tc := range []struct {
		authority string
		want      string
		ok        bool
	}{
		{"localhost", "localhost", true},
		{"LocalHost:8080", "localhost", true},
		{"[::1]:8080", "::1", true},
		{"[::1]", "::1", true},
		// net.SplitHostPort splits at the last colon without checking what
		// follows, so these must not be read as the allowed host prefix.
		{"localhost:8080.evil.com", "", false},
		{"localhost:evil", "", false},
		{"", "", false},
	} {
		got, ok := authorityHost(tc.authority)
		if ok != tc.ok || got != tc.want {
			t.Errorf("authorityHost(%q) = (%q, %v), want (%q, %v)", tc.authority, got, ok, tc.want, tc.ok)
		}
	}
}

func TestOriginAllowedRejectsMalformedOrigins(t *testing.T) {
	allowed := map[string]bool{"localhost": true}
	for _, tc := range []struct {
		origin string
		want   bool
	}{
		{"http://localhost", true},
		{"https://localhost:8080", true},
		{"null", false},
		{"localhost", false},        // schemeless
		{"file://localhost", false}, // not an http(s) origin
		{"https://evil.com", false},
		{"https://localhost:8080.evil.com", false}, // invalid port
		{"https://evil.com/localhost", false},
	} {
		if got := originAllowed(allowed, tc.origin); got != tc.want {
			t.Errorf("originAllowed(%q) = %v, want %v", tc.origin, got, tc.want)
		}
	}
}

// TestForbiddenHostWithMalformedPort proves the rebinding guard rejects an
// authority whose port is not numeric rather than reading it as its prefix.
func TestForbiddenHostWithMalformedPort(t *testing.T) {
	up, _ := newUpstreamServer(t, "echo")
	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{{ID: "u", URL: up.URL}}})

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "localhost:8080.evil.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("Host %q should be forbidden, got %d", req.Host, resp.StatusCode)
	}
}

// TestHealthFanOutIsSingleFlighted proves /health cannot be used to
// multiply one unauthenticated request into a ping against every upstream:
// a burst of polls shares one fan-out, bounded by healthCacheTTL.
func TestHealthFanOutIsSingleFlighted(t *testing.T) {
	var pings atomic.Int64
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1.0"}, nil)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), `"method":"ping"`) {
				pings.Add(1)
			}
			r.Body = io.NopCloser(strings.NewReader(string(body)))
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(up.Close)

	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{{ID: "u", URL: up.URL}}})

	const polls = 20
	for range polls {
		resp, err := http.Get(ts.URL + "/health")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	// The loop runs well inside a second, so one or two collections is the
	// honest ceiling; the point is that it is not one per poll.
	if got := pings.Load(); got > 3 {
		t.Fatalf("%d polls produced %d upstream pings; the fan-out is not being shared", polls, got)
	}
	if pings.Load() == 0 {
		t.Fatal("no upstream ping at all: /health is not actually probing")
	}
}

// TestHealthCacheInvalidatedByReload proves the shared collection never
// answers a probe from a retired routing snapshot.
func TestHealthCacheInvalidatedByReload(t *testing.T) {
	up, _ := newUpstreamServer(t, "echo")
	cfg := &config.Config{Upstreams: []config.Upstream{{ID: "one", URL: up.URL}}}
	ts, gw := startGateway(t, cfg)

	if body := getString(t, ts.URL+"/health"); !strings.Contains(body, `"one"`) {
		t.Fatalf("health missing the initial upstream: %s", body)
	}
	next := &config.Config{Upstreams: []config.Upstream{
		{ID: "one", URL: up.URL, Namespace: "one"},
		{ID: "two", URL: up.URL, Namespace: "two"},
	}}
	if err := gw.Reload(next); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if body := getString(t, ts.URL+"/health"); !strings.Contains(body, `"two"`) {
		t.Fatalf("health served a pre-reload snapshot: %s", body)
	}
}

func getString(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// TestOrphanedSubscriptionReleased proves the gateway does not hold an
// upstream subscription forever when the subscribing client disconnects
// without unsubscribing.
func TestOrphanedSubscriptionReleased(t *testing.T) {
	const uri = "file:///watched.txt"
	var subscribed, unsubscribed atomic.Int64
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1.0"}, &mcp.ServerOptions{
		Capabilities: &mcp.ServerCapabilities{Resources: &mcp.ResourceCapabilities{Subscribe: true}},
		SubscribeHandler: func(context.Context, *mcp.SubscribeRequest) error {
			subscribed.Add(1)
			return nil
		},
		UnsubscribeHandler: func(context.Context, *mcp.UnsubscribeRequest) error {
			unsubscribed.Add(1)
			return nil
		},
	})
	server.AddResource(&mcp.Resource{URI: uri, Name: "watched"},
		func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, Text: "x"}}}, nil
		})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)

	ts, gw := startGateway(t, &config.Config{Upstreams: []config.Upstream{{ID: "u", URL: up.URL}}})

	session := connect(t, ts.URL, nil)
	if err := session.Subscribe(context.Background(), &mcp.SubscribeParams{URI: uri}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if subscribed.Load() != 1 {
		t.Fatalf("upstream did not receive the subscription: %d", subscribed.Load())
	}
	// Disconnect without unsubscribing — exactly what a crashed or
	// impatient client does.
	_ = session.Close()

	deadline := time.Now().Add(5 * time.Second)
	for {
		gw.reapSubscribers()
		if unsubscribed.Load() == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("orphaned subscription was never released upstream")
		}
		time.Sleep(20 * time.Millisecond)
	}

	gw.subMu.Lock()
	remaining := len(gw.subscribers)
	gw.subMu.Unlock()
	if remaining != 0 {
		t.Fatalf("subscriber ref-count table still holds %d entries", remaining)
	}
}

// TestDiscoveryRefusesRedirect proves a discovery source cannot redirect the
// gateway — and its bearer credential — to a host of its choosing.
func TestDiscoveryRefusesRedirect(t *testing.T) {
	var attackerHits atomic.Int64
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerHits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"upstreams": []any{}})
	}))
	t.Cleanup(attacker.Close)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/doc", http.StatusFound)
	}))
	t.Cleanup(source.Close)

	up, _ := newUpstreamServer(t, "echo")
	gw, err := New(&config.Config{Upstreams: []config.Upstream{{ID: "u", URL: up.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Close)

	d := newDiscoverer(gw, &config.Discovery{URL: source.URL})
	if _, err := d.fetch(); err == nil {
		t.Fatal("discovery followed a redirect; it must refuse")
	}
	if n := attackerHits.Load(); n != 0 {
		t.Fatalf("redirect target was contacted %d times", n)
	}
}
