package audit

import (
	"context"
	"encoding/json"
	neturl "net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"github.com/fold-run/fold/config"
)

// otlpSink emits audit events as OpenTelemetry log records over OTLP/HTTP.
//
// It is built on the OTel SDK's own pipeline — LoggerProvider → batch
// processor → OTLP exporter — rather than by encoding OTLP by hand. The wire
// format is where a hand-rolled emitter goes quietly wrong: proto3 JSON
// renders 64-bit fields as strings, severity is a numeric enum with a
// specified text mapping, and attribute values are tagged unions. Those are
// details the exporter already gets right and fold has no reason to
// re-derive.
//
// What fold keeps is the accounting. The exporter is wrapped so every export
// is counted as delivered or dropped and failed batches can be dead-lettered:
// audit is the single exit door, and a sink whose failures are invisible
// would undo the guarantee the other sinks now make.
type otlpSink struct {
	provider *sdklog.LoggerProvider
	logger   otellog.Logger
	report   func(outcome string, n int)
}

// newOTLPSink builds the pipeline. The context bounds only construction; the
// exporter's own client governs request timeouts.
func newOTLPSink(rawURL string, headers map[string]string, retry retryPolicy, dead *deadLetter, report func(string, int)) (*otlpSink, error) {
	// A bare base URL follows the OTEL_EXPORTER_OTLP_ENDPOINT convention: the
	// default signal path applies. WithEndpointURL would post to "/" instead,
	// which is silent — the collector 404s and the records are simply gone.
	// The tracing exporter is normalized the same way (gateway/otel.go); an
	// explicit path is honored as given.
	endpoint := rawURL
	if u, err := neturl.Parse(endpoint); err == nil && (u.Path == "" || u.Path == "/") {
		endpoint = strings.TrimSuffix(endpoint, "/") + "/v1/logs"
	}
	opts := []otlploghttp.Option{
		otlploghttp.WithEndpointURL(endpoint),
		otlploghttp.WithRetry(otlploghttp.RetryConfig{
			// fold's audit.retry knobs drive the exporter's own backoff, so
			// one sink's configuration means the same thing as another's.
			// The exporter bounds by elapsed time rather than attempts, so
			// the attempt count is expressed as the time those attempts would
			// have taken.
			Enabled:         retry.maxAttempts > 1,
			InitialInterval: retry.initial,
			MaxInterval:     retry.max,
			MaxElapsedTime:  retry.elapsedBudget(),
		}),
	}
	if len(headers) > 0 {
		opts = append(opts, otlploghttp.WithHeaders(headers))
	}
	exp, err := otlploghttp.New(context.Background(), opts...)
	if err != nil {
		return nil, err
	}
	// The semconv version must match the one the SDK's default resource
	// carries (the same one gateway/otel.go uses for spans): Merge fails on a
	// schema-URL conflict, and the fallback below would then quietly ship
	// records labelled unknown_service — which is how this was found, in a
	// collector's output rather than in a test.
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName("fold"),
	))
	if err != nil {
		res = resource.Default()
	}
	provider := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(&countingExporter{
			Exporter: exp,
			report:   report,
			dead:     dead,
		})),
	)
	return &otlpSink{
		provider: provider,
		logger:   provider.Logger("github.com/fold-run/fold/audit"),
		report:   report,
	}, nil
}

// Emit converts one audit event into a log record and hands it to the SDK.
//
// The body is a short human-readable summary and the structured fields are
// attributes, which is the mapping OTel expects — a body carrying serialized
// JSON would be a blob no backend can filter on. Attribute names match the
// span attributes fold already emits (mcp.method, fold.upstream, ...), so a
// trace and its audit record join on the same keys.
func (s *otlpSink) Emit(e Event) {
	var r otellog.Record
	r.SetTimestamp(e.Time)
	r.SetObservedTimestamp(time.Now())
	sev, text := severityFor(e.Outcome)
	r.SetSeverity(sev)
	r.SetSeverityText(text)
	r.SetBody(attribute.StringValue(summarize(e)))
	r.AddAttributes(otlpAttributes(e)...)
	s.logger.Emit(context.Background(), r)
}

// Close flushes what is queued and shuts the pipeline down, bounded so a dead
// collector cannot hold up gateway shutdown.
func (s *otlpSink) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return s.provider.Shutdown(ctx)
}

// summarize is the record body: enough to read in a log viewer without
// unpacking attributes.
func summarize(e Event) string {
	s := e.Method
	if e.Name != "" {
		s += " " + e.Name
	}
	if e.Outcome != "" {
		s += " → " + string(e.Outcome)
	}
	return s
}

