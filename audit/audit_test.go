package audit

import (
	"encoding/json"
	"io"
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

func TestNilAndEmptyConfigDisableAudit(t *testing.T) {
	for name, cfg := range map[string]*config.Audit{
		"nil":      nil,
		"no sinks": {},
	} {
		if l := New(cfg); l != nil {
			t.Errorf("%s config: expected nil logger, got %v", name, l)
		}
	}
	// A nil logger must swallow events without panicking — the gateway
	// calls Emit unconditionally.
	var l *Logger
	l.Emit(Event{Method: "tools/call"})
}

func TestWebhookSinkDeliversBatchesWithHeaders(t *testing.T) {
	var mu sync.Mutex
	var got []Event
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var batch []Event
		if err := json.Unmarshal(body, &batch); err != nil {
			t.Errorf("webhook body is not an event batch: %v", err)
		}
		mu.Lock()
		got = append(got, batch...)
		auth = r.Header.Get("Authorization")
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)

	l := New(&config.Audit{Sinks: []config.AuditSink{{
		Type:    "webhook",
		URL:     srv.URL,
		Headers: map[string]string{"Authorization": "Bearer sink-token"},
	}}})
	if l == nil {
		t.Fatal("expected a logger")
	}

	for range 5 {
		l.Emit(Event{Method: "tools/call", Outcome: OutcomeOK})
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 5 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 5 {
		t.Fatalf("webhook received %d events, want 5", len(got))
	}
	if auth != "Bearer sink-token" {
		t.Errorf("configured header not sent: Authorization = %q", auth)
	}
	for _, e := range got {
		if e.Time.IsZero() {
			t.Error("event delivered without a stamped timestamp")
		}
		if e.Method != "tools/call" || e.Outcome != OutcomeOK {
			t.Errorf("event mangled in transit: %+v", e)
		}
	}
}

// TestWebhookSinkNeverBlocksRequestPath: with the delivery worker wedged on
// a stalled endpoint and the buffer overfull, Emit must drop rather than
// block — audit cannot add latency to the proxy path.
func TestWebhookSinkNeverBlocksRequestPath(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	l := New(&config.Audit{Sinks: []config.AuditSink{{Type: "webhook", URL: srv.URL}}})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 3000 { // well past the 1024 buffer
			l.Emit(Event{Method: "tools/call"})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Emit blocked on a full webhook buffer")
	}
}

func TestUnknownSinkTypeIgnored(t *testing.T) {
	l := New(&config.Audit{Sinks: []config.AuditSink{{Type: "carrier-pigeon"}}})
	if l == nil {
		t.Fatal("logger should exist even if no sink matched") // matches New's contract
	}
	l.Emit(Event{Method: "tools/call"}) // must not panic with zero sinks
}

// TestWebhookSinkRefusesRedirect proves a redirecting audit sink cannot pull
// the configured headers — commonly a delivery token — and the event batch
// itself onto a host of its choosing.
func TestWebhookSinkRefusesRedirect(t *testing.T) {
	var attackerHits atomic.Int64
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer attacker.Close()

	delivered := make(chan struct{}, 1)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case delivered <- struct{}{}:
		default:
		}
		http.Redirect(w, r, attacker.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer sink.Close()

	l := New(&config.Audit{Sinks: []config.AuditSink{{
		Type: "webhook", URL: sink.URL,
		Headers: map[string]string{"X-Api-Key": "SUPER-SECRET"},
	}}})
	l.Emit(Event{Method: "tools/call", Principal: "alice"})

	select {
	case <-delivered:
	case <-time.After(5 * time.Second):
		t.Fatal("the webhook sink never delivered")
	}
	// Give a followed redirect every chance to land before asserting.
	time.Sleep(200 * time.Millisecond)
	if n := attackerHits.Load(); n != 0 {
		t.Fatalf("redirect target received %d deliveries; redirects must be refused", n)
	}
}

// TestWebhookBearerSecretRef covers the reason this exists: `headers` takes
// static values, so a receiver that authenticates leaves the token written
// into the config document — the one part of a fold config that then cannot
// be checked in, logged, or handed to somebody debugging a federation. The
// audit trail is the sink most likely to need a credential.
func TestWebhookBearerSecretRef(t *testing.T) {
	var got string
	received := make(chan struct{}, 1)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		select {
		case received <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	t.Setenv("AUDIT_TOKEN", "s3cret")
	logger := New(&config.Audit{Sinks: []config.AuditSink{{
		Type: "webhook", URL: sink.URL, BearerSecretRef: "AUDIT_TOKEN",
	}}})
	if errs := logger.StartupErrors(); len(errs) != 0 {
		t.Fatalf("startup errors: %v", errs)
	}
	logger.Emit(Event{Method: "tools/call"})

	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery")
	}
	logger.Close()

	if got != "Bearer s3cret" {
		t.Fatalf("Authorization = %q", got)
	}
}

// A sink told to authenticate and unable to must not deliver unauthenticated:
// the receiver refuses the batch, it retries, it dead-letters, and the trail
// looks delivered while it is not. Saying so once at startup is the honest
// failure.
func TestWebhookBearerSecretRefEmptyIsAStartupError(t *testing.T) {
	t.Setenv("AUDIT_TOKEN", "")
	logger := New(&config.Audit{Sinks: []config.AuditSink{{
		Type: "webhook", URL: "https://sink.test/audit", BearerSecretRef: "AUDIT_TOKEN",
	}}})

	errs := logger.StartupErrors()
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "AUDIT_TOKEN") {
		t.Fatalf("startup errors = %v, want one naming the variable", errs)
	}
}

