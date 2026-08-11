package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// rotatingFile appends JSON lines and rotates by size, keeping a bounded
// number of previous files.
//
// Rotation renames in place — audit.jsonl becomes audit.jsonl.1, .1 becomes
// .2, and the oldest is deleted — so the live name never changes and a `tail
// -F` or a log shipper watching it keeps working across a rotation.
//
// Hand-rolled rather than pulled in: the requirement is "append lines, do not
// fill the disk", and a dependency whose feature list is compression,
// time-based rotation, and cleanup schedules is a larger surface than the
// problem. Sixty lines that fold's own tests cover is the cheaper trade.
type rotatingFile struct {
	path     string
	maxBytes int64
	maxFiles int

	mu   sync.Mutex
	f    *os.File
	size int64
}

const (
	defaultMaxSizeMb = 100
	defaultMaxFiles  = 5
)

func newRotatingFile(path string, maxSizeMb, maxFiles int) (*rotatingFile, error) {
	if maxSizeMb <= 0 {
		maxSizeMb = defaultMaxSizeMb
	}
	if maxFiles <= 0 {
		maxFiles = defaultMaxFiles
	}
	r := &rotatingFile{
		path:     path,
		maxBytes: int64(maxSizeMb) * 1024 * 1024,
		maxFiles: maxFiles,
	}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

// open creates the parent directory and opens the file for appending.
//
// 0o750 on the directory and 0o600 on the file: audit records name principals
// and the tools they invoked, so world-readable is the wrong default for
// either. An operator who needs a log shipper to read them grants that
// deliberately, by group.
func (r *rotatingFile) open() error {
	if dir := filepath.Dir(r.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("audit file sink: %w", err)
		}
	}
	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("audit file sink: %w", err)
	}
	r.f = f
	// Start from the existing size so a restart does not reset the rotation
	// clock and let a file grow past its bound.
	if st, err := f.Stat(); err == nil {
		r.size = st.Size()
	}
	return nil
}

// writeLine appends one line, rotating first when it would not fit.
func (r *rotatingFile) writeLine(line []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return fmt.Errorf("audit file sink: closed")
	}
	if r.size+int64(len(line))+1 > r.maxBytes && r.size > 0 {
		if err := r.rotate(); err != nil {
			return err
		}
	}
	n, err := r.f.Write(append(line, '\n'))
	r.size += int64(n)
	return err
}

// rotate shifts the numbered files up and reopens the live name. Caller holds
// the mutex.
func (r *rotatingFile) rotate() error {
	if err := r.f.Close(); err != nil {
		return err
	}
	// Delete the oldest, then walk down so each rename lands on a free name.
	_ = os.Remove(fmt.Sprintf("%s.%d", r.path, r.maxFiles))
	for i := r.maxFiles - 1; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", r.path, i)
		to := fmt.Sprintf("%s.%d", r.path, i+1)
		if _, err := os.Stat(from); err == nil {
			_ = os.Rename(from, to)
		}
	}
	if err := os.Rename(r.path, r.path+".1"); err != nil {
		// The live file is gone from under us (someone else moved it). Reopen
		// and keep writing rather than losing every subsequent event.
		_ = r.open()
		return err
	}
	r.size = 0
	return r.open()
}

func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// fileSink writes one JSON event per line to a rotating file.
type fileSink struct {
	rf     *rotatingFile
	report func(outcome string, n int)
}

func (s *fileSink) Emit(e Event) {
	data, err := json.Marshal(e)
	if err != nil {
		s.report(OutcomeDropped, 1)
		return
	}
	if err := s.rf.writeLine(data); err != nil {
		s.report(OutcomeDropped, 1)
		return
	}
	s.report(OutcomeDelivered, 1)
}

func (s *fileSink) Close() error { return s.rf.Close() }
