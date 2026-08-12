package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/fold-run/fold/config"
)

// rejectCount reads fold_http_rejections_total for one reason.
func rejectCount(t *testing.T, gw *Gateway, reason string) float64 {
	t.Helper()
	fams, err := gw.metrics.registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() != "fold_http_rejections_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "reason" && l.GetValue() == reason {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// TestChunkedOversizedBodyIsAudited: a chunked body carries no Content-Length
// for the edge check, so the cap only trips mid-read inside the handler.
// That refusal must exit through the same metric and audit event as the
// declared-length branch — the single exit door has no chunked side gate.
func TestChunkedOversizedBodyIsAudited(t *testing.T) {
	up, _ := newUpstreamServer(t, "echo")
	auditPath := t.TempDir() + "/audit.jsonl"
	ts, gw := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "a", Namespace: "a", URL: up.URL}},
		Server:    &config.ServerSection{MaxBodyBytes: 1024},
		Audit:     &config.Audit{Sinks: []config.AuditSink{{Type: "file", Path: auditPath}}},
	})

	// An io.Reader with no known length forces chunked transfer encoding, so
	// the request sails past the Content-Length pre-check.
	body := io.LimitReader(repeatReader('x'), 64*1024)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	if got := rejectCount(t, gw, "body_too_large"); got != 1 {
		t.Fatalf("body_too_large rejections = %v, want 1", got)
	}
	events := readAuditEvents(t, auditPath, "http")
	found := false
	for _, e := range events {
		if strings.Contains(e.Error, "maxBodyBytes") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no audit event for the chunked overflow; events: %+v", events)
	}
}

// repeatReader yields an endless stream of one byte.
type repeatReader byte

func (r repeatReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}
