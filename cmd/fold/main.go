// Command fold runs the fold-go MCP gateway: one governed endpoint between
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

	"github.com/fold-run/fold-go/config"
	"github.com/fold-run/fold-go/gateway"
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
		host        = flag.String("host", "", "address to bind (default all interfaces)")
		validate    = flag.Bool("validate", false, "validate the config and exit")
		showVersion = flag.Bool("version", false, "print the version and exit")
		logFormat   = flag.String("log-format", "text", "log format: text | json")
		logLevel    = flag.String("log-level", "info", "log level: debug | info | warn | error")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("fold", gateway.Version())
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
	var cfg *config.Config
	var err error
	if strings.HasPrefix(strings.TrimSpace(path), "{") {
		// FOLD_CONFIG may carry the JSON document itself (convenient for
		// container env injection), mirroring fold on Workers.
		cfg, err = config.Parse([]byte(path))
	} else {
		cfg, err = config.Load(path)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fold: %v\n", err)
		os.Exit(1)
	}
	if *validate {
		fmt.Println("config OK")
		return
	}

	logger := newLogger(*logFormat, *logLevel)

	gw, err := gateway.New(cfg, gateway.WithLogger(logger))
	if err != nil {
		fmt.Fprintf(os.Stderr, "fold: %v\n", err)
		os.Exit(1)
	}
	defer gw.Close()

	addr := *host + ":" + strconv.Itoa(*port)
	srv := &http.Server{Addr: addr, Handler: gw.Handler()}

	go func() {
		logger.Info("listening", "addr", addr, "mcpPath", cfg.MCPPath())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	logger.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}
