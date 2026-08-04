package breaker

import (
	"testing"
	"time"
)

func TestOpensAfterThreshold(t *testing.T) {
	b := New(3, 30*time.Second)
	now := time.Now()
	b.now = func() time.Time { return now }

	for i := range 3 {
		if !b.Allow() {
			t.Fatalf("closed breaker should allow (i=%d)", i)
		}
		b.Record(false)
	}
	if b.Allow() {
		t.Error("breaker should be open after 3 failures")
	}
	if b.State() != Open {
		t.Errorf("state = %v, want open", b.State())
	}

	// Probe after half-open interval; failure re-opens immediately.
	now = now.Add(31 * time.Second)
	if !b.Allow() {
		t.Fatal("half-open breaker should admit one probe")
	}
	if b.Allow() {
		t.Error("only one probe at a time")
	}
	b.Record(false)
	if b.Allow() {
		t.Error("failed probe should re-open the circuit")
	}

	// A successful probe closes it.
	now = now.Add(31 * time.Second)
	if !b.Allow() {
		t.Fatal("should admit probe again")
	}
	b.Record(true)
	if !b.Allow() || b.State() != Closed {
		t.Error("successful probe should close the circuit")
	}
}

func TestSuccessResetsFailures(t *testing.T) {
	b := New(3, time.Second)
	b.Record(false)
	b.Record(false)
	b.Record(true)
	b.Record(false)
	b.Record(false)
	if !b.Allow() {
		t.Error("failure count should reset on success")
	}
}

func TestNilBreaker(t *testing.T) {
	var b *Breaker
	if !b.Allow() || b.State() != Closed {
		t.Error("nil breaker must be always-closed")
	}
	b.Record(false) // must not panic
}
