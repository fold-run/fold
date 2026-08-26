package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/audit"
	"github.com/fold-run/fold/config"
	"github.com/fold-run/fold/internal/state"
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

// The outbound twin of maxBodyBytes. Every other thing fold fetches is
// bounded; the upstream data path was not, which made a buggy or hostile
// upstream able to spend the gateway's memory. The bound refuses rather than
// truncates — a shortened body would be the response rewriting fold declines
// — so the tests below assert an error, a counted event, and an audit record,
// never a partial result.

const (
	// capBytes is the smallest bound config validation accepts — low enough
	// that a deliberately large payload trips it, and high enough that the
	// handshake, the list, and an ordinary result do not.
	capBytes  = 64 << 10
	bulkSmall = 1 << 10
	bulkBig   = 512 << 10
	// A stream of many events, each comfortably under the bound, whose total
	// is comfortably over it.
	chattyEvents = 40
	chattyBytes  = 4 << 10
)

// cappedCount reads fold_upstream_response_capped_total for one upstream.
func cappedCount(t *testing.T, gw *Gateway, upstream string) float64 {
	t.Helper()
	fams, err := gw.metrics.registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() != "fold_upstream_response_capped_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "upstream" && l.GetValue() == upstream {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// newBulkUpstream is a real SDK MCP server whose response sizes are known:
// "small" and "big" return fixed payloads, and "chatty" emits a run of
// progress notifications — separate SSE events on the same response body —
// before returning. jsonResponse picks the framing, because the bound's unit
// depends on it: a whole body for application/json, one event for a stream.
func newBulkUpstream(t *testing.T, jsonResponse bool) *httptest.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "bulk", Version: "1.0"}, nil)
	payload := func(n int) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("x", n)}}}
	}
	server.AddTool(&mcp.Tool{Name: "small", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return payload(bulkSmall), nil
		})
	server.AddTool(&mcp.Tool{Name: "big", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return payload(bulkBig), nil
		})
	server.AddTool(&mcp.Tool{Name: "chatty", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			for i := range chattyEvents {
				_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
					ProgressToken: "bulk",
					Progress:      float64(i),
					Message:       strings.Repeat("y", chattyBytes),
				})
			}
			return payload(bulkSmall), nil
		})
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{JSONResponse: jsonResponse},
	))
	t.Cleanup(ts.Close)
	return ts
}

// An upstream that returns more than it was allowed to is unavailable, not
// truncated: the caller gets -31041, the refusal is counted per upstream, and
// it leaves an audit event like every other terminal response.
func TestUpstreamResponseCapRefusesOversized(t *testing.T) {
	for _, tc := range []struct {
		name         string
		jsonResponse bool
	}{
		{name: "event stream", jsonResponse: false},
		{name: "json response", jsonResponse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newBulkUpstream(t, tc.jsonResponse)
			auditPath := t.TempDir() + "/audit.jsonl"
			ts, gw := startGateway(t, &config.Config{
				Upstreams: []config.Upstream{{ID: "u", Namespace: "u", URL: up.URL, MaxResponseBytes: capBytes}},
				Audit:     &config.Audit{Sinks: []config.AuditSink{{Type: "file", Path: auditPath}}},
			})
			session := connect(t, ts.URL, nil)

			_, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "u__big"})
			if err == nil {
				t.Fatal("an oversized upstream response was served to the caller")
			}
			if !strings.Contains(err.Error(), "unavailable") && !strings.Contains(err.Error(), "request failed") {
				t.Errorf("unexpected error text: %v", err)
			}
			if got := cappedCount(t, gw, "u"); got != 1 {
				t.Errorf("fold_upstream_response_capped_total{upstream=u} = %v, want 1", got)
			}

			events := readAuditEvents(t, auditPath, "tools/call")
			if len(events) != 1 {
				t.Fatalf("tools/call audit events = %d, want 1: %+v", len(events), events)
			}
			if events[0].Outcome != audit.OutcomeUpstreamDown || events[0].Upstream != "u" {
				t.Errorf("audit event = %+v, want outcome %q on upstream u", events[0], audit.OutcomeUpstreamDown)
			}
		})
	}
}

// A response under the bound is ordinary traffic, and the bound is not armed
// by the handshake or the list that precedes it.
func TestUpstreamResponseUnderCapSucceeds(t *testing.T) {
	up := newBulkUpstream(t, false)
	ts, gw := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", Namespace: "u", URL: up.URL, MaxResponseBytes: capBytes}},
	})
	session := connect(t, ts.URL, nil)

	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	out, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "u__small"})
	if err != nil {
		t.Fatalf("CallTool under the bound: %v", err)
	}
	if got := len(out.Content[0].(*mcp.TextContent).Text); got != bulkSmall {
		t.Errorf("result was %d bytes, want %d — the bound must never truncate", got, bulkSmall)
	}
	if got := cappedCount(t, gw, "u"); got != 0 {
		t.Errorf("fold_upstream_response_capped_total{upstream=u} = %v, want 0", got)
	}
}

