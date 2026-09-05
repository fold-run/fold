package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

func liveSessions(gw *Gateway) int {
	n := 0
	for range gw.server.Sessions() {
		n++
	}
	return n
}

// A default SDK client probes the modern era with a session-less
// server/discover POST before it initializes. The stateful handler accepted
// that probe and minted a session for it; the client then fell back to a
// legacy initialize in a second session and only ever deleted the second —
// so every connecting client left one orphan behind until the idle timeout.
// The load harness's session column is how it showed up: 10 sessions held
// for 8 live connections, 658 by the end of a sweep whose clients had all
// closed. After a client connects, calls, and closes, fold must hold nothing
// for it.
func TestClosedClientLeavesNoSessionBehind(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	ts, gw := startGateway(t, &config.Config{Upstreams: []config.Upstream{{ID: "u", URL: up.URL}}})

	for i := range 3 {
		session := connect(t, ts.URL, nil) // default options: latest protocol, discover first
		if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "tool"}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if got := liveSessions(gw); got != 1 {
			t.Fatalf("client %d: %d sessions held for one live client; the discover probe minted an orphan", i, got)
		}
		if err := session.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
		deadline := time.Now().Add(2 * time.Second)
		for liveSessions(gw) != 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if got := liveSessions(gw); got != 0 {
			t.Fatalf("client %d: %d sessions still held after the client closed", i, got)
		}
	}
}

// The probe itself is refused the way the SDK client understands as "this
// server does not speak the era": a 405 it wraps as rejected and answers with
// the legacy handshake — the same synthetic refusal fold's own upstream leg
// has always given upstreams. A discover carrying a session id is not a
// probe and is left to the SDK.
func TestDiscoverProbeIsRefusedWithoutMintingASession(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	ts, gw := startGateway(t, &config.Config{Upstreams: []config.Upstream{{ID: "u", URL: up.URL}}})

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("server/discover answered %d, want 405", resp.StatusCode)
	}
	if resp.Header.Get("Allow") == "" {
		t.Fatal("a 405 must carry an Allow header (RFC 9110 §15.5.6)")
	}
	if resp.Header.Get("Mcp-Session-Id") != "" || liveSessions(gw) != 0 {
		t.Fatalf("the refused probe minted a session (header %q, %d held)", resp.Header.Get("Mcp-Session-Id"), liveSessions(gw))
	}

	// An ordinary initialize is untouched by the peek: it still creates a
	// session and answers with its id.
	init, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`))
	init.Header.Set("Content-Type", "application/json")
	init.Header.Set("Accept", "application/json, text/event-stream")
	ir, err := http.DefaultClient.Do(init)
	if err != nil {
		t.Fatal(err)
	}
	ir.Body.Close()
	if ir.StatusCode != http.StatusOK || ir.Header.Get("Mcp-Session-Id") == "" {
		t.Fatalf("initialize after the peek: status %d, session %q", ir.StatusCode, ir.Header.Get("Mcp-Session-Id"))
	}
}
