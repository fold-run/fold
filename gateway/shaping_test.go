package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

// Per-principal filtering has always been fold's answer to oversized tool
// lists: a caller's tools/list is the subset they may invoke, computed with no
// model in the loop. Two things were missing and are covered here — the
// reduction was invisible, and it was unbounded.

// shapingUpstream is a fixture with a predictable catalogue.
func shapingUpstream(t *testing.T, ns string, tools ...string) config.Upstream {
	t.Helper()
	up, _ := newUpstreamServer(t, tools...)
	return config.Upstream{ID: ns, URL: up.URL, Namespace: ns}
}

// A cap bounds what one rule makes visible, and the truncation is announced —
// a cap that hid capability silently would be worse than no cap.
func TestMaxItemsCapsAListAndSaysSo(t *testing.T) {
	cfg := &config.Config{
		Upstreams: []config.Upstream{shapingUpstream(t, "a", "alpha", "beta", "gamma", "delta")},
		Policy: &config.Policy{
			DefaultDecision: "deny",
			Rules: []config.PolicyRule{{
				ID:       "capped",
				MaxItems: 2,
				Allow:    []config.PolicyAllow{{Server: "*", Methods: []string{"tools/call"}, Names: []string{"*"}}},
			}},
		},
	}
	ts, _ := startGateway(t, cfg)
	session := connect(t, ts.URL, nil)

	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(res.Tools) != 2 {
		t.Fatalf("tools = %d (%v), want 2 — the rule's cap", len(res.Tools), toolNamesOf(res.Tools))
	}
	marker, ok := res.Meta[metaTruncated]
	if !ok {
		t.Fatal("a truncated list must say so in _meta; a silent cap is capability disappearing for no stated reason")
	}
	m, ok := marker.(map[string]any)
	if !ok {
		t.Fatalf("truncation marker = %T, want an object", marker)
	}
	if m["dropped"] == nil {
		t.Fatalf("truncation marker %v does not say how many were dropped", m)
	}
}

// Without a cap nothing changes, which is what keeps this additive for every
// existing deployment.
func TestNoCapLeavesTheListWhole(t *testing.T) {
	cfg := &config.Config{
		Upstreams: []config.Upstream{shapingUpstream(t, "a", "alpha", "beta", "gamma")},
		Policy: &config.Policy{
			DefaultDecision: "deny",
			Rules: []config.PolicyRule{{
				ID:    "open",
				Allow: []config.PolicyAllow{{Server: "*", Methods: []string{"tools/call"}, Names: []string{"*"}}},
			}},
		},
	}
	ts, _ := startGateway(t, cfg)
	session := connect(t, ts.URL, nil)

	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(res.Tools) != 3 {
		t.Fatalf("tools = %d, want all 3", len(res.Tools))
	}
	if _, ok := res.Meta[metaTruncated]; ok {
		t.Fatal("an uncapped list was marked truncated")
	}
}

// The cap bounds visibility, not authority: policy still decides invocation,
// and a capped-out tool is not thereby denied. Truncation is a context bound —
// treating it as an authorization boundary would be a second, weaker policy
// engine hiding inside the list path.
func TestCappedToolIsStillCallable(t *testing.T) {
	cfg := &config.Config{
		Upstreams: []config.Upstream{shapingUpstream(t, "a", "alpha", "beta", "gamma")},
		Policy: &config.Policy{
			DefaultDecision: "deny",
			Rules: []config.PolicyRule{{
				ID:       "capped",
				MaxItems: 1,
				Allow:    []config.PolicyAllow{{Server: "*", Methods: []string{"tools/call"}, Names: []string{"*"}}},
			}},
		},
	}
	ts, _ := startGateway(t, cfg)
	session := connect(t, ts.URL, nil)
	ctx := context.Background()

	res, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(res.Tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(res.Tools))
	}
	// A name the cap withheld from the list is still authorized to call.
	for _, name := range []string{"a__alpha", "a__beta", "a__gamma"} {
		if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name}); err != nil {
			t.Fatalf("call %s rejected: a cap bounds the list, it does not revoke a grant: %v", name, err)
		}
	}
}

// Caps are per rule, so two principals matched by different rules get
// different bounds — the same shape as every other thing policy decides.
func TestCapsArePerRule(t *testing.T) {
	iss := newFixtureIssuer(t)
	up := shapingUpstream(t, "a", "alpha", "beta", "gamma", "delta")
	cfg := authedConfig(iss, []config.Upstream{up}, &config.Policy{
		DefaultDecision: "deny",
		Rules: []config.PolicyRule{
			{
				ID: "tight", MaxItems: 1,
				Subjects: &config.PolicySubjects{Groups: []string{"small-context"}},
				Allow:    []config.PolicyAllow{{Server: "*", Methods: []string{"tools/call"}, Names: []string{"*"}}},
			},
			{
				ID:       "wide",
				Subjects: &config.PolicySubjects{Groups: []string{"everything"}},
				Allow:    []config.PolicyAllow{{Server: "*", Methods: []string{"tools/call"}, Names: []string{"*"}}},
			},
		},
	})
	ts, _ := startGateway(t, cfg)
	ctx := context.Background()

	tight := connect(t, ts.URL, map[string]string{
		"Authorization": "Bearer " + iss.mint(t, "alice", "https://gw.example.com", []string{"small-context"}),
	})
	wide := connect(t, ts.URL, map[string]string{
		"Authorization": "Bearer " + iss.mint(t, "bob", "https://gw.example.com", []string{"everything"}),
	})

	small, err := tight.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(small.Tools) != 1 {
		t.Fatalf("capped principal saw %d tools, want 1", len(small.Tools))
	}
	all, err := wide.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Tools) != 4 {
		t.Fatalf("uncapped principal saw %d tools, want 4", len(all.Tools))
	}
}

// The reduction becomes a number an operator can read: offered is what the
// upstreams returned, served is what the caller got, capped is what a bound
// removed. Without the denominator, "served 3 tools" says nothing.
func TestShapingMetricsCountOfferedServedAndCapped(t *testing.T) {
	cfg := &config.Config{
		Upstreams: []config.Upstream{shapingUpstream(t, "a", "alpha", "beta", "gamma", "delta")},
		Policy: &config.Policy{
			DefaultDecision: "deny",
			Rules: []config.PolicyRule{{
				ID:       "capped",
				MaxItems: 2,
				Allow: []config.PolicyAllow{{
					Server: "*", Methods: []string{"tools/call"},
					// Only three of the four are grantable; the cap then takes
					// one of those, so the two mechanisms are distinguishable
					// in the counters.
					Names: []string{"alpha", "beta", "gamma"},
				}},
			}},
		},
	}
	ts, _ := startGateway(t, cfg)
	session := connect(t, ts.URL, nil)
	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	scrape := scrapeMetrics(t, ts.URL)
	for _, want := range []struct{ stage, count string }{
		{"offered", " 4"}, // every tool the upstream returned
		{"served", " 2"},  // what the caller received
		{"capped", " 1"},  // grantable, but past the bound
	} {
		line := metricLine(scrape, `fold_list_items_total{method="tools/call",stage="`+want.stage+`"}`)
		if line == "" {
			t.Fatalf("no %s series:\n%s", want.stage, scrape)
		}
		if !strings.HasSuffix(line, want.count) {
			t.Errorf("%s = %q, want it to end %q", want.stage, line, want.count)
		}
	}
}
