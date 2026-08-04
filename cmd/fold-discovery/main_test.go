package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestMain re-execs the test binary as the fold-discovery CLI when
// FOLD_DISCOVERY_TEST_MAIN is set, mirroring cmd/fold's pattern.
func TestMain(m *testing.M) {
	if os.Getenv("FOLD_DISCOVERY_TEST_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

func TestVersionFlag(t *testing.T) {
	cmd := exec.Command(os.Args[0], "--version")
	cmd.Env = append(os.Environ(), "FOLD_DISCOVERY_TEST_MAIN=1")
	out, err := cmd.Output()
	if err != nil || !strings.HasPrefix(string(out), "fold-discovery ") {
		t.Errorf("--version: err=%v out=%q", err, out)
	}
}

func TestOutsideClusterWithoutAPIFails(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "FOLD_DISCOVERY_TEST_MAIN=1",
		"KUBERNETES_SERVICE_HOST=", "KUBERNETES_SERVICE_PORT=")
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err == nil {
		t.Fatal("expected failure outside a cluster without --kube-api")
	}
	if !strings.Contains(errb.String(), "--kube-api") {
		t.Errorf("stderr should point at --kube-api: %q", errb.String())
	}
}

// TestServeAgainstFakeAPI boots the real CLI against a fake Kubernetes API
// and reads the document it serves.
func TestServeAgainstFakeAPI(t *testing.T) {
	api := httptest(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	cmd := exec.Command(os.Args[0],
		"--kube-api", api,
		"--host", "127.0.0.1",
		"--port", fmt.Sprint(port),
		"--interval", "100ms",
		"--log-format", "json")
	cmd.Env = append(os.Environ(), "FOLD_DISCOVERY_TEST_MAIN=1")
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	url := fmt.Sprintf("http://127.0.0.1:%d/upstreams.json", port)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil && resp.StatusCode == http.StatusOK {
			var doc struct {
				Upstreams []map[string]any `json:"upstreams"`
			}
			err := json.NewDecoder(resp.Body).Decode(&doc)
			resp.Body.Close()
			if err != nil {
				t.Fatalf("document not JSON: %v", err)
			}
			if len(doc.Upstreams) != 1 || doc.Upstreams[0]["id"] != "prod-search" {
				t.Fatalf("document = %+v", doc)
			}
			return
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("producer never served the document; stderr: %s", errb.String())
}

// httptest starts a minimal fake Kubernetes API returning one labeled
// Service, and returns its URL. (Named to avoid importing net/http/httptest
// alongside the exec-based flow above.)
func httptest(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/services", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"items":[{"metadata":{"name":"search","namespace":"prod"},"spec":{"ports":[{"name":"mcp","port":8080}]}}]}`)
	})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + l.Addr().String()
}
