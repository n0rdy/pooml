package services

import (
	"context"
	"sync"
	"time"
)

const (
	throttlingWindowMs   = 60 * 1000      // sliding window for counting failures
	throttlingMaxFails   = 5              // failures within window that trigger a lockout
	throttlingLockoutMs  = 60 * 1000      // lockout duration after threshold reached
	throttlingSweepMs    = 5 * 60 * 1000  // background cleanup interval
	throttlingStaleMs    = 10 * 60 * 1000 // entries idle this long are removed
	throttlingMaxEntries = 10000          // hard cap on tracked IPs; oldest evicted on overflow
)

type throttlingEntry struct {
	failures    []int64
	lockedUntil int64
	lastSeenMs  int64
}

// ThrottlingService tracks failed-auth attempts per IP and locks out IPs
// that exceed the threshold within a sliding window. In-memory only:
// process restart wipes all state.
type ThrottlingService struct {
	entries map[string]*throttlingEntry
	mu      sync.Mutex
}

func NewThrottlingService() *ThrottlingService {
	return &ThrottlingService{
		entries: make(map[string]*throttlingEntry),
	}
}

// RunSweeper drops idle entries until ctx is cancelled. Blocks.
func (ts *ThrottlingService) RunSweeper(ctx context.Context) {
	ticker := time.NewTicker(throttlingSweepMs * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case now := <-ticker.C:
			ts.sweep(now.UnixMilli())
		case <-ctx.Done():
			return
		}
	}
}

// Locked-out IPs survive the sweep regardless of idleness: evicting one early
// would hand the attacker a fresh failure budget before the lockout expires.
func (ts *ThrottlingService) sweep(nowMs int64) {
	cutoff := nowMs - throttlingStaleMs

	ts.mu.Lock()
	defer ts.mu.Unlock()

	for ip, e := range ts.entries {
		if e.lastSeenMs < cutoff && nowMs >= e.lockedUntil {
			delete(ts.entries, ip)
		}
	}
}

func (ts *ThrottlingService) IsLocked(ip string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	e, ok := ts.entries[ip]
	if !ok {
		return false
	}
	return time.Now().UnixMilli() < e.lockedUntil
}

func (ts *ThrottlingService) RecordFailure(ip string) {
	now := time.Now().UnixMilli()
	cutoff := now - throttlingWindowMs

	ts.mu.Lock()
	defer ts.mu.Unlock()

	e, ok := ts.entries[ip]
	if !ok {
		if len(ts.entries) >= throttlingMaxEntries {
			ts.evictOldestEntryLocked()
		}
		e = &throttlingEntry{}
		ts.entries[ip] = e
	}

	pruned := e.failures[:0]
	for _, t := range e.failures {
		if t > cutoff {
			pruned = append(pruned, t)
		}
	}
	e.failures = append(pruned, now)
	e.lastSeenMs = now

	if len(e.failures) >= throttlingMaxFails {
		e.lockedUntil = now + throttlingLockoutMs
		// Clear failures so the next window starts with a fresh budget once the
		// lockout expires; otherwise old timestamps inside the sliding window
		// would re-trigger a lock on the very first post-lockout failure.
		e.failures = e.failures[:0]
	}
}

// evictOldestEntryLocked removes the entry with the smallest lastSeenMs.
// Used as a memory-DoS guard when the entry map hits its hard cap. Caller
// must hold ts.mu.
func (ts *ThrottlingService) evictOldestEntryLocked() {
	var oldestIP string
	var oldestMs int64
	first := true
	for ip, e := range ts.entries {
		if first || e.lastSeenMs < oldestMs {
			oldestMs = e.lastSeenMs
			oldestIP = ip
			first = false
		}
	}
	delete(ts.entries, oldestIP)
}
