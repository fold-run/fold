package gateway

import (
	"log/slog"
	"runtime/debug"
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
