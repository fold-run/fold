package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"github.com/fold-run/fold/audit"
	"github.com/fold-run/fold/config"
	"github.com/fold-run/fold/internal/state"
)

// readAuditEvents parses a file sink's output, keeping events for method.
func readAuditEvents(t *testing.T, path, method string) []audit.Event {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit file: %v", err)
	}
	var out []audit.Event
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var e audit.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("bad audit line %q: %v", line, err)
		}
		if e.Method == method {
			out = append(out, e)
		}
	}
	return out
}

// TestEMAExchangeIsAudited: the token endpoint is an authorization server,
// and its terminal responses — the mint, the replay, the bad assertion —
// each leave exactly one audit record. The replay is the one that matters:
// before this, an attacker replaying ID-JAGs under the rate limit was
// invisible to the trail.
func TestEMAExchangeIsAudited(t *testing.T) {
	setEMAKey(t)
	idp := newFixtureIssuer(t)
	up, _ := newUpstreamServer(t, "tool")
	cfg := emaConfig(idp, []config.Upstream{{ID: "a", Namespace: "a", URL: up.URL}})
	auditPath := t.TempDir() + "/audit.jsonl"
	cfg.Audit = &config.Audit{Sinks: []config.AuditSink{{Type: "file", Path: auditPath}}}
	ts, _ := startGateway(t, cfg)

	assertion := idJAG(t, idp, "alice", "jag-audited")
	if status, body := redeem(t, ts.URL, assertion); status != http.StatusOK {
		t.Fatalf("first redeem: %d %v", status, body)
	}
	if status, _ := redeem(t, ts.URL, assertion); status != http.StatusBadRequest {
		t.Fatalf("replay should be refused, got %d", status)
	}
	if status, _ := redeem(t, ts.URL, "not-a-jwt"); status != http.StatusBadRequest {
		t.Fatalf("garbage assertion should be refused, got %d", status)
	}

	events := readAuditEvents(t, auditPath, "oauth/token")
	if len(events) != 3 {
		t.Fatalf("oauth/token audit events = %d, want 3: %+v", len(events), events)
	}
	mint, replay, garbage := events[0], events[1], events[2]
	if mint.Outcome != audit.OutcomeOK || mint.Decision != "minted" || mint.Principal != "alice" || mint.Issuer != idp.server.URL {
		t.Fatalf("mint event = %+v", mint)
	}
	// The replay is alertable on the structured decision field, not only by
	// substring-matching the error text.
	if replay.Outcome != audit.OutcomeUnauthenticated || replay.Decision != "replayed" || replay.Principal != "alice" {
		t.Fatalf("replay event = %+v", replay)
	}
	if garbage.Outcome != audit.OutcomeUnauthenticated || garbage.Decision != "invalid_grant" {
		t.Fatalf("garbage event = %+v", garbage)
	}
}

// TestUpstreamDownMessageRedacted: a fold-minted -32041 must not carry the
// raw transport error — it names internal endpoints and can name secret
// env vars, the same strings /health redacts for untrusted callers.
func TestUpstreamDownMessageRedacted(t *testing.T) {
	provider := state.NewMemory()
	t.Cleanup(func() { _ = provider.Close() })
	u := newUpstream(config.Upstream{
		ID:       "internal",
		URL:      "http://secret-internal-host.invalid:9",
		Timeouts: &config.Timeouts{ConnectMs: 200},
	}, provider)
	t.Cleanup(u.Close)

	err := u.ping(context.Background())
	var wire *jsonrpc.Error
	if !asWireError(err, &wire) || wire.Code != codeUpstreamDown {
		t.Fatalf("expected -32041, got %v", err)
	}
	if strings.Contains(wire.Message, "secret-internal-host") {
		t.Fatalf("minted error leaks the endpoint: %q", wire.Message)
	}
	if !strings.Contains(wire.Message, `upstream "internal" unavailable`) {
		t.Fatalf("unexpected message shape: %q", wire.Message)
	}
}

// TestIdleTimeoutBody: silence beyond the idle bound cuts the stream with a
// recognizable error; a stream that keeps delivering stays open across
// several idle windows.
func TestIdleTimeoutBody(t *testing.T) {
	t.Run("hung stream is cut", func(t *testing.T) {
		pr, _ := io.Pipe() // no writer: permanent silence
		body := newIdleTimeoutBody(pr, 100*time.Millisecond)
		defer body.Close()
		done := make(chan error, 1)
		go func() {
			_, err := body.Read(make([]byte, 64))
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "idle") {
				t.Fatalf("expected an idle-timeout error, got %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("read never unblocked")
		}
	})

	t.Run("active stream survives", func(t *testing.T) {
		pr, pw := io.Pipe()
		body := newIdleTimeoutBody(pr, 300*time.Millisecond)
		defer body.Close()
		stop := make(chan struct{})
		go func() {
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					if _, err := pw.Write([]byte("data: x\n\n")); err != nil {
						return
					}
				case <-stop:
					_ = pw.Close()
					return
				}
			}
		}()
		deadline := time.Now().Add(1 * time.Second) // > 3 idle windows
		buf := make([]byte, 64)
		for time.Now().Before(deadline) {
			if _, err := body.Read(buf); err != nil {
				t.Fatalf("active stream was cut: %v", err)
			}
		}
		close(stop)
	})
}

// TestConnectBoundsFailoverWalk: one connect attempt walks at most three
// candidates, so a K-replica upstream that is entirely down costs a bounded
// wait per caller rather than K×connectTimeout.
func TestConnectBoundsFailoverWalk(t *testing.T) {
	provider := state.NewMemory()
	t.Cleanup(func() { _ = provider.Close() })
	u := newUpstream(config.Upstream{
		ID: "wide",
		URLs: []string{
			"http://127.0.0.1:1", "http://127.0.0.1:2", "http://127.0.0.1:3",
			"http://127.0.0.1:4", "http://127.0.0.1:5",
		},
		Timeouts: &config.Timeouts{ConnectMs: 200},
	}, provider)
	t.Cleanup(u.Close)

	if _, err := u.connect(context.Background(), nil); err == nil {
		t.Fatal("connect against five dead endpoints should fail")
	}
	down := 0
	for _, ep := range u.endpoints.snapshot(true) {
		if !ep.Healthy {
			down++
		}
	}
	if down != maxConnectCandidates {
		t.Fatalf("endpoints ejected by one connect walk = %d, want %d", down, maxConnectCandidates)
	}
}

// TestDiscoveryIntervalClamped: a poll-interval typo cannot turn the gateway
// into a flood source against its own discovery source.
func TestDiscoveryIntervalClamped(t *testing.T) {
	up, _ := newUpstreamServer(t, "echo")
	_, gw := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "a", Namespace: "a", URL: up.URL}},
	})
	d := newDiscoverer(gw, &config.Discovery{URL: "https://registry.example/mcp.json", IntervalMs: 1})
	if d.interval != time.Second {
		t.Fatalf("interval = %v, want the 1s floor", d.interval)
	}
	close(d.stop)
}
