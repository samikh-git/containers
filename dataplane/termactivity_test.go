package dataplane

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTerminalActivityThrottleAndScan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "terminal-activity.jsonl")
	log := &TerminalActivityLog{Path: path}
	base := time.Now()
	clock := base
	log.now = func() time.Time { return clock }

	// A burst of keystrokes inside the throttle window is one event.
	log.Touch("ws1")
	for range 100 {
		clock = clock.Add(100 * time.Millisecond)
		log.Touch("ws1")
	}
	// Past the throttle window: a second event; other workspaces are
	// throttled independently.
	clock = base.Add(terminalThrottle + time.Second)
	log.Touch("ws1")
	log.Touch("ws2")

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(b), "\n"); got != 3 {
		t.Fatalf("expected 3 events (throttled), got %d:\n%s", got, b)
	}

	// The idle monitor's scanner reads back the newest time per workspace.
	times, err := LastActivityTimes(path)
	if err != nil {
		t.Fatal(err)
	}
	want := base.Add(terminalThrottle + time.Second).UTC()
	if got := times["ws1"]; !got.Equal(want) {
		t.Fatalf("ws1 newest = %v, want %v", got, want)
	}
	if _, ok := times["ws2"]; !ok {
		t.Fatal("ws2 missing from scan")
	}
}

func TestTerminalActivityDisabledIsSafe(t *testing.T) {
	// nil receiver and empty path are both no-ops — the proxy calls Touch
	// unconditionally.
	var nilLog *TerminalActivityLog
	nilLog.Touch("ws1")
	(&TerminalActivityLog{}).Touch("ws1")
}
