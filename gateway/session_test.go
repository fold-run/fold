package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/fold-run/fold/config"
)

func downstreamSessionCount(gw *Gateway) int {
	n := 0
	for range gw.server.Sessions() {
		n++
	}
	return n
}

// TestSessionIdleExpiry: a downstream session whose client goes quiet is
// closed after server.sessionIdleTimeoutMs, so abandoned clients (which are
// under no protocol obligation to DELETE their session) cannot grow session
// state without bound.
func TestSessionIdleExpiry(t *testing.T) {
	up, _ := newUpstreamServer(t, "echo")
	ts, gw := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "a", Namespace: "a", URL: up.URL}},
		Server:    &config.ServerSection{SessionIdleTimeoutMs: 250},
	})

	session := connect(t, ts.URL, nil)
	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	// The SDK handshake can leave more than one server-side session (the
	// count is an SDK detail, not the contract); what matters is that all of
	// them expire once the client goes quiet.
	if got := downstreamSessionCount(gw); got == 0 {
		t.Fatal("no live session after connect")
	}

	// Idle past the timeout; the SDK closes the session from its own timer,
	// with no further client traffic required.
	deadline := time.Now().Add(5 * time.Second)
	for downstreamSessionCount(gw) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("session still live %v after a 250ms idle timeout", 5*time.Second)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestSessionIdleTimeoutDefaults pins the config contract: absent → 30
// minutes, explicit value honored, negative → never expire (0).
func TestSessionIdleTimeoutDefaults(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.Config
		want int
	}{
		{"absent section", config.Config{}, 30 * 60 * 1000},
		{"absent field", config.Config{Server: &config.ServerSection{}}, 30 * 60 * 1000},
		{"explicit", config.Config{Server: &config.ServerSection{SessionIdleTimeoutMs: 5000}}, 5000},
		{"negative disables", config.Config{Server: &config.ServerSection{SessionIdleTimeoutMs: -1}}, 0},
	}
	for _, tc := range cases {
		if got := tc.cfg.SessionIdleTimeoutMs(); got != tc.want {
			t.Errorf("%s: SessionIdleTimeoutMs() = %d, want %d", tc.name, got, tc.want)
		}
	}
}
