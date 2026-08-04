// Package state abstracts the gateway's shared runtime state — rate-limit
// windows, circuit-breaker state, and list caches — behind interfaces with
// two providers: in-process (default) and Redis (set REDIS_URL or
// server.redisUrl), which makes a fleet of gateway instances behave as one.
package state

import (
	"context"
	"time"

	"github.com/fold-run/fold/internal/breaker"
	"github.com/fold-run/fold/internal/cache"
	"github.com/fold-run/fold/internal/ratelimit"
)

// Limiter admits requests against a shared budget.
type Limiter interface {
	// Allow reports whether one more request fits and, when it does not,
	// how long to wait before retrying.
	Allow(ctx context.Context) (ok bool, retryAfter time.Duration)
}

// Breaker is a shared circuit breaker.
type Breaker interface {
	Allow(ctx context.Context) bool
	Record(ctx context.Context, success bool)
	State(ctx context.Context) breaker.State
}

// ListCache caches serialized list results with a TTL, invalidated by
// prefix.
type ListCache interface {
	// GetOrFill returns the cached bytes for key, or runs fill and caches
	// its result for ttl. ttl <= 0 bypasses caching.
	GetOrFill(ctx context.Context, key string, ttl time.Duration, fill func(context.Context) ([]byte, error)) ([]byte, error)
	// Invalidate drops entries whose key has the given prefix.
	Invalidate(ctx context.Context, prefix string)
}

// Provider constructs the gateway's shared-state primitives.
type Provider interface {
	// Limiter returns a rate limiter for scope admitting rpm requests per
	// minute. rpm <= 0 returns an unlimited limiter.
	Limiter(scope string, rpm int) Limiter
	// Breaker returns a circuit breaker for scope.
	Breaker(scope string, threshold int, halfOpenAfter time.Duration) Breaker
	// ListCache returns the list cache for scope.
	ListCache(scope string) ListCache
	// Close releases provider resources.
	Close() error
}

// ---- In-memory provider (default) ----

// Memory is the in-process provider; state is per gateway instance.
type Memory struct{}

// NewMemory returns the in-process state provider.
func NewMemory() *Memory { return &Memory{} }

func (*Memory) Limiter(_ string, rpm int) Limiter {
	return memLimiter{l: ratelimit.New(rpm)}
}

func (*Memory) Breaker(_ string, threshold int, halfOpenAfter time.Duration) Breaker {
	return memBreaker{b: breaker.New(threshold, halfOpenAfter)}
}

func (*Memory) ListCache(_ string) ListCache {
	return &memCache{c: cache.New()}
}

func (*Memory) Close() error { return nil }

type memLimiter struct{ l *ratelimit.Limiter }

func (m memLimiter) Allow(context.Context) (bool, time.Duration) { return m.l.Allow() }

type memBreaker struct{ b *breaker.Breaker }

func (m memBreaker) Allow(context.Context) bool          { return m.b.Allow() }
func (m memBreaker) Record(_ context.Context, ok bool)   { m.b.Record(ok) }
func (m memBreaker) State(context.Context) breaker.State { return m.b.State() }

type memCache struct{ c *cache.Cache }

func (m *memCache) GetOrFill(ctx context.Context, key string, ttl time.Duration, fill func(context.Context) ([]byte, error)) ([]byte, error) {
	v, err := m.c.GetOrFill(ctx, key, ttl, func(ctx context.Context) (any, error) {
		return fill(ctx)
	})
	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}

func (m *memCache) Invalidate(_ context.Context, prefix string) { m.c.Invalidate(prefix) }
