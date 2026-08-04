package state

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fold-run/fold/internal/breaker"
)

// redisOpTimeout caps every Redis operation so a stalled Redis cannot stall
// the request path; on timeout or error the primitives fail open.
const redisOpTimeout = 500 * time.Millisecond

// Redis is the shared-state provider backed by a Redis instance; a fleet of
// gateways pointed at the same Redis behaves as one gateway for rate
// limits, circuit breakers, and list caches.
type Redis struct {
	client *redis.Client
}

// NewRedis connects and validates a Redis provider from a redis:// URL.
func NewRedis(url string) (*Redis, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("redis url: %w", err)
	}
	client := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return &Redis{client: client}, nil
}

func (r *Redis) Close() error { return r.client.Close() }

func (r *Redis) Limiter(scope string, rpm int) Limiter {
	if rpm <= 0 {
		return memLimiter{l: nil} // unlimited
	}
	return &redisLimiter{client: r.client, scope: scope, rpm: rpm, window: time.Minute}
}

func (r *Redis) Breaker(scope string, threshold int, halfOpenAfter time.Duration) Breaker {
	if threshold <= 0 {
		return memBreaker{b: nil}
	}
	return &redisBreaker{client: r.client, scope: scope, threshold: threshold, halfOpenAfter: halfOpenAfter}
}

func (r *Redis) ListCache(scope string) ListCache {
	return &redisCache{client: r.client, scope: scope}
}

func (r *Redis) Once(scope string) Once {
	return &redisOnce{client: r.client, scope: scope, fallback: NewMemOnce()}
}

// redisOnce records single-use keys fleet-wide with SET NX. A Redis outage
// falls back to the per-instance recorder rather than failing open outright:
// replays on the same instance are still caught, which keeps the common
// replay (immediate re-redemption) blocked even while Redis is down.
type redisOnce struct {
	client   *redis.Client
	scope    string
	fallback *MemOnce
}

func (o *redisOnce) TryOnce(ctx context.Context, key string, ttl time.Duration) bool {
	rctx, cancel := opCtx(ctx)
	defer cancel()
	ok, err := o.client.SetNX(rctx, fmt.Sprintf("fold:once:%s:%s", o.scope, key), "1", ttl).Result()
	if err != nil {
		return o.fallback.TryOnce(ctx, key, ttl)
	}
	return ok
}

func opCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), redisOpTimeout)
}

// ---- rate limiter ----

// redisLimiter is a two-bucket sliding window over shared counters:
// INCR the current fixed window, weight in the previous one by overlap.
type redisLimiter struct {
	client *redis.Client
	scope  string
	rpm    int
	window time.Duration
}

func (l *redisLimiter) key(idx int64) string {
	return fmt.Sprintf("fold:rl:%s:%d", l.scope, idx)
}

func (l *redisLimiter) Allow(ctx context.Context) (bool, time.Duration) {
	ctx, cancel := opCtx(ctx)
	defer cancel()

	now := time.Now()
	idx := now.UnixNano() / int64(l.window)
	elapsed := float64(now.UnixNano()%int64(l.window)) / float64(l.window)

	pipe := l.client.Pipeline()
	prevCmd := pipe.Get(ctx, l.key(idx-1))
	curCmd := pipe.Incr(ctx, l.key(idx))
	pipe.Expire(ctx, l.key(idx), 2*l.window)
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return true, 0 // fail open
	}
	prev, _ := strconv.ParseFloat(prevCmd.Val(), 64)
	cur := float64(curCmd.Val())
	effective := prev*(1-elapsed) + cur
	if effective > float64(l.rpm) {
		return false, time.Duration(float64(l.window) * (1 - elapsed))
	}
	return true, 0
}

// ---- circuit breaker ----

// redisBreaker shares consecutive-failure state: any instance's failures
// count toward opening the circuit, and one probe fleet-wide is admitted
// after the half-open interval (coordinated with SET NX).
type redisBreaker struct {
	client        *redis.Client
	scope         string
	threshold     int
	halfOpenAfter time.Duration
}

