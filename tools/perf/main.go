// Load test: throughput and tail latency for one gateway instance under a
// concurrency sweep. Where bench/latency_test.go answers "how much latency
// does fold add?" (sequential, budget-enforced, CI-gated), this answers "how
// many requests per second does one instance sustain, and at what p99?" —
// the numbers an enterprise buyer asks for.
//
// Topology: three processes, so the load generator, the gateway, and the
// upstream never share a scheduler. The gateway is the REAL production
// entry — the built ./cmd/fold binary with a config file — not a
// test-harness assembly.
//
//	driver (this process) ──▶ fold binary ──▶ fixture upstream (child)
//
// Each stage runs both DIRECT (driver → upstream) and FOLD (driver →
// gateway → upstream) so the upstream's own ceiling is visible: fold's RPS
// can never exceed direct's, and the gap between them is the honest cost.
//
// The driver models real MCP clients: each "connection" is an official-SDK
// client session (initialize once, then sequential calls), because fold's
// client side is session-keyed — there is no stateless one-shot POST to
// hammer, and a load test should measure what a real client experiences.
//
// Knobs (environment):
//
//	FOLD_LOAD_MODE         "namespaced" (body parse + name rewrite — the
//	                       multi-upstream enterprise path, default) or
//	                       "passthrough" (single-upstream path)
//	FOLD_LOAD_CONNECTIONS  comma-separated sweep (default "8,64,256")
//	FOLD_LOAD_DURATION     seconds per measured stage (default 10)
//	FOLD_LOAD_WARMUP       seconds of unmeasured warmup per stage (default 3)
//	FOLD_LOAD_SCENARIOS    comma-separated: tools/call,tools/list (default both;
//	                       note tools/list through fold rides the list cache)
//	FOLD_LOAD_UPSTREAMS    fixture upstreams to federate (default 1). The
//	                       tools/list path fans out to every one of them,
//	                       merges, filters, namespaces, and paginates, so its
//	                       cost scales with this and with FOLD_LOAD_TOOLS —
//	                       a size-1 federation does not exercise any of it.
//	FOLD_LOAD_TOOLS        tools each fixture upstream exposes (default 1)
//	FOLD_LOAD_JSON         path to write full results as JSON
//	FOLD_LOAD_FOLD_URL     bench an already-running gateway at this /mcp URL
//	                       instead of spawning the topology (pair with
//	                       FOLD_LOAD_DIRECT_URL to keep the direct baseline;
//	                       omit it to run the fold stages alone)
//	FOLD_LOAD_DIRECT_URL   /mcp URL for the direct baseline when
//	                       FOLD_LOAD_FOLD_URL is set
//	FOLD_LOAD_METRICS_URL  the gateway's /metrics URL when FOLD_LOAD_FOLD_URL
//	                       is set and metrics live on another listener
//	                       (default: FOLD_LOAD_FOLD_URL with /mcp → /metrics)
//
// Besides throughput and latency, every fold stage records what the gateway
// process itself looked like at the end of it — resident memory, goroutines,
// live downstream sessions, scraped from its own /metrics — and the spawned
// topology reports the process's peak RSS at exit. A ten-second stage cannot
// see a slow leak, but it can see what one session costs and what the
// process weighs at each concurrency, which is what a memory limit is set
// from.
//
// Run from the repo root: `make loadtest` or `go run ./tools/perf`.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	upstreamRole := flag.Bool("upstream", false, "run as the fixture upstream (internal)")
	flag.Parse()
	if *upstreamRole {
		upstreamMain()
		return
	}
	if err := runnerMain(); err != nil {
		fmt.Fprintln(os.Stderr, "FATAL:", err)
		os.Exit(1)
	}
}

// upstreamID names the i-th fixture upstream; it is both the config id and,
// in namespaced mode, the namespace its tools are exposed under.
func upstreamID(i int) string { return fmt.Sprintf("load%02d", i) }

// --- fixture upstream (child process) --------------------------------------

