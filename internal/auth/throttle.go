package auth

import (
	"sync"
	"time"
)

// Throttle counts failures per key in a fixed window and blocks a key once it
// reaches the limit. It is in-memory, which is adequate for a single-binary
// deployment; counters reset on restart.
type Throttle struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	now     func() time.Time
	entries map[string]*throttleEntry
	sweepAt time.Time
}

type throttleEntry struct {
	count int
	reset time.Time
}

// NewThrottle allows limit failures per key within window.
func NewThrottle(limit int, window time.Duration, now func() time.Time) *Throttle {
	if now == nil {
		now = time.Now
	}
	return &Throttle{limit: limit, window: window, now: now, entries: map[string]*throttleEntry{}}
}

// Blocked reports whether key has reached its failure limit, and if so how
// long until it is allowed again.
func (t *Throttle) Blocked(key string) (bool, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.sweep(now)
	e, ok := t.entries[key]
	if !ok || !now.Before(e.reset) {
		return false, 0
	}
	if e.count >= t.limit {
		return true, e.reset.Sub(now)
	}
	return false, 0
}

// Fail records a failure for key.
func (t *Throttle) Fail(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	e, ok := t.entries[key]
	if !ok || !now.Before(e.reset) {
		e = &throttleEntry{reset: now.Add(t.window)}
		t.entries[key] = e
	}
	e.count++
}

// Reset clears the failures for key (e.g. after a successful login).
func (t *Throttle) Reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, key)
}

// sweep drops expired entries at most once per window so the map cannot grow
// without bound. Callers must hold t.mu.
func (t *Throttle) sweep(now time.Time) {
	if now.Before(t.sweepAt) {
		return
	}
	for k, e := range t.entries {
		if !now.Before(e.reset) {
			delete(t.entries, k)
		}
	}
	t.sweepAt = now.Add(t.window)
}
