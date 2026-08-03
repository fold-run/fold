package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold-go/config"
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
	for i := 0; i < 12 && !(limitedA && limitedB); i++ {
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
