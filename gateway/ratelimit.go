package gateway

import (
	"sync"
	"time"
)

// rateLimiter is a per-(workspace, route) sliding-window counter.
//
// The gateway holds the organization's provider key, so the blast radius of
// a leaked session token is "whatever that key can be billed for". Nothing
// else in the design bounds that: tokens are revocable but revocation needs
// somebody to notice first. A ceiling turns an unbounded loss into a rate,
// which is the difference between an incident and a catastrophe.
//
// A window counter (not a token bucket) on purpose: bursts inside the window
// are fine — agents are bursty — what matters is the hourly ceiling that
// falls out of a per-minute cap.
type rateLimiter struct {
	mu      sync.Mutex
	windows map[string]*rateWindow
}

type rateWindow struct {
	start time.Time
	count int
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{windows: make(map[string]*rateWindow)}
}

// allow records one request against key and reports whether it fits under
// limit. A non-positive limit disables the cap.
func (l *rateLimiter) allow(key string, limit int, now time.Time) bool {
	if limit <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	w, ok := l.windows[key]
	if !ok || now.Sub(w.start) >= time.Minute {
		l.windows[key] = &rateWindow{start: now, count: 1}
		l.sweep(now)
		return true
	}
	if w.count >= limit {
		return false
	}
	w.count++
	return true
}

// sweep drops windows that have aged out. Called on window rollover rather
// than on a timer: the map only grows when new workspaces appear, and every
// new window is a chance to clean up after workspaces that are gone. Caller
// holds the lock.
func (l *rateLimiter) sweep(now time.Time) {
	if len(l.windows) < 64 {
		return
	}
	for k, w := range l.windows {
		if now.Sub(w.start) >= 2*time.Minute {
			delete(l.windows, k)
		}
	}
}