// A federation that legitimately moves large payloads turns the bound off,
// and gets exactly the behaviour it had before there was one.
func TestUpstreamResponseCapDisabled(t *testing.T) {
	up := newBulkUpstream(t, false)
	ts, gw := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", Namespace: "u", URL: up.URL, MaxResponseBytes: -1}},
	})
	session := connect(t, ts.URL, nil)

	out, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "u__big"})
	if err != nil {
		t.Fatalf("CallTool with the bound disabled: %v", err)
	}
	if got := len(out.Content[0].(*mcp.TextContent).Text); got != bulkBig {
		t.Errorf("result was %d bytes, want %d", got, bulkBig)
	}
	if got := cappedCount(t, gw, "u"); got != 0 {
		t.Errorf("fold_upstream_response_capped_total{upstream=u} = %v, want 0", got)
	}
}

// The per-event distinction. A stream is unbounded in total by design — a
// subscription open for a day is healthy — so the bound applies to one event.
// A connection-wide count would cut this call at an arbitrary point, and the
// caller would see an upstream failure for traffic nothing was wrong with.
func TestUpstreamResponseCapBoundsEventsNotStreams(t *testing.T) {
	up := newBulkUpstream(t, false)
	ts, gw := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", Namespace: "u", URL: up.URL, MaxResponseBytes: capBytes}},
	})
	session := connect(t, ts.URL, nil)

	if total := chattyEvents * chattyBytes; total <= capBytes {
		t.Fatalf("fixture is not exercising the distinction: %d bytes total, bound %d", total, capBytes)
	}
	out, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "u__chatty"})
	if err != nil {
		t.Fatalf("a stream whose events are each under the bound was cut: %v", err)
	}
	if got := len(out.Content[0].(*mcp.TextContent).Text); got != bulkSmall {
		t.Errorf("result was %d bytes, want %d", got, bulkSmall)
	}
	if got := cappedCount(t, gw, "u"); got != 0 {
		t.Errorf("fold_upstream_response_capped_total{upstream=u} = %v, want 0", got)
	}
}

// countEvents carries the last byte of each read so a frame terminator split
// across two reads still resets the count. Without the carry, a stream whose
// events straddle read boundaries accumulates until it trips the bound —
// which is the same false positive as a connection-wide count, only rarer and
// therefore worse.
func TestSizeCappedBodyFrameTerminatorStraddlesReads(t *testing.T) {
	event := "event: message\ndata: " + strings.Repeat("z", 64)
	// Each chunk ends mid-terminator: the "\n" that closes the frame lands in
	// the next read.
	var chunks []string
	for range 40 {
		chunks = append(chunks, event+"\n")
		chunks = append(chunks, "\n")
	}
	limit := int64(len(event) * 3) // one event fits; three do not
	body := newSizeCappedBody(io.NopCloser(&chunkReader{chunks: chunks}), limit, true, nil)
	t.Cleanup(func() { _ = body.Close() })

	total := 0
	buf := make([]byte, 4096)
	for {
		n, err := body.Read(buf)
		total += n
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("stream cut after %d bytes: %v (each event is %d bytes, bound %d)", total, err, len(event), limit)
		}
	}
	if want := 40 * (len(event) + 2); total != want {
		t.Errorf("read %d bytes, want %d", total, want)
	}
	// A single event over the bound still trips it, terminator carry or not.
	over := newSizeCappedBody(io.NopCloser(&chunkReader{chunks: []string{
		"data: " + strings.Repeat("z", int(limit)), strings.Repeat("z", int(limit)), "\n\n",
	}}), limit, true, nil)
	t.Cleanup(func() { _ = over.Close() })
	var err error
	for err == nil {
		_, err = over.Read(buf)
	}
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("an oversized single event was not cut: %v", err)
	}
}

// chunkReader hands back one prepared chunk per Read, so a test controls
// exactly where the read boundaries fall.
type chunkReader struct{ chunks []string }

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[0])
	if n < len(r.chunks[0]) {
		r.chunks[0] = r.chunks[0][n:]
		return n, nil
	}
	r.chunks = r.chunks[1:]
	return n, nil
}

// Nothing oversized is cached. The list path is where the bound earns its
// keep: an unbounded list is decoded, cached, and — with Redis configured —
// pushed into shared fleet state, so a refused list must leave the cache
// exactly as empty as it found it.
func TestCappedListIsNotCached(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "bulk-list", Version: "1.0"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        "huge",
		Description: strings.Repeat("d", bulkBig),
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})
	up := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(up.Close)

	u := newUpstream(config.Upstream{ID: "u", URL: up.URL, MaxResponseBytes: capBytes}, state.NewMemory())
	t.Cleanup(u.Close)
	ctx := context.Background()

	if _, err := u.listTools(ctx); err == nil {
		t.Fatal("an oversized tools/list was served")
	}

	filled := false
	if _, err := u.lists.GetOrFill(ctx, "tools", time.Minute, func(context.Context) ([]byte, error) {
		filled = true
		return []byte("[]"), nil
	}); err != nil {
		t.Fatalf("probe the list cache: %v", err)
	}
	if !filled {
		t.Error("the refused list left an entry in the cache")
	}
}
