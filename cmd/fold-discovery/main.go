// Command fold-discovery produces fold's discovery document from Kubernetes
// Services: it lists Services matching a label selector on an interval,
// maps them to upstream entries via fold.run/* annotations, and serves
// {"upstreams": [...]} for a fold gateway's discovery.url to poll.
//
//	fold-discovery --port 8090                  # in-cluster
//	fold-discovery --kube-api http://127.0.0.1:8001   # via kubectl proxy
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

	"github.com/fold-run/fold/internal/kubediscovery"
)

// version is stamped at build time via -ldflags="-X main.version=v...".
var version = "dev"

func main() {
	var (
		port        = flag.Int("port", 8090, "port to serve the discovery document on")
		host        = flag.String("host", "0.0.0.0", "address to bind")
		namespace   = flag.String("namespace", "", "namespace to watch (default: cluster-wide)")
		selector    = flag.String("selector", kubediscovery.DefaultSelector, "label selector for Services to federate")
		interval    = flag.Duration("interval", 15*time.Second, "Kubernetes list interval")
		kubeAPI     = flag.String("kube-api", "", "Kubernetes API base URL (default: in-cluster)")
		tokenFile   = flag.String("token-file", "", "bearer token file for the Kubernetes API (default: in-cluster service account)")
		caFile      = flag.String("ca-file", "", "CA bundle for the Kubernetes API (default: in-cluster CA)")
		bearerEnv   = flag.String("bearer-env", "", "environment variable whose value callers must present as a Bearer token")
		allowAuth   = flag.String("allow-auth-strategies", "", "comma-separated auth strategies Services may carry in fold.run/config (default: none)")
		allowRefs   = flag.String("allow-secret-refs", "", "comma-separated env var names Services may reference in secretRef fields (default: none)")
		reservedIDs = flag.String("reserved-ids", "", "comma-separated ids/namespaces Services may not claim (list the gateway's static upstream ids)")
		unprefixed  = flag.Bool("allow-unprefixed-ids", false, "disable namespace prefixing of ids and MCP namespaces (single-tenant clusters only)")
		credHosts   = flag.String("allow-credential-hosts", "", "comma-separated hosts (\"*.\" wildcards ok) a credentialed Service may send secrets to; empty = unrestricted")
		minProbeMs  = flag.Int("min-health-interval-ms", 1000, "floor for a Service's healthCheck.intervalMs")
		logFormat   = flag.String("log-format", "text", "log format: text | json")
		logLevel    = flag.String("log-level", "info", "log level: debug | info | warn | error")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("fold-discovery", version)
		return
	}

	logger := newLogger(*logFormat, *logLevel)

	api, token, ca := *kubeAPI, *tokenFile, *caFile
	if api == "" {
		var err error
		api, token, ca, err = kubediscovery.InClusterDefaults()
		if err != nil {
			fmt.Fprintf(os.Stderr, "fold-discovery: %v\n", err)
			os.Exit(1)
		}
	}
	client, err := kubediscovery.NewClient(api, token, ca)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fold-discovery: %v\n", err)
		os.Exit(1)
	}

	bearer := ""
	if *bearerEnv != "" {
		if bearer = os.Getenv(*bearerEnv); bearer == "" {
			fmt.Fprintf(os.Stderr, "fold-discovery: --bearer-env %s: environment variable is empty\n", *bearerEnv)
			os.Exit(1)
		}
	}

	producer := &kubediscovery.Producer{
		Client:    client,
		Namespace: *namespace,
		Selector:  *selector,
		Interval:  *interval,
		Bearer:    bearer,
		Map: kubediscovery.MapOptions{
			AllowedAuthStrategies:    splitList(*allowAuth),
			AllowedSecretRefs:        splitList(*allowRefs),
			ReservedIDs:              splitList(*reservedIDs),
			UnprefixedIDs:            *unprefixed,
			AllowedCredentialHosts:   splitList(*credHosts),
			MinHealthCheckIntervalMs: *minProbeMs,
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
		"selector", *selector, "namespace", *namespace, "interval", interval.String())
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "fold-discovery: %v\n", err)
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
