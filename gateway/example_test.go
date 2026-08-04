package gateway_test

import (
	"log"
	"log/slog"
	"net/http"

	"github.com/fold-run/fold/config"
	"github.com/fold-run/fold/gateway"
)

// Example shows embedding fold in another Go service: build a Gateway from
// the single JSON config document, mount its handler, and close it on
// shutdown. The handler serves the MCP endpoint plus the operational
// endpoints (health, metrics, OAuth protected-resource metadata).
func Example() {
	cfg, err := config.Parse([]byte(`{
		"upstreams": [
			{"id": "github", "url": "https://mcp.example.com/mcp", "namespace": "github"}
		]
	}`))
	if err != nil {
		log.Fatal(err)
	}

	gw, err := gateway.New(cfg, gateway.WithLogger(slog.Default()))
	if err != nil {
		log.Fatal(err)
	}
	defer gw.Close()

	log.Fatal(http.ListenAndServe("127.0.0.1:8080", gw.Handler()))
}

// Example_hotReload shows applying a new configuration to a running
// gateway: the upstream set and policy swap atomically, live sessions to
// unchanged upstreams survive, and a rejected document (validation failure,
// or a change to a construction-wired section) leaves the running
// configuration serving.
func Example_hotReload() {
	cfg, err := config.Parse([]byte(`{
		"upstreams": [
			{"id": "github", "url": "https://mcp.example.com/mcp", "namespace": "github"}
		]
	}`))
	if err != nil {
		log.Fatal(err)
	}
	gw, err := gateway.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer gw.Close()

	next, err := config.Parse([]byte(`{
		"upstreams": [
			{"id": "github", "url": "https://mcp.example.com/mcp", "namespace": "github"},
			{"id": "search", "url": "https://mcp.search.example.com/mcp", "namespace": "search"}
		]
	}`))
	if err != nil {
		log.Fatal(err)
	}
	if err := gw.Reload(next); err != nil {
		log.Printf("reload rejected, old config still serving: %v", err)
	}
}
