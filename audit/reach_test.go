package audit

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fold-run/fold/config"
)

// Audit is fold's single exit door: every terminal response produces exactly
// one event, including the denials. A sink that fires once and gives up turns
// a receiver's thirty-second restart into a permanent hole in the record, and
// — worse — a silent one. These cover the delivery guarantees that close that,
// and the counting that makes a remaining gap visible.

// counted collects delivery outcomes the way the gateway's metrics do.
type counted struct {
	mu sync.Mutex
	n  map[string]int
}

func newCounted() *counted { return &counted{n: map[string]int{}} }

func (c *counted) observe(sink, outcome string, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n[outcome] += n
}

func (c *counted) get(outcome string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[outcome]
}

// waitFor polls until cond holds, so tests do not race the delivery worker.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A receiver that fails and recovers must not cost events — this is the whole
// point of the feature.
func TestWebhookRetriesUntilTheReceiverRecovers(t *testing.T) {
	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable) // down, then back
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newCounted()
	l := New(&config.Audit{Sinks: []config.AuditSink{{
		Type: "webhook", URL: srv.URL,
		Retry: &config.AuditRetry{MaxAttempts: 5, InitialBackoffMs: 1, MaxBackoffMs: 5},
	}}}, WithObserver(c.observe))
	defer l.Close()

	l.Emit(Event{Method: "tools/call", Name: "a__b"})

	waitFor(t, "delivery after retries", func() bool { return c.get(OutcomeDelivered) == 1 })
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3 (two failures then success)", got)
	}
	if c.get(OutcomeRetried) < 2 {
		t.Fatalf("retried = %d, want at least 2 — retries must be visible, not just effective", c.get(OutcomeRetried))
	}
	if c.get(OutcomeDropped) != 0 {
		t.Fatalf("dropped = %d, want 0", c.get(OutcomeDropped))
	}
}

// A receiver that rejects the payload will reject it identically every time;
// retrying only delays the dead letter.
func TestWebhookDoesNotRetryAPermanentRejection(t *testing.T) {
	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	dir := t.TempDir()
	dlPath := filepath.Join(dir, "dead.jsonl")
	c := newCounted()
	l := New(&config.Audit{Sinks: []config.AuditSink{{
		Type: "webhook", URL: srv.URL, DeadLetterPath: dlPath,
		Retry: &config.AuditRetry{MaxAttempts: 5, InitialBackoffMs: 1, MaxBackoffMs: 5},
	}}}, WithObserver(c.observe))
	defer l.Close()

	l.Emit(Event{Method: "tools/call", Name: "a__b"})

	waitFor(t, "dead letter", func() bool { return c.get(OutcomeDeadLettered) == 1 })
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1 — a 400 is not a transient failure", got)
	}
}

// Giving up has to leave something an operator can replay.
func TestExhaustedDeliveryIsDeadLettered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	dlPath := filepath.Join(t.TempDir(), "nested", "dead.jsonl") // directory created on demand
	c := newCounted()
	l := New(&config.Audit{Sinks: []config.AuditSink{{
		Type: "webhook", URL: srv.URL, DeadLetterPath: dlPath,
		Retry: &config.AuditRetry{MaxAttempts: 2, InitialBackoffMs: 1, MaxBackoffMs: 2},
	}}}, WithObserver(c.observe))
	defer l.Close()

	l.Emit(Event{Method: "tools/call", Name: "a__b", Outcome: OutcomeDenied})

	waitFor(t, "dead letter file", func() bool { return c.get(OutcomeDeadLettered) == 1 })

	data, err := os.ReadFile(dlPath)
	if err != nil {
		t.Fatalf("dead-letter file: %v", err)
	}
	var got Event
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &got); err != nil {
		t.Fatalf("dead-letter line is not a JSON event: %v", err)
	}
	if got.Name != "a__b" || got.Outcome != OutcomeDenied {
		t.Fatalf("dead-lettered event = %+v, want the original preserved", got)
	}
	if c.get(OutcomeDropped) != 0 {
		t.Fatal("an event was counted as dropped despite being dead-lettered")
	}
}

// Without a dead-letter path there is nowhere to put an abandoned event, and
// the only thing worse than losing it is losing it quietly.
func TestExhaustedDeliveryWithoutDeadLetterIsCounted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newCounted()
	l := New(&config.Audit{Sinks: []config.AuditSink{{
		Type: "webhook", URL: srv.URL,
		Retry: &config.AuditRetry{MaxAttempts: 2, InitialBackoffMs: 1, MaxBackoffMs: 2},
	}}}, WithObserver(c.observe))
	defer l.Close()

	l.Emit(Event{Method: "ping"})
	waitFor(t, "counted drop", func() bool { return c.get(OutcomeDropped) == 1 })
}

