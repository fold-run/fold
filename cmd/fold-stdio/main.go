// Command fold-stdio runs one stdio MCP server and exposes it over streamable
// HTTP, so the gateway can federate servers that have no network endpoint.
//
// The command is fixed at startup from argv and is never taken from a request,
// a config document, or discovery — see docs/design-stdio.md.
//
//	fold-stdio --port 8091 -- npx -y @modelcontextprotocol/server-filesystem /data
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fold-run/fold/internal/stdiobridge"
)

// version is stamped at build time, like the gateway's.
var version = "dev"

func main() {
	var (
		port        = flag.Int("port", 8091, "port to serve the MCP endpoint on")
		host        = flag.String("host", "127.0.0.1", "address to bind — loopback by default; 0.0.0.0 is a deliberate act")
		maxSessions = flag.Int("max-sessions", stdiobridge.DefaultMaxSessions, "maximum concurrent sessions, each of which is one child process")
		maxBody     = flag.Int64("max-body-bytes", stdiobridge.DefaultMaxBodyBytes, "maximum request body size")
		envPass     = flag.String("env-passthrough", "", "comma-separated environment variable names to pass to the server (default: none)")
		dir         = flag.String("dir", "", "working directory for the server (default: this process's)")
		bearerEnv   = flag.String("bearer-env", "", "environment variable whose value callers must present as a Bearer token")
		probe       = flag.Bool("probe", false, "start the server once to check it runs, then exit")
		logFormat   = flag.String("log-format", "text", "log format: text | json")
		logLevel    = flag.String("log-level", "info", "log level: debug | info | warn | error")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Usage = usage
	flag.Parse()

	if *showVersion {
		fmt.Println("fold-stdio", version)
		return
	}

	command := flag.Args()
	if len(command) == 0 {
		fmt.Fprintln(os.Stderr, "fold-stdio: no server command given")
		usage()
		os.Exit(2)
	}

	logger := newLogger(*logFormat, *logLevel)

	// The child's environment is built from an explicit allowlist. It never
	// inherits this process's, so a bearer secret held here is not handed to
	// the server being supervised.
	names := splitList(*envPass)
	env := stdiobridge.BuildEnv(names)

	bearer := ""
	if *bearerEnv != "" {
		if bearer = os.Getenv(*bearerEnv); bearer == "" {
			fmt.Fprintf(os.Stderr, "fold-stdio: --bearer-env %s: environment variable is empty\n", *bearerEnv)
			os.Exit(1)
		}
	}

	// The shim runs a process on demand, so an unauthenticated bind beyond
	// loopback is an open exec-over-HTTP surface. Refuse it rather than
	// document against it — the container entrypoint binds 0.0.0.0, which
	// makes this the shipped default's guard rail.
	if bearer == "" && !isLoopback(*host) {
		fmt.Fprintf(os.Stderr, "fold-stdio: refusing to bind %s without --bearer-env: "+
			"a non-loopback bind must require a token\n", *host)
		os.Exit(2)
	}

	bridge, err := stdiobridge.New(stdiobridge.Options{
		Command:      command,
		Env:          env,
		Dir:          *dir,
		MaxSessions:  *maxSessions,
		MaxBodyBytes: *maxBody,
		Logger:       logger,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "fold-stdio: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = bridge.Close() }()

	if *probe {
		pctx, pcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer pcancel()
		if err := bridge.Probe(pctx); err != nil {
			fmt.Fprintf(os.Stderr, "fold-stdio: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("server OK")
		return
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	addr := *host + ":" + strconv.Itoa(*port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           routes(bridge, bearer),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		logger.Info("shutting down")
		sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer scancel()
		_ = srv.Shutdown(sctx)
		// Children are reaped after the listener stops, so no session is
		// killed mid-request and no process is left orphaned.
		_ = bridge.Close()
	}()

	logger.Info("serving stdio server over http",
		"version", version, "addr", addr,
		"command", strings.Join(command, " "),
		"maxSessions", *maxSessions, "envPassthrough", names,
		"authRequired", bearer != "")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "fold-stdio: %v\n", err)
		os.Exit(1)
	}
}

// routes wires the three endpoints. /mcp is the gateway's upstream URL;
// /health matches the gateway's own health semantics so chart probes and
// healthCheck.intervalMs behave identically against a shim.
func routes(b *stdiobridge.Bridge, bearer string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/mcp", requireBearer(bearer, b))
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		// A shim that cannot start its server is unhealthy: reporting that
		// lets the gateway's breaker eject the endpoint instead of hanging.
		// Health is memoized and single-flighted, so probing it in a loop
		// costs at most one spawn per second.
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		status, code := "ok", http.StatusOK
		if err := b.Health(ctx); err != nil {
			status, code = "unhealthy", http.StatusServiceUnavailable
		}
		writeJSON(w, code, map[string]any{
			"status":  status,
			"version": version,
			"stats":   b.Stats(),
		})
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(renderMetrics(b.Stats())))
	})
	return mux
}

// renderMetrics builds the Prometheus exposition text. Assembling it in a
// buffer keeps the write to one call, and makes the format testable without
// an HTTP round trip.
func renderMetrics(s stdiobridge.Stats) string {
	var b strings.Builder
	gauge := func(name, help string, v any) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %v\n", name, help, name, name, v)
	}
	counter := func(name, help string, v int64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
	}
	gauge("fold_stdio_sessions", "Current downstream sessions, one child process each.", s.Sessions)
	gauge("fold_stdio_max_sessions", "Configured session ceiling.", s.MaxSessions)
	counter("fold_stdio_spawned_total", "Child processes started.", s.Spawned)
	counter("fold_stdio_spawn_errors_total", "Child processes that failed to start.", s.SpawnErrors)
	counter("fold_stdio_rejected_total", "Sessions refused at the ceiling.", s.Rejected)
	fmt.Fprintf(&b, "# HELP fold_stdio_build_info Build information.\n"+
		"# TYPE fold_stdio_build_info gauge\nfold_stdio_build_info{version=%q} 1\n", version)
	return b.String()
}

// requireBearer gates the MCP endpoint when --bearer-env is set. The shim runs
// a process on demand, so it must not be an open exec-over-HTTP surface on a
// shared network — the same reason fold-discovery has this flag.
func requireBearer(bearer string, next http.Handler) http.Handler {
	if bearer == "" {
		return next
	}
	want := []byte("Bearer " + bearer)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func usage() {
	fmt.Fprintf(os.Stderr, `fold-stdio %s — expose one stdio MCP server over streamable HTTP.

Usage:
  fold-stdio [flags] -- <command> [args...]

Example:
  fold-stdio --port 8091 -- npx -y @modelcontextprotocol/server-filesystem /data

The command is fixed at startup and is never taken from a request.

Flags:
`, version)
	flag.PrintDefaults()
}

// isLoopback reports whether a bind address is confined to the local host.
// The empty string and 0.0.0.0/:: mean "every interface", which is not.
func isLoopback(host string) bool {
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
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
