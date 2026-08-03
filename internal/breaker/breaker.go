// Package breaker implements a per-upstream circuit breaker: N consecutive
// failures open the circuit; a single probe is admitted after the half-open
// interval, and its outcome closes or re-opens the circuit.
package breaker

import (
	"sync"
	"time"
)

// State is the breaker's current disposition.
type State string

const (
	Closed   State = "closed"
	Open     State = "open"
	HalfOpen State = "half-open"
)

// Breaker is a consecutive-failure circuit breaker.
type Breaker struct {
	mu               sync.Mutex
	failureThreshold int
	halfOpenAfter    time.Duration

	state    State
	failures int
	openedAt time.Time
	probing  bool
	now      func() time.Time
}

// New returns a breaker that opens after threshold consecutive failures and
// admits a probe after halfOpenAfter. A threshold <= 0 returns nil, which
// all methods treat as always-closed.
func New(threshold int, halfOpenAfter time.Duration) *Breaker {
	if threshold <= 0 {
		return nil
	}
	return &Breaker{
		failureThreshold: threshold,
		halfOpenAfter:    halfOpenAfter,
		state:            Closed,
		now:              time.Now,
	}
}

// Allow reports whether a request may proceed. In the half-open state only
// one probe is admitted at a time.
func (b *Breaker) Allow() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Closed:
		return true
	case Open:
		if b.now().Sub(b.openedAt) >= b.halfOpenAfter {
			b.state = HalfOpen
			b.probing = true
			return true
		}
		return false
	case HalfOpen:
		if b.probing {
			return false
		}
		b.probing = true
		return true
	}
	return true
}

// Record reports a request outcome to the breaker.
func (b *Breaker) Record(success bool) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if success {
		b.state = Closed
		b.failures = 0
		b.probing = false
		return
	}
	b.probing = false
	b.failures++
	if b.state == HalfOpen || b.failures >= b.failureThreshold {
		b.state = Open
		b.openedAt = b.now()
	}
}

// State returns the breaker's current state.
func (b *Breaker) State() State {
	if b == nil {
		return Closed
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == Open && b.now().Sub(b.openedAt) >= b.halfOpenAfter {
		return HalfOpen
	}
	return b.state
}
