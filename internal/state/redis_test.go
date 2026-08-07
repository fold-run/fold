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
	for i := range 4 {
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
	for range 100 {
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
	for i := range 3 {
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

// A single-use key admitted on one instance is a replay on every instance.
func TestRedisOnceSharedAcrossInstances(t *testing.T) {
	a, b := twoProviders(t)
	ctx := context.Background()

	oa := a.Once("emajti")
	ob := b.Once("emajti")

	if !oa.TryOnce(ctx, "jti-1", time.Minute) {
		t.Fatal("first use should be admitted")
	}
	if ob.TryOnce(ctx, "jti-1", time.Minute) {
		t.Error("replay on another instance was admitted")
	}
	if !ob.TryOnce(ctx, "jti-2", time.Minute) {
		t.Error("distinct key should be admitted")
	}
}

// A Redis outage falls back to the per-instance recorder: same-instance
// replays stay blocked rather than failing open outright.
func TestRedisOnceOutageFallback(t *testing.T) {
	mr := miniredis.RunT(t)
	r, err := NewRedis("redis://" + mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	o := r.Once("emajti")
	mr.Close() // outage

	ctx := context.Background()
	if !o.TryOnce(ctx, "jti-1", time.Minute) {
		t.Fatal("first use during outage should be admitted via fallback")
	}
	if o.TryOnce(ctx, "jti-1", time.Minute) {
		t.Error("same-instance replay during outage was admitted")
	}
}

// The record store is the ownership index's backing: two instances must
// agree on what it holds.
func TestRedisStoreSharedRecords(t *testing.T) {
	a, b := twoProviders(t)
	ctx := context.Background()

	sa, sb := a.Store("task"), b.Store("task")
	sa.Set(ctx, "t1", []byte(`{"u":"alpha"}`), time.Hour)

	got, ok := sb.Get(ctx, "t1")
	if !ok || string(got) != `{"u":"alpha"}` {
		t.Fatalf("second instance sees %q, %v", got, ok)
	}
	if _, ok := sb.Get(ctx, "absent"); ok {
		t.Error("an unwritten key must read as absent")
	}

	// Scopes are separate namespaces.
	if _, ok := b.Store("other").Get(ctx, "t1"); ok {
		t.Error("record leaked across scopes")
	}

	sb.Delete(ctx, "t1")
	if _, ok := sa.Get(ctx, "t1"); ok {
		t.Error("delete did not propagate")
	}
}

func TestRedisStoreBatch(t *testing.T) {
	a, b := twoProviders(t)
	ctx := context.Background()

	a.Store("task").SetMany(ctx, map[string][]byte{
		"t1": []byte("one"), "t2": []byte("two"), "t3": []byte("three"),
	}, time.Hour)

	got := b.Store("task").GetMany(ctx, []string{"t1", "missing", "t3"})
	if len(got) != 2 || string(got["t1"]) != "one" || string(got["t3"]) != "three" {
		t.Fatalf("GetMany = %v", got)
	}
	if _, present := got["missing"]; present {
		t.Error("GetMany reported an absent key as present")
	}
	if len(b.Store("task").GetMany(ctx, nil)) != 0 {
		t.Error("GetMany over no keys should be empty")
	}
}

func TestRedisStoreExpires(t *testing.T) {
	mr := miniredis.RunT(t)
	p, err := NewRedis("redis://" + mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })

	s := p.Store("task")
	s.Set(context.Background(), "t1", []byte("v"), time.Minute)
	mr.FastForward(2 * time.Minute)
	if _, ok := s.Get(context.Background(), "t1"); ok {
		t.Fatal("record outlived its ttl")
	}
}

// A Redis outage falls back to the locally mirrored records rather than to
// no records at all — the same posture as the single-use recorder.
func TestRedisStoreOutageFallsBackToMirror(t *testing.T) {
	mr := miniredis.RunT(t)
	p, err := NewRedis("redis://" + mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })

	ctx := context.Background()
	s := p.Store("task")
	s.Set(ctx, "t1", []byte("mine"), time.Hour)
	s.SetMany(ctx, map[string][]byte{"t2": []byte("also-mine")}, time.Hour)

	mr.Close() // outage

	if got, ok := s.Get(ctx, "t1"); !ok || string(got) != "mine" {
		t.Errorf("Get during outage = %q, %v; want the mirrored record", got, ok)
	}
	if got := s.GetMany(ctx, []string{"t1", "t2"}); len(got) != 2 {
		t.Errorf("GetMany during outage = %v; want both mirrored records", got)
	}
	// A key this instance never wrote is genuinely unknown, outage or not.
	if _, ok := s.Get(ctx, "elsewhere"); ok {
		t.Error("outage invented a record")
	}
}
