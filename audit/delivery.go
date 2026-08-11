package audit

import (
	"encoding/json"
	"math/rand/v2"
	"time"

	"github.com/fold-run/fold/config"
)

// Delivery outcomes reported to the observer. Audit is the single exit door,
// so the interesting number is not how many events were written but how many
// were not: a sink that quietly drops is indistinguishable from a quiet
// system.
const (
	OutcomeDelivered    = "delivered"
	OutcomeRetried      = "retried"       // an attempt failed and will be tried again
	OutcomeDeadLettered = "dead_lettered" // delivery abandoned, written to the dead-letter file
	OutcomeDropped      = "dropped"       // abandoned with nowhere to put it, or the buffer was full
)

// retryPolicy is the resolved form of config.AuditRetry.
type retryPolicy struct {
	maxAttempts int
	initial     time.Duration
	max         time.Duration
}

const (
	defaultMaxAttempts = 4
	defaultInitial     = 500 * time.Millisecond
	defaultMaxBackoff  = 30 * time.Second
)

// resolveRetry fills in the defaults. Retry is on by default rather than
// opt-in: a receiver restarting is the ordinary case, and the old behaviour —
// one attempt, then silence — lost exactly the events an operator would later
// go looking for.
func resolveRetry(c *config.AuditRetry) retryPolicy {
	p := retryPolicy{maxAttempts: defaultMaxAttempts, initial: defaultInitial, max: defaultMaxBackoff}
	if c == nil {
		return p
	}
	if c.MaxAttempts > 0 {
		p.maxAttempts = c.MaxAttempts
	}
	if c.InitialBackoffMs > 0 {
		p.initial = time.Duration(c.InitialBackoffMs) * time.Millisecond
	}
	if c.MaxBackoffMs > 0 {
		p.max = time.Duration(c.MaxBackoffMs) * time.Millisecond
	}
	if p.initial > p.max {
		p.initial = p.max
	}
	return p
}

// backoff returns the delay before attempt n (1-based), doubling from the
// initial delay and capped, with full jitter.
//
// Jitter is not decoration: every gateway instance in a fleet watches the same
// receiver, so they all fail at the same instant and would otherwise retry in
// lockstep — turning a receiver's brief stumble into a synchronised stampede
// against it just as it comes back.
func (p retryPolicy) backoff(attempt int) time.Duration {
	d := p.initial
	for range attempt - 1 {
		d *= 2
		if d >= p.max {
			d = p.max
			break
		}
	}
	if d <= 0 {
		return 0
	}
	// Equal jitter: half the delay, plus a random half. It spreads a fleet's
	// retries without ever exceeding the configured cap — full jitter around
	// the delay would overshoot it by half, which is a cap that does not cap.
	half := int64(d) / 2
	//nolint:gosec // G404: spreading retries in time, not generating a secret
	return time.Duration(half + rand.Int64N(half+1))
}

// deadLetter appends abandoned events to a file, so "we gave up" leaves
// something an operator can replay rather than a gap.
type deadLetter struct {
	rf     *rotatingFile
	report func(outcome string, n int)
}

func newDeadLetter(path string, report func(string, int)) (*deadLetter, error) {
	// Bounded like any other file fold writes: a dead-letter file that fills
	// the disk takes the gateway down, which is a worse outcome than the
	// delivery failure that started it.
	rf, err := newRotatingFile(path, defaultMaxSizeMb, defaultMaxFiles)
	if err != nil {
		return nil, err
	}
	return &deadLetter{rf: rf, report: report}, nil
}

func (d *deadLetter) write(batch []Event) {
	if d == nil {
		return
	}
	for _, e := range batch {
		data, err := json.Marshal(e)
		if err != nil {
			d.report(OutcomeDropped, 1)
			continue
		}
		if err := d.rf.writeLine(data); err != nil {
			d.report(OutcomeDropped, 1)
			continue
		}
		d.report(OutcomeDeadLettered, 1)
	}
}

func (d *deadLetter) Close() error {
	if d == nil {
		return nil
	}
	return d.rf.Close()
}
