package gateway

import (
	"context"
	"net/http"
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

// injectTraceContext copies any captured trace context onto outgoing
// upstream request headers.
func injectTraceContext(ctx context.Context, hdr http.Header) {
	tc, ok := ctx.Value(traceContextKey{}).(traceContext)
	if !ok {
		return
	}
	hdr.Set("traceparent", tc.traceparent)
	if tc.tracestate != "" {
		hdr.Set("tracestate", tc.tracestate)
	}
}
