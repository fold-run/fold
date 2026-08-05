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
//	FOLD_LOAD_JSON         path to write full results as JSON
//	FOLD_LOAD_FOLD_URL     bench an already-running gateway at this /mcp URL
//	                       instead of spawning the topology (pair with
//	                       FOLD_LOAD_DIRECT_URL to keep the direct baseline;
//	                       omit it to run the fold stages alone)
//	FOLD_LOAD_DIRECT_URL   /mcp URL for the direct baseline when
//	                       FOLD_LOAD_FOLD_URL is set
//
// Run from the repo root: `make loadtest` or `go run ./tools/perf`.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// --- fixture upstream (child process) --------------------------------------

func upstreamMain() {
	server := mcp.NewServer(&mcp.Implementation{Name: "load-upstream", Version: "1.0"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        "echo",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "upstream listen:", err)
		os.Exit(1)
	}
	fmt.Printf("UPSTREAM_URL=http://%s/mcp\n", ln.Addr())
	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	if err := http.Serve(ln, mux); err != nil {
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
}

func loadConfig() (config, error) {
	cfg := config{
		mode:     envOr("FOLD_LOAD_MODE", "namespaced"),
		duration: time.Duration(envInt("FOLD_LOAD_DURATION", 10)) * time.Second,
		warmup:   time.Duration(envInt("FOLD_LOAD_WARMUP", 3)) * time.Second,
		jsonOut:  os.Getenv("FOLD_LOAD_JSON"),
	}
	if cfg.mode != "namespaced" && cfg.mode != "passthrough" {
		return cfg, fmt.Errorf("FOLD_LOAD_MODE must be namespaced or passthrough, got %q", cfg.mode)
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
		return sweep(cfg, targets)
	}

	tmp, err := os.MkdirTemp("", "fold-loadtest-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	// The real production entry: build ./cmd/fold and run the binary.
	foldBin := filepath.Join(tmp, "fold")
	build := exec.Command("go", "build", "-o", foldBin, "./cmd/fold")
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
	upstream := exec.Command(exe, "-upstream")
	upstreamOut, err := upstream.StdoutPipe()
	if err != nil {
		return err
	}
	upstream.Stderr = os.Stderr
	if err := upstream.Start(); err != nil {
		return err
	}
	upstreamURL, err := scanFor(upstreamOut, "UPSTREAM_URL=")
	if err != nil {
		shutdown(upstream)
		return err
	}
	watch(upstream, "upstream")

	type upstreamEntry struct {
		ID        string `json:"id"`
		URL       string `json:"url"`
		Namespace string `json:"namespace,omitempty"`
	}
	entry := upstreamEntry{ID: "load", URL: upstreamURL}
	if cfg.mode == "namespaced" {
		entry.Namespace = "load"
	}
	foldCfg, _ := json.Marshal(map[string]any{"upstreams": []upstreamEntry{entry}})
	cfgPath := filepath.Join(tmp, "fold.config.json")
	if err := os.WriteFile(cfgPath, foldCfg, 0o600); err != nil {
		shutdown(upstream)
		return err
	}

	port, err := freePort()
	if err != nil {
		shutdown(upstream)
		return err
	}
	gateway := exec.Command(foldBin, "--config", cfgPath, "--port", strconv.Itoa(port))
	gateway.Stderr = os.Stderr
	if err := gateway.Start(); err != nil {
		shutdown(upstream)
		return err
	}
	watch(gateway, "gateway")
	defer shutdown(upstream, gateway)

	gatewayURL := fmt.Sprintf("http://127.0.0.1:%d/mcp", port)
	if err := waitHealthy(fmt.Sprintf("http://127.0.0.1:%d/healthz", port)); err != nil {
		return err
	}

	fmt.Printf("upstream  %s\n", upstreamURL)
	fmt.Printf("gateway   %s  (real binary, pid %d)\n", gatewayURL, gateway.Process.Pid)
	return sweep(cfg, [][2]string{{"direct", upstreamURL}, {"fold", gatewayURL}})
}

// sweep drives the full scenario × connections × target matrix and reports.
func sweep(cfg config, targets [][2]string) error {
	toolName := func(target string) string {
		if target == "fold" && cfg.mode == "namespaced" {
			return "load__echo"
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

	fmt.Printf("%-18s  %5s  %8s  %9s  %9s  %9s  %7s\n",
		"target/scenario", "conns", "req/s", "p50", "p90", "p99", "non-ok")
	var results []stageResult
	for _, scenario := range cfg.scenarios {
		for _, conns := range cfg.connections {
			for _, t := range targets {
				r, err := runStage(t[1], scenario, toolName(t[0]), conns, cfg.warmup, cfg.duration)
				if err != nil {
					return fmt.Errorf("stage %s %s c=%d: %w", t[0], scenario, conns, err)
				}
				r.Target, r.Scenario, r.Connections = t[0], scenario, conns
				fmt.Printf("%-18s  %5d  %8.0f  %8.1fms  %8.1fms  %8.1fms  %7d\n",
					t[0]+" "+scenario, conns, r.RPS, r.P50Ms, r.P90Ms, r.P99Ms, r.Errors)
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
			"results":     results,
		}, "", "  ")
		if err := os.WriteFile(cfg.jsonOut, out, 0o644); err != nil {
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
	defer session.Close()
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
		defer s.Close()
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
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func waitHealthy(url string) error {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		res, err := http.Get(url)
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("gateway never became healthy at %s", url)
}
