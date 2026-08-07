// Package audit emits one event per terminal gateway response — including
// 401s, 403s, and 429s — to configured sinks (stdout, webhook).
package audit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/fold-run/fold/config"
)

// Outcome classifies how a request terminated.
type Outcome string

// Outcomes mirror the terminal responses the gateway can produce; every
// audit event carries exactly one.
const (
	OutcomeOK              Outcome = "ok"
	OutcomeError           Outcome = "error"
	OutcomeDenied          Outcome = "denied"
	OutcomeRateLimited     Outcome = "rate_limited"
	OutcomeUnauthenticated Outcome = "unauthenticated"
	OutcomeUpstreamDown    Outcome = "upstream_down"
	OutcomeForbidden       Outcome = "forbidden" // host/origin rejected (DNS rebinding)
)

// Event is one audit record.
type Event struct {
	Time      time.Time `json:"time"`
	Principal string    `json:"principal,omitempty"` // subject, or "" when auth is disabled
	Issuer    string    `json:"issuer,omitempty"`
	Method    string    `json:"method"`             // MCP method, e.g. "tools/call"
	Name      string    `json:"name,omitempty"`     // namespaced tool/prompt name
	Upstream  string    `json:"upstream,omitempty"` // routed upstream id
	Decision  string    `json:"decision,omitempty"` // "allow" | "deny"
	RuleID    string    `json:"ruleId,omitempty"`   // matching policy rule
	Outcome   Outcome   `json:"outcome"`
	Error     string    `json:"error,omitempty"`
	LatencyMs int64     `json:"latencyMs"`
}

// Sink receives audit events.
type Sink interface {
	Emit(Event)
}

// Logger fans events out to sinks. A nil *Logger drops everything.
type Logger struct {
	sinks []Sink
}

// New builds a logger from config. Absent config → no audit emission.
func New(cfg *config.Audit) *Logger {
	if cfg == nil || len(cfg.Sinks) == 0 {
		return nil
	}
	l := &Logger{}
	for _, s := range cfg.Sinks {
		switch s.Type {
		case "stdout":
			l.sinks = append(l.sinks, &stdoutSink{})
		case "webhook":
			l.sinks = append(l.sinks, newWebhookSink(s.URL, s.Headers))
		}
	}
	return l
}

// Emit delivers an event to every sink.
func (l *Logger) Emit(e Event) {
	if l == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	for _, s := range l.sinks {
		s.Emit(e)
	}
}

type stdoutSink struct{ mu sync.Mutex }

func (s *stdoutSink) Emit(e Event) {
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = fmt.Fprintln(os.Stdout, string(data))
}

// webhookSink POSTs batches of events, delivered asynchronously so audit
// cannot add latency to the request path.
type webhookSink struct {
	url     string
	headers map[string]string
	client  *http.Client
	ch      chan Event
}

func newWebhookSink(url string, headers map[string]string) *webhookSink {
	s := &webhookSink{
		url:     url,
		headers: headers,
		client: &http.Client{
			Timeout: 10 * time.Second,
			// A POST carrying the sink's configured headers (commonly an API
			// token) and a batch of audit records naming principals and
			// tools. Go replays the body verbatim on 307/308 and keeps the
			// headers on any same-domain redirect, so a redirecting sink
			// would hand both to whatever host it names. A redirect is never
			// a legitimate step in delivering a webhook.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return fmt.Errorf("audit webhook: refusing redirect from %q to %q",
					via[0].URL.Redacted(), req.URL.Redacted())
			},
		},
		ch: make(chan Event, 1024),
	}
	go s.run()
	return s
}

func (s *webhookSink) Emit(e Event) {
	select {
	case s.ch <- e:
	default: // full buffer: drop rather than block the request path
	}
}

func (s *webhookSink) run() {
	for e := range s.ch {
		batch := []Event{e}
	drain:
		for len(batch) < 100 {
			select {
			case next := <-s.ch:
				batch = append(batch, next)
			default:
				break drain
			}
		}
		s.post(batch)
	}
}

func (s *webhookSink) post(batch []Event) {
	data, err := json.Marshal(batch)
	if err != nil {
		return
	}
	req, err := http.NewRequest(http.MethodPost, s.url, bytes.NewReader(data))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range s.headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}
