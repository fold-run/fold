package state

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fold-run/fold/internal/breaker"
	"github.com/fold-run/fold/internal/ratelimit"
)

// redisOpTimeout caps every Redis operation so a stalled Redis cannot stall
// the request path; on timeout or error the primitives fail open.
const redisOpTimeout = 500 * time.Millisecond

// Redis is the shared-state provider backed by a Redis instance; a fleet of
// gateways pointed at the same Redis behaves as one gateway for rate
// limits, circuit breakers, and list caches.
type Redis struct {
	client *redis.Client

	// degraded holds the OnDegraded callback (func(kind string)); atomic
	// because the gateway registers it after limiters may already exist.
	degraded atomic.Value
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
		_ = client.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return &Redis{client: client}, nil
}

// Close implements Provider.
func (r *Redis) Close() error { return r.client.Close() }

// Limiter implements Provider.
func (r *Redis) Limiter(scope string, rpm int) Limiter {
	if rpm <= 0 {
		return memLimiter{l: nil} // unlimited
	}
	return &redisLimiter{
		client:   r.client,
		scope:    scope,
		rpm:      rpm,
		window:   time.Minute,
		fallback: memLimiter{l: ratelimit.New(rpm)},
		note:     r.noteDegraded,
	}
}

// Breaker implements Provider.
func (r *Redis) Breaker(scope string, threshold int, halfOpenAfter time.Duration) Breaker {
	if threshold <= 0 {
		return memBreaker{b: nil}
	}
	return &redisBreaker{
		client:        r.client,
		scope:         scope,
		threshold:     threshold,
		halfOpenAfter: halfOpenAfter,
		fallback:      memBreaker{b: breaker.New(threshold, halfOpenAfter)},
		note:          r.noteDegraded,
	}
}

// OnDegraded registers fn to be told each time a rate-limit or breaker
// decision was made per-instance because Redis was unreachable
// (kind: "limiter" | "breaker"). The gateway wires it into
// fold_state_degraded_total — fail-open is deliberate, but a fleet whose
// limits are momentarily per-instance must be visible, exactly as budgets
// already are. Safe to call before or after limiters exist.
func (r *Redis) OnDegraded(fn func(kind string)) {
	r.degraded.Store(fn)
}

func (r *Redis) noteDegraded(kind string) {
	if fn, ok := r.degraded.Load().(func(string)); ok && fn != nil {
		fn(kind)
	}
}

// ListCache implements Provider.
func (r *Redis) ListCache(scope string) ListCache {
	return &redisCache{client: r.client, scope: scope}
}

// Once implements Provider.
func (r *Redis) Once(scope string) Once {
	return &redisOnce{client: r.client, scope: scope, fallback: NewMemOnce()}
}

// Store implements Provider.
func (r *Redis) Store(scope string) Store {
	return &redisStore{client: r.client, scope: scope, mirror: NewMemStore()}
}

// redisStore shares records fleet-wide. Redis is authoritative — reads go
// there first, so an instance never serves a record another instance has
// since replaced — but every write is mirrored locally, so an outage
// degrades to the per-instance behaviour the memory provider gives rather
// than to no records at all. Fail-open, like every other primitive here:
// the caller treats an unreachable Redis as an absent record and takes
// whatever fallback path it already has for one it has never seen.
type redisStore struct {
	client *redis.Client
	scope  string
	mirror *MemStore
}

func (s *redisStore) key(k string) string { return "fold:store:" + s.scope + ":" + k }

func (s *redisStore) Get(ctx context.Context, key string) ([]byte, bool) {
	rctx, cancel := opCtx(ctx)
	defer cancel()
	data, err := s.client.Get(rctx, s.key(key)).Bytes()
	if err == nil {
		return data, true
	}
	if err == redis.Nil {
		return nil, false // authoritative absence: do not consult the mirror
	}
	return s.mirror.Get(ctx, key)
}

func (s *redisStore) GetMany(ctx context.Context, keys []string) map[string][]byte {
	if len(keys) == 0 {
		return map[string][]byte{}
	}
	full := make([]string, len(keys))
	for i, k := range keys {
		full[i] = s.key(k)
	}
	rctx, cancel := opCtx(ctx)
	defer cancel()
	vals, err := s.client.MGet(rctx, full...).Result()
	if err != nil {
		return s.mirror.GetMany(ctx, keys)
	}
	out := make(map[string][]byte, len(keys))
	for i, v := range vals {
		if str, ok := v.(string); ok {
			out[keys[i]] = []byte(str)
		}
	}
	return out
}

func (s *redisStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) {
	s.mirror.Set(ctx, key, value, ttl)
	rctx, cancel := opCtx(ctx)
	defer cancel()
	s.client.Set(rctx, s.key(key), value, ttl)
}

func (s *redisStore) SetMany(ctx context.Context, entries map[string][]byte, ttl time.Duration) {
	if len(entries) == 0 {
		return
	}
	s.mirror.SetMany(ctx, entries, ttl)
	rctx, cancel := opCtx(ctx)
	defer cancel()
	pipe := s.client.Pipeline()
	for k, v := range entries {
		pipe.Set(rctx, s.key(k), v, ttl)
	}
	_, _ = pipe.Exec(rctx) // fail open: the mirror already holds them
}