func upstreamMain() {
	server := mcp.NewServer(&mcp.Implementation{Name: "load-upstream", Version: "1.0"}, nil)
	echo := func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	}
	// "echo" is always present — it is what the tools/call scenario invokes.
	// Any extra tools exist to give the list path something to merge, and
	// carry a representative schema because a real tool list is mostly
	// schema by volume.
	server.AddTool(&mcp.Tool{
		Name:        "echo",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, echo)
	filler := json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"the search query to run against the corpus"},"limit":{"type":"integer"},"filters":{"type":"array","items":{"type":"string"}}},"required":["query"]}`)
	for i := 1; i < envInt("FOLD_LOAD_TOOLS", 1); i++ {
		server.AddTool(&mcp.Tool{
			Name:        fmt.Sprintf("tool-%04d", i),
			Description: "a representative tool with a representative description string",
			InputSchema: filler,
		}, echo)
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "upstream listen:", err)
		os.Exit(1)
	}
	fmt.Printf("UPSTREAM_URL=http://%s/mcp\n", ln.Addr())
	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.Serve(ln); err != nil {
		fmt.Fprintln(os.Stderr, "upstream serve:", err)
		os.Exit(1)
	}
}

// --- config -----------------------------------------------------------------

type config struct {
	mode        string
	connections []int
	duration    time.Duration
	warmup      time.Duration
	scenarios   []string
	jsonOut     string
	upstreams   int
	tools       int
}

func loadConfig() (config, error) {
	cfg := config{
		mode:      envOr("FOLD_LOAD_MODE", "namespaced"),
		duration:  time.Duration(envInt("FOLD_LOAD_DURATION", 10)) * time.Second,
		warmup:    time.Duration(envInt("FOLD_LOAD_WARMUP", 3)) * time.Second,
		jsonOut:   os.Getenv("FOLD_LOAD_JSON"),
		upstreams: envInt("FOLD_LOAD_UPSTREAMS", 1),
		tools:     envInt("FOLD_LOAD_TOOLS", 1),
	}
	if cfg.mode != "namespaced" && cfg.mode != "passthrough" {
		return cfg, fmt.Errorf("FOLD_LOAD_MODE must be namespaced or passthrough, got %q", cfg.mode)
	}
	if cfg.upstreams < 1 {
		return cfg, fmt.Errorf("FOLD_LOAD_UPSTREAMS must be at least 1, got %d", cfg.upstreams)
	}
	if cfg.tools < 1 {
		return cfg, fmt.Errorf("FOLD_LOAD_TOOLS must be at least 1, got %d", cfg.tools)
	}
	// Passthrough is by definition a single un-namespaced upstream.
	if cfg.mode == "passthrough" && cfg.upstreams != 1 {
		return cfg, fmt.Errorf("FOLD_LOAD_MODE=passthrough requires FOLD_LOAD_UPSTREAMS=1, got %d", cfg.upstreams)
	}
	for _, s := range strings.Split(envOr("FOLD_LOAD_CONNECTIONS", "8,64,256"), ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("bad FOLD_LOAD_CONNECTIONS entry %q", s)
		}
		cfg.connections = append(cfg.connections, n)
	}
	for _, s := range strings.Split(envOr("FOLD_LOAD_SCENARIOS", "tools/call,tools/list"), ",") {
		s = strings.TrimSpace(s)
		if s != "tools/call" && s != "tools/list" {
			return cfg, fmt.Errorf("unknown scenario %q", s)
		}
		cfg.scenarios = append(cfg.scenarios, s)
	}
	return cfg, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// --- runner -----------------------------------------------------------------

type stageResult struct {
	Target      string  `json:"target"`
	Scenario    string  `json:"scenario"`
	Connections int     `json:"connections"`
	DurationSec float64 `json:"durationSec"`
	RPS         float64 `json:"rps"`
	P50Ms       float64 `json:"p50Ms"`
	P90Ms       float64 `json:"p90Ms"`
	P99Ms       float64 `json:"p99Ms"`
	Errors      int     `json:"errors"`

	// Gateway process state at the end of a fold stage, from its /metrics;
	// zero on direct stages and when no metrics URL is known.
	GatewayRSSBytes   float64 `json:"gatewayRssBytes,omitempty"`
	GatewayGoroutines float64 `json:"gatewayGoroutines,omitempty"`
	GatewaySessions   float64 `json:"gatewaySessions,omitempty"`
}

// processSnapshot is what the gateway looked like at one moment.
type processSnapshot struct {
	RSSBytes   float64 `json:"rssBytes"`
	Goroutines float64 `json:"goroutines"`
	Sessions   float64 `json:"sessions"`
}

// scrapeProcess reads the three gauges a sizing decision needs from a
// Prometheus exposition. Missing series read as zero rather than failing the
// run: an external gateway may have its metrics on a listener the driver
// cannot reach, and a missing column is better than an aborted sweep.
func scrapeProcess(metricsURL string) (processSnapshot, error) {
	var snap processSnapshot
	if metricsURL == "" {
		return snap, nil
	}
	res, err := http.Get(metricsURL) //nolint:gosec // URL built by this harness or given by the operator
	if err != nil {
		return snap, err
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return snap, err
	}
	for _, line := range strings.Split(string(body), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		v, err := strconv.ParseFloat(f[1], 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "process_resident_memory_bytes":
			snap.RSSBytes = v
		case "go_goroutines":
			snap.Goroutines = v
		case "fold_downstream_sessions":
			snap.Sessions = v
		}
	}
	return snap, nil
}

// peakRSSBytes reads the child's maximum resident set after it has exited.
// ru_maxrss is kilobytes on Linux and bytes on Darwin.
func peakRSSBytes(state *os.ProcessState) float64 {
	if state == nil {
		return 0
	}
	ru, ok := state.SysUsage().(*syscall.Rusage)
	if !ok || ru == nil {
		return 0
	}
	peak := float64(ru.Maxrss)
	if runtime.GOOS != "darwin" {
		peak *= 1024
	}
	return peak
}

func humanBytes(b float64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GiB", b/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", b/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KiB", b/(1<<10))
	default:
		return fmt.Sprintf("%.0f B", b)
	}
}

func runnerMain() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	// External-target mode: drive an already-running gateway (and optional
	// direct baseline) instead of spawning the loopback topology.
	if foldURL := os.Getenv("FOLD_LOAD_FOLD_URL"); foldURL != "" {
		targets := [][2]string{{"fold", foldURL}}
		if directURL := os.Getenv("FOLD_LOAD_DIRECT_URL"); directURL != "" {
			targets = append([][2]string{{"direct", directURL}}, targets...)
		}
		metricsURL := envOr("FOLD_LOAD_METRICS_URL", strings.TrimSuffix(foldURL, "/mcp")+"/metrics")
		return sweep(cfg, targets, metricsURL)
	}

	tmp, err := os.MkdirTemp("", "fold-loadtest-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	// The real production entry: build ./cmd/fold and run the binary.
	foldBin := filepath.Join(tmp, "fold")
	build := exec.Command("go", "build", "-o", foldBin, "./cmd/fold") //nolint:gosec // harness builds its own topology
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("build ./cmd/fold (run from the repo root): %w", err)
	}

	// A dead process under load must abort the run: benching a corpse
	// measures connection-refused turnaround, not the gateway.
	var shuttingDown bool
	var mu sync.Mutex
	watch := func(cmd *exec.Cmd, name string) {
		go func() {
			err := cmd.Wait()
			mu.Lock()
			defer mu.Unlock()
			if !shuttingDown {
				fmt.Fprintf(os.Stderr, "FATAL: %s exited mid-run (%v) — aborting\n", name, err)
				os.Exit(1)
			}
		}()
	}
	shutdown := func(cmds ...*exec.Cmd) {
		mu.Lock()
		shuttingDown = true
		mu.Unlock()
		for _, c := range cmds {
			if c.Process != nil {
				_ = c.Process.Kill()
			}
		}
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// One fixture process per federated upstream, each carrying the same tool
	// count. The first is also the DIRECT baseline's target, so the direct
	// and fold stages hit an identical server.
	type upstreamEntry struct {
		ID        string `json:"id"`
		URL       string `json:"url"`
		Namespace string `json:"namespace,omitempty"`
	}
	var upstreams []*exec.Cmd
	var entries []upstreamEntry
	for i := range cfg.upstreams {
		up := exec.Command(exe, "-upstream") //nolint:gosec // re-exec self as the fixture upstream
		up.Env = append(os.Environ(), fmt.Sprintf("FOLD_LOAD_TOOLS=%d", cfg.tools))
		out, err := up.StdoutPipe()
		if err != nil {
			shutdown(upstreams...)
			return err
		}
		up.Stderr = os.Stderr
		if err := up.Start(); err != nil {
			shutdown(upstreams...)
			return err
		}
		upstreams = append(upstreams, up)
		url, err := scanFor(out, "UPSTREAM_URL=")
		if err != nil {
			shutdown(upstreams...)
			return err
		}
		watch(up, fmt.Sprintf("upstream-%d", i))

		entry := upstreamEntry{ID: upstreamID(i), URL: url}
		if cfg.mode == "namespaced" {
			entry.Namespace = entry.ID
		}
		entries = append(entries, entry)
	}
	upstreamURL := entries[0].URL
	foldCfg, _ := json.Marshal(map[string]any{"upstreams": entries})
	cfgPath := filepath.Join(tmp, "fold.config.json")
	if err := os.WriteFile(cfgPath, foldCfg, 0o600); err != nil {
		shutdown(upstreams...)
		return err
	}

	port, err := freePort()
	if err != nil {
		shutdown(upstreams...)
		return err
	}
	gateway := exec.Command(foldBin, "--config", cfgPath, "--port", strconv.Itoa(port)) //nolint:gosec // the binary we just built
	gateway.Stderr = os.Stderr
	if err := gateway.Start(); err != nil {
		shutdown(upstreams...)
		return err
	}
	watch(gateway, "gateway")
	defer shutdown(append(upstreams, gateway)...)

	gatewayURL := fmt.Sprintf("http://127.0.0.1:%d/mcp", port)
	metricsURL := fmt.Sprintf("http://127.0.0.1:%d/metrics", port)
	if err := waitHealthy(fmt.Sprintf("http://127.0.0.1:%d/health", port)); err != nil {
		return err
	}

	fmt.Printf("upstream  %s\n", upstreamURL)
	fmt.Printf("gateway   %s  (real binary, pid %d)\n", gatewayURL, gateway.Process.Pid)
	sweepErr := sweep(cfg, [][2]string{{"direct", upstreamURL}, {"fold", gatewayURL}}, metricsURL)

	// Peak RSS is a property of the whole run, read from the kernel once the
	// process has exited: the number a memory limit is set against.
	shutdown(append(upstreams, gateway)...)
	_ = gateway.Wait()
	if peak := peakRSSBytes(gateway.ProcessState); peak > 0 {
		fmt.Printf("gateway peak RSS over the run: %s (ru_maxrss)\n", humanBytes(peak))
		if cfg.jsonOut != "" {
			appendJSON(cfg.jsonOut, "gatewayPeakRssBytes", peak)
		}
	}
	return sweepErr
}

// appendJSON adds one top-level field to an already-written results file.
func appendJSON(path, key string, value any) {
	data, err := os.ReadFile(path) //nolint:gosec // path the harness itself wrote
	if err != nil {
		return
	}
	var doc map[string]any
	if json.Unmarshal(data, &doc) != nil {
		return
	}
	doc[key] = value
	out, _ := json.MarshalIndent(doc, "", "  ")
	_ = os.WriteFile(path, out, 0o600)
}

// sweep drives the full scenario × connections × target matrix and reports.
// metricsURL is the gateway's exposition, read at the end of every fold stage.
func sweep(cfg config, targets [][2]string, metricsURL string) error {
	toolName := func(target string) string {
		if target == "fold" && cfg.mode == "namespaced" {
			return upstreamID(0) + "__echo"
		}
		return "echo"
	}

	fmt.Printf("sweep     mode=%s  connections=%v  duration=%s  warmup=%s  scenarios=%v\n\n",
		cfg.mode, cfg.connections, cfg.duration, cfg.warmup, cfg.scenarios)

	// One real request before benching: a load test that measures a fast
	// error path instead of proxied successes is worse than none.
	for _, t := range targets {
		for _, scenario := range cfg.scenarios {
			if err := probe(t[1], scenario, toolName(t[0])); err != nil {
				return fmt.Errorf("probe %s %s: %w", t[0], scenario, err)
			}
		}
	}

	idle, err := scrapeProcess(metricsURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "note: gateway metrics unavailable at %s (%v); process columns will be empty\n", metricsURL, err)
		metricsURL = ""
	} else if metricsURL != "" {
		fmt.Printf("gateway idle: RSS %s, %.0f goroutines\n\n", humanBytes(idle.RSSBytes), idle.Goroutines)
	}

	fmt.Printf("%-18s  %5s  %8s  %9s  %9s  %9s  %7s  %10s  %6s  %5s\n",
		"target/scenario", "conns", "req/s", "p50", "p90", "p99", "non-ok", "gw RSS", "gorou", "sess")
	var results []stageResult
	for _, scenario := range cfg.scenarios {
		for _, conns := range cfg.connections {
			for _, t := range targets {
				r, err := runStage(t[1], scenario, toolName(t[0]), conns, cfg.warmup, cfg.duration)
				if err != nil {
					return fmt.Errorf("stage %s %s c=%d: %w", t[0], scenario, conns, err)
				}
				r.Target, r.Scenario, r.Connections = t[0], scenario, conns
				proc := ""
				if t[0] == "fold" && metricsURL != "" {
					// runStage has returned, so the driver's sessions are
					// closed; what the gateway still holds for them until its
					// own sweeps run is the deployment-realistic residue of a
					// load peak, and that is what gets recorded.
					if snap, err := scrapeProcess(metricsURL); err == nil {
						r.GatewayRSSBytes, r.GatewayGoroutines, r.GatewaySessions = snap.RSSBytes, snap.Goroutines, snap.Sessions
						proc = fmt.Sprintf("  %10s  %6.0f  %5.0f", humanBytes(snap.RSSBytes), snap.Goroutines, snap.Sessions)
					}
				}
				fmt.Printf("%-18s  %5d  %8.0f  %8.1fms  %8.1fms  %8.1fms  %7d%s\n",
					t[0]+" "+scenario, conns, r.RPS, r.P50Ms, r.P90Ms, r.P99Ms, r.Errors, proc)
				results = append(results, r)
			}
		}
		fmt.Println()
	}

	// Headline: best fold RPS and its retention vs direct at the same stage.
	var foldBest *stageResult
	for i := range results {
		r := &results[i]
		if r.Target == "fold" && r.Scenario == "tools/call" &&
			(foldBest == nil || r.RPS > foldBest.RPS) {
			foldBest = r
		}
	}
	if foldBest != nil {
		retention := "?"
		for _, r := range results {
			if r.Target == "direct" && r.Scenario == "tools/call" && r.Connections == foldBest.Connections {
				retention = fmt.Sprintf("%.0f%%", foldBest.RPS/r.RPS*100)
			}
		}
		fmt.Printf("headline: one fold instance sustained %.0f req/s tools/call at %d connections "+
			"(p99 %.1fms, %s of the upstream's direct ceiling)\n",
			foldBest.RPS, foldBest.Connections, foldBest.P99Ms, retention)
	}

	if cfg.jsonOut != "" {
		out, _ := json.MarshalIndent(map[string]any{
			"mode":        cfg.mode,
			"connections": cfg.connections,
			"durationSec": cfg.duration.Seconds(),
			"gatewayIdle": idle,
			"results":     results,
		}, "", "  ")
		if err := os.WriteFile(cfg.jsonOut, out, 0o600); err != nil {
			return err
		}
		fmt.Println("wrote", cfg.jsonOut)
	}

	totalErrs := 0
	for _, r := range results {
		totalErrs += r.Errors
	}
	if totalErrs > 0 {
		return fmt.Errorf("%d errored calls across stages", totalErrs)
	}
	return nil
}

// --- driver -----------------------------------------------------------------

// connect opens one SDK client session — one modeled MCP client.
func connect(url string, httpClient *http.Client) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "fold-loadtest", Version: "0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             url,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
		// A load test must not paper over failures with resends: one call,
		// one request. Retries would inflate server-side counts while the
		// driver reports a single slow/failed call.
		MaxRetries: -1,
	}, nil)
}

func call(session *mcp.ClientSession, scenario, tool string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if scenario == "tools/list" {
		_, err := session.ListTools(ctx, nil)
		return err
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      tool,
		Arguments: map[string]any{"value": "load-test-payload"},
	})
	if err != nil {
		return err
	}
	if res.IsError {
		return fmt.Errorf("tool error result")
	}
	return nil
}

func probe(url, scenario, tool string) error {
	session, err := connect(url, http.DefaultClient)
	if err != nil {
		return err
	}
	defer func() { _ = session.Close() }()
	return call(session, scenario, tool)
}

// runStage drives `conns` concurrent sessions in sequential-call loops:
// warmup samples are discarded, measured samples aggregate into RPS and
// percentiles. Session setup happens before the clock starts.
func runStage(url, scenario, tool string, conns int, warmup, duration time.Duration) (stageResult, error) {
	sessions := make([]*mcp.ClientSession, conns)
	for i := range sessions {
		// One http.Client per session: each modeled connection owns its
		// socket, like one autocannon connection — and pooled transports
		// showed cross-session interference at higher concurrency.
		s, err := connect(url, &http.Client{Transport: &http.Transport{
			MaxIdleConns:        4,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
		}})
		if err != nil {
			return stageResult{}, fmt.Errorf("connect session %d: %w", i, err)
		}
		sessions[i] = s
		defer func() { _ = s.Close() }()
	}

	start := time.Now()
	warmupEnd := start.Add(warmup)
	measureEnd := warmupEnd.Add(duration)

	type lane struct {
		lats     []time.Duration
		errs     int
		firstErr error
	}
	lanes := make([]lane, conns)
	var wg sync.WaitGroup
	for i := range sessions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			session := sessions[i]
			for {
				begin := time.Now()
				if begin.After(measureEnd) {
					return
				}
				err := call(session, scenario, tool)
				if begin.After(warmupEnd) {
					if err != nil {
						lanes[i].errs++
						if lanes[i].firstErr == nil {
							lanes[i].firstErr = err
						}
					} else {
						lanes[i].lats = append(lanes[i].lats, time.Since(begin))
					}
				}
			}
		}(i)
	}
	wg.Wait()

	var all []time.Duration
	errs := 0
	for _, l := range lanes {
		all = append(all, l.lats...)
		errs += l.errs
		if l.firstErr != nil {
			fmt.Fprintf(os.Stderr, "stage error sample: %v\n", l.firstErr)
			break
		}
	}
	slices.Sort(all)
	ms := func(p float64) float64 {
		if len(all) == 0 {
			return 0
		}
		return float64(all[int(float64(len(all)-1)*p)]) / float64(time.Millisecond)
	}
	return stageResult{
		DurationSec: duration.Seconds(),
		RPS:         float64(len(all)) / duration.Seconds(),
		P50Ms:       ms(0.50),
		P90Ms:       ms(0.90),
		P99Ms:       ms(0.99),
		Errors:      errs,
	}, nil
}

// --- plumbing ---------------------------------------------------------------

func scanFor(out interface{ Read([]byte) (int, error) }, prefix string) (string, error) {
	scanner := bufio.NewScanner(out)
	deadline := time.After(15 * time.Second)
	found := make(chan string, 1)
	go func() {
		for scanner.Scan() {
			if v, ok := strings.CutPrefix(scanner.Text(), prefix); ok {
				found <- v
				return
			}
		}
	}()
	select {
	case v := <-found:
		return v, nil
	case <-deadline:
		return "", fmt.Errorf("timed out waiting for %s from child", prefix)
	}
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	return port, ln.Close()
}

func waitHealthy(url string) error {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		res, err := http.Get(url) //nolint:gosec // loopback URL built by this harness
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("gateway never became healthy at %s", url)
}
