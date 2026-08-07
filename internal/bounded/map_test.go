package bounded

import (
	"fmt"
	"testing"
	"time"
)

func TestMapIsBounded(t *testing.T) {
	m := New[int](4)
	for i := range 100 {
		m.Store(fmt.Sprintf("k%d", i), i, 0)
	}
	if n := m.Len(); n > 8 {
		t.Fatalf("map grew to %d entries with max 4", n)
	}
	// The most recent writes survive; a miss is the caller's problem to
	// handle, and every fold call site falls back to a probe.
	if v, ok := m.Load("k99"); !ok || v != 99 {
		t.Fatalf("most recent key was evicted: %v %v", v, ok)
	}
}

func TestMapPromotesOnRead(t *testing.T) {
	m := New[string](4)
	m.Store("hot", "v", 0)
	for i := range 4 { // force one rotation; "hot" lands in prev
		m.Store(fmt.Sprintf("k%d", i), "x", 0)
	}
	if _, ok := m.Load("hot"); !ok {
		t.Fatal("key in the previous generation should still be readable")
	}
	// The read promoted it into the live generation, so the next rotation
	// must not drop it — that is what keeps an in-use key resident.
	for i := range 4 {
		m.Store(fmt.Sprintf("j%d", i), "x", 0)
	}
	if _, ok := m.Load("hot"); !ok {
		t.Fatal("a key read during the last generation was dropped anyway")
	}
}

func TestMapLoadOrStore(t *testing.T) {
	m := New[int](8)
	if v, loaded := m.LoadOrStore("k", 1, 0); loaded || v != 1 {
		t.Fatalf("first LoadOrStore = (%v, %v), want (1, false)", v, loaded)
	}
	if v, loaded := m.LoadOrStore("k", 2, 0); !loaded || v != 1 {
		t.Fatalf("second LoadOrStore = (%v, %v), want (1, true)", v, loaded)
	}
}

func TestMapExpiry(t *testing.T) {
	m := New[string](8)
	now := time.Now()
	m.now = func() time.Time { return now }

	m.Store("ttl", "v", time.Minute)
	m.Store("forever", "v", 0)
	if _, ok := m.Load("ttl"); !ok {
		t.Fatal("entry should be live before its ttl elapses")
	}

	now = now.Add(2 * time.Minute)
	if _, ok := m.Load("ttl"); ok {
		t.Fatal("expired entry must read as absent")
	}
	if _, ok := m.Load("forever"); !ok {
		t.Fatal("a zero ttl must mean no expiry")
	}
	// An expired entry is dropped on read, not merely hidden.
	if n := m.Len(); n != 1 {
		t.Fatalf("expired entry still resident: Len = %d", n)
	}
	// LoadOrStore must replace an expired entry rather than return it.
	if v, loaded := m.LoadOrStore("ttl", "fresh", time.Minute); loaded || v != "fresh" {
		t.Fatalf("LoadOrStore over an expired entry = (%v, %v)", v, loaded)
	}
}

func TestMapDelete(t *testing.T) {
	m := New[int](4)
	m.Store("a", 1, 0)
	for i := range 4 { // push "a" into the previous generation
		m.Store(fmt.Sprintf("k%d", i), i, 0)
	}
	m.Delete("a")
	if _, ok := m.Load("a"); ok {
		t.Fatal("Delete must clear both generations")
	}
}
