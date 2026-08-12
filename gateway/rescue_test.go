package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/audit"
	"github.com/fold-run/fold/config"
	"github.com/fold-run/fold/internal/state"
)

// panicsRecovered sums fold_panics_total across sites.
func panicsRecovered(t *testing.T, gw *Gateway) float64 {
	t.Helper()
	fams, err := gw.metrics.registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	total := 0.0
	for _, f := range fams {
		if f.GetName() != "fold_panics_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			total += m.GetCounter().GetValue()
		}
	}
	return total
}

// TestRouteSafeConvertsPanic proves the middleware's containment: a panic on
// the dispatch path becomes a generic internal error on the wire — carrying
// none of the panic's content — instead of ending the process.
func TestRouteSafeConvertsPanic(t *testing.T) {
	up, _ := newUpstreamServer(t, "echo")
	_, gw := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "a", Namespace: "a", URL: up.URL}},
	})

	evt := audit.Event{}
	res, err := gw.routeSafe(context.Background(), "custom/method", nil, &evt,
		func(context.Context, string, mcp.Request) (mcp.Result, error) {
			panic("secret internal detail")
		})
	if res != nil {
		t.Fatalf("expected nil result, got %v", res)
	}
	var wire *jsonrpc.Error
	if !asWireError(err, &wire) {
		t.Fatalf("expected a wire error, got %v", err)
	}
	if wire.Code != jsonrpc.CodeInternalError {
		t.Fatalf("expected code %d, got %d", jsonrpc.CodeInternalError, wire.Code)
	}
	if wire.Message != "internal gateway error" {
		t.Fatalf("panic content leaked to the wire: %q", wire.Message)
	}
	if got := panicsRecovered(t, gw); got != 1 {
		t.Fatalf("fold_panics_total = %v, want 1", got)
	}
}

// TestMiddlewarePanicIsAuditedOnce drives the full federationMiddleware —
// not routeSafe in isolation — over a panicking handler, pinning the
// single-exit-door invariant: the client hears a generic -32603, and exactly
// one audit event (outcome "error") is written for the request.
func TestMiddlewarePanicIsAuditedOnce(t *testing.T) {
	up, _ := newUpstreamServer(t, "echo")
	auditPath := t.TempDir() + "/audit.jsonl"
	_, gw := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "a", Namespace: "a", URL: up.URL}},
		Audit:     &config.Audit{Sinks: []config.AuditSink{{Type: "file", Path: auditPath}}},
	})

	h := gw.federationMiddleware(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		panic("boom")
	})
	res, err := h(context.Background(), "custom/explode", &mcp.ServerRequest[*mcp.PingParams]{})
	if res != nil {
		t.Fatalf("expected nil result, got %v", res)
	}
	var wire *jsonrpc.Error
	if !asWireError(err, &wire) || wire.Code != jsonrpc.CodeInternalError {
		t.Fatalf("expected -32603 wire error, got %v", err)
	}

	// The file sink writes synchronously on Emit, so the record is on disk
	// by the time the middleware returned.
	data, rerr := os.ReadFile(auditPath)
	if rerr != nil {
		t.Fatalf("read audit file: %v", rerr)
	}
	var events []audit.Event
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var e audit.Event
		if uerr := json.Unmarshal([]byte(line), &e); uerr != nil {
			t.Fatalf("bad audit line %q: %v", line, uerr)
		}
		if e.Method == "custom/explode" {
			events = append(events, e)
		}
	}
	if len(events) != 1 {
		t.Fatalf("audit events for the request = %d, want exactly 1", len(events))
	}
	if events[0].Outcome != audit.OutcomeError {
		t.Fatalf("audit outcome = %q, want %q", events[0].Outcome, audit.OutcomeError)
	}
}

// TestFanOutRecoversWorkerPanic: a panic in one fan-out worker degrades to
// that upstream failing — the partial-failure shape — and the other worker's
// result survives.
func TestFanOutRecoversWorkerPanic(t *testing.T) {
	provider := state.NewMemory()
	t.Cleanup(func() { _ = provider.Close() })
	ups := []*upstream{
		newUpstream(config.Upstream{ID: "good"}, provider),
		newUpstream(config.Upstream{ID: "boom"}, provider),
	}
	results, failed := fanOut(context.Background(), ups, func(_ context.Context, u *upstream) (string, error) {
		if u.cfg.ID == "boom" {
			panic("worker panic")
		}
		return "ok:" + u.cfg.ID, nil
	})
	if len(failed) != 1 || failed[0] != "boom" {
		t.Fatalf("failed = %v, want [boom]", failed)
	}
	if results[0] != "ok:good" {
		t.Fatalf("surviving result = %q, want ok:good", results[0])
	}
}

// TestSafelyKeepsLoopBodiesAlive: the background-loop wrapper swallows a
// panic (recording it) so the caller's loop continues.
func TestSafelyKeepsLoopBodiesAlive(t *testing.T) {
	up, _ := newUpstreamServer(t, "echo")
	_, gw := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "a", Namespace: "a", URL: up.URL}},
	})
	ran := false
	gw.safely("test", func() { panic(fmt.Errorf("tick gone wrong")) })
	gw.safely("test", func() { ran = true })
	if !ran {
		t.Fatal("second iteration did not run")
	}
	if got := panicsRecovered(t, gw); got != 1 {
		t.Fatalf("fold_panics_total = %v, want 1", got)
	}
}
