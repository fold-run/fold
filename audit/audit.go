// Package audit emits one event per terminal gateway response — including
// 401s, 403s, and 429s — to configured sinks (stdout, webhook).
package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
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
	// OutcomeBudgetExhausted is distinct from OutcomeRateLimited because the
	// remedy differs: a rate limit clears in seconds, an exhausted budget not
	// until the period rolls over. Collapsing them would make "we are being
	// throttled" and "we have spent the month" the same line in a SIEM.
	OutcomeBudgetExhausted Outcome = "budget_exhausted"
	// OutcomeWarned is a finding rather than a request outcome: fold noticed
	// something an operator should see and did not change what it served.
	// Definition drift is the first of these — see
	// docs/design-definition-pinning.md. It is deliberately not an error: the
	// request succeeded, and an SLO that counted findings as failures would
	// page someone for a working federation.
	OutcomeWarned Outcome = "warned"
	// OutcomeHookDenied is the external decision hook refusing a call, kept
	// distinct from OutcomeDenied because an operator reading the trail needs
	// to know whether their policy or their inspector said no — the remedies
	// live in different systems, and often in different teams.
	OutcomeHookDenied Outcome = "hook_denied"
)

// Event is one audit record.
type Event struct {
	Time time.Time `json:"time"`
	// Instance names the gateway replica that emitted this record. A fleet
	// behind one Redis produces one trail from many processes, and without
	// this an operator reading it cannot tell which of them saw the request —
	// which is the first question asked of any record that looks wrong.
	// Resolved once at construction; see resolveInstance for where from.
	Instance  string `json:"instance,omitempty"`
	Principal string `json:"principal,omitempty"` // subject, or "" when auth is disabled
	Issuer    string `json:"issuer,omitempty"`
	Method    string `json:"method"`             // MCP method, e.g. "tools/call"
	Name      string `json:"name,omitempty"`     // namespaced tool/prompt name
	Upstream  string `json:"upstream,omitempty"` // routed upstream id
	// Direction is set only on the reverse path — "server_initiated", for the
	// sampling and elicitation requests an upstream makes of the caller's
	// client. Its absence means the ordinary client-to-upstream direction, so
	// every event fold emitted before this field existed reads unchanged.
	Direction string `json:"direction,omitempty"`
	// Tenant is the group the principal resolved to, when tenancy is
	// configured. Empty means no tenant matched — which is not an error, just
	// a caller governed by the gateway-wide rules.
	Tenant string `json:"tenant,omitempty"`
	// Decision is "allow" | "deny" for policy-gated invocations; for
	// oauth/token events it carries the exchange outcome verbatim
	// ("minted", "replayed", "invalid_grant", ...) so a detected replay is
	// alertable on a structured field.
	Decision string `json:"decision,omitempty"`
	RuleID   string `json:"ruleId,omitempty"`

	// MissingScopes names the scopes a denial said would have satisfied a
	// rule that otherwise granted the invocation. Present only on denials
	// where scopes were the sole obstacle, so a trail can distinguish "not
	// authorized" from "not authorized yet" without re-deriving it.
	MissingScopes []string `json:"missingScopes,omitempty"` // matching policy rule
	Outcome       Outcome  `json:"outcome"`
	Error         string   `json:"error,omitempty"`
	LatencyMs     int64    `json:"latencyMs"`

	// Consumption metering. fold records what it can observe; a billing
	// system consumes these. Nothing here is estimated — see
	// docs/design-consumption.md for why there is no token count.

	// UpstreamCalls is how many upstream invocations this one request cost.
	// It is not always 1: a federated list fans out to every upstream, so
	// this is where a cheap-looking client request shows its real price.
	UpstreamCalls int `json:"upstreamCalls,omitempty"`
	// ItemsServed is how many tools/prompts/resources a list returned after
	// per-principal policy filtering — the size of the surface this caller
	// was actually handed, which is what lands in a model's context.
	ItemsServed int `json:"itemsServed,omitempty"`

	// ItemsCapped counts list items a policy rule's maxItems dropped —
	// distinct from items policy made invisible, because the remedies differ:
	// one is a grant the caller does not have, the other a bound the operator
	// set. The result's _meta says the same thing to the client.
	ItemsCapped int `json:"itemsCapped,omitempty"`
	// HookOutcome is what the external decision hook said: "allow", "deny",
	// or "error". Present only when a hook inspected this request. The error
	// case is the one worth alerting on: with onError "allow" it means the
	// call proceeded uninspected, which a fail-open deployment would
	// otherwise have no record of.
	HookOutcome string `json:"hookOutcome,omitempty"`

	// Usage carries counters an upstream published in its result `_meta`,
	// verbatim. fold never synthesizes these; an absent field means the
	// upstream reported nothing, not that nothing was consumed.
	Usage map[string]any `json:"usage,omitempty"`
}

