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
