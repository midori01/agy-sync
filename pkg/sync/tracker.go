package sync

import (
	"sync"

	"agy-sync/pkg/terminal"
)

// ByteTracker tracks uploaded (pushed) and downloaded (pulled) bytes across a sync session.
type ByteTracker struct {
	mu          sync.Mutex
	BytesPushed int64
	BytesPulled int64
}

// NewByteTracker creates a new initialized ByteTracker.
func NewByteTracker() *ByteTracker {
	return &ByteTracker{}
}

// AddPush adds n bytes to the pushed total.
func (t *ByteTracker) AddPush(n int64) {
	if t == nil || n <= 0 {
		return
	}
	t.mu.Lock()
	t.BytesPushed += n
	t.mu.Unlock()
}

// AddPull adds n bytes to the pulled total.
func (t *ByteTracker) AddPull(n int64) {
	if t == nil || n <= 0 {
		return
	}
	t.mu.Lock()
	t.BytesPulled += n
	t.mu.Unlock()
}

// Total returns total transferred bytes (pushed + pulled).
func (t *ByteTracker) Total() int64 {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.BytesPushed + t.BytesPulled
}

// Summary returns a clean human-readable summary string (e.g. "18.5 MB (↑ 14.2 MB, ↓ 4.3 MB)").
func (t *ByteTracker) Summary() string {
	if t == nil {
		return "0 B"
	}
	t.mu.Lock()
	pushed := t.BytesPushed
	pulled := t.BytesPulled
	tot := pushed + pulled
	t.mu.Unlock()

	if tot == 0 {
		return "0 B"
	}
	return terminal.FormatBytes(tot) + " (↑ " + terminal.FormatBytes(pushed) + ", ↓ " + terminal.FormatBytes(pulled) + ")"
}
