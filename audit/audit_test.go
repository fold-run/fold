package audit

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
