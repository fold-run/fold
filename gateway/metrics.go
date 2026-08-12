package gateway

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/fold-run/fold/internal/breaker"
)

// metricsSet is the gateway's Prometheus instrumentation. Each Gateway owns
// its registry, so embedding multiple gateways (or tests) never collide.
type metricsSet struct {
	registry *prometheus.Registry

	requests    *prometheus.CounterVec   // MCP requests by method and outcome
	requestDur  *prometheus.HistogramVec // MCP request duration by method
	upstreamReq *prometheus.CounterVec   // upstream requests by upstream and outcome
	upstreamDur *prometheus.HistogramVec // upstream request duration by upstream
	httpRejects *prometheus.CounterVec   // HTTP-level rejections by reason
	discovery   *prometheus.CounterVec   // discovery syncs by outcome
	budgetDegr  *prometheus.CounterVec   // budget decisions made without shared state
	fanOut      prometheus.Histogram     // upstream invocations per downstream request

	// Tenant-scoped series. These are separate metrics rather than a tenant
	// label on the two above, and the reason is the v1 contract: metric names
	// *and label sets* are frozen (README, "API stability"), so adding a
	// label to fold_requests_total would break every dashboard and recording
	// rule built on it. New names are additive, which is what the contract
	// permits — see docs/design-tenancy.md, "the record".
	//
	// Their cardinality is the number of declared tenants, which comes from
	// the config document and is therefore the operator's own choice, exactly
	// like the upstream label. They carry no per-principal dimension.
	tenantReq   *prometheus.CounterVec // requests by tenant and outcome
	tenantCalls *prometheus.CounterVec // upstream invocations attributed to a tenant

	// auditEvents counts delivery outcomes per sink. A dropped or
	// dead-lettered event is the audit trail being incomplete, which is the
	// one failure the trail cannot report about itself.
	auditEvents *prometheus.CounterVec

	// listItems makes the filtering visible: fold has always shrunk a
	// caller's list to what they may invoke, and nothing said by how much.
	listItems *prometheus.CounterVec

	// panics counts recovered panics by site (see rescue.go). The gateway
	// survives them by design, but every one is a bug — alert on non-zero.
	panics *prometheus.CounterVec
}

func newMetricsSet(current func() []*upstream) *metricsSet {
	m := &metricsSet{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fold_requests_total",
			Help: "MCP requests handled by the gateway, by method and outcome.",
		}, []string{"method", "outcome"}),
		requestDur: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "fold_request_duration_seconds",
			Help:    "MCP request duration through the gateway, by method.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method"}),
		upstreamReq: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fold_upstream_requests_total",
			Help: "Requests proxied to upstreams, by upstream and outcome.",
		}, []string{"upstream", "outcome"}),
		upstreamDur: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "fold_upstream_request_duration_seconds",
			Help:    "Upstream request duration, by upstream.",
			Buckets: prometheus.DefBuckets,
		}, []string{"upstream"}),
		httpRejects: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fold_http_rejections_total",
			Help: "Requests rejected before the MCP layer, by reason.",
		}, []string{"reason"}),
		discovery: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fold_discovery_syncs_total",
			Help: "Upstream-discovery polls by outcome: applied, unchanged, rejected, error.",
		}, []string{"outcome"}),
		fanOut: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "fold_request_upstream_calls",
			Help: "Upstream invocations per downstream request. A federated list fans out to every upstream, so this is where a cheap-looking client request shows its real cost.",
			// Federation sizes, not latencies: 1 is the named-call case, the
			// tail is the width of the federation.
			Buckets: []float64{1, 2, 5, 10, 20, 50, 100},
		}),
		budgetDegr: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fold_budget_degraded_total",
			Help: "Budget decisions made per-instance because shared state was unreachable, by scope. Non-zero means the fleet is not enforcing one allowance — alert on it.",
		}, []string{"scope"}),
		tenantReq: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fold_tenant_requests_total",
			Help: "MCP requests by tenant and outcome. Only requests whose principal resolved to a tenant are counted; everything else is in fold_requests_total, which counts all of them.",
		}, []string{"tenant", "outcome"}),
		listItems: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fold_list_items_total",
			Help: "List items by method and stage: offered by upstreams, served to the caller, and capped by a policy rule's maxItems. served/offered is the context reduction per-principal filtering already performs.",
		}, []string{"method", "stage"}),
		auditEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fold_audit_events_total",
			Help: "Audit events by sink type and delivery outcome: delivered, retried, dead_lettered, dropped. Audit is the single exit door — anything but delivered means the record is incomplete somewhere.",
		}, []string{"sink", "outcome"}),
		tenantCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fold_tenant_upstream_calls_total",
			Help: "Upstream invocations attributed to a tenant — the same unit a tenant budget is charged in, so this is what a budget is spent on, live.",
		}, []string{"tenant"}),
		panics: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fold_panics_total",
			Help: "Panics recovered by the gateway, by site. The process survives them by design, but every one is a bug — alert on non-zero.",
		}, []string{"site"}),
	}
	m.registry.MustRegister(
		m.requests, m.requestDur, m.upstreamReq, m.upstreamDur, m.httpRejects, m.discovery,
		m.budgetDegr, m.fanOut, m.tenantReq, m.tenantCalls, m.auditEvents, m.listItems, m.panics,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	build := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "fold_build_info",
		Help: "Build metadata; value is always 1.",
	}, []string{"version"})
	build.WithLabelValues(version).Set(1)
	m.registry.MustRegister(build)
	m.registry.MustRegister(&upstreamCollector{current: current})
	return m
}

// upstreamCollector exports per-upstream gauges resolved at scrape time
// against the gateway's current routing snapshot, so hot-reloaded upstreams
// appear (and retired ones disappear) without re-registration.
type upstreamCollector struct {
	current func() []*upstream
}

