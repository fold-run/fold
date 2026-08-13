package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

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

// TestHealthzIsGone: /healthz was the pre-v1.5 health path, kept as a
// deprecated alias through v1.8 and removed in v1.9. The test exists so the
// removal is a decision on record rather than something that could drift
// back in — probes must be pointed at /health.
func TestHealthzIsGone(t *testing.T) {
	up, _ := newUpstreamServer(t, "echo")
	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{{ID: "u", URL: up.URL}}})

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/healthz status = %d, want 404 — the alias was removed in v1.9", resp.StatusCode)
	}
}
