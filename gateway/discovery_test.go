package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fold-run/fold/config"
)

// discoveryRegistry is a mutable discovery source: store a document string
// to serve it, store "" to answer 500.
func discoveryRegistry(t *testing.T, requireBearer string) (*httptest.Server, *atomic.Value) {
	t.Helper()
	var doc atomic.Value
	doc.Store(`{"upstreams":[]}`)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requireBearer != "" && r.Header.Get("Authorization") != "Bearer "+requireBearer {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body := doc.Load().(string)
		if body == "" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(ts.Close)
	return ts, &doc
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(msg)
}

// TestDiscoveryAddsAndRemovesUpstreams: the discovery document shapes the
// federation — upstreams it lists join alongside the static set (visible to
// connected clients without reconnecting), and disappear when delisted. The
// bearer credential from bearerSecretRef authenticates the poll.
func TestDiscoveryAddsAndRemovesUpstreams(t *testing.T) {
	upA, _ := newUpstreamServer(t, "alpha_tool")
	upB, _ := newUpstreamServer(t, "beta_tool")
	registry, doc := discoveryRegistry(t, "reg-token")
	t.Setenv("DISC_TOKEN", "reg-token")

	ts, gw := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "a", URL: upA.URL, Namespace: "a"}},
		Discovery: &config.Discovery{URL: registry.URL, IntervalMs: 50, BearerSecretRef: "DISC_TOKEN"},
	})
	session := connect(t, ts.URL, nil)

	listNames := func() string {
		res, err := session.ListTools(context.Background(), nil)
		if err != nil {
			return "err:" + err.Error()
		}
		return strings.Join(toolNames(res), ",")
	}

	// The registry announces upstream B; the federation grows.
	doc.Store(fmt.Sprintf(`{"upstreams":[{"id":"b","url":%q,"namespace":"b"}]}`, upB.URL))
	waitFor(t, 5*time.Second, func() bool { return strings.Contains(listNames(), "b__beta_tool") },
		"discovered upstream never became routable")
	if !strings.Contains(listNames(), "a__alpha_tool") {
		t.Error("static upstream lost after discovery applied")
	}

	// The registry delists B; the federation shrinks.
	doc.Store(`{"upstreams":[]}`)
	waitFor(t, 5*time.Second, func() bool { return !strings.Contains(listNames(), "b__") },
		"delisted upstream never left the federation")
	if len(gw.rt().upstreams) != 1 {
		t.Errorf("routing holds %d upstreams, want 1", len(gw.rt().upstreams))
	}
}

// TestDiscoveryFailSafe: a colliding document is rejected whole, and a
// broken source keeps the last good set serving.
func TestDiscoveryFailSafe(t *testing.T) {
	upA, _ := newUpstreamServer(t, "alpha_tool")
	upB, _ := newUpstreamServer(t, "beta_tool")
	registry, doc := discoveryRegistry(t, "")

	_, gw := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "a", URL: upA.URL, Namespace: "a"}},
		Discovery: &config.Discovery{URL: registry.URL, IntervalMs: 50},
	})

	doc.Store(fmt.Sprintf(`{"upstreams":[{"id":"b","url":%q,"namespace":"b"}]}`, upB.URL))
	waitFor(t, 5*time.Second, func() bool { return len(gw.rt().upstreams) == 2 },
		"good document never applied")

	// A document colliding with a static upstream id must not corrupt
	// routing: rejected whole, last good set (a + b) keeps serving.
	doc.Store(fmt.Sprintf(`{"upstreams":[{"id":"a","url":%q,"namespace":"x"}]}`, upB.URL))
	time.Sleep(200 * time.Millisecond) // several sync intervals
	if got := len(gw.rt().upstreams); got != 2 {
		t.Errorf("colliding document altered routing: %d upstreams, want 2", got)
	}
	if gw.rt().byNamespace["x"] != nil {
		t.Error("colliding document's upstream leaked into routing")
	}

	// Source outage: last good set keeps serving.
	doc.Store("")
	time.Sleep(200 * time.Millisecond)
	if got := len(gw.rt().upstreams); got != 2 {
		t.Errorf("source outage altered routing: %d upstreams, want 2", got)
	}
}

// TestReloadPreservesDiscovered: a base config reload (the SIGHUP / --watch
// path) replaces the static set but carries the discovered set forward.
func TestReloadPreservesDiscovered(t *testing.T) {
	upA, _ := newUpstreamServer(t, "alpha_tool")
	upB, _ := newUpstreamServer(t, "beta_tool")
	upC, _ := newUpstreamServer(t, "gamma_tool")
	registry, doc := discoveryRegistry(t, "")

	discovery := &config.Discovery{URL: registry.URL, IntervalMs: 50}
	_, gw := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "a", URL: upA.URL, Namespace: "a"}},
		Discovery: discovery,
	})
	doc.Store(fmt.Sprintf(`{"upstreams":[{"id":"b","url":%q,"namespace":"b"}]}`, upB.URL))
	waitFor(t, 5*time.Second, func() bool { return gw.rt().byID["b"] != nil },
		"discovered upstream never applied")

	if err := gw.Reload(&config.Config{
		Upstreams: []config.Upstream{
			{ID: "a", URL: upA.URL, Namespace: "a"},
			{ID: "c", URL: upC.URL, Namespace: "c"},
		},
		Discovery: discovery,
	}); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	rt := gw.rt()
	for _, id := range []string{"a", "b", "c"} {
		if rt.byID[id] == nil {
			t.Errorf("upstream %q missing after base reload (have %d upstreams)", id, len(rt.upstreams))
		}
	}
}
