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

// TestHealthDoesNotProbeCallerDerivedUpstreams proves a federation whose
// credentials belong to the caller is reported unprobed rather than down. A
// probe carries no principal, so pinging such an upstream fails at Apply on
// every poll — which used to charge its budget, record a breaker failure
// (five polls open the circuit), and leave /health answering 503 forever, so
// an orchestrator never marked the process ready while it was serving every
// authenticated caller correctly.
func TestHealthDoesNotProbeCallerDerivedUpstreams(t *testing.T) {
	iss := newFixtureIssuer(t)
	up, _ := newUpstreamServer(t, "tool")
	ts, g := startGateway(t, authedConfig(iss, []config.Upstream{
		{ID: "u", URL: up.URL, Auth: &config.UpstreamAuth{Strategy: "passthrough"}, CacheTTLMs: -1},
	}, nil))

	// Well past the breaker's default failure threshold of 5. Collected
	// directly rather than over HTTP so the 1s health cache does not turn the
	// loop into a single collection.
	for i := range 10 {
		statuses, healthy, probeable := g.collectUpstreamHealth(context.Background(), g.rt())
		if len(statuses) != 1 {
			t.Fatalf("collection %d returned %d statuses, want 1", i, len(statuses))
		}
		if !statuses[0].Unprobed {
			t.Fatalf("collection %d: caller-derived upstream was probed anyway (connected=%v err=%q)",
				i, statuses[0].Connected, statuses[0].Error)
		}
		if probeable != 0 || healthy != 0 {
			t.Fatalf("collection %d: healthy=%d probeable=%d, want 0/0 — an unprobed upstream is neither",
				i, healthy, probeable)
		}
	}

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/health = %d, want 200: nothing is probeable, so nothing is down", resp.StatusCode)
	}
	var body struct {
		Status    string `json:"status"`
		Upstreams []struct {
			Connected bool `json:"connected"`
			Unprobed  bool `json:"unprobed"`
		} `json:"upstreams"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" {
		t.Errorf("/health status = %q, want ok", body.Status)
	}
	if len(body.Upstreams) != 1 || !body.Upstreams[0].Unprobed || body.Upstreams[0].Connected {
		t.Errorf("/health upstreams = %+v, want one unprobed and not connected", body.Upstreams)
	}

	// The circuit is the point: after ten collections the upstream must still
	// serve the callers it can actually authenticate.
	session := connect(t, ts.URL, map[string]string{
		"Authorization": "Bearer " + iss.mint(t, "dana", "https://gw.example.com", nil),
	})
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "tool"}); err != nil {
		t.Fatalf("health collection tripped the upstream for real callers: %v", err)
	}
}

// TestHealthStillReportsProbeableUpstreamsDown proves the unprobed carve-out
// is not a blanket amnesty: an upstream the gateway *can* reach still decides
// the verdict, and a federation with nothing reachable still answers 503.
func TestHealthStillReportsProbeableUpstreamsDown(t *testing.T) {
	iss := newFixtureIssuer(t)
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	t.Cleanup(dead.Close)
	live, _ := newUpstreamServer(t, "tool")

	ts, _ := startGateway(t, authedConfig(iss, []config.Upstream{
		{ID: "caller", URL: live.URL, Namespace: "caller",
			Auth: &config.UpstreamAuth{Strategy: "passthrough"}, CacheTTLMs: -1},
		{ID: "dead", URL: dead.URL, Namespace: "dead"},
	}, nil))

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("/health = %d, want 503: the one probeable upstream is down", resp.StatusCode)
	}
}

// TestActiveProbesSkipCallerDerivedUpstreams proves healthCheck is ignored
// for an upstream whose credential belongs to the caller. Probing it would
// fail at Apply every interval and eject every endpoint, taking the upstream
// down for the clients it serves perfectly well.
func TestActiveProbesSkipCallerDerivedUpstreams(t *testing.T) {
	iss := newFixtureIssuer(t)
	up, _ := newUpstreamServer(t, "tool")
	_, g := startGateway(t, authedConfig(iss, []config.Upstream{
		{ID: "u", URL: up.URL, Auth: &config.UpstreamAuth{Strategy: "passthrough"},
			CacheTTLMs: -1, HealthCheck: &config.HealthCheck{IntervalMs: 1}},
	}, nil))

	u := g.rt().upstreams[0]
	if u.probeStop != nil {
		t.Fatal("the probe loop started for a caller-derived upstream")
	}
	for _, ep := range u.endpoints.snapshot(true) {
		if !ep.Healthy {
			t.Fatalf("endpoint %q was ejected by a probe that cannot hold a credential", ep.URL)
		}
	}
}
