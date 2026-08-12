package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/fold-run/fold/audit"
	"github.com/fold-run/fold/config"
)

// gwTracer is fold's first-party OpenTelemetry instrumentation: one server
// span per MCP request and one client span per guarded upstream call,
// exported over OTLP/HTTP via a batching processor so the hot path never
// waits on the collector. A nil *gwTracer (tracing not configured) makes
// every method a no-op — the request path pays one nil check.
type gwTracer struct {
	provider *sdktrace.TracerProvider
	tracer   trace.Tracer
	prop     propagation.TextMapPropagator

	// recordPrincipal gates the enduser.id span attribute — opt-in, because
	// the subject is personal data and trace backends commonly have broader
	// access than the audit trail (which carries the same identity).
	recordPrincipal bool
}

func newGwTracer(cfg *config.Tracing) (*gwTracer, error) {
	// A bare base URL follows the OTEL_EXPORTER_OTLP_ENDPOINT convention:
	// the default signal path applies. (The SDK's WithEndpointURL would
	// post to "/" instead.) An explicit path is honored as given.
	endpoint := cfg.OTLPEndpoint
	if u, err := url.Parse(endpoint); err == nil && (u.Path == "" || u.Path == "/") {
		endpoint = strings.TrimSuffix(endpoint, "/") + "/v1/traces"
	}
	exporter, err := otlptracehttp.New(context.Background(),
		otlptracehttp.WithEndpointURL(endpoint))
	if err != nil {
		return nil, fmt.Errorf("tracing: %w", err)
	}
	name := cfg.ServiceName
	if name == "" {
		name = "fold"
	}
	ratio := cfg.SampleRatio
	if ratio <= 0 {
		ratio = 1
	}
	res, err := sdkresource.Merge(sdkresource.Default(), sdkresource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(name),
		semconv.ServiceVersion(version),
	))
	if err != nil {
		return nil, fmt.Errorf("tracing: %w", err)
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		// Parent-based: a caller that sampled its trace stays sampled end to
		// end; the ratio applies only to traces fold roots itself.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
	)
	return &gwTracer{
		provider: provider,
		tracer:   provider.Tracer("github.com/fold-run/fold/gateway"),
		prop: propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{}, propagation.Baggage{}),
		recordPrincipal: cfg.RecordPrincipal,
	}, nil
}

// startServer opens the per-request server span, parented on the caller's
// W3C trace context when the incoming headers carry one.
func (t *gwTracer) startServer(ctx context.Context, method string, hdr http.Header) (context.Context, trace.Span) {
	if t == nil {
		return ctx, nil
	}
	if hdr != nil {
		ctx = t.prop.Extract(ctx, propagation.HeaderCarrier(hdr))
	}
	return t.tracer.Start(ctx, method,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attribute.String("mcp.method", method)))
}

// endServer stamps the request's terminal audit fields onto its span — the
// span and the audit event describe the same outcome by construction.
func (t *gwTracer) endServer(span trace.Span, evt *audit.Event, err error) {
	if t == nil || span == nil {
		return
	}
	attrs := []attribute.KeyValue{attribute.String("fold.outcome", string(evt.Outcome))}
	if evt.Name != "" {
		attrs = append(attrs, attribute.String("mcp.name", evt.Name))
	}
	if evt.Upstream != "" {
		attrs = append(attrs, attribute.String("fold.upstream", evt.Upstream))
	}
	if evt.Decision != "" {
		attrs = append(attrs, attribute.String("fold.policy.decision", evt.Decision))
	}
	if evt.RuleID != "" {
		attrs = append(attrs, attribute.String("fold.policy.rule", evt.RuleID))
	}
	if evt.Principal != "" && t.recordPrincipal {
		attrs = append(attrs, attribute.String("enduser.id", evt.Principal))
	}
	span.SetAttributes(attrs...)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, evt.Error)
	}
	span.End()
}

// startClient opens the client span for one guarded upstream call.
func (t *gwTracer) startClient(ctx context.Context, upstreamID string) (context.Context, trace.Span) {
	if t == nil {
		return ctx, nil
	}
	return t.tracer.Start(ctx, "upstream "+upstreamID,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("fold.upstream", upstreamID)))
}

// endClient closes an upstream client span with its guard outcome.
func endClient(span trace.Span, outcome string) {
	if span == nil {
		return
	}
	span.SetAttributes(attribute.String("fold.outcome", outcome))
	if outcome != "ok" {
		span.SetStatus(codes.Error, outcome)
	}
	span.End()
}

// shutdown flushes buffered spans, bounded so Close cannot hang on a dead
// collector.
func (t *gwTracer) shutdown() {
	if t == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = t.provider.Shutdown(ctx)
}
