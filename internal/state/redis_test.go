package state

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/fold-run/fold/internal/breaker"
)

// twoProviders simulates two gateway instances sharing one Redis.
func twoProviders(t *testing.T) (*Redis, *Redis) {
	t.Helper()
	mr := miniredis.RunT(t)
	a, err := NewRedis("redis://" + mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	b, err := NewRedis("redis://" + mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return a, b
}

func TestRedisLimiterSharedBudget(t *testing.T) {
	a, b := twoProviders(t)
	ctx := context.Background()

	la := a.Limiter("up:x", 4)
	lb := b.Limiter("up:x", 4)

	admitted := 0
	for i := 0; i < 4; i++ {
		// Alternate instances: the budget must be shared, not per instance.
		l := la
		if i%2 == 1 {
			l = lb
		}
		if ok, _ := l.Allow(ctx); ok {
			admitted++
		}
	}
	if admitted != 4 {
		t.Fatalf("admitted %d of the shared budget of 4", admitted)
	}
	if ok, retry := lb.Allow(ctx); ok {
		t.Fatal("5th request should exceed the shared budget")
	} else if retry <= 0 || retry > time.Minute {
		t.Errorf("retryAfter = %v", retry)
	}
}

func TestRedisLimiterUnlimited(t *testing.T) {
	a, _ := twoProviders(t)
	l := a.Limiter("up:x", 0)
	for i := 0; i < 100; i++ {
		if ok, _ := l.Allow(context.Background()); !ok {
			t.Fatal("rpm<=0 must be unlimited")
		}
	}
}

func TestRedisBreakerSharedState(t *testing.T) {
	a, b := twoProviders(t)
	ctx := context.Background()

	ba := a.Breaker("up:x", 3, 30*time.Second)
	bb := b.Breaker("up:x", 3, 30*time.Second)

	// Failures observed by instance A open the circuit for instance B.
	for i := 0; i < 3; i++ {
		if !ba.Allow(ctx) {
			t.Fatalf("closed breaker should allow (i=%d)", i)
		}
		ba.Record(ctx, false)
	}
	if bb.Allow(ctx) {
		t.Fatal("instance B should see the open circuit")
	}
	if got := bb.State(ctx); got != breaker.Open {
		t.Fatalf("state = %v, want open", got)
	}

	// A success anywhere closes it everywhere.
	ba.Record(ctx, true)
	if !bb.Allow(ctx) || bb.State(ctx) != breaker.Closed {
		t.Fatal("success should close the circuit fleet-wide")
	}
}

func TestRedisBreakerSingleProbe(t *testing.T) {
	mr := miniredis.RunT(t)
	p, err := NewRedis("redis://" + mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	ctx := context.Background()

	br := p.Breaker("up:x", 1, 50*time.Millisecond)
	br.Record(ctx, false) // opens
	if br.Allow(ctx) {
		t.Fatal("open circuit must reject")
	}

	// After the half-open interval, exactly one probe is admitted.
	time.Sleep(60 * time.Millisecond)
	if got := br.State(ctx); got != breaker.HalfOpen {
		t.Fatalf("state = %v, want half-open", got)
	}
	if !br.Allow(ctx) {
		t.Fatal("half-open should admit one probe")
	}
	if br.Allow(ctx) {
		t.Fatal("only one probe fleet-wide")
	}
	br.Record(ctx, true)
	if !br.Allow(ctx) {
		t.Fatal("successful probe should close the circuit")
	}
}

func TestRedisCacheSharedAndInvalidated(t *testing.T) {
	a, b := twoProviders(t)
	ctx := context.Background()

	ca := a.ListCache("up:x")
	cb := b.ListCache("up:x")

	fills := 0
	fill := func(context.Context) ([]byte, error) {
		fills++
		return []byte(`["v1"]`), nil
	}

	// Instance A fills; instance B hits the shared entry.
	if v, err := ca.GetOrFill(ctx, "tools", time.Minute, fill); err != nil || string(v) != `["v1"]` {
		t.Fatalf("fill: %v %s", err, v)
	}
	if v, err := cb.GetOrFill(ctx, "tools", time.Minute, fill); err != nil || string(v) != `["v1"]` {
		t.Fatalf("shared hit: %v %s", err, v)
	}
	if fills != 1 {
		t.Fatalf("fills = %d, want 1 (cache shared across instances)", fills)
	}

	// Invalidation on A orphans the entry for B too, and family matching
	// covers subkeys ("resources" invalidates "resources/templates").
	ca.Invalidate(ctx, "tools")
	if _, err := cb.GetOrFill(ctx, "tools", time.Minute, fill); err != nil {
		t.Fatal(err)
	}
	if fills != 2 {
		t.Fatalf("fills = %d, want 2 after invalidation", fills)
	}

	if _, err := ca.GetOrFill(ctx, "resources/templates", time.Minute, fill); err != nil {
		t.Fatal(err)
	}
	ca.Invalidate(ctx, "resources")
	if _, err := cb.GetOrFill(ctx, "resources/templates", time.Minute, fill); err != nil {
		t.Fatal(err)
	}
	if fills != 4 {
		t.Fatalf("fills = %d, want 4 (family invalidation)", fills)
	}
}

func TestRedisFailOpen(t *testing.T) {
	mr := miniredis.RunT(t)
	p, err := NewRedis("redis://" + mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	ctx := context.Background()

	l := p.Limiter("up:x", 1)
	br := p.Breaker("up:x", 1, time.Second)
	c := p.ListCache("up:x")

	mr.Close() // Redis goes away

	if ok, _ := l.Allow(ctx); !ok {
		t.Error("limiter must fail open when Redis is down")
	}
	if !br.Allow(ctx) {
		t.Error("breaker must fail open when Redis is down")
	}
	v, err := c.GetOrFill(ctx, "tools", time.Minute, func(context.Context) ([]byte, error) {
		return []byte("fresh"), nil
	})
	if err != nil || string(v) != "fresh" {
		t.Errorf("cache must fall through to fill when Redis is down: %v %s", err, v)
	}
}