// The caller's config is not written into. A sink that mutates the map it was
// handed leaves a credential in whatever that caller does with it next.
func TestWebhookBearerSecretRefDoesNotMutateTheConfig(t *testing.T) {
	t.Setenv("AUDIT_TOKEN", "s3cret")
	headers := map[string]string{"X-Fold": "yes"}
	cfg := &config.Audit{Sinks: []config.AuditSink{{
		Type: "webhook", URL: "https://sink.test/audit",
		Headers: headers, BearerSecretRef: "AUDIT_TOKEN",
	}}}

	logger := New(cfg)
	logger.Close()

	if _, leaked := headers["Authorization"]; leaked {
		t.Fatalf("the credential was written into the caller's headers: %v", headers)
	}
}

// Every record says which replica produced it. A fleet behind one Redis emits
// one trail from many processes, and "which of them saw this" is the first
// question asked of a record that looks wrong.
func TestEventsCarryTheEmittingInstance(t *testing.T) {
	t.Setenv("FOLD_INSTANCE_ID", "fold-7c9f")

	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l := New(&config.Audit{Sinks: []config.AuditSink{{Type: "file", Path: path}}})
	if l == nil {
		t.Fatal("expected a logger")
	}
	l.Emit(Event{Method: "tools/call", Outcome: OutcomeOK})
	// An event that names its own instance keeps it: the gateway never sets
	// this, but an embedder relaying records from elsewhere may.
	l.Emit(Event{Method: "tools/call", Outcome: OutcomeOK, Instance: "relayed-from"})
	l.Close()

	var got []Event
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the audit file: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("audit line is not an event: %v", err)
		}
		got = append(got, e)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 events, got %d", len(got))
	}
	if got[0].Instance != "fold-7c9f" {
		t.Errorf("stamped instance = %q, want fold-7c9f", got[0].Instance)
	}
	if got[1].Instance != "relayed-from" {
		t.Errorf("preset instance = %q, want relayed-from", got[1].Instance)
	}
}

// With no FOLD_INSTANCE_ID the hostname stands in, which is what Docker and
// Kubernetes set per container and per pod — so the field is populated in the
// deployments that have a fleet without anyone configuring it.
func TestInstanceFallsBackToHostname(t *testing.T) {
	t.Setenv("FOLD_INSTANCE_ID", "")
	host, err := os.Hostname()
	if err != nil {
		t.Skip("no hostname available")
	}
	if got := resolveInstance(); got != host {
		t.Errorf("resolveInstance() = %q, want hostname %q", got, host)
	}
}

// requireDurable is an assertion about the deployment, and a declared durable
// sink that will not open is exactly the case config validation cannot catch.
func TestRequireDurableFailsWhenTheDurableSinkDoesNotOpen(t *testing.T) {
	// A regular file standing where the sink expects a directory: the sink
	// creates missing directories, so this is what an unopenable path looks
	// like.
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Audit{
		RequireDurable: true,
		Sinks: []config.AuditSink{
			{Type: "stdout"},
			{Type: "file", Path: filepath.Join(blocked, "audit.jsonl")},
		},
	}
	l := New(cfg)
	if l == nil {
		t.Fatal("expected a logger")
	}
	t.Cleanup(l.Close)
	if len(l.StartupErrors()) == 0 {
		t.Fatal("expected the file sink to fail construction")
	}
	if err := l.DurabilityError(); err == nil {
		t.Fatal("expected a durability error when the only durable sink did not start")
	}
	// stdout is still serving events; the guard is about what survives, not
	// about whether anything is emitting.
	if len(l.sinks) != 1 {
		t.Errorf("expected stdout to remain, got %d sinks", len(l.sinks))
	}
}

func TestRequireDurableSatisfiedByAConstructedFileSink(t *testing.T) {
	cfg := &config.Audit{
		RequireDurable: true,
		Sinks:          []config.AuditSink{{Type: "file", Path: filepath.Join(t.TempDir(), "audit.jsonl")}},
	}
	l := New(cfg)
	if l == nil {
		t.Fatal("expected a logger")
	}
	t.Cleanup(l.Close)
	if err := l.DurabilityError(); err != nil {
		t.Fatalf("unexpected durability error: %v", err)
	}
}

// Off by default: a document that says nothing about durability is not
// second-guessed, however non-durable its sinks are.
func TestRequireDurableIsOptIn(t *testing.T) {
	l := New(&config.Audit{Sinks: []config.AuditSink{{Type: "stdout"}}})
	if l == nil {
		t.Fatal("expected a logger")
	}
	t.Cleanup(l.Close)
	if err := l.DurabilityError(); err != nil {
		t.Fatalf("unexpected durability error with requireDurable unset: %v", err)
	}
}
