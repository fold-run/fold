package stdiobridge

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// A panic on any bridge goroutine used to end the shim — and every stdio
// server it fronted. rescue swallows it after counting and logging it, the
// gateway's own posture, so the blast radius is one session rather than all.
func TestRescueCountsAndLogs(t *testing.T) {
	var buf bytes.Buffer
	b := &Bridge{log: slog.New(slog.NewTextHandler(&buf, nil))}
	func() {
		defer b.rescue("test")
		panic("gone wrong")
	}()
	if got := b.Stats().Panics; got != 1 {
		t.Fatalf("Panics = %d, want 1", got)
	}
	logs := buf.String()
	if !strings.Contains(logs, "panic recovered") || !strings.Contains(logs, "site=test") || !strings.Contains(logs, "gone wrong") {
		t.Fatalf("panic not logged with site and value:\n%s", logs)
	}
}
