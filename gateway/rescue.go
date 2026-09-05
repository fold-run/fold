package gateway

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/fold-run/fold/audit"
)

// Panic containment. The SDK dispatches each JSON-RPC request on its own
// goroutine, and fold launches more for fan-out, sweeps, probes, and
// discovery — none of which net/http's handler recovery sits above. Without
// these, one panic anywhere on those goroutines ends the process for every
// tenant and every upstream; if the trigger is in an upstream's steady-state
// output, the gateway crash-loops. A recovered panic is never silent: it is
// counted (fold_panics_total) and logged with its stack.
//
// recover() only works when called directly by a deferred function, so each
// helper here is the deferred function itself — never wrapped once more.
//
// On the request path the containment boundary is deliberately route
// (routeSafe in router.go), not federationMiddleware: recovering outside the
// middleware would skip the audit emit and answer the client without a
// record, breaking "audit is the single exit door". The pre/post-route code
// the middleware itself runs (tracer start, meter, the emit) stays
// uncovered on purpose — it is fold-owned, input-independent plumbing, and
// an outer recover could not audit its own failure reliably. Do not "fix"
// this by moving the recover outward.
//
// recoverHTTP, below, is not that outer recover. It wraps the HTTP mux, so
// it sees only panics that never entered the JSON-RPC dispatch: /health,
// /icons, the well-known documents, the token endpoint. net/http already
// recovers those per connection — the process survives — but it recovers
// them into its own logger, uncounted and unaudited, so an operator paging
// on fold_panics_total would see nothing while an unauthenticated endpoint
// panicked on every request. This counts and records them under site
// "http", answering 500 when nothing has been written yet.

// notePanic records one recovered panic. metrics may be nil (bare test
// fixtures build upstreams without a gateway); the logger never is.
func notePanic(log *slog.Logger, m *metricsSet, site string, r any) {
	if m != nil {
		m.panicked(site)
	}
	log.Error("panic recovered", "site", site, "panic", r, "stack", string(debug.Stack()))
}

// rescue is deferred at the top of gateway goroutines whose death would
// otherwise be the process's: it swallows the panic after recording it.
func (g *Gateway) rescue(site string) {
	if r := recover(); r != nil {
		notePanic(g.log, g.metrics, site, r)
	}
}

// recoverHTTP is the HTTP-level containment described above.
func (g *Gateway) recoverHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tw := &headerTracker{ResponseWriter: w}
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler {
				// net/http's own "close the connection quietly" signal;
				// not a fold panic, and swallowing it would change what
				// the handler asked for.
				panic(rec)
			}
			notePanic(g.log, g.metrics, "http", rec)
			g.audit.Emit(audit.Event{Method: "http", Outcome: audit.OutcomeError,
				Error: fmt.Sprintf("panic serving %s %s (recovered)", r.Method, r.URL.Path)})
			if !tw.wrote {
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(tw, r)
	})
}

// headerTracker remembers whether a response has started, so recoverHTTP
// knows whether a 500 can still be sent. Flush and Unwrap keep SSE and
// http.ResponseController working through it.
type headerTracker struct {
	http.ResponseWriter
	wrote bool
}

func (t *headerTracker) WriteHeader(code int) {
	t.wrote = true
	t.ResponseWriter.WriteHeader(code)
}

func (t *headerTracker) Write(p []byte) (int, error) {
	t.wrote = true
	return t.ResponseWriter.Write(p)
}

func (t *headerTracker) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (t *headerTracker) Unwrap() http.ResponseWriter { return t.ResponseWriter }

// safely runs one iteration of a background loop under rescue, so a
// poisoned tick is dropped instead of ending the loop for the life of the
// process.
func (g *Gateway) safely(site string, fn func()) {
	defer g.rescue(site)
	fn()
}

// rescue is Gateway.rescue for goroutines an upstream launches itself
// (probes, SDK-invoked notification handlers).
func (u *upstream) rescue(site string) {
	if r := recover(); r != nil {
		notePanic(u.log, u.metrics, site, r)
	}
}
