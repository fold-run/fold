package gateway

import (
	"net/http"
	"strings"
	"testing"

	"github.com/fold-run/fold/config"
)

// server.allowedHosts is the one Host guard. The SDK's streamable handler has
// its own — refuse a non-loopback Host on a loopback local address — and with
// both active a gateway bound to 127.0.0.1 behind a same-host proxy
// forwarding its public Host was refused by the SDK after fold's allowlist
// had admitted it, with an error that named neither. The httptest server here
// is exactly that shape: loopback address, rewritten Host.
func TestAllowedHostsIsTheOnlyHostGuard(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: up.URL}},
		Server:    &config.ServerSection{AllowedHosts: []string{"gw.example"}},
	})
	post := func(host string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var sb strings.Builder
		buf := make([]byte, 512)
		n, _ := resp.Body.Read(buf)
		sb.Write(buf[:n])
		return resp.StatusCode, sb.String()
	}

	// The allowlisted public Host, arriving over loopback: fold admits it and
	// the SDK must not second-guess that.
	if code, body := post("gw.example"); code != http.StatusOK {
		t.Fatalf("allowlisted Host over a loopback connection answered %d: %s", code, strings.TrimSpace(body))
	}
	// A Host outside the allowlist is still refused — by fold, in fold's words.
	if code, body := post("evil.example"); code != http.StatusForbidden || !strings.Contains(body, "forbidden host") {
		t.Fatalf("Host outside the allowlist answered %d %q; want fold's 403 forbidden host", code, strings.TrimSpace(body))
	}
	// And the loopback names the SDK's guard used to admit are not admitted
	// here, because an explicit allowlist replaces the loopback seed.
	if code, _ := post("localhost"); code != http.StatusForbidden {
		t.Fatalf("Host: localhost answered %d under an explicit allowlist; want 403", code)
	}
}
