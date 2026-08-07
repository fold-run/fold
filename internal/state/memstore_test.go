package state

import (
	"context"
	"testing"
	"time"
)

// The in-process store is what a gateway without Redis runs on, so it must
// honour the same contract the Redis one does.
func TestMemStoreContract(t *testing.T) {
	ctx := context.Background()
	s := NewMemory().Store("task")

	if _, ok := s.Get(ctx, "absent"); ok {
		t.Error("an unwritten key must read as absent")
	}
	s.Set(ctx, "t1", []byte("one"), time.Hour)
	if got, ok := s.Get(ctx, "t1"); !ok || string(got) != "one" {
		t.Fatalf("Get = %q, %v", got, ok)
	}

	s.SetMany(ctx, map[string][]byte{"t2": []byte("two"), "t3": []byte("three")}, time.Hour)
	got := s.GetMany(ctx, []string{"t1", "missing", "t3"})
	if len(got) != 2 || string(got["t1"]) != "one" || string(got["t3"]) != "three" {
		t.Fatalf("GetMany = %v", got)
	}

	s.Delete(ctx, "t1")
	if _, ok := s.Get(ctx, "t1"); ok {
		t.Error("Delete left the record readable")
	}
}

func TestMemStoreExpires(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	s.Set(ctx, "t1", []byte("v"), time.Nanosecond)
	time.Sleep(time.Millisecond)
	if _, ok := s.Get(ctx, "t1"); ok {
		t.Fatal("record outlived its ttl")
	}
}
