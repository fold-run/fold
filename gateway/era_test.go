package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/fold-run/fold/config"
)

// TestModernEraIsRefusedByTheTransport is the server-side half of the
// protocol-era drift canary. TestNotificationFanIn pins the client half — that
// a default-options SDK client negotiates below 2026-07-28 against fold — but a
// client is free not to be the SDK, and "our client falls back" is a weaker
// claim than "the era cannot be reached".
//
// It cannot, today, and the refusal comes from the SDK's streamable handler
// rather than from anything fold does: fold runs stateful because session-keyed
// bridging needs it, and the handler serves 2026-07-28 only with
// StreamableHTTPOptions.Stateless. A caller that insists on the era — by header
// or by per-request _meta — is refused before fold's router sees the request
// at all. (How it is refused is the SDK's business and has already changed
// once; this pins only that it is.)
//
// That is what makes this a canary rather than a behaviour test. Several things
// fold has not done are safe only while this holds: results relayed from the
// router carry no resultType of their own (the SDK stamps "complete" on
// whatever fold returns, which would be wrong for a relayed input-required
// result), resources/subscribe is still advertised though the era replaced it,
// and logging/setLevel is still handled though the era removed it. When the SDK
// lifts the restriction this test fails, and that list is the work it is
// failing to announce.
func TestModernEraIsRefusedByTheTransport(t *testing.T) {
	up, _ := newUpstreamServer(t, "alpha")
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: up.URL}},
	})

	// A bare request declaring the modern era: no initialize, no session id.
	// Both spellings the wire allows are tried, because a future SDK could
	// honour one and not the other.
	for _, tc := range []struct {
		name   string
		header map[string]string
		body   string
	}{
		{
			name:   "protocol version header",
			header: map[string]string{"Mcp-Protocol-Version": "2026-07-28"},
			body:   `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`,
		},
		{
			name: "per-request _meta",
			body: `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{` +
				`"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
				`"io.modelcontextprotocol/clientCapabilities":{}}}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)

			// What this pins is that the request was *refused*, not how. The
			// SDK has already changed the shape once — v1.7.0 answers an HTTP
			// 400, and its main branch returns a JSON-RPC error instead
			// (modelcontextprotocol/go-sdk#1143) — and a canary that asserted
			// the status would have failed on that bump for a reason nobody
			// cares about, which is how a canary gets deleted rather than
			// read. Any refusal is fine. Being served is not.
			refused := resp.StatusCode != http.StatusOK || strings.Contains(string(body), `"error"`)
			if !refused {
				t.Fatalf("the gateway served a %s request declaring the 2026-07-28 era.\n"+
					"The SDK's stateless-only restriction has been lifted, and fold's router is "+
					"now reachable on an era it was never audited for. Before flipping this test, "+
					"work through the era gaps in README \"Not implemented\" and docs/roadmap.md — "+
					"relayed results carry no resultType, resources/subscribe and logging/setLevel "+
					"are advertised though the era retired them, and the bridged-session apparatus "+
					"has no counterpart there.\nbody: %s", tc.name, body)
			}
		})
	}
}
