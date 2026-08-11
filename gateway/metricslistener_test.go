package gateway

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fold-run/fold/config"
)

// server.metricsAddr exists because of a specific failure: fold's
// DNS-rebinding protection covers /metrics like every other path, so a
// scraper that reaches the gateway by any name outside server.allowedHosts is
// answered 403 — a ServiceMonitor scraping a pod IP, or Prometheus scraping a
// compose service name, sees only "target down".
//
// The fix is not to exempt the path. A scrape names upstream ids, namespaces,
// tenant ids, and multi-endpoint upstreams' endpoint URLs; on a loopback-bound
// development gateway, exempting it would let any page the operator visits
// read the shape of their federation. Moving the endpoint to a listener that
// is not a browser-reachable origin settles both halves, and these cover both.

// freeAddr reserves a loopback port and releases it, so the gateway can bind
// it. Racy in principle, fine in practice, and it beats a hardcoded port that
// collides with whatever else the machine is running.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func get(t *testing.T, url, host string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// The behaviour that motivated the field, pinned so it cannot regress
// unnoticed: on the main mux, a scrape under an unlisted Host is refused.
func TestMetricsOnMainMuxIsHostChecked(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: up.URL}},
	})

	if code, _ := get(t, ts.URL+"/metrics", ""); code != http.StatusOK {
		t.Fatalf("localhost scrape = %d, want 200", code)
	}
	// What a pod-IP or service-name scrape looks like to fold.
	code, body := get(t, ts.URL+"/metrics", "10.1.2.3:8080")
	if code != http.StatusForbidden {
		t.Fatalf("scrape under an unlisted Host = %d, want 403 — rebinding protection covers /metrics", code)
	}
	if strings.Contains(body, "fold_requests_total") {
		t.Fatal("a refused scrape returned metrics anyway")
	}
}

// With the listener configured, the same scrape succeeds under any Host,
// because this listener is not an origin a browser can be steered to.
func TestMetricsListenerServesAnyHost(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	addr := freeAddr(t)
	_, _ = startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: up.URL}},
		Server:    &config.ServerSection{MetricsAddr: addr},
	})

	for _, host := range []string{"", "10.1.2.3:9090", "fold.default.svc:9090"} {
		code, body := get(t, "http://"+addr+"/metrics", host)
		if code != http.StatusOK {
			t.Fatalf("scrape with Host %q = %d, want 200", host, code)
		}
		if !strings.Contains(body, "fold_build_info") {
			t.Fatalf("scrape with Host %q returned no fold metrics", host)
		}
	}
	// Liveness rides along, so a probe on this network needs no Host dance.
	if code, _ := get(t, "http://"+addr+"/health", "10.1.2.3:9090"); code != http.StatusOK && code != http.StatusServiceUnavailable {
		t.Fatalf("/health on the telemetry listener = %d, want 200 or 503", code)
	}
}

// Moving the endpoint has to actually move it. Serving it in both places
// would leave the public origin exposing exactly what the move was for.
func TestMetricsLeavesThePublicMux(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	addr := freeAddr(t)
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: up.URL}},
		Server:    &config.ServerSection{MetricsAddr: addr},
	})

	code, body := get(t, ts.URL+"/metrics", "")
	if code == http.StatusOK && strings.Contains(body, "fold_requests_total") {
		t.Fatal("/metrics is still served on the public mux; the move must be a move, not a copy")
	}
	if code != http.StatusNotFound {
		t.Fatalf("public /metrics = %d, want 404 once the endpoint has moved", code)
	}
	// The rest of the public surface is untouched.
	if code, _ := get(t, ts.URL+"/health", ""); code != http.StatusOK && code != http.StatusServiceUnavailable {
		t.Fatalf("/health on the public mux = %d, want it unaffected", code)
	}
}

// Close releases the port: a gateway that leaks its telemetry listener makes
// every embedder's test suite flaky, and hides the leak until the second bind.
func TestMetricsListenerClosesWithTheGateway(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	addr := freeAddr(t)
	cfg := &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: up.URL}},
		Server:    &config.ServerSection{MetricsAddr: addr},
	}
	gw, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if code, _ := get(t, "http://"+addr+"/metrics", ""); code != http.StatusOK {
		t.Fatalf("scrape before Close = %d, want 200", code)
	}
	gw.Close()

	// Binding the same port again is the check that the listener is gone.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("port still held after Close: %v", err)
	}
	ln.Close()
}

// A bad address fails construction rather than logging and serving on without
// metrics — the operator asked for a listener.
func TestMetricsListenerBindFailureFailsNew(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	// Hold a port, then ask the gateway for the same one.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	_, err = New(&config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: up.URL}},
		Server:    &config.ServerSection{MetricsAddr: ln.Addr().String()},
	})
	if err == nil {
		t.Fatal("New succeeded with an unbindable metricsAddr")
	}
	if !strings.Contains(err.Error(), "metricsAddr") {
		t.Fatalf("error = %v, want it to name the field", err)
	}
}

// MetricsHandler is the embedder's route to the same exposition without
// handing the gateway a listener to own.
func TestMetricsHandlerIsMountable(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	_, gw := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: up.URL}},
	})
	own := httptest.NewServer(gw.MetricsHandler())
	t.Cleanup(own.Close)
	code, body := get(t, own.URL, "anything.example:1234")
	if code != http.StatusOK || !strings.Contains(body, "fold_build_info") {
		t.Fatalf("mounted MetricsHandler = %d, body has metrics: %v", code, strings.Contains(body, "fold_build_info"))
	}
}

// The server section is construction-wired, and this field is part of why:
// moving the endpoint under a running gateway would leave a scraper pointed at
// a port that stopped answering, with the reload reporting success.
func TestReloadRejectsAMetricsAddrChange(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	addr := freeAddr(t)
	cfg := &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: up.URL}},
		Server:    &config.ServerSection{MetricsAddr: addr},
	}
	_, gw := startGateway(t, cfg)

	next := *cfg
	next.Server = &config.ServerSection{MetricsAddr: freeAddr(t)}
	err := gw.Reload(&next)
	if err == nil {
		t.Fatal("reload accepted a metricsAddr change; the server section is construction-wired")
	}
	if !strings.Contains(err.Error(), "server") {
		t.Fatalf("error = %v, want it to name the section", err)
	}
	// The original listener is still the one serving.
	if code, _ := get(t, "http://"+addr+"/metrics", ""); code != http.StatusOK {
		t.Fatalf("scrape after a rejected reload = %d, want the old listener still serving", code)
	}
}
