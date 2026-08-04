package ratelimit

import (
	"testing"
	"time"
)

func TestBudgetEnforced(t *testing.T) {
	l := New(3)
	now := time.Now()
	l.now = func() time.Time { return now }

	for i := range 3 {
		if ok, _ := l.Allow(); !ok {
			t.Fatalf("request %d should be admitted", i)
		}
	}
	ok, retry := l.Allow()
	if ok {
		t.Fatal("4th request in the window should be rejected")
	}
	if retry <= 0 || retry > time.Minute {
		t.Errorf("retryAfter = %v", retry)
	}

	// After the window fully rolls past, the budget is fresh.
	now = now.Add(2 * time.Minute)
	if ok, _ := l.Allow(); !ok {
		t.Error("budget should reset after the window passes")
	}
}

func TestSlidingWindowWeighting(t *testing.T) {
	l := New(10)
	now := time.Now()
	l.now = func() time.Time { return now }

	for range 10 {
		l.Allow()
	}
	// Half a window later, roughly half the budget is back.
	now = now.Add(90 * time.Second)
	admitted := 0
	for range 10 {
		if ok, _ := l.Allow(); ok {
			admitted++
		}
	}
	if admitted < 3 || admitted > 7 {
		t.Errorf("admitted %d mid-window, want roughly half the budget", admitted)
	}
}

func TestNilLimiter(t *testing.T) {
	var l *Limiter
	if ok, _ := l.Allow(); !ok {
		t.Error("nil limiter must be unlimited")
	}
	if New(0) != nil {
		t.Error("rpm<=0 should return nil limiter")
	}
}
