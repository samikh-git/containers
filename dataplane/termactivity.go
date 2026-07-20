package dataplane

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Terminal-input activity: the fourth idle condition (DESIGN §7 — "no
// WebSocket activity"). The serve process appends an event whenever a human
// types into a workspace's web terminal; the watch process scans the file
// each tick, exactly like the gateway ledger carries model-traffic activity
// between the same two processes. Output deliberately does not count: real
// work shows up in the CPU condition, and a silent watched terminal is
// indistinguishable from an abandoned tab.

// terminalThrottle caps writes to one event per workspace per interval —
// a fast typist is one line every 30s, not one per keystroke. Coarser than
// any sane idle window, so no activity is ever missed by throttling.
const terminalThrottle = 30 * time.Second

// TerminalActivityLog appends terminal-input events to an append-only JSONL
// file. Safe for concurrent use; write failures are dropped silently because
// the reader fails toward "active" anyway (idle.go) and a terminal must
// never break because bookkeeping did.
type TerminalActivityLog struct {
	Path string

	mu   sync.Mutex
	last map[string]time.Time
	now  func() time.Time // test seam
}

// Touch records input on a workspace's terminal, throttled per workspace.
func (l *TerminalActivityLog) Touch(wsID string) {
	if l == nil || l.Path == "" || wsID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.now == nil {
		l.now = time.Now
	}
	if l.last == nil {
		l.last = map[string]time.Time{}
	}
	now := l.now()
	if now.Sub(l.last[wsID]) < terminalThrottle {
		return
	}
	l.last[wsID] = now

	// Same {workspace,time} shape the ledger scanner reads (idle.go).
	line, err := json.Marshal(struct {
		Workspace string    `json:"workspace"`
		Time      time.Time `json:"time"`
		Type      string    `json:"type"`
	}{wsID, now.UTC(), "terminal"})
	if err != nil {
		return
	}
	f, err := os.OpenFile(l.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(line, '\n'))
}
