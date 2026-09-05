package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/fold-run/fold/config"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestMain re-execs the test binary as the fold CLI when FOLD_TEST_MAIN is
// set, so the subprocess tests below exercise the real main() — flag
// parsing, exit codes, and shutdown — without a separate build step.
func TestMain(m *testing.M) {
	if os.Getenv("FOLD_TEST_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

const validConfig = `{"upstreams":[{"id":"u","url":"https://example.com/mcp"}]}`

func runFold(t *testing.T, env map[string]string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "FOLD_TEST_MAIN=1")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code = 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("run: %v", err)
	}
	return out.String(), errb.String(), code
}

func TestSchemaFlag(t *testing.T) {
	stdout, _, code := runFold(t, nil, "--schema")
	if code != 0 {
		t.Fatalf("--schema: exit=%d", code)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("--schema output is not JSON: %v", err)
	}
	if doc["title"] != "fold gateway configuration" {
		t.Errorf("--schema printed the wrong document: title=%v", doc["title"])
	}
}

func TestVersionFlag(t *testing.T) {
	stdout, _, code := runFold(t, nil, "--version")
	if code != 0 || !strings.HasPrefix(stdout, "fold ") {
		t.Errorf("--version: code=%d stdout=%q", code, stdout)
	}
}

func TestMissingConfigIsUsageError(t *testing.T) {
	_, stderr, code := runFold(t, nil)
	if code != 2 {
		t.Errorf("no config: exit=%d, want 2", code)
	}
	if !strings.Contains(stderr, "--config") {
		t.Errorf("stderr should point at --config: %q", stderr)
	}
}

func TestValidateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fold.config.json")
	if err := os.WriteFile(path, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, code := runFold(t, nil, "--config", path, "--validate")
	if code != 0 || !strings.Contains(stdout, "config OK") {
		t.Errorf("validate: code=%d stdout=%q", code, stdout)
	}
}

func TestValidateInlineEnvConfig(t *testing.T) {
	stdout, _, code := runFold(t, map[string]string{"FOLD_CONFIG": validConfig}, "--validate")
	if code != 0 || !strings.Contains(stdout, "config OK") {
		t.Errorf("inline FOLD_CONFIG: code=%d stdout=%q", code, stdout)
	}
}

func TestInvalidConfigFails(t *testing.T) {
	for name, doc := range map[string]string{
		"no upstreams": `{"upstreams":[]}`,
		"not json":     `{`,
	} {
		_, stderr, code := runFold(t, map[string]string{"FOLD_CONFIG": doc}, "--validate")
		if code != 1 {
			t.Errorf("%s: exit=%d, want 1", name, code)
		}
		if !strings.Contains(stderr, "fold:") {
			t.Errorf("%s: stderr %q missing fold: prefix", name, stderr)
		}
	}
}

// syncBuffer is a Buffer safe to read while the subprocess is still writing.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// startServing boots the fold CLI as a subprocess on a free port with the
// given config file and args, and waits until /health answers.
func startServing(t *testing.T, configPath string, extraArgs ...string) (cmd *exec.Cmd, port int, stderr *syncBuffer) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port = l.Addr().(*net.TCPAddr).Port
	l.Close()

	args := append([]string{"--config", configPath, "--port", fmt.Sprint(port), "--log-format", "json"}, extraArgs...)
	cmd = exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "FOLD_TEST_MAIN=1")
	stderr = &syncBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})

	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return cmd, port, stderr
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("server never answered %s; stderr: %s", url, stderr.String())
	return nil, 0, nil
}

