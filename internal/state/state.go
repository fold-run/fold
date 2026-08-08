// Package state abstracts the gateway's shared runtime state — rate-limit
// windows, circuit-breaker state, and list caches — behind interfaces with
// two providers: in-process (default) and Redis (set REDIS_URL or
// server.redisUrl), which makes a fleet of gateway instances behave as one.
package state

import (
	"context"
	"sync"
	"time"

	"github.com/fold-run/fold/internal/bounded"
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

// Once records single-use keys (e.g. token replay protection).
type Once interface {
	// TryOnce records key for ttl and reports whether this was the first
	// use. false means the key was already recorded — a replay.
	TryOnce(ctx context.Context, key string, ttl time.Duration) bool
}

// Store holds small opaque records, each with its own TTL. Unlike ListCache
// it is not a fill-through cache: absence is meaningful to the caller and
// writes are explicit. Used for records a fleet must agree on rather than
// each instance observing for itself — task ownership, today.
//
// The batch forms exist because the federated task list resolves ownership
// for a whole page at once; issuing one round trip per task would make list
// latency scale with the size of the federation's task set.
type Store interface {
	// Get returns the record for key. Absent — including when the backing
	// store is unreachable — reads as (nil, false).
	Get(ctx context.Context, key string) ([]byte, bool)
	// GetMany returns the records present among keys, omitting absent ones.
	GetMany(ctx context.Context, keys []string) map[string][]byte
	// Set writes key for ttl (ttl <= 0 never expires).
	Set(ctx context.Context, key string, value []byte, ttl time.Duration)
	// SetMany writes every entry with the same ttl in one round trip.
	SetMany(ctx context.Context, entries map[string][]byte, ttl time.Duration)
	// Delete removes key.
	Delete(ctx context.Context, key string)
}

// maxStoreEntries bounds the in-process store, and the local mirror the
// Redis store falls back to during an outage.
const maxStoreEntries = 50_000

// Provider constructs the gateway's shared-state primitives.
type Provider interface {
	// Limiter returns a rate limiter for scope admitting rpm requests per
	// minute. rpm <= 0 returns an unlimited limiter.
	Limiter(scope string, rpm int) Limiter
	// Budget returns an accumulating allowance for scope over a
	// calendar-aligned period. limit <= 0 returns an unlimited budget.
	Budget(scope string, period Period, limit int64) Budget
	// Breaker returns a circuit breaker for scope.
	Breaker(scope string, threshold int, halfOpenAfter time.Duration) Breaker
	// ListCache returns the list cache for scope.
	ListCache(scope string) ListCache
	// Once returns the single-use recorder for scope.
	Once(scope string) Once
	// Store returns the shared record store for scope.
	Store(scope string) Store
	// Close releases provider resources.
	Close() error
}

// ---- In-memory provider (default) ----

// Memory is the in-process provider; state is per gateway instance.
type Memory struct{}

// NewMemory returns the in-process state provider.
func NewMemory() *Memory { return &Memory{} }

// Limiter implements Provider.
func (*Memory) Limiter(_ string, rpm int) Limiter {
	return memLimiter{l: ratelimit.New(rpm)}
}

// Breaker implements Provider.
func (*Memory) Breaker(_ string, threshold int, halfOpenAfter time.Duration) Breaker {
	return memBreaker{b: breaker.New(threshold, halfOpenAfter)}
}

// ListCache implements Provider.
func (*Memory) ListCache(_ string) ListCache {
	return &memCache{c: cache.New()}
}

// Once implements Provider.
func (*Memory) Once(_ string) Once { return NewMemOnce() }

// Store implements Provider.
func (*Memory) Store(_ string) Store { return NewMemStore() }

// Close implements Provider.
func (*Memory) Close() error { return nil }

// MemStore is the in-process record store. Exported so the Redis store can
// mirror into it and fall back on it during an outage.
type MemStore struct{ m *bounded.Map[[]byte] }

// NewMemStore returns an empty in-process record store.
func NewMemStore() *MemStore { return &MemStore{m: bounded.New[[]byte](maxStoreEntries)} }

// Get implements Store.
func (s *MemStore) Get(_ context.Context, key string) ([]byte, bool) { return s.m.Load(key) }

// GetMany implements Store.
func (s *MemStore) GetMany(_ context.Context, keys []string) map[string][]byte {
	out := make(map[string][]byte, len(keys))
	for _, k := range keys {
		if v, ok := s.m.Load(k); ok {
			out[k] = v
		}
	}
	return out
}

// Set implements Store.
func (s *MemStore) Set(_ context.Context, key string, value []byte, ttl time.Duration) {
	s.m.Store(key, value, ttl)
}

// SetMany implements Store.
func (s *MemStore) SetMany(_ context.Context, entries map[string][]byte, ttl time.Duration) {
	for k, v := range entries {
		s.m.Store(k, v, ttl)
	}
}

// Delete implements Store.
func (s *MemStore) Delete(_ context.Context, key string) { s.m.Delete(key) }

// MemOnce is the in-process single-use recorder. Exported so the Redis
// provider can fall back to it during an outage.
type MemOnce struct {
	mu   sync.Mutex
	seen map[string]time.Time // key → expiry
}

// NewMemOnce returns an in-process single-use recorder.
func NewMemOnce() *MemOnce { return &MemOnce{seen: map[string]time.Time{}} }

// TryOnce reports whether key is fresh, recording it for ttl.
func (o *MemOnce) TryOnce(_ context.Context, key string, ttl time.Duration) bool {
	now := time.Now()
	o.mu.Lock()
	defer o.mu.Unlock()
	// Lazily drop expired records so the map tracks only live keys.
	for k, exp := range o.seen {
		if exp.Before(now) {
			delete(o.seen, k)
		}
	}
	if exp, ok := o.seen[key]; ok && exp.After(now) {
		return false
	}
	o.seen[key] = now.Add(ttl)
	return true
}

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