func (s *redisStore) Delete(ctx context.Context, key string) {
	s.mirror.Delete(ctx, key)
	rctx, cancel := opCtx(ctx)
	defer cancel()
	s.client.Del(rctx, s.key(key))
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
	fallback memLimiter        // per-instance mirror, decisive during outages
	note     func(kind string) // degraded-decision observer (Redis.noteDegraded)

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
		// Fail open across the fleet, but not to nothing: the local window
		// — fed on every healthy decision below, so it is warm when the
		// outage starts — keeps enforcing this instance's share. Same
		// degrade-to-per-instance posture as budgets, Once, and Store.
		l.note("limiter")
		return l.fallback.Allow(ctx)
	}
	// Keep the local mirror warm; its verdict is ignored while Redis is the
	// authority.
	_, _ = l.fallback.Allow(ctx)
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

	// fallback mirrors every Record (in-memory, effectively free), so when
	// Redis is unreachable this instance's own view of the upstream —
	// already warm — takes over the Allow decision. Its verdict is ignored
	// while Redis answers; note reports each degraded decision.
	fallback memBreaker
	note     func(kind string)
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
		// Redis unreachable: this instance's warm local mirror decides, so
		// an outage degrades to per-instance breaking rather than to a
		// breaker that never opens (see the fallback field).
		b.note("breaker")
		return b.fallback.Allow(ctx)
	}
	if time.Now().UnixMilli() < openUntil {
		return false
	}
	// Half-open: admit one probe fleet-wide.
	ok, err := b.client.SetNX(ctx, b.probeKey(), "1", b.halfOpenAfter).Result()
	if err != nil {
		b.note("breaker")
		return b.fallback.Allow(ctx)
	}
	return ok
}

func (b *redisBreaker) Record(ctx context.Context, success bool) {
	// Mirror every outcome locally first — this is what makes the fallback
	// warm instead of blind when an outage starts.
	b.fallback.Record(ctx, success)
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
		_, _ = pipe.Exec(ctx) // fail open on Redis errors
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

// ---- budget ----

// budgetAddScript is the atomic check-and-increment behind a shared budget.
//
// It must be one atomic unit, not INCRBY-then-compare in Go: two instances
// incrementing concurrently would each see a different total and could both
// admit past the allowance. Doing the comparison inside the script also lets a
// rejected Add leave the counter untouched, so an over-budget caller retrying
// in a loop does not inflate the recorded consumption.
//
// KEYS[1] counter   ARGV[1] units   ARGV[2] limit   ARGV[3] ttl seconds
// Returns {allowed, used}.
var budgetAddScript = redis.NewScript(`
local used = tonumber(redis.call('GET', KEYS[1]) or '0')
local n    = tonumber(ARGV[1])
local limit= tonumber(ARGV[2])
if used + n > limit then
  return {0, used}
end
used = redis.call('INCRBY', KEYS[1], n)
if used == n then
  redis.call('EXPIRE', KEYS[1], ARGV[3])
end
return {1, used}
`)

// redisBudget accumulates consumption fleet-wide against a calendar-aligned
// allowance. The key carries the window, so rollover needs no sweeper: the old
// window's key simply stops being addressed and expires on its own.
//
// Fail-open matches every other primitive here — a Redis blip must not refuse
// all service — but a budget failing open is unbounded in a direction that
// costs money, so the fallback is a per-instance budget rather than no budget,
// and every degraded decision says so in its result.
type redisBudget struct {
	client   *redis.Client
	scope    string
	period   Period
	limit    int64
	fallback Budget
}

func (b *redisBudget) key(now time.Time) string {
	return fmt.Sprintf("fold:budget:%s:%s", b.scope, b.period.key(now))
}

func (b *redisBudget) Add(ctx context.Context, n int64) BudgetResult {
	now := time.Now()
	rctx, cancel := opCtx(ctx)
	defer cancel()

	res, err := budgetAddScript.Run(rctx, b.client,
		[]string{b.key(now)}, n, b.limit, int(b.period.ttl(now).Seconds())).Int64Slice()
	if err != nil || len(res) != 2 {
		return b.degrade(b.fallback.Add(ctx, n))
	}
	return BudgetResult{
		Allowed: res[0] == 1,
		Used:    res[1],
		Limit:   b.limit,
		Resets:  b.period.next(now),
	}
}

func (b *redisBudget) Used(ctx context.Context) BudgetResult {
	now := time.Now()
	rctx, cancel := opCtx(ctx)
	defer cancel()

	used, err := b.client.Get(rctx, b.key(now)).Int64()
	if err != nil && err != redis.Nil {
		return b.degrade(b.fallback.Used(ctx))
	}
	return BudgetResult{
		Allowed: used < b.limit,
		Used:    used,
		Limit:   b.limit,
		Resets:  b.period.next(now),
	}
}

// degrade marks a result as decided per-instance, so the caller can audit and
// alert on a fleet that is running unbudgeted.
func (b *redisBudget) degrade(r BudgetResult) BudgetResult {
	r.Degraded = true
	r.Limit = b.limit
	return r
}

// Budget implements Provider.
func (r *Redis) Budget(scope string, period Period, limit int64) Budget {
	if limit <= 0 {
		return unlimitedBudget{}
	}
	if !period.Valid() {
		period = PeriodMonth
	}
	return &redisBudget{
		client:   r.client,
		scope:    scope,
		period:   period,
		limit:    limit,
		fallback: newMemBudget(period, limit),
	}
}