// waitHealthzContains polls /health until the body contains want.
func waitHealthzContains(t *testing.T, port int, want string, timeout time.Duration) bool {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			body := make([]byte, 64<<10)
			n, _ := resp.Body.Read(body)
			resp.Body.Close()
			if strings.Contains(string(body[:n]), want) {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

const twoUpstreamConfig = `{"upstreams":[
	{"id":"u1","url":"https://example.com/mcp","namespace":"u1"},
	{"id":"u2","url":"https://example.org/mcp","namespace":"u2"}]}`

// TestSIGHUPReloadsConfig: editing the config file and sending SIGHUP makes
// the new upstream set live without a restart, visible via /health.
func TestSIGHUPReloadsConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fold.config.json")
	if err := os.WriteFile(path, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd, port, stderr := startServing(t, path)

	if err := os.WriteFile(path, []byte(twoUpstreamConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	if !waitHealthzContains(t, port, `"u2"`, 5*time.Second) {
		t.Fatalf("upstream u2 never appeared after SIGHUP; stderr: %s", stderr.String())
	}
}

// TestWatchReloadsConfig: with --watch, a config-file change is picked up by
// the mtime poll with no signal at all.
func TestWatchReloadsConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fold.config.json")
	if err := os.WriteFile(path, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd, port, stderr := startServing(t, path, "--watch")
	_ = cmd

	// A fresh mtime is what the watcher keys on; the write provides it.
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(path, []byte(twoUpstreamConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitHealthzContains(t, port, `"u2"`, 10*time.Second) {
		t.Fatalf("upstream u2 never appeared under --watch; stderr: %s", stderr.String())
	}
}

// TestSIGHUPKeepsRunningOnBadConfig: a reload that fails validation is
// rejected and the server keeps serving the old configuration.
func TestSIGHUPKeepsRunningOnBadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fold.config.json")
	if err := os.WriteFile(path, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd, port, stderr := startServing(t, path)

	if err := os.WriteFile(path, []byte(`{"upstreams":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(stderr.String(), "config rejected") {
		time.Sleep(25 * time.Millisecond)
	}
	if !strings.Contains(stderr.String(), "config rejected") {
		t.Fatalf("expected reload-rejected log; stderr: %s", stderr.String())
	}
	if !waitHealthzContains(t, port, `"u"`, 2*time.Second) {
		t.Fatal("server stopped serving the old config after a bad reload")
	}
}

// TestServeAndGracefulShutdown boots the real server, confirms it answers
// /health, and expects SIGTERM to produce a clean (code 0) exit.
func TestServeAndGracefulShutdown(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	cmd := exec.Command(os.Args[0], "--port", fmt.Sprint(port), "--log-format", "json")
	cmd.Env = append(os.Environ(), "FOLD_TEST_MAIN=1", "FOLD_CONFIG="+validConfig)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// Any HTTP answer proves the process is serving; the status itself
	// reflects upstream health, which is 503 here (the example.com upstream
	// is unreachable) and not what this test asserts.
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	deadline := time.Now().Add(5 * time.Second)
	up := false
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			up = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !up {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("server never answered %s; stderr: %s", url, errb.String())
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("SIGTERM should exit cleanly, got %v; stderr: %s", err, errb.String())
		}
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatal("server did not shut down within 10s of SIGTERM")
	}
	if !strings.Contains(errb.String(), "shutting down") {
		t.Errorf("expected graceful-shutdown log, stderr: %s", errb.String())
	}
}

// TestRunHealthcheck: the self-probe answers "is the process serving" — any
// HTTP response is healthy, including /health's 503-when-all-upstreams-are-
// down, which must never restart a container. Only a dead process fails.
func TestRunHealthcheck(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}

	if got := runHealthcheck(u.Hostname(), port); got != 0 {
		t.Fatalf("healthcheck against a serving (degraded) process = %d, want 0", got)
	}
	ts.Close()
	if got := runHealthcheck(u.Hostname(), port); got != 1 {
		t.Fatalf("healthcheck against a dead port = %d, want 1", got)
	}
}

// Auth off and a non-loopback bind is the one combination the defaults are
// designed to keep apart, and until now it produced no warning at all. Each
// half alone is a supported choice and stays quiet.
func TestWarnsWhenExposedWithoutAuth(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
		host string
		warn bool
	}{
		{"quick start on loopback", &config.Config{}, "127.0.0.1", false},
		{"quick start on localhost", &config.Config{}, "localhost", false},
		{"quick start on ::1", &config.Config{}, "::1", false},
		{"auth off, exposed", &config.Config{}, "0.0.0.0", true},
		{"auth off, every interface", &config.Config{}, "", true},
		{"auth off, a real address", &config.Config{}, "10.0.0.5", true},
		{"auth on, exposed", &config.Config{Auth: &config.Auth{Mode: "required"}}, "0.0.0.0", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			warnIfExposedWithoutAuth(tc.cfg, tc.host, slog.New(slog.NewTextHandler(&buf, nil)))
			got := strings.Contains(buf.String(), "auth is disabled and the listener is bound beyond loopback")
			if got != tc.warn {
				t.Fatalf("warned=%v, want %v; log: %s", got, tc.warn, buf.String())
			}
		})
	}
}

// A SIGTERM that arrives while a client holds an SSE stream used to exit 1:
// the drain expired, Shutdown returned its deadline error, and main treated
// the expected end of a routine stop as a failure. Orchestrators read exit 1
// as a crash. The stream is still severed at the bound; the exit is 0 and the
// log says what happened.
func TestSIGTERMWithOpenStreamExitsZero(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	cmd := exec.Command(os.Args[0], "--port", fmt.Sprint(port), "--log-format", "json", "--drain-timeout", "500ms")
	cmd.Env = append(os.Environ(), "FOLD_TEST_MAIN=1", "FOLD_CONFIG="+validConfig)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(base + "/health"); err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	// Open a session and hold its standalone SSE stream open.
	initReq, _ := http.NewRequest(http.MethodPost, base+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`))
	initReq.Header.Set("Content-Type", "application/json")
	initReq.Header.Set("Accept", "application/json, text/event-stream")
	init, err := http.DefaultClient.Do(initReq)
	if err != nil {
		cmd.Process.Kill()
		t.Fatal(err)
	}
	io.Copy(io.Discard, init.Body)
	init.Body.Close()
	sid := init.Header.Get("Mcp-Session-Id")
	if sid == "" {
		cmd.Process.Kill()
		t.Fatalf("no session id from initialize (status %d)", init.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, base+"/mcp", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Mcp-Session-Id", sid)
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		cmd.Process.Kill()
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK {
		cmd.Process.Kill()
		t.Fatalf("standalone stream status %d", stream.StatusCode)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SIGTERM with an open stream must exit 0, got %v; stderr: %s", err, errb.String())
		}
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatal("did not exit within 10s of SIGTERM")
	}
	if !strings.Contains(errb.String(), "drain deadline passed") {
		t.Fatalf("expected the drain-deadline warning; stderr: %s", errb.String())
	}
}
