package audit

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/fold-run/fold/config"
)

// The otlp-logs sink is built on the OTel SDK's pipeline rather than on a
// hand-written encoder, because the wire format is where a hand-rolled emitter
// goes wrong quietly — proto3 JSON renders 64-bit fields as strings, severity
// is a numeric enum, attribute values are tagged unions. These tests assert
// the parts fold is actually responsible for: that records reach the endpoint
// at all, that the event's fields survive as attributes, that severity
// reflects the outcome, and that fold's delivery accounting still applies to a
// sink whose transport it does not own.

// otlpCapture is a collector stand-in: it records the raw OTLP request bodies.
type otlpCapture struct {
	mu     sync.Mutex
	bodies [][]byte
	paths  []string
	status int
}

func (c *otlpCapture) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, body)
		c.paths = append(c.paths, r.URL.Path)
		status := c.status
		c.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
	}
}

func (c *otlpCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

func (c *otlpCapture) lastPath() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.paths) == 0 {
		return ""
	}
	return c.paths[len(c.paths)-1]
}

// Records reach the collector, at OTLP's standard path — which fold gets from
// the exporter rather than by knowing the convention itself.
func TestOTLPSinkDeliversToTheStandardPath(t *testing.T) {
	cap := &otlpCapture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	c := newCounted()
	l := New(&config.Audit{Sinks: []config.AuditSink{{
		Type: "otlp-logs", URL: srv.URL,
	}}}, WithObserver(c.observe))

	l.Emit(Event{Method: "tools/call", Name: "a__b", Outcome: OutcomeOK})
	l.Close() // Shutdown flushes the batch processor

	if cap.count() == 0 {
		t.Fatal("no OTLP request reached the collector")
	}
	if got := cap.lastPath(); got != "/v1/logs" {
		t.Fatalf("OTLP path = %q, want /v1/logs", got)
	}
	if c.get(OutcomeDelivered) == 0 {
		t.Fatal("delivery was not counted; this sink's losses would be invisible")
	}
}

// Severity is derived from the outcome, and a refusal is a warning rather than
// an error: policy denying a call is the gateway working as configured.
func TestSeverityDistinguishesRefusalFromFailure(t *testing.T) {
	for _, tc := range []struct {
		outcome Outcome
		want    string
	}{
		{OutcomeOK, "INFO"},
		{OutcomeDenied, "WARN"},
		{OutcomeRateLimited, "WARN"},
		{OutcomeBudgetExhausted, "WARN"},
		{OutcomeError, "ERROR"},
		{OutcomeUpstreamDown, "ERROR"},
	} {
		if _, got := severityFor(tc.outcome); got != tc.want {
			t.Errorf("severity for %q = %s, want %s", tc.outcome, got, tc.want)
		}
	}
}

// The structured fields become attributes under the same names fold's spans
// use, so a trace and its audit record join on the same keys.
func TestAttributesMirrorTheSpanNames(t *testing.T) {
	attrs := otlpAttributes(Event{
		Method: "tools/call", Name: "a__b", Upstream: "a", Outcome: OutcomeDenied,
		Decision: "deny", RuleID: "r1", Tenant: "acme", Principal: "alice",
		Issuer: "https://idp", LatencyMs: 12, UpstreamCalls: 1, ItemsServed: 3,
	})
	got := map[string]string{}
	for _, kv := range attrs {
		got[string(kv.Key)] = kv.Value.String()
	}
	for _, want := range []string{
		"mcp.method", "mcp.name", "fold.upstream", "fold.outcome",
		"fold.policy.decision", "fold.policy.rule", "fold.tenant",
		"enduser.id", "fold.latency_ms", "fold.upstream_calls", "fold.items_served",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("attribute %q missing from %v", want, got)
		}
	}
	// Empty fields are skipped rather than emitted blank.
	sparse := otlpAttributes(Event{Method: "ping"})
	if len(sparse) != 1 {
		t.Fatalf("sparse event produced %d attributes, want just the method", len(sparse))
	}
}

// A collector that refuses everything must not swallow the loss: fold's
// accounting wraps the SDK's exporter precisely so this sink cannot become the
// one destination whose failures no metric reports.
func TestOTLPFailureIsDeadLettered(t *testing.T) {
	cap := &otlpCapture{status: http.StatusBadRequest} // permanent: not retried
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	dlPath := filepath.Join(t.TempDir(), "dead.jsonl")
	c := newCounted()
	l := New(&config.Audit{Sinks: []config.AuditSink{{
		Type: "otlp-logs", URL: srv.URL, DeadLetterPath: dlPath,
		Retry: &config.AuditRetry{MaxAttempts: 1, InitialBackoffMs: 1, MaxBackoffMs: 2},
	}}}, WithObserver(c.observe))

	l.Emit(Event{Method: "tools/call", Name: "a__b", Outcome: OutcomeDenied})
	l.Close()

	waitFor(t, "dead-lettered record", func() bool { return c.get(OutcomeDeadLettered) > 0 })

	data, err := os.ReadFile(dlPath)
	if err != nil {
		t.Fatalf("dead-letter file: %v", err)
	}
	line := strings.TrimSpace(strings.Split(strings.TrimSpace(string(data)), "\n")[0])
	var out otlpEventJSON
	if err := json.Unmarshal([]byte(line), &out); err != nil {
		t.Fatalf("dead-letter line is not JSON: %v", err)
	}
	if !strings.Contains(out.Body, "a__b") {
		t.Fatalf("dead-lettered body = %q, want the event summary", out.Body)
	}
	if out.Attributes["fold.outcome"] != string(OutcomeDenied) {
		t.Fatalf("dead-lettered attributes lost the outcome: %v", out.Attributes)
	}
}

// A malformed endpoint is caught by config validation rather than at sink
// construction: the exporter accepts an unparseable URL and falls back to its
// defaults, which would send fold's audit trail somewhere nobody asked for.
// `fold --validate` and the chart's init container are where this belongs.
func TestBadOTLPURLIsRejectedByValidation(t *testing.T) {
	for _, bad := range []string{"", "://not a url", "collector:4318"} {
		cfg := &config.Config{
			Upstreams: []config.Upstream{{ID: "u", URL: "https://example.com/mcp"}},
			Audit:     &config.Audit{Sinks: []config.AuditSink{{Type: "otlp-logs", URL: bad}}},
		}
		if err := cfg.Validate(); err == nil {
			t.Errorf("otlp-logs url %q passed validation; the exporter would silently use its own default endpoint", bad)
		}
	}
	// The good case still validates, with or without an explicit path.
	for _, good := range []string{"http://collector:4318", "https://collector:4318/v1/logs"} {
		cfg := &config.Config{
			Upstreams: []config.Upstream{{ID: "u", URL: "https://example.com/mcp"}},
			Audit:     &config.Audit{Sinks: []config.AuditSink{{Type: "otlp-logs", URL: good}}},
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("otlp-logs url %q rejected: %v", good, err)
		}
	}
}
