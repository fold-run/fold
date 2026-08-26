package config

import (
	"strings"
	"testing"
)

// Negative durations and thresholds used to silently mean "use the default";
// the schema has always said minimum 0, and Validate now refuses the same
// documents the schema does.
func TestNegativeDurationsRejected(t *testing.T) {
	base := func() *Config {
		return &Config{Upstreams: []Upstream{{ID: "a", Namespace: "a", URL: "http://127.0.0.1:1"}}}
	}
	cases := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"negative connectMs", func(c *Config) {
			c.Upstreams[0].Timeouts = &Timeouts{ConnectMs: -1}
		}, "timeouts must not be negative"},
		{"negative requestMs", func(c *Config) {
			c.Upstreams[0].Timeouts = &Timeouts{RequestMs: -5}
		}, "timeouts must not be negative"},
		{"negative streamIdleMs", func(c *Config) {
			c.Upstreams[0].Timeouts = &Timeouts{StreamIdleMs: -1}
		}, "timeouts must not be negative"},
		{"negative failureThreshold", func(c *Config) {
			c.Upstreams[0].CircuitBreaker = &CircuitBreaker{FailureThreshold: -2}
		}, "circuitBreaker values must not be negative"},
		{"negative halfOpenAfterMs", func(c *Config) {
			c.Upstreams[0].CircuitBreaker = &CircuitBreaker{HalfOpenAfterMs: -1}
		}, "circuitBreaker values must not be negative"},
	}
	for _, tc := range cases {
		cfg := base()
		tc.mut(cfg)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate() = %v, want error containing %q", tc.name, err, tc.want)
		}
	}
	// Zero values stay valid — they mean "default", as they always have.
	cfg := base()
	cfg.Upstreams[0].Timeouts = &Timeouts{}
	cfg.Upstreams[0].CircuitBreaker = &CircuitBreaker{}
	if err := cfg.Validate(); err != nil {
		t.Errorf("zero values should validate: %v", err)
	}
}

// requireDurable is an assertion the operator makes about their deployment,
// so the document that cannot honour it is refused rather than started —
// fold --validate says so before a rollout does.
func TestRequireDurableRejectsADocumentWithNoDurableSink(t *testing.T) {
	base := func(sinks ...AuditSink) *Config {
		return &Config{
			Upstreams: []Upstream{{ID: "a", Namespace: "a", URL: "http://127.0.0.1:1"}},
			Audit:     &Audit{RequireDurable: true, Sinks: sinks},
		}
	}
	rejected := map[string][]AuditSink{
		"stdout alone":                         {{Type: "stdout"}},
		"webhook without a dead-letter path":   {{Type: "webhook", URL: "https://siem.example.com"}},
		"otlp-logs without a dead-letter path": {{Type: "otlp-logs", URL: "http://collector:4318"}},
		"no sinks at all":                      nil,
	}
	for name, sinks := range rejected {
		err := base(sinks...).Validate()
		if err == nil || !strings.Contains(err.Error(), "requireDurable") {
			t.Errorf("%s: expected a requireDurable rejection, got %v", name, err)
		}
	}

	accepted := map[string][]AuditSink{
		"a file sink": {{Type: "file", Path: "/var/log/fold/audit.jsonl"}},
		"a webhook that dead-letters": {{
			Type: "webhook", URL: "https://siem.example.com", DeadLetterPath: "/var/log/fold/dead.jsonl",
		}},
		"otlp-logs that dead-letters": {{
			Type: "otlp-logs", URL: "http://collector:4318", DeadLetterPath: "/var/log/fold/dead.jsonl",
		}},
		"stdout alongside a file sink": {
			{Type: "stdout"},
			{Type: "file", Path: "/var/log/fold/audit.jsonl"},
		},
	}
	for name, sinks := range accepted {
		if err := base(sinks...).Validate(); err != nil {
			t.Errorf("%s: expected acceptance, got %v", name, err)
		}
	}

	// Unset, the same non-durable documents are fine: this is opt-in.
	cfg := base(AuditSink{Type: "stdout"})
	cfg.Audit.RequireDurable = false
	if err := cfg.Validate(); err != nil {
		t.Errorf("requireDurable unset should not constrain sinks: %v", err)
	}
}
