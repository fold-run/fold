package state

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestPeriodKeyAndReset(t *testing.T) {
	// Mid-month, mid-day, mid-hour, so every boundary is a real rollover.
	at := time.Date(2026, time.January, 15, 10, 30, 0, 0, time.UTC)
	cases := []struct {
		period Period
		key    string
		resets time.Time
	}{
		{PeriodHour, "2026-01-15T10", time.Date(2026, time.January, 15, 11, 0, 0, 0, time.UTC)},
		{PeriodDay, "2026-01-15", time.Date(2026, time.January, 16, 0, 0, 0, 0, time.UTC)},
		{PeriodMonth, "2026-01", time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		if got := c.period.key(at); got != c.key {
			t.Errorf("%s key = %q, want %q", c.period, got, c.key)
		}
		if got := c.period.next(at); !got.Equal(c.resets) {
			t.Errorf("%s next = %v, want %v", c.period, got, c.resets)
		}
	}
}

// Month arithmetic is where naive "add 30 days" implementations break.
func TestPeriodMonthRollsAcrossYearAndShortMonths(t *testing.T) {
	cases := []struct{ at, want time.Time }{
		{time.Date(2026, time.December, 31, 23, 59, 0, 0, time.UTC), time.Date(2027, time.January, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2028, time.February, 29, 12, 0, 0, 0, time.UTC), time.Date(2028, time.March, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, time.January, 31, 12, 0, 0, 0, time.UTC), time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		if got := PeriodMonth.next(c.at); !got.Equal(c.want) {
			t.Errorf("next(%v) = %v, want %v", c.at, got, c.want)
		}
	}
}

// A budget accumulates: what is spent stays spent until the window rolls.
// This is the property that distinguishes it from the sliding-window limiter.
func TestBudgetAccumulatesAndRejects(t *testing.T) {
	b := newMemBudget(PeriodDay, 10)
	ctx := context.Background()

	for i := range 10 {
		if r := b.Add(ctx, 1); !r.Allowed {
			t.Fatalf("unit %d rejected, want admitted", i)
		}
	}
	r := b.Add(ctx, 1)
	if r.Allowed {
		t.Fatal("11th unit admitted past a limit of 10")
	}
	if r.Used != 10 || r.Limit != 10 {
		t.Fatalf("used/limit = %d/%d, want 10/10", r.Used, r.Limit)
	}
}

// A rejected Add must consume nothing: an over-budget caller retrying in a
// loop should not inflate recorded consumption.
func TestRejectedAddConsumesNothing(t *testing.T) {
	b := newMemBudget(PeriodDay, 5)
	ctx := context.Background()
	b.Add(ctx, 5)

	for range 10 {
		b.Add(ctx, 1)
	}
	if r := b.Used(ctx); r.Used != 5 {
		t.Fatalf("used = %d after 10 rejected adds, want 5", r.Used)
	}
}

// An Add larger than the whole allowance is rejected rather than partially
// applied — there is no such thing as spending half a call.
func TestOversizedAddIsRejectedWhole(t *testing.T) {
	b := newMemBudget(PeriodDay, 5)
	ctx := context.Background()
	if r := b.Add(ctx, 9); r.Allowed {
		t.Fatal("an add larger than the allowance was admitted")
	}
	if r := b.Used(ctx); r.Used != 0 {
		t.Fatalf("used = %d, want 0 — a rejected oversized add consumed budget", r.Used)
	}
}

// The window resets at the calendar boundary, not on a rolling basis.
func TestBudgetRollsOverAtPeriodBoundary(t *testing.T) {
	now := time.Date(2026, time.March, 10, 23, 59, 0, 0, time.UTC)
	b := &memBudget{period: PeriodDay, limit: 2, now: func() time.Time { return now }}
	ctx := context.Background()

	b.Add(ctx, 2)
	if r := b.Add(ctx, 1); r.Allowed {
		t.Fatal("admitted past the allowance before the boundary")
	}

	now = now.Add(2 * time.Minute) // past midnight UTC
	r := b.Add(ctx, 1)
	if !r.Allowed {
		t.Fatal("rejected after the period boundary — the window did not roll")
	}
	if r.Used != 1 {
		t.Fatalf("used = %d after rollover, want 1 — the counter did not reset", r.Used)
	}
}

// Used must not consume, or reading a dashboard would spend the budget.
func TestUsedDoesNotConsume(t *testing.T) {
	b := newMemBudget(PeriodDay, 3)
	ctx := context.Background()
	b.Add(ctx, 1)
	for range 5 {
		b.Used(ctx)
	}
	if r := b.Used(ctx); r.Used != 1 {
		t.Fatalf("used = %d after five reads, want 1", r.Used)
	}
}

func TestUnlimitedBudget(t *testing.T) {
	b := newMemBudget(PeriodMonth, 0)
	ctx := context.Background()
	for range 100 {
		if r := b.Add(ctx, 1_000_000); !r.Allowed {
			t.Fatal("unlimited budget rejected")
		}
	}
	if r := b.Used(ctx); r.Limit != 0 {
		t.Fatalf("limit = %d, want 0 for an unlimited budget", r.Limit)
	}
}

// Concurrent Adds must never admit past the allowance.
func TestMemBudgetConcurrentAddsRespectLimit(t *testing.T) {
	const limit = 100
	b := newMemBudget(PeriodDay, limit)
	ctx := context.Background()

	var mu sync.Mutex
	admitted := 0
	var wg sync.WaitGroup
	for range 500 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.Add(ctx, 1).Allowed {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if admitted != limit {
		t.Fatalf("admitted %d of 500 against a limit of %d", admitted, limit)
	}
}

// ---- Redis ----

// The point of shared state: two instances must spend one allowance, not one
// each. Without this a fleet of N gateways enforces N times the budget.
func TestRedisBudgetIsSharedAcrossInstances(t *testing.T) {
	a, b := twoProviders(t)
	ctx := context.Background()

	ba := a.Budget("srv", PeriodMonth, 6)
	bb := b.Budget("srv", PeriodMonth, 6)

	admitted := 0
	for i := range 10 {
		bud := ba
		if i%2 == 1 {
			bud = bb
		}
		if bud.Add(ctx, 1).Allowed {
			admitted++
		}
	}
	if admitted != 6 {
		t.Fatalf("admitted %d across two instances, want 6 — the budget is not shared", admitted)
	}
}

// The atomic check-and-increment must hold under concurrency from both
// instances at once, which is the case a Go-side compare-then-increment loses.
func TestRedisBudgetConcurrentAcrossInstances(t *testing.T) {
	a, b := twoProviders(t)
	ctx := context.Background()
	const limit = 50

	ba := a.Budget("race", PeriodMonth, limit)
	bb := b.Budget("race", PeriodMonth, limit)

	var mu sync.Mutex
	admitted := 0
	var wg sync.WaitGroup
	for i := range 300 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			bud := ba
			if i%2 == 1 {
				bud = bb
			}
			if bud.Add(ctx, 1).Allowed {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if admitted != limit {
		t.Fatalf("admitted %d concurrently against a limit of %d", admitted, limit)
	}
}

func TestRedisBudgetRejectedAddConsumesNothing(t *testing.T) {
	a, _ := twoProviders(t)
	ctx := context.Background()
	bud := a.Budget("nores", PeriodMonth, 3)

	bud.Add(ctx, 3)
	for range 10 {
		bud.Add(ctx, 1)
	}
	if r := bud.Used(ctx); r.Used != 3 {
		t.Fatalf("used = %d after rejected adds, want 3", r.Used)
	}
}

// Windows are keyed by the calendar, so a new period is a new key — rollover
// needs no sweeper, and the old key expires on its own.
func TestRedisBudgetWindowIsKeyedByPeriod(t *testing.T) {
	a, _ := twoProviders(t)
	rb, ok := a.Budget("keyed", PeriodDay, 5).(*redisBudget)
	if !ok {
		t.Fatal("expected a redisBudget")
	}
	jan := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	feb := time.Date(2026, time.February, 1, 12, 0, 0, 0, time.UTC)
	if rb.key(jan) == rb.key(feb) {
		t.Fatalf("both windows key to %q — consumption would carry across periods", rb.key(jan))
	}
}

// An unreachable Redis must not refuse all service, but it must say so: a
// fleet running unbudgeted is something an operator has to be able to see.
func TestRedisBudgetFailsOpenAndSaysSo(t *testing.T) {
	mr := miniredis.RunT(t)
	p, err := NewRedis("redis://" + mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	ctx := context.Background()
	bud := p.Budget("outage", PeriodMonth, 5)

	if r := bud.Add(ctx, 1); r.Degraded {
		t.Fatal("healthy Redis reported Degraded")
	}
	mr.Close() // the outage

	r := bud.Add(ctx, 1)
	if !r.Allowed {
		t.Fatal("refused service during a Redis outage — budgets fail open")
	}
	if !r.Degraded {
		t.Fatal("failed open silently; the result must report Degraded")
	}
	if u := bud.Used(ctx); !u.Degraded {
		t.Fatal("Used failed open silently")
	}
}

// Falling back to a per-instance budget still enforces something: an outage
// degrades to per-instance enforcement, not to none.
func TestRedisBudgetFallbackStillEnforces(t *testing.T) {
	mr := miniredis.RunT(t)
	p, err := NewRedis("redis://" + mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	ctx := context.Background()
	bud := p.Budget("fallback", PeriodMonth, 3)
	mr.Close()

	// Kept short: every call to a dead Redis costs the op timeout before it
	// falls back, so this loop is deliberately just long enough to prove the
	// fallback both admits and refuses.
	admitted := 0
	for range 5 {
		if bud.Add(ctx, 1).Allowed {
			admitted++
		}
	}
	if admitted != 3 {
		t.Fatalf("admitted %d during the outage, want 3 — the fallback enforces nothing", admitted)
	}
}

func TestRedisUnlimitedBudget(t *testing.T) {
	a, _ := twoProviders(t)
	ctx := context.Background()
	bud := a.Budget("none", PeriodMonth, 0)
	for range 50 {
		if !bud.Add(ctx, 100).Allowed {
			t.Fatal("unlimited budget rejected")
		}
	}
}