// Sink receives audit events.
type Sink interface {
	Emit(Event)
}

// Logger fans events out to sinks. A nil *Logger drops everything.
type Logger struct {
	sinks       []Sink
	closers     []io.Closer
	observer    Observer
	panicHook   PanicHook
	scrub       *config.AuditScrub
	startupErrs []error
	instance    string
	// requireDurable and durable are the two halves of the startup guard:
	// what the operator asked for, and whether a sink that delivers on it was
	// actually built. Both are needed because a declared durable sink can
	// still fail to construct — a file path that will not open — and that is
	// precisely the case the guard exists for.
	requireDurable bool
	durable        bool
}

// PanicHook is told about a panic recovered inside a delivery worker: the
// recovered value and the stack at recovery.
type PanicHook func(recovered any, stack []byte)

// Observer is told the fate of events, by sink type and outcome (delivered,
// retried, dead_lettered, dropped). The gateway turns this into metrics: audit
// is the single exit door, so an event that never arrives has to be countable
// somewhere, or a silent sink is indistinguishable from a silent system.
type Observer func(sinkType, outcome string, n int)

// Option configures a Logger at construction.
type Option func(*Logger)

// observer is stored on the Logger so sinks can report through it.
var noopObserver Observer = func(string, string, int) {}

// WithObserver reports delivery outcomes.
func WithObserver(o Observer) Option {
	return func(l *Logger) {
		if o != nil {
			l.observer = o
		}
	}
}

// WithPanicHook routes recovered delivery-worker panics into the caller's
// panic accounting, so this package's recoveries alert exactly like the
// gateway's own (fold_panics_total, the "panic recovered" log line). Absent,
// a recovered panic still writes stderr — never silent, merely unaggregated.
func WithPanicHook(h PanicHook) Option {
	return func(l *Logger) {
		if h != nil {
			l.panicHook = h
		}
	}
}

// New builds a logger from config. Absent config → no audit emission.
//
// A sink that cannot be constructed — a file path that will not open — is
// reported and skipped rather than failing the gateway: losing one destination
// should not take the endpoint down, and the error is visible at startup.
func New(cfg *config.Audit, opts ...Option) *Logger {
	if cfg == nil || len(cfg.Sinks) == 0 {
		return nil
	}
	l := &Logger{
		observer:       noopObserver,
		instance:       resolveInstance(),
		requireDurable: cfg.RequireDurable,
		scrub:          cfg.Scrub,
	}
	for _, opt := range opts {
		opt(l)
	}
	for _, s := range cfg.Sinks {
		report := func(outcome string, n int) { l.observer(s.Type, outcome, n) }
		built := len(l.sinks)
		switch s.Type {
		case "stdout":
			l.sinks = append(l.sinks, &stdoutSink{})
		case "webhook":
			headers, err := webhookHeaders(s)
			if err != nil {
				// Skipped rather than sent unauthenticated. A receiver that
				// requires a credential will refuse the batch, retry it, and
				// dead-letter it — an audit trail that looks delivered and is
				// not. Failing at startup says so once, loudly.
				l.startupErrs = append(l.startupErrs, err)
				continue
			}
			var dl *deadLetter
			if s.DeadLetterPath != "" {
				if dl, err = newDeadLetter(s.DeadLetterPath, report); err != nil {
					l.startupErrs = append(l.startupErrs, err)
					dl = nil
				}
			}
			w := newHTTPSink(s.URL, headers, jsonBatch, resolveRetry(s.Retry), dl, report)
			w.onPanic = l.panicHook
			l.sinks = append(l.sinks, w)
			l.closers = append(l.closers, w)
		case "otlp-logs":
			var dl *deadLetter
			if s.DeadLetterPath != "" {
				var err error
				if dl, err = newDeadLetter(s.DeadLetterPath, report); err != nil {
					l.startupErrs = append(l.startupErrs, err)
					dl = nil
				}
			}
			o, err := otlpLogsSink(s, l.instance, dl, report)
			if err != nil {
				l.startupErrs = append(l.startupErrs, err)
				_ = dl.Close()
				continue
			}
			l.sinks = append(l.sinks, o)
			l.closers = append(l.closers, o)
		case "file":
			rf, err := newRotatingFile(s.Path, s.MaxSizeMb, s.MaxFiles)
			if err != nil {
				l.startupErrs = append(l.startupErrs, err)
				continue
			}
			fs := &fileSink{rf: rf, report: report}
			l.sinks = append(l.sinks, fs)
			l.closers = append(l.closers, fs)
		}
		// Durability is credited to sinks that were actually constructed, not
		// to the ones the document declared.
		if len(l.sinks) > built && s.Durable() {
			l.durable = true
		}
	}
	return l
}