// severityFor maps an audit outcome to an OTel severity. A refusal is a
// warning rather than an error: policy denying a call is the gateway working,
// and logging it at ERROR would train an operator to ignore ERROR.
func severityFor(o Outcome) (otellog.Severity, string) {
	switch o {
	case OutcomeError, OutcomeUpstreamDown:
		return otellog.SeverityError, "ERROR"
	case OutcomeDenied, OutcomeRateLimited, OutcomeBudgetExhausted,
		OutcomeForbidden, OutcomeUnauthenticated:
		return otellog.SeverityWarn, "WARN"
	default:
		return otellog.SeverityInfo, "INFO"
	}
}

// otlpAttributes renders an event's structured fields, skipping the empty
// ones so a backend's attribute list stays meaningful.
func otlpAttributes(e Event) []attribute.KeyValue {
	attrs := []attribute.KeyValue{attribute.String("mcp.method", e.Method)}
	add := func(k, v string) {
		if v != "" {
			attrs = append(attrs, attribute.String(k, v))
		}
	}
	add("mcp.name", e.Name)
	add("fold.upstream", e.Upstream)
	add("fold.outcome", string(e.Outcome))
	add("fold.policy.decision", e.Decision)
	add("fold.policy.rule", e.RuleID)
	add("fold.tenant", e.Tenant)
	add("enduser.id", e.Principal)
	add("enduser.issuer", e.Issuer)
	add("error.message", e.Error)
	if e.LatencyMs > 0 {
		attrs = append(attrs, attribute.Int64("fold.latency_ms", e.LatencyMs))
	}
	if e.UpstreamCalls > 0 {
		attrs = append(attrs, attribute.Int("fold.upstream_calls", e.UpstreamCalls))
	}
	if e.ItemsServed > 0 {
		attrs = append(attrs, attribute.Int("fold.items_served", e.ItemsServed))
	}
	return attrs
}

// countingExporter is fold's accounting wrapped around the SDK's exporter: the
// batch processor calls Export, and this records whether the records arrived.
// Without it, this sink would be the one destination whose losses no metric
// reports.
type countingExporter struct {
	*otlploghttp.Exporter
	report func(outcome string, n int)
	dead   *deadLetter
}

func (c *countingExporter) Export(ctx context.Context, records []sdklog.Record) error {
	err := c.Exporter.Export(ctx, records)
	if err == nil {
		c.report(OutcomeDelivered, len(records))
		return nil
	}
	// The exporter has already exhausted its own retry by the time it returns
	// an error, so this batch is not coming back.
	if c.dead != nil {
		c.dead.writeRecords(records)
		return err
	}
	c.report(OutcomeDropped, len(records))
	return err
}

func (c *countingExporter) Shutdown(ctx context.Context) error {
	err := c.Exporter.Shutdown(ctx)
	if c.dead != nil {
		_ = c.dead.Close()
	}
	return err
}

// otlpEventJSON is the dead-letter shape for an OTLP record: the fields that
// survive the conversion, so an abandoned record is still replayable as JSON
// rather than lost because it was mid-pipeline when delivery failed.
type otlpEventJSON struct {
	Time       time.Time         `json:"time"`
	Severity   string            `json:"severity"`
	Body       string            `json:"body"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

// writeRecords dead-letters SDK records. They have already been converted out
// of audit.Event by this point, so they are written in their converted form
// and labelled as such — a replay tool needs to know which shape it is
// reading.
func (d *deadLetter) writeRecords(records []sdklog.Record) {
	if d == nil {
		return
	}
	for i := range records {
		r := records[i]
		out := otlpEventJSON{
			Time:       r.Timestamp(),
			Severity:   r.SeverityText(),
			Body:       r.Body().AsString(),
			Attributes: map[string]string{},
		}
		r.WalkAttributes(func(kv attribute.KeyValue) bool {
			out.Attributes[string(kv.Key)] = kv.Value.String()
			return true
		})
		data, err := json.Marshal(out)
		if err != nil {
			d.report(OutcomeDropped, 1)
			continue
		}
		if err := d.rf.writeLine(data); err != nil {
			d.report(OutcomeDropped, 1)
			continue
		}
		d.report(OutcomeDeadLettered, 1)
	}
}

// otlpLogsSink builds the sink from config, returning the error for the
// caller to report and skip on.
func otlpLogsSink(s config.AuditSink, dead *deadLetter, report func(string, int)) (*otlpSink, error) {
	return newOTLPSink(s.URL, s.Headers, resolveRetry(s.Retry), dead, report)
}
