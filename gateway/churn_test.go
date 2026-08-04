package gateway

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

// TestReloadDiscoveryChurn interleaves everything with a lifecycle: base
// config reloads (upstream added/removed, policy engine swapped), the
// discovery document flapping, active health probes against a dead replica,
// and concurrent client traffic — under the race detector. The per-feature
// tests prove each mechanism works; this one exists to catch their
// interleavings (a retired upstream's drain racing a probe loop racing a
// fresh snapshot). Clients tolerate every error during the storm; the test
// asserts the gateway converges to full correctness once it ends.
func TestReloadDiscoveryChurn(t *testing.T) {
	upA, _ := newUpstreamServer(t, "alpha_tool")
	upB, _ := newUpstreamServer(t, "beta_tool")
	upC, _ := newUpstreamServer(t, "gamma_tool")

	// A dead replica keeps the balancer's failover and the probe loop busy.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadURL := "http://" + l.Addr().String()
	l.Close()

	registry, doc := discoveryRegistry(t, "")
	discovery := &config.Discovery{URL: registry.URL, IntervalMs: 20}
	baseSmall := &config.Config{
		Upstreams: []config.Upstream{{
			ID: "a", URLs: []string{upA.URL, deadURL}, Namespace: "a",
			HealthCheck: &config.HealthCheck{IntervalMs: 20},
		}},
		Discovery: discovery,
	}
	baseBig := &config.Config{
		Upstreams: []config.Upstream{
			baseSmall.Upstreams[0],
			{ID: "c", URL: upC.URL, Namespace: "c"},
		},
		Policy: &config.Policy{
			DefaultDecision: "deny",
			Rules: []config.PolicyRule{{
				ID:    "all",
				Allow: []config.PolicyAllow{{Server: "*"}},
			}},
		},
		Discovery: discovery,
	}
	withB := fmt.Sprintf(`{"upstreams":[{"id":"b","url":%q,"namespace":"b"}]}`, upB.URL)

	ts, gw := startGateway(t, baseSmall)

	// Client goroutines hammer lists and calls throughout the storm.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			session := connect(t, ts.URL, nil)
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = session.ListTools(context.Background(), nil)
				_, _ = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "a__alpha_tool"})
				_, _ = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "b__beta_tool"})
			}
		})
	}

	// The storm: alternate base reloads and discovery flaps for ~1.5s.
	deadline := time.Now().Add(1500 * time.Millisecond)
	for i := 0; time.Now().Before(deadline); i++ {
		if i%2 == 0 {
			doc.Store(withB)
			if err := gw.Reload(baseBig); err != nil {
				t.Errorf("Reload(baseBig) #%d: %v", i, err)
			}
		} else {
			doc.Store(`{"upstreams":[]}`)
			if err := gw.Reload(baseSmall); err != nil {
				t.Errorf("Reload(baseSmall) #%d: %v", i, err)
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(stop)
	wg.Wait()

	// Settle into a known state and require full correctness.
	doc.Store(withB)
	if err := gw.Reload(baseBig); err != nil {
		t.Fatalf("final Reload: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return gw.rt().byID["b"] != nil },
		"discovered upstream never settled after churn")

	session := connect(t, ts.URL, nil)
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("post-churn ListTools: %v", err)
	}
	names := strings.Join(toolNames(res), ",")
	for _, want := range []string{"a__alpha_tool", "b__beta_tool", "c__gamma_tool"} {
		if !strings.Contains(names, want) {
			t.Errorf("post-churn list %q missing %q", names, want)
		}
	}
	for _, name := range []string{"a__alpha_tool", "b__beta_tool", "c__gamma_tool"} {
		if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name}); err != nil {
			t.Errorf("post-churn CallTool(%s): %v", name, err)
		}
	}
}