// resolveInstance names the process that emits the records: FOLD_INSTANCE_ID
// when set, otherwise the hostname — which Docker sets per container and
// Kubernetes per pod, so a fleet is attributable with no configuration at all.
//
// Deliberately not a config field. The value has to differ per replica, and
// the config document is the one thing every replica shares; a field there
// would either be identical across the fleet (useless) or force a
// per-replica document (worse).
func resolveInstance() string {
	if v := strings.TrimSpace(os.Getenv("FOLD_INSTANCE_ID")); v != "" {
		return v
	}
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return ""
}

// DurabilityError reports the one audit misconfiguration fold refuses to run
// with: `requireDurable` set, and not one constructed sink that keeps what it
// could not deliver. The caller makes it fatal — a gateway that starts here
// serves traffic while producing a trail the operator has already said is not
// good enough, which is worse than not starting.
//
// config.Validate rejects the same shape earlier, so reaching this means a
// declared durable sink failed to construct.
func (l *Logger) DurabilityError() error {
	if l == nil || !l.requireDurable || l.durable {
		return nil
	}
	// The startup errors are folded in rather than left to the log: a refused
	// startup may be the only output an operator sees, and "no durable sink"
	// without the path that would not open is a message that sends them
	// looking in the wrong place.
	return errors.Join(append([]error{
		errors.New("audit.requireDurable is set but no durable sink was started"),
	}, l.startupErrs...)...)
}

// StartupErrors reports sinks that could not be constructed, so the caller can
// log them. Empty when every configured sink was built.
func (l *Logger) StartupErrors() []error {
	if l == nil {
		return nil
	}
	return l.startupErrs
}

// Close releases sinks that hold resources (open files, delivery workers).
// Buffered events are flushed on a bounded best-effort basis; a shutdown that
// waits indefinitely on an unreachable webhook is worse than a lost tail.
func (l *Logger) Close() {
	if l == nil {
		return
	}
	for _, c := range l.closers {
		_ = c.Close()
	}
}

// Emit delivers an event to every sink.
func (l *Logger) Emit(e Event) {
	if l == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	if e.Instance == "" {
		e.Instance = l.instance
	}
	l.scrubEvent(&e)
	for _, s := range l.sinks {
		s.Emit(e)
	}
}

