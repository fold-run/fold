package audit

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
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