var (
	breakerStateDesc = prometheus.NewDesc(
		"fold_upstream_breaker_state",
		"Circuit breaker state per upstream: 0 closed, 1 half-open, 2 open.",
		[]string{"upstream"}, nil)
	endpointHealthyDesc = prometheus.NewDesc(
		"fold_upstream_endpoint_healthy",
		"Balancer view per endpoint of a multi-endpoint upstream: 1 in rotation, 0 ejected after a connect failure.",
		[]string{"upstream", "endpoint"}, nil)
)

func (c *upstreamCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- breakerStateDesc
	ch <- endpointHealthyDesc
}

func (c *upstreamCollector) Collect(ch chan<- prometheus.Metric) {
	for _, u := range c.current() {
		v := 0.0
		switch u.breaker.State(context.Background()) {
		case breaker.Open:
			v = 2
		case breaker.HalfOpen:
			v = 1
		}
		ch <- prometheus.MustNewConstMetric(breakerStateDesc, prometheus.GaugeValue, v, u.cfg.ID)
		if len(u.cfg.URLs) > 0 {
			for _, ep := range u.endpoints.snapshot(true) {
				healthy := 0.0
				if ep.Healthy {
					healthy = 1
				}
				ch <- prometheus.MustNewConstMetric(endpointHealthyDesc, prometheus.GaugeValue, healthy, u.cfg.ID, ep.URL)
			}
		}
	}
}

// sessionCollector exports session-population gauges resolved at scrape
// time, like upstreamCollector. Sessions are where an abandoned client turns
// into retained memory (each holds gateway state, and its subscriptions pin
// upstream ones), so their population must be watchable — expiry without a
// gauge would leave a leak and its fix equally invisible.
type sessionCollector struct {
	downstream func() int         // live downstream MCP sessions
	upstreams  func() []*upstream // for per-upstream bridged session counts
}

var (
	downstreamSessionsDesc = prometheus.NewDesc(
		"fold_downstream_sessions",
		"Live downstream MCP sessions. Sustained growth means clients mint sessions faster than server.sessionIdleTimeoutMs expires them.",
		nil, nil)
	bridgedSessionsDesc = prometheus.NewDesc(
		"fold_upstream_bridged_sessions",
		"Per-client bridged sessions currently held against an upstream.",
		[]string{"upstream"}, nil)
)

func (c *sessionCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- downstreamSessionsDesc
	ch <- bridgedSessionsDesc
}

func (c *sessionCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(downstreamSessionsDesc, prometheus.GaugeValue, float64(c.downstream()))
	for _, u := range c.upstreams() {
		ch <- prometheus.MustNewConstMetric(bridgedSessionsDesc, prometheus.GaugeValue, float64(u.bridgedCount()), u.cfg.ID)
	}
}

func (m *metricsSet) observeRequest(method, outcome string, d time.Duration) {
	m.requests.WithLabelValues(method, outcome).Inc()
	m.requestDur.WithLabelValues(method).Observe(d.Seconds())
}

func (m *metricsSet) observeUpstream(upstreamID, outcome string, d time.Duration) {
	m.upstreamReq.WithLabelValues(upstreamID, outcome).Inc()
	m.upstreamDur.WithLabelValues(upstreamID).Observe(d.Seconds())
}

func (m *metricsSet) reject(reason string) {
	m.httpRejects.WithLabelValues(reason).Inc()
}

// observeFanOut records what one downstream request cost in upstream calls.
func (m *metricsSet) observeFanOut(n int) {
	m.fanOut.Observe(float64(n))
}

// observeTenant records one request against its tenant: the outcome, and what
// it cost in upstream invocations — the unit the tenant's budget is charged
// in, so an operator can watch an allowance being spent without waiting for
// the audit stream to be aggregated somewhere else.
//
// An untenanted request records nothing rather than recording an empty label:
// a "" series would look like a tenant, and the request is already counted in
// fold_requests_total.
func (m *metricsSet) observeTenant(tenant, outcome string, upstreamCalls int) {
	if tenant == "" {
		return
	}
	m.tenantReq.WithLabelValues(tenant, outcome).Inc()
	if upstreamCalls > 0 {
		m.tenantCalls.WithLabelValues(tenant).Add(float64(upstreamCalls))
	}
}

// observeBudgetDegraded counts a budget decision taken without shared state.
// Fail-open is deliberate, but a fleet running unbudgeted must be visible.
func (m *metricsSet) observeBudgetDegraded(scope string) {
	m.budgetDegr.WithLabelValues(scope).Inc()
}

// observeListShaping records what one list request offered, served, and had
// capped. Counting all three lets an operator read the reduction as a ratio
// rather than a number without a denominator.
func (m *metricsSet) observeListShaping(method string, offered, served, capped int) {
	if offered == 0 {
		return
	}
	m.listItems.WithLabelValues(method, "offered").Add(float64(offered))
	m.listItems.WithLabelValues(method, "served").Add(float64(served))
	if capped > 0 {
		m.listItems.WithLabelValues(method, "capped").Add(float64(capped))
	}
}

// panicked counts one recovered panic (see rescue.go).
func (m *metricsSet) panicked(site string) {
	m.panics.WithLabelValues(site).Inc()
}

// observeAudit records the fate of audit events for one sink.
func (m *metricsSet) observeAudit(sink, outcome string, n int) {
	m.auditEvents.WithLabelValues(sink, outcome).Add(float64(n))
}

func (m *metricsSet) discoverySync(outcome string) {
	m.discovery.WithLabelValues(outcome).Inc()
}

func (m *metricsSet) handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