// The file sink is a destination in its own right, and the dead letter's
// machinery — so its rotation is what keeps either from filling a disk.
func TestFileSinkWritesAndRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	c := newCounted()
	l := New(&config.Audit{Sinks: []config.AuditSink{{
		// 1 MiB is the floor the config allows; the rotation itself is
		// exercised directly below, where the size can be made small.
		Type: "file", Path: path, MaxSizeMb: 1, MaxFiles: 2,
	}}}, WithObserver(c.observe))
	defer l.Close()

	for i := range 3 {
		l.Emit(Event{Method: "tools/call", Name: fmt.Sprintf("tool-%d", i)})
	}
	if c.get(OutcomeDelivered) != 3 {
		t.Fatalf("delivered = %d, want 3", c.get(OutcomeDelivered))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("file has %d lines, want 3 (one JSON event per line)", len(lines))
	}
	var e Event
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
		t.Fatalf("line is not a JSON event: %v", err)
	}
	if e.Time.IsZero() {
		t.Error("event written without a timestamp")
	}
}

// Rotation renames in place so the live name never changes — a tail or a log
// shipper watching the path keeps working across one.
func TestRotationKeepsTheLiveNameAndBoundsFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	rf, err := newRotatingFile(path, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	rf.maxBytes = 200 // small enough to rotate within the test

	for i := range 60 {
		if err := rf.writeLine([]byte(fmt.Sprintf(`{"n":%d,"pad":"aaaaaaaaaaaaaaaaaaaa"}`, i))); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the live file must keep its name across rotations: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected a rotated file at .1: %v", err)
	}
	// maxFiles=2 keeps .1 and .2 and no more.
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Fatal("rotation kept more files than maxFiles allows")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) > 3 {
		t.Fatalf("directory holds %d files, want at most the live file plus 2 rotations", len(entries))
	}
}

// A path that cannot be opened must not take the gateway down with it, but it
// must not be silent either.
func TestUnopenableFileSinkIsReportedAndSkipped(t *testing.T) {
	// A path under a file (not a directory) cannot be created.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "notadir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := New(&config.Audit{Sinks: []config.AuditSink{
		{Type: "file", Path: filepath.Join(blocker, "audit.jsonl")},
		{Type: "stdout"},
	}})
	defer l.Close()

	if len(l.StartupErrors()) != 1 {
		t.Fatalf("startup errors = %v, want exactly one for the unopenable file", l.StartupErrors())
	}
	// The healthy sink is still wired.
	if len(l.sinks) != 1 {
		t.Fatalf("sinks = %d, want the stdout sink to survive its neighbour", len(l.sinks))
	}
}

// Backoff has to grow and stay bounded, and it has to be jittered: every
// instance in a fleet watches the same receiver and fails at the same instant.
func TestBackoffGrowsJittersAndIsBounded(t *testing.T) {
	p := resolveRetry(&config.AuditRetry{MaxAttempts: 6, InitialBackoffMs: 100, MaxBackoffMs: 800})
	seen := map[time.Duration]bool{}
	for attempt := 1; attempt <= 6; attempt++ {
		d := p.backoff(attempt)
		if d > p.max {
			t.Fatalf("backoff(%d) = %v, over the cap %v", attempt, d, p.max)
		}
		seen[d] = true
	}
	if len(seen) < 3 {
		t.Fatalf("backoff produced %d distinct delays across 6 attempts; jitter is not working", len(seen))
	}
	// Later attempts must be able to exceed the first delay's ceiling.
	var maxLate time.Duration
	for range 20 {
		if d := p.backoff(5); d > maxLate {
			maxLate = d
		}
	}
	if maxLate <= p.initial {
		t.Fatalf("attempt 5 never exceeded the initial delay (%v); backoff is not growing", p.initial)
	}
}

// Defaults matter here: retry is on without configuration, because the old
// behaviour — one attempt, then silence — is the bug being fixed.
func TestRetryIsOnByDefault(t *testing.T) {
	p := resolveRetry(nil)
	if p.maxAttempts < 2 {
		t.Fatalf("default maxAttempts = %d, want retry enabled without configuration", p.maxAttempts)
	}
	if p.initial <= 0 || p.max < p.initial {
		t.Fatalf("default backoff is incoherent: initial=%v max=%v", p.initial, p.max)
	}
}