func (b *redisBreaker) failKey() string  { return "fold:cb:" + b.scope + ":failures" }
func (b *redisBreaker) openKey() string  { return "fold:cb:" + b.scope + ":openUntil" }
func (b *redisBreaker) probeKey() string { return "fold:cb:" + b.scope + ":probe" }

func (b *redisBreaker) Allow(ctx context.Context) bool {
	ctx, cancel := opCtx(ctx)
	defer cancel()

	openUntil, err := b.client.Get(ctx, b.openKey()).Int64()
	if err == redis.Nil {
		return true // closed
	}
	if err != nil {
		return true // fail open
	}
	if time.Now().UnixMilli() < openUntil {
		return false
	}
	// Half-open: admit one probe fleet-wide.
	ok, err := b.client.SetNX(ctx, b.probeKey(), "1", b.halfOpenAfter).Result()
	if err != nil {
		return true
	}
	return ok
}

func (b *redisBreaker) Record(ctx context.Context, success bool) {
	ctx, cancel := opCtx(ctx)
	defer cancel()

	if success {
		b.client.Del(ctx, b.failKey(), b.openKey(), b.probeKey())
		return
	}
	pipe := b.client.Pipeline()
	failsCmd := pipe.Incr(ctx, b.failKey())
	pipe.Expire(ctx, b.failKey(), time.Hour)
	if _, err := pipe.Exec(ctx); err != nil {
		return
	}
	if int(failsCmd.Val()) >= b.threshold {
		until := time.Now().Add(b.halfOpenAfter).UnixMilli()
		pipe := b.client.Pipeline()
		pipe.Set(ctx, b.openKey(), until, b.halfOpenAfter+time.Hour)
		pipe.Del(ctx, b.probeKey())
		pipe.Exec(ctx)
	}
}

func (b *redisBreaker) State(ctx context.Context) breaker.State {
	ctx, cancel := opCtx(ctx)
	defer cancel()

	openUntil, err := b.client.Get(ctx, b.openKey()).Int64()
	if err != nil {
		return breaker.Closed
	}
	if time.Now().UnixMilli() < openUntil {
		return breaker.Open
	}
	return breaker.HalfOpen
}

// ---- list cache ----

// redisCache shares serialized list results. Invalidation is O(1): each key
// family has a generation counter baked into the data keys, so bumping the
// generation orphans every entry in the family at once.
type redisCache struct {
	client *redis.Client
	scope  string
}

// family groups cache keys for invalidation: the segment before the first
// "/" ("resources/templates" invalidates with "resources").
func family(key string) string {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i]
		}
	}
	return key
}

func (c *redisCache) genKey(fam string) string {
	return fmt.Sprintf("fold:cache:%s:gen:%s", c.scope, fam)
}

func (c *redisCache) dataKey(fam string, gen int64, key string) string {
	return fmt.Sprintf("fold:cache:%s:%s:%d:%s", c.scope, fam, gen, key)
}

func (c *redisCache) GetOrFill(ctx context.Context, key string, ttl time.Duration, fill func(context.Context) ([]byte, error)) ([]byte, error) {
	if ttl <= 0 {
		return fill(ctx)
	}
	fam := family(key)

	rctx, cancel := opCtx(ctx)
	gen, err := c.client.Get(rctx, c.genKey(fam)).Int64()
	if err != nil && err != redis.Nil {
		cancel()
		return fill(ctx) // fail open
	}
	data, err := c.client.Get(rctx, c.dataKey(fam, gen, key)).Bytes()
	cancel()
	if err == nil {
		return data, nil
	}

	data, ferr := fill(ctx)
	if ferr != nil {
		return nil, ferr
	}
	rctx, cancel = opCtx(ctx)
	c.client.Set(rctx, c.dataKey(fam, gen, key), data, ttl)
	cancel()
	return data, nil
}

func (c *redisCache) Invalidate(ctx context.Context, prefix string) {
	ctx, cancel := opCtx(ctx)
	defer cancel()
	c.client.Incr(ctx, c.genKey(family(prefix)))
}
