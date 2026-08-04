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
}

func newMetricsSet(ups []*upstream) *metricsSet {
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
	}
	m.registry.MustRegister(
		m.requests, m.requestDur, m.upstreamReq, m.upstreamDur, m.httpRejects,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	build := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "fold_build_info",
		Help: "Build metadata; value is always 1.",
	}, []string{"version"})
	build.WithLabelValues(version).Set(1)
	m.registry.MustRegister(build)

	for _, u := range ups {
		m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name:        "fold_upstream_breaker_state",
			Help:        "Circuit breaker state per upstream: 0 closed, 1 half-open, 2 open.",
			ConstLabels: prometheus.Labels{"upstream": u.cfg.ID},
		}, func() float64 {
			switch u.breaker.State(context.Background()) {
			case breaker.Open:
				return 2
			case breaker.HalfOpen:
				return 1
			}
			return 0
		}))
	}
	return m
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

func (m *metricsSet) handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
