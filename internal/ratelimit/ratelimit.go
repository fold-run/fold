// Package ratelimit implements a sliding-window request limiter.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter admits at most rpm requests per rolling minute.
type Limiter struct {
	mu     sync.Mutex
	rpm    int
	window time.Duration
	// two-bucket sliding window: previous and current fixed windows,
	// weighted by overlap. Cheap and accurate enough for a gateway guard.
	curStart time.Time
	curCount int
	prev     int
	now      func() time.Time
}

// New returns a limiter admitting rpm requests per minute.
// rpm <= 0 returns nil, which Allow treats as unlimited.
func New(rpm int) *Limiter {
	if rpm <= 0 {
		return nil
	}
	return &Limiter{rpm: rpm, window: time.Minute, now: time.Now}
}

// Allow reports whether one more request fits in the window and, when it
// does not, how long the caller should wait before retrying.
func (l *Limiter) Allow() (ok bool, retryAfter time.Duration) {
	if l == nil {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if l.curStart.IsZero() {
		l.curStart = now
	}
	for now.Sub(l.curStart) >= l.window {
		if now.Sub(l.curStart) >= 2*l.window {
			l.prev, l.curCount = 0, 0
			l.curStart = now
			break
		}
		l.prev = l.curCount
		l.curCount = 0
		l.curStart = l.curStart.Add(l.window)
	}
	elapsed := float64(now.Sub(l.curStart)) / float64(l.window)
	effective := float64(l.prev)*(1-elapsed) + float64(l.curCount)
	if effective+1 > float64(l.rpm) {
		return false, l.window - now.Sub(l.curStart)
	}
	l.curCount++
	return true, 0
}