// scrubEvent applies the configured audit scrub before delivery. It is a
// value edit, not a copy, because the caller has already handed the event
// to audit and no longer depends on its original contents.
func (l *Logger) scrubEvent(e *Event) {
	if l.scrub == nil {
		return
	}
	if len(l.scrub.RedactUsageKeys) > 0 && len(e.Usage) > 0 {
		for _, k := range l.scrub.RedactUsageKeys {
			delete(e.Usage, k)
		}
	}
	if l.scrub.MaxErrorLength > 0 && len(e.Error) > l.scrub.MaxErrorLength {
		e.Error = e.Error[:l.scrub.MaxErrorLength]
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

// encoder turns a batch of events into a request body. The two HTTP sinks
// differ only in this and in their URL, which is why they share everything
// else: two copies of retry and dead-letter logic would drift into two
// different delivery guarantees, and nobody would notice until the day the
// records mattered.
type encoder func([]Event) ([]byte, error)

// httpSink POSTs batches of events, delivered asynchronously so audit cannot
// add latency to the request path.
type httpSink struct {
	url     string
	headers map[string]string
	encode  encoder
	client  *http.Client
	ch      chan Event
	retry   retryPolicy
	dead    *deadLetter
	report  func(outcome string, n int)
	onPanic PanicHook // nil → stderr fallback in deliverSafe
	done    chan struct{}
}

func newHTTPSink(url string, headers map[string]string, enc encoder, retry retryPolicy, dead *deadLetter, report func(string, int)) *httpSink {
	s := &httpSink{
		url:     url,
		headers: headers,
		encode:  enc,
		retry:   retry,
		dead:    dead,
		report:  report,
		done:    make(chan struct{}),
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

func (s *httpSink) Emit(e Event) {
	select {
	case s.ch <- e:
	default:
		// Full buffer: drop rather than block the request path. Audit must
		// never become the reason a request is slow — but a drop is counted,
		// because the alternative is losing events invisibly.
		s.report(OutcomeDropped, 1)
	}
}

// Close stops the delivery worker and releases the dead-letter file. In-flight
// retries are abandoned: shutdown does not wait out a backoff against a
// receiver that is already known to be failing.
func (s *httpSink) Close() error {
	close(s.done)
	return s.dead.Close()
}

func (s *httpSink) run() {
	for {
		var e Event
		select {
		case e = <-s.ch:
		case <-s.done:
			return
		}
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
		s.deliverSafe(batch)
	}
}

// deliverSafe keeps the delivery worker alive across a panic. The worker is
// the one goroutine draining this sink's buffer: unrecovered, a panic ends
// the process; merely swallowed at the loop, it would end the worker and
// silently turn every later event into a drop. The batch it was carrying is
// counted as dropped — the observer contract that a lost event is always
// countable holds even here — and the panic itself goes to the hook, so it
// alerts like every other recovered panic rather than masquerading as a
// sink failure.
func (s *httpSink) deliverSafe(batch []Event) {
	defer func() {
		if r := recover(); r != nil {
			s.report(OutcomeDropped, len(batch))
			if s.onPanic != nil {
				s.onPanic(r, debug.Stack())
			} else {
				fmt.Fprintf(os.Stderr, "fold: audit: panic recovered in delivery worker: %v\n%s", r, debug.Stack())
			}
		}
	}()
	s.deliver(batch)
}

// deliver posts a batch, retrying transient failures with backoff and
// dead-lettering what it finally cannot deliver.
//
// The retries happen on this worker, not per event: it is the one goroutine
// draining the buffer, so a receiver that is down applies backpressure into
// the buffer and then into counted drops, rather than into an unbounded pile
// of goroutines each holding a batch.
func (s *httpSink) deliver(batch []Event) {
	data, err := s.encode(batch)
	if err != nil {
		s.report(OutcomeDropped, len(batch))
		return
	}
	for attempt := 1; attempt <= s.retry.maxAttempts; attempt++ {
		err := s.post(data)
		if err == nil {
			s.report(OutcomeDelivered, len(batch))
			return
		}
		if !err.retryable || attempt == s.retry.maxAttempts {
			break
		}
		s.report(OutcomeRetried, len(batch))
		select {
		case <-time.After(s.retry.backoff(attempt)):
		case <-s.done:
			return
		}
	}
	if s.dead != nil {
		s.dead.write(batch)
		return
	}
	s.report(OutcomeDropped, len(batch))
}

// jsonBatch is the webhook body: the events as a JSON array, which is what
// every receiver built against fold's documented audit shape expects.
func jsonBatch(batch []Event) ([]byte, error) { return json.Marshal(batch) }

// postError distinguishes "try again" from "this will never work". A 400 from
// a receiver that dislikes the payload will be disliked identically four
// times; retrying it only delays the dead letter.
type postError struct {
	err       error
	retryable bool
}

func (s *httpSink) post(data []byte) *postError {
	req, err := http.NewRequest(http.MethodPost, s.url, bytes.NewReader(data))
	if err != nil {
		return &postError{err: err, retryable: false}
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range s.headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		// Connection refused, DNS failure, timeout: the receiver is down or
		// unreachable, which is exactly what retry is for.
		return &postError{err: err, retryable: true}
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return &postError{err: fmt.Errorf("audit webhook: status %d", resp.StatusCode), retryable: true}
	default:
		return &postError{err: fmt.Errorf("audit webhook: status %d", resp.StatusCode), retryable: false}
	}
}

// webhookHeaders resolves what a webhook sink sends.
//
// The configured headers, plus the bearer token named by bearerSecretRef —
// which exists so the credential is not in the config document, the same way
// discovery and the decision hook take theirs. An empty variable is an error
// rather than an omission: a sink configured to authenticate and silently not
// doing so delivers nothing and says nothing.
func webhookHeaders(s config.AuditSink) (map[string]string, error) {
	if s.BearerSecretRef == "" {
		return s.Headers, nil
	}
	token := os.Getenv(s.BearerSecretRef)
	if token == "" {
		return nil, fmt.Errorf(
			"audit webhook %s: bearerSecretRef %s: environment variable is empty",
			s.URL, s.BearerSecretRef)
	}

	// Copied rather than written into: the config belongs to the caller, and
	// a sink that mutates it leaves a credential in whatever the caller does
	// with that map next.
	headers := make(map[string]string, len(s.Headers)+1)
	for k, v := range s.Headers {
		headers[k] = v
	}
	headers["Authorization"] = "Bearer " + token
	return headers, nil
}
