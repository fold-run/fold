// Command fold-registry produces fold's discovery document from an MCP
// Registry: it fetches the latest published version of each server named in an
// allowlist, maps the ones it can federate to upstream entries, and serves
// {"upstreams": [...]} for a fold gateway's discovery.url to poll.
//
//	fold-registry --servers ./servers.json
//	fold-registry --servers ./servers.json --registry-url https://registry.internal
//
// The allowlist is not optional and there is no "federate everything" mode:
// which servers an organization trusts is the decision this producer exists to
// record, not one it should infer.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fold-run/fold/internal/mcpregistry"
)

// version is stamped at build time via -ldflags="-X main.version=v...".
var version = "dev"

func main() {
	var (
		servers     = flag.String("servers", "", "path to the allowlist document (required)")
		registryURL = flag.String("registry-url", mcpregistry.DefaultRegistry, "base URL of the MCP Registry to read")
		registryEnv = flag.String("registry-bearer-env", "", "environment variable holding a bearer token for a registry that authenticates readers")
		port        = flag.Int("port", 8091, "port to serve the discovery document on")
		host        = flag.String("host", "0.0.0.0", "address to bind")
		interval    = flag.Duration("interval", 5*time.Minute, "registry poll interval")
		bearerEnv   = flag.String("bearer-env", "", "environment variable whose value callers must present as a Bearer token")
		reservedIDs = flag.String("reserved-ids", "", "comma-separated ids/namespaces registry entries may not claim (list the gateway's static upstream ids)")
		secretHdrs  = flag.Bool("allow-secret-headers", false, "federate entries whose remote requires a secret header (they will fail every call: this producer emits no credentials)")
		logFormat   = flag.String("log-format", "text", "log format: text | json")
		logLevel    = flag.String("log-level", "info", "log level: debug | info | warn | error")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("fold-registry", version)
		return
	}
	if *servers == "" {
		fmt.Fprintln(os.Stderr, "fold-registry: --servers is required")
		os.Exit(1)
	}

	logger := newLogger(*logFormat, *logLevel)

	raw, err := os.ReadFile(*servers)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fold-registry: %v\n", err)
		os.Exit(1)
	}
	list, err := mcpregistry.ParseAllowlist(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fold-registry: %s: %v\n", *servers, err)
		os.Exit(1)
	}

	registryBearer := ""
	if *registryEnv != "" {
		if registryBearer = os.Getenv(*registryEnv); registryBearer == "" {
			fmt.Fprintf(os.Stderr, "fold-registry: --registry-bearer-env %s: environment variable is empty\n", *registryEnv)
			os.Exit(1)
		}
	}
	client, err := mcpregistry.NewClient(*registryURL, registryBearer)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fold-registry: %v\n", err)
		os.Exit(1)
	}

	bearer := ""
	if *bearerEnv != "" {
		if bearer = os.Getenv(*bearerEnv); bearer == "" {
			fmt.Fprintf(os.Stderr, "fold-registry: --bearer-env %s: environment variable is empty\n", *bearerEnv)
			os.Exit(1)
		}
	}

	producer := &mcpregistry.Producer{
		Client:    client,
		Allowlist: list,
		Interval:  *interval,
		Bearer:    bearer,
		Map: mcpregistry.MapOptions{
			ReservedIDs:        splitList(*reservedIDs),
			AllowSecretHeaders: *secretHdrs,
		},
		Log: logger,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go producer.Run(ctx)

	addr := *host + ":" + strconv.Itoa(*port)
	srv := &http.Server{Addr: addr, Handler: producer.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		logger.Info("shutting down")
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		_ = srv.Shutdown(sctx)
	}()
	logger.Info("serving discovery document", "version", version, "addr", addr,
		"registry", *registryURL, "servers", len(list.Servers), "interval", interval.String())
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "fold-registry: %v\n", err)
		os.Exit(1)
	}
}

// splitList parses a comma-separated flag value, dropping empties.
func splitList(raw string) []string {
	var out []string
	for _, v := range strings.Split(raw, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func newLogger(format, level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if format == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(h)
}
