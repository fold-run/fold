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
		budgetDegr: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fold_budget_degraded_total",
			Help: "Budget decisions made per-instance because shared state was unreachable, by scope. Non-zero means the fleet is not enforcing one allowance — alert on it.",
		}, []string{"scope"}),
	}
	m.registry.MustRegister(
		m.requests, m.requestDur, m.upstreamReq, m.upstreamDur, m.httpRejects, m.discovery,
		m.budgetDegr,
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

// observeBudgetDegraded counts a budget decision taken without shared state.
// Fail-open is deliberate, but a fleet running unbudgeted must be visible.
func (m *metricsSet) observeBudgetDegraded(scope string) {
	m.budgetDegr.WithLabelValues(scope).Inc()
}

func (m *metricsSet) discoverySync(outcome string) {
	m.discovery.WithLabelValues(outcome).Inc()
}

func (m *metricsSet) handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
