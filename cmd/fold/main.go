// Command fold runs the fold MCP gateway: one governed endpoint between
// every MCP client and every MCP server.
//
//	fold --config fold.config.json --port 8080
//	fold --config fold.config.json --validate
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

	"github.com/fold-run/fold/config"
	"github.com/fold-run/fold/gateway"
)

// newLogger builds the structured logger from the CLI flags.
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

func main() {
	var (
		configPath  = flag.String("config", "", "path to fold.config.json (or set FOLD_CONFIG to a path or inline JSON)")
		port        = flag.Int("port", 8080, "port to listen on")
		host        = flag.String("host", "127.0.0.1", "address to bind; set 0.0.0.0 to expose beyond loopback")
		validate    = flag.Bool("validate", false, "validate the config and exit")
		showSchema  = flag.Bool("schema", false, "print the config JSON Schema and exit")
		showVersion = flag.Bool("version", false, "print the version and exit")
		watch       = flag.Bool("watch", false, "watch the config file and hot-reload on change (SIGHUP always reloads)")
		logFormat   = flag.String("log-format", "text", "log format: text | json")
		logLevel    = flag.String("log-level", "info", "log level: debug | info | warn | error")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("fold", gateway.Version())
		return
	}
	if *showSchema {
		_, _ = os.Stdout.Write(config.Schema())
		return
	}

	path := *configPath
	if path == "" {
		path = os.Getenv("FOLD_CONFIG")
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, "fold: --config <path> (or FOLD_CONFIG) is required")
		os.Exit(2)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fold: %v\n", err)
		os.Exit(1)
	}
	if *validate {
		fmt.Println("config OK")
		return
	}

	logger := newLogger(*logFormat, *logLevel)

	watchPath := ""
	if *watch {
		if isInline(path) {
			logger.Warn("--watch ignored: config came from inline FOLD_CONFIG JSON, there is no file to watch")
		} else {
			watchPath = path
		}
	}
	addr := *host + ":" + strconv.Itoa(*port)
	if err := run(cfg, path, watchPath, logger, addr); err != nil {
		fmt.Fprintf(os.Stderr, "fold: %v\n", err)
		os.Exit(1)
	}
}

// isInline reports whether the config source is the JSON document itself
// rather than a file path.
func isInline(source string) bool {
	return strings.HasPrefix(strings.TrimSpace(source), "{")
}

// loadConfig reads the config from source: a file path, or — convenient for
// container env injection — the inline JSON document itself.
func loadConfig(source string) (*config.Config, error) {
	if isInline(source) {
		return config.Parse([]byte(source))
	}
	return config.Load(source)
}

// configWatch polls path's modification time and signals ch on change. A
// coarse mtime poll needs no filesystem-notification dependency and behaves
// correctly across the atomic-rename updates editors and Kubernetes
// ConfigMap mounts perform.
func configWatch(path string, ch chan<- struct{}, stop <-chan struct{}) {
	last := time.Time{}
	if st, err := os.Stat(path); err == nil { //nolint:gosec // path is the operator-supplied config location
		last = st.ModTime()
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			st, err := os.Stat(path) //nolint:gosec // path is the operator-supplied config location
			if err != nil {
				continue // transiently missing during an atomic replace
			}
			if mt := st.ModTime(); mt != last {
				last = mt
				select {
				case ch <- struct{}{}:
				default:
				}
			}
		case <-stop:
			return
		}
	}
}

// run serves the gateway on addr until SIGINT/SIGTERM or a server error,
// then shuts down gracefully. SIGHUP — and, with watchPath set, a change to
// the config file — hot-reloads the upstream set and policy from source
// without dropping the listener. Kept separate from main so deferred cleanup
// (gateway sessions, state provider) runs on every exit path.
func run(cfg *config.Config, source, watchPath string, logger *slog.Logger, addr string) error {
	gw, err := gateway.New(cfg, gateway.WithLogger(logger))
	if err != nil {
		return err
	}
	defer gw.Close()

	// No write timeout: MCP responses ride long-lived SSE streams.
	srv := &http.Server{Addr: addr, Handler: gw.Handler(), ReadHeaderTimeout: 10 * time.Second}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", addr, "mcpPath", cfg.MCPPath())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	reload := func(trigger string) {
		next, err := loadConfig(source)
		if err != nil {
			logger.Error("reload: config rejected, keeping the running configuration", "trigger", trigger, "err", err)
			return
		}
		if err := gw.Reload(next); err != nil {
			logger.Error("reload failed, keeping the running configuration", "trigger", trigger, "err", err)
		}
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	changed := make(chan struct{}, 1)
	if watchPath != "" {
		watchStop := make(chan struct{})
		defer close(watchStop)
		go configWatch(watchPath, changed, watchStop)
	}
	for {
		select {
		case err := <-errCh:
			return err
		case <-hup:
			reload("SIGHUP")
		case <-changed:
			reload("file change")
		case <-stop:
			logger.Info("shutting down")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return srv.Shutdown(ctx)
		}
	}
}
