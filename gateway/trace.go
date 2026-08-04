package gateway

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// traceContext carries W3C Trace Context headers from an incoming request
// so upstream calls join the caller's distributed trace.
type traceContext struct {
	traceparent string
	tracestate  string
}

type traceContextKey struct{}

// withTraceContext captures traceparent/tracestate from incoming request
// headers into ctx. A no-op when the caller sent no trace context.
func withTraceContext(ctx context.Context, hdr http.Header) context.Context {
	if hdr == nil {
		return ctx
	}
	tp := hdr.Get("traceparent")
	if tp == "" {
		return ctx
	}
	return context.WithValue(ctx, traceContextKey{}, traceContext{
		traceparent: tp,
		tracestate:  hdr.Get("tracestate"),
	})
}

// otelInjector serializes an active span context onto outgoing headers.
var otelInjector = propagation.TraceContext{}

// injectTraceContext writes trace context onto outgoing upstream request
// headers. With first-party tracing active there is a live span in ctx —
// inject it, so the upstream hop parents under fold's client span while
// keeping the caller's trace id. Otherwise fall back to copying the
// caller's headers through verbatim (propagation-only mode).
func injectTraceContext(ctx context.Context, hdr http.Header) {
	if trace.SpanContextFromContext(ctx).IsValid() {
		otelInjector.Inject(ctx, propagation.HeaderCarrier(hdr))
		return
	}
	tc, ok := ctx.Value(traceContextKey{}).(traceContext)
	if !ok {
		return
	}
	hdr.Set("traceparent", tc.traceparent)
	if tc.tracestate != "" {
		hdr.Set("tracestate", tc.tracestate)
	}
}
