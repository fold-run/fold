package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fold-run/fold/config"
)

// The instance rides the real request path, not just the audit package's own
// tests: the gateway constructs the logger, and a record that reaches a sink
// without an instance is a fleet trail nobody can attribute.
func TestAuditRecordsNameTheEmittingInstance(t *testing.T) {
	t.Setenv("FOLD_INSTANCE_ID", "fold-a1b2")
	up, _ := newUpstreamServer(t, "echo")
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "a", Namespace: "a", URL: up.URL}},
		Audit:     &config.Audit{Sinks: []config.AuditSink{{Type: "file", Path: auditPath}}},
	})

	sess := connect(t, ts.URL, nil)
	if _, err := sess.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	_ = sess.Close()

	events := readAuditEvents(t, auditPath, "tools/list")
	if len(events) == 0 {
		t.Fatal("no tools/list audit events")
	}
	for _, e := range events {
		if e.Instance != "fold-a1b2" {
			t.Errorf("audit event instance = %q, want fold-a1b2", e.Instance)
		}
	}
}

// requireDurable is the one audit misconfiguration that stops the gateway.
// The document here is valid — a file sink is durable — and the path is not,
// which is the case only construction can find.
func TestRequireDurableRefusesToStartWhenTheSinkCannotOpen(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	up, _ := newUpstreamServer(t, "echo")
	cfg := &config.Config{
		Upstreams: []config.Upstream{{ID: "a", Namespace: "a", URL: up.URL}},
		Audit: &config.Audit{
			RequireDurable: true,
			Sinks: []config.AuditSink{
				{Type: "stdout"},
				{Type: "file", Path: filepath.Join(blocked, "audit.jsonl")},
			},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the document itself declares a durable sink: %v", err)
	}
	gw, err := New(cfg)
	if err == nil {
		gw.Close()
		t.Fatal("expected New to refuse to start with no durable sink")
	}
	if !strings.Contains(err.Error(), "requireDurable") {
		t.Errorf("error does not name the setting that refused: %v", err)
	}
}

// The same configuration with a writable path starts, so the guard refuses a
// broken deployment rather than the feature.
func TestRequireDurableStartsWhenTheSinkOpens(t *testing.T) {
	up, _ := newUpstreamServer(t, "echo")
	cfg := &config.Config{
		Upstreams: []config.Upstream{{ID: "a", Namespace: "a", URL: up.URL}},
		Audit: &config.Audit{
			RequireDurable: true,
			Sinks:          []config.AuditSink{{Type: "file", Path: filepath.Join(t.TempDir(), "audit.jsonl")}},
		},
	}
	gw, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw.Close()
}
