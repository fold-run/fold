// Package bounded provides a size-bounded, TTL-aware key→value map for the
// gateway's per-instance affinity records and its in-process shared-state
// store.
//
// The stores built on it are all keyed by identifiers the gateway does not
// choose and cannot bound — upstream resource URIs, upstream-minted task
// ids, verified principal identities — so an ordinary map is a slow leak a
// busy federation drives on its own. Eviction is safe at every call site:
// each falls back to a path that already handles an unknown key (a resource
// or task probe, a freshly built limiter).
package bounded

import (
	"sync"
	"time"
)

// Map is a bounded key→value map with per-entry expiry.
//
// The bound is generational rather than LRU: when the live generation
// fills, it becomes the previous generation and a fresh one starts. Reads
// check both and promote what they find, so a key in use survives a
// rotation and only keys untouched for a full generation are dropped. Every
// operation is O(1) with no ordering bookkeeping, at a resident cost of at
// most 2×max entries.
type Map[V any] struct {
	mu   sync.Mutex
	max  int
	now  func() time.Time // swapped in tests
	cur  map[string]entry[V]
	prev map[string]entry[V]
}

type entry[V any] struct {
	value   V
	expires time.Time // zero: never expires
}

func (e entry[V]) live(now time.Time) bool {
	return e.expires.IsZero() || now.Before(e.expires)
}

// New returns an empty map holding at most limit entries per generation.
func New[V any](limit int) *Map[V] {
	return &Map[V]{max: limit, now: time.Now, cur: make(map[string]entry[V])}
}

// Load returns the value for key. A hit in the previous generation is
// promoted into the live one, so an in-use key is not dropped by the next
// rotation. An expired entry reads as absent and is dropped.
func (m *Map[V]) Load(key string) (V, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if e, ok := m.cur[key]; ok {
		if e.live(now) {
			return e.value, true
		}
		delete(m.cur, key)
	}
	if e, ok := m.prev[key]; ok {
		delete(m.prev, key)
		if e.live(now) {
			m.cur[key] = e // promote: it is live after all
			return e.value, true
		}
	}
	var zero V
	return zero, false
}

// Store sets key for ttl (ttl <= 0 never expires), rotating generations
// when the live one is full.
func (m *Map[V]) Store(key string, value V, ttl time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.storeLocked(key, value, ttl)
}

// LoadOrStore returns the existing live value for key, or stores and
// returns value when there is none. loaded reports which happened.
func (m *Map[V]) LoadOrStore(key string, value V, ttl time.Duration) (actual V, loaded bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if e, ok := m.cur[key]; ok && e.live(now) {
		return e.value, true
	}
	if e, ok := m.prev[key]; ok {
		delete(m.prev, key)
		if e.live(now) {
			m.cur[key] = e
			return e.value, true
		}
	}
	m.storeLocked(key, value, ttl)
	return value, false
}

// Delete removes key from both generations.
func (m *Map[V]) Delete(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.cur, key)
	delete(m.prev, key)
}

func (m *Map[V]) storeLocked(key string, value V, ttl time.Duration) {
	if _, ok := m.cur[key]; !ok && len(m.cur) >= m.max {
		m.prev, m.cur = m.cur, make(map[string]entry[V])
	}
	delete(m.prev, key) // one home per key, so len(cur) is an honest bound
	e := entry[V]{value: value}
	if ttl > 0 {
		e.expires = m.now().Add(ttl)
	}
	m.cur[key] = e
}

// Len reports the number of resident records across both generations,
// including any not yet reclaimed by a read.
func (m *Map[V]) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.cur) + len(m.prev)
}
