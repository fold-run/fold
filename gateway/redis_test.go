package gateway

import (
	"bytes"
	"context"
	"github.com/fold-run/fold/config"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestRedisSharedGateways runs two gateway instances against one Redis and
// verifies they behave as one: shared list cache and a shared per-upstream
// rate-limit budget.
func TestRedisSharedGateways(t *testing.T) {
	mr := miniredis.RunT(t)
	up, _ := newUpstreamServer(t, "tool")

	mkConfig := func() *config.Config {
		return &config.Config{
			Upstreams: []config.Upstream{{
				ID: "u", URL: up.URL,
				RateLimit: &config.RateLimit{RequestsPerMinute: 8},
			}},
			Server: &config.ServerSection{RedisURL: "redis://" + mr.Addr()},
		}
	}
	tsA, _ := startGateway(t, mkConfig())
	tsB, _ := startGateway(t, mkConfig())

	sessA := connect(t, tsA.URL, nil)
	sessB := connect(t, tsB.URL, nil)

	// Both instances serve the same federated list (via the shared cache).
	for _, sess := range []*mcp.ClientSession{sessA, sessB} {
		res, err := sess.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		if got := strings.Join(toolNames(res), ","); got != "tool" {
			t.Fatalf("tool list = %q", got)
		}
	}

	// The per-upstream budget is fleet-wide: calls through either instance
	// draw it down (connect handshakes and lists consumed part of it), so
	// alternating calls must eventually rate-limit on BOTH instances.
	var limitedA, limitedB bool
	for i := 0; i < 12 && (!limitedA || !limitedB); i++ {
		if _, err := sessA.CallTool(context.Background(), &mcp.CallToolParams{Name: "tool"}); err != nil && strings.Contains(err.Error(), "rate limit") {
			limitedA = true
		}
		if _, err := sessB.CallTool(context.Background(), &mcp.CallToolParams{Name: "tool"}); err != nil && strings.Contains(err.Error(), "rate limit") {
			limitedB = true
		}
	}
	if !limitedA || !limitedB {
		t.Fatalf("shared budget should limit both instances (A=%v B=%v)", limitedA, limitedB)
	}
}

// A tenant's allowance is one allowance across the fleet, not one per
// instance. Without shared state a tenant granted a million calls a month
// gets a million per replica, which is the failure the budget exists to
// prevent — so the scope key has to be the tenant id and nothing
// instance-local.
func TestRedisSharedTenantBudget(t *testing.T) {
	mr := miniredis.RunT(t)
	up, _ := newUpstreamServer(t, "tool")
	iss := newFixtureIssuer(t)

	mkConfig := func() *config.Config {
		cfg := authedConfig(iss, []config.Upstream{{ID: "u", URL: up.URL, Namespace: "u"}}, nil)
		cfg.Server = &config.ServerSection{RedisURL: "redis://" + mr.Addr()}
		cfg.Tenants = []config.Tenant{
			acmeTenant(&config.Budget{Period: "day", UpstreamCalls: 2}, nil),
		}
		return cfg
	}
	tsA, _ := startGateway(t, mkConfig())
	tsB, _ := startGateway(t, mkConfig())

	token := iss.mintClaims(t, jwt.MapClaims{
		"sub": "alice", "aud": "https://gw.example.com", "org_id": "acme",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	hdr := map[string]string{"Authorization": "Bearer " + token}
	sessA := connect(t, tsA.URL, hdr)
	sessB := connect(t, tsB.URL, hdr)
	ctx := context.Background()

	// One call through each instance spends the tenant's two.
	if _, err := sessA.CallTool(ctx, &mcp.CallToolParams{Name: "u__tool"}); err != nil {
		t.Fatalf("first call on A rejected: %v", err)
	}
	if _, err := sessB.CallTool(ctx, &mcp.CallToolParams{Name: "u__tool"}); err != nil {
		t.Fatalf("first call on B rejected: %v", err)
	}
	// The third is refused wherever it lands.
	for name, sess := range map[string]*mcp.ClientSession{"A": sessA, "B": sessB} {
		if _, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "u__tool"}); err == nil {
			t.Fatalf("instance %s admitted a call past the fleet-wide tenant allowance", name)
		}
	}
}

// Redis unreachable at boot used to be fatal — exit 1 — while the same outage
// one second later was survived per-instance. That asymmetry is a
// rolling-restart hazard: a Redis blip during a deploy failed every new pod
// while the old ones would have carried on. Boot is now fail-open like
// everything after it: the gateway starts, says so once, counts it, and
// enforces from its local mirror until Redis answers.
func TestRedisUnreachableAtBootStartsDegraded(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := l.Addr().String()
	l.Close() // nothing listens here now

	var buf bytes.Buffer
	up, _ := newUpstreamServer(t, "tool")
	gw, err := New(&config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: up.URL, RateLimit: &config.RateLimit{RequestsPerMinute: 2}}},
		Server:    &config.ServerSection{RedisURL: "redis://" + deadAddr},
	}, WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))
	if err != nil {
		t.Fatalf("New failed with Redis down: %v — boot must fail open like every later operation", err)
	}
	t.Cleanup(gw.Close)
	if !strings.Contains(buf.String(), "redis unreachable at startup") {
		t.Fatalf("no startup log for a degraded start; got:\n%s", buf.String())
	}
	ts := httptest.NewServer(gw.Handler())
	t.Cleanup(ts.Close)

	// The local mirror still enforces: two calls pass, the third is limited.
	session := connect(t, ts.URL, nil)
	limited := false
	for range 4 {
		if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "tool"}); err != nil && strings.Contains(err.Error(), "rate limit") {
			limited = true
		}
	}
	if !limited {
		t.Fatal("per-upstream rate limit did not trip with Redis down; the local mirror is not enforcing")
	}

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `fold_state_degraded_total{kind="startup"} 1`) {
		t.Fatalf("degraded start not counted; metrics:\n%s", body)
	}
}
