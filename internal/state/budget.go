package state

import (
	"context"
	"sync"
	"time"
)

// A Period is a calendar-aligned budget window.
//
// Calendar alignment is the whole point, and the reason a budget cannot be a
// [Limiter] with a longer window: that limiter is a two-bucket sliding window,
// so an exhausted caller is readmitted gradually as the trailing window
// elapses. A budget must instead reset at a boundary an operator can predict,
// explain to a customer, and reconcile against.
//
// Boundaries are UTC. A local-time month would move under a DST transition and
// differ between instances in different zones, which for a fleet-wide counter
// means two instances disagreeing about which month it is.
type Period string

// The supported budget periods.
const (
	PeriodHour  Period = "hour"
	PeriodDay   Period = "day"
	PeriodMonth Period = "month"
)

// Valid reports whether p is a known period.
func (p Period) Valid() bool {
	switch p {
	case PeriodHour, PeriodDay, PeriodMonth:
		return true
	}
	return false
}

// key returns the identifier of the window containing t. Two instances
// computing this for the same instant must agree, so it is derived from the
// calendar rather than from elapsed time since some arbitrary epoch.
func (p Period) key(t time.Time) string {
	t = t.UTC()
	switch p {
	case PeriodHour:
		return t.Format("2006-01-02T15")
	case PeriodDay:
		return t.Format("2006-01-02")
	default:
		return t.Format("2006-01")
	}
}

// next returns the instant the window containing t resets.
func (p Period) next(t time.Time) time.Time {
	t = t.UTC()
	switch p {
	case PeriodHour:
		return t.Truncate(time.Hour).Add(time.Hour)
	case PeriodDay:
		y, m, d := t.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
	default:
		y, m, _ := t.Date()
		return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
	}
}

// ttl bounds how long a window's record must outlive the window itself. One
// extra period of slack means a clock skewed slightly behind still finds the
// record it wrote.
func (p Period) ttl(now time.Time) time.Duration {
	return p.next(now).Sub(now) + time.Hour
}

// A BudgetResult is the outcome of a budget check.
type BudgetResult struct {
	// Allowed reports whether the consumption fits the allowance.
	Allowed bool
	// Used and Limit describe the window after this check. Limit is 0 for an
	// unlimited budget.
	Used, Limit int64
	// Resets is when the window rolls over. It is not a retry delay: the
	// answer to an exhausted monthly budget is "not until the 1st", and a
	// caller that backs off by this amount would sleep for a fortnight.
	Resets time.Time
	// Degraded reports that shared state was unreachable, so this decision
	// was made per-instance rather than fleet-wide. Budgets fail open — a
	// dependency blip must not refuse all service — but silence would let a
	// fleet run unbudgeted without anyone noticing, so the caller is expected
	// to surface this in audit and metrics.
	Degraded bool
}

// Budget admits consumption against a fixed, calendar-aligned allowance that
// resets at the period boundary.
//
// Unlike [Limiter], which smooths a rate, a Budget accumulates: what is spent
// stays spent until the window rolls over.
type Budget interface {
	// Add records n units of consumption and reports whether they fit. A
	// rejected Add records nothing — an over-budget caller does not dig the
	// hole deeper by retrying.
	Add(ctx context.Context, n int64) BudgetResult
	// Used reports the window's state without consuming any of it. A budget
	// that can only reject is one an operator discovers at 100%; this is what
	// makes 80% visible.
	Used(ctx context.Context) BudgetResult
}

// unlimitedBudget is returned when no allowance is configured. It admits
// everything and reports a zero Limit, which callers read as "no budget".
type unlimitedBudget struct{}

func (unlimitedBudget) Add(context.Context, int64) BudgetResult {
	return BudgetResult{Allowed: true}
}

func (unlimitedBudget) Used(context.Context) BudgetResult {
	return BudgetResult{Allowed: true}
}

// memBudget is the in-process budget: correct for one instance, which for a
// fleet is a per-instance budget rather than a budget. The gateway warns when
// one is configured without shared state.
type memBudget struct {
	period Period
	limit  int64

	mu sync.Mutex
	// windowEnd is when the current window rolls. Tracking the boundary
	// rather than the window's key keeps Add allocation-free: formatting a
	// key string per call would allocate on the request path, and this
	// counter is consulted on every upstream invocation.
	windowEnd time.Time
	used      int64
	now       func() time.Time
}

func newMemBudget(period Period, limit int64) Budget {
	if limit <= 0 {
		return unlimitedBudget{}
	}
	if !period.Valid() {
		period = PeriodMonth
	}
	return &memBudget{period: period, limit: limit, now: time.Now}
}

// roll resets the counter when the calendar window has changed. Caller holds mu.
func (b *memBudget) roll(now time.Time) {
	if !now.Before(b.windowEnd) {
		b.windowEnd, b.used = b.period.next(now), 0
	}
}

func (b *memBudget) Add(_ context.Context, n int64) BudgetResult {
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roll(now)
	if b.used+n > b.limit {
		return BudgetResult{Used: b.used, Limit: b.limit, Resets: b.windowEnd}
	}
	b.used += n
	return BudgetResult{Allowed: true, Used: b.used, Limit: b.limit, Resets: b.windowEnd}
}

func (b *memBudget) Used(context.Context) BudgetResult {
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roll(now)
	return BudgetResult{
		Allowed: b.used < b.limit,
		Used:    b.used,
		Limit:   b.limit,
		Resets:  b.windowEnd,
	}
}

// Budget implements Provider.
func (*Memory) Budget(_ string, period Period, limit int64) Budget {
	return newMemBudget(period, limit)
}
