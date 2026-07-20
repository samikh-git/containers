package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeProbes struct {
	cpuActive map[string]bool
	busy      map[string]bool
	cpuErr    error
}

func (f *fakeProbes) CPUActive(_ context.Context, wsID string) (bool, error) {
	return f.cpuActive[wsID], f.cpuErr
}
func (f *fakeProbes) AgentBusy(_ context.Context, wsID string) (bool, error) {
	return f.busy[wsID], nil
}

func newIdleFixture(t *testing.T) (*IdleMonitor, *fakeProbes, *fakeRuntime, *Router) {
	t.Helper()
	router, rt, _ := newTestRouter(t)
	probes := &fakeProbes{cpuActive: map[string]bool{}, busy: map[string]bool{}}
	m := NewIdleMonitor(router, probes, 10*time.Minute, "")
	return m, probes, rt, router
}

// advance moves the monitor's clock.
func advance(m *IdleMonitor, d time.Duration) {
	base := time.Now()
	m.now = func() time.Time { return base.Add(d) }
}

func TestIdleWorkspaceHibernatesAfterWindow(t *testing.T) {
	m, _, rt, router := newIdleFixture(t)
	ctx := context.Background()
	if _, err := router.Up(ctx, WorkspaceSpec{ID: "ws1", Image: "img"}); err != nil {
		t.Fatal(err)
	}

	// First tick starts the clock; nothing hibernates.
	ids, err := m.Tick(ctx)
	if err != nil || len(ids) != 0 {
		t.Fatalf("first tick: ids=%v err=%v", ids, err)
	}

	// Still idle after the window: hibernated.
	advance(m, 11*time.Minute)
	ids, err = m.Tick(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "ws1" {
		t.Fatalf("expected ws1 hibernated, got %v", ids)
	}
	if _, running := rt.running["ws1"]; running {
		t.Fatal("ws1 still running after hibernate")
	}
	// Hibernate snapshot exists.
	snaps, err := os.ReadDir(filepath.Join(router.Storage.(*DirStorage).wsDir("ws1"), "snapshots"))
	if err != nil || len(snaps) == 0 {
		t.Fatalf("no hibernate snapshot: %v", err)
	}
}

func TestAnyActivityConditionKeepsWorkspaceAlive(t *testing.T) {
	cases := []struct {
		name string
		set  func(m *IdleMonitor, p *fakeProbes, ledgerDir string)
	}{
		{"cpu active", func(_ *IdleMonitor, p *fakeProbes, _ string) {
			p.cpuActive["ws1"] = true
		}},
		{"agent busy", func(_ *IdleMonitor, p *fakeProbes, _ string) {
			p.busy["ws1"] = true
		}},
		{"probe error counts as active", func(_ *IdleMonitor, p *fakeProbes, _ string) {
			p.cpuErr = errors.New("stats unavailable")
		}},
		{"recent model traffic", func(m *IdleMonitor, _ *fakeProbes, dir string) {
			// Event timestamped beyond both tick clocks, so it stays
			// "recent" for the whole test regardless of clock advances.
			path := filepath.Join(dir, "audit.jsonl")
			e := map[string]any{"workspace": "ws1", "time": time.Now().Add(20 * time.Minute)}
			b, _ := json.Marshal(e)
			os.WriteFile(path, append(b, '\n'), 0o600)
			m.LedgerPath = path
		}},
		{"recent terminal input", func(m *IdleMonitor, _ *fakeProbes, dir string) {
			// Written through the real log — the same producer the serve
			// process uses — with its clock pinned past both tick clocks.
			path := filepath.Join(dir, "terminal-activity.jsonl")
			log := &TerminalActivityLog{Path: path}
			log.now = func() time.Time { return time.Now().Add(20 * time.Minute) }
			log.Touch("ws1")
			m.TerminalActivityPath = path
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, p, rt, router := newIdleFixture(t)
			ctx := context.Background()
			if _, err := router.Up(ctx, WorkspaceSpec{ID: "ws1", Image: "img"}); err != nil {
				t.Fatal(err)
			}
			tc.set(m, p, t.TempDir())

			if _, err := m.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			// Conditions are re-probed each tick; since they still hold
			// past the window, the workspace must survive.
			advance(m, 11*time.Minute)
			ids, err := m.Tick(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(ids) != 0 {
				t.Fatalf("workspace hibernated despite activity (%s)", tc.name)
			}
			if _, running := rt.running["ws1"]; !running {
				t.Fatalf("workspace stopped despite activity (%s)", tc.name)
			}
		})
	}
}

func TestActivityResetsIdleClock(t *testing.T) {
	m, p, rt, router := newIdleFixture(t)
	ctx := context.Background()
	if _, err := router.Up(ctx, WorkspaceSpec{ID: "ws1", Image: "img"}); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Tick(ctx); err != nil { // clock starts
		t.Fatal(err)
	}

	// Activity at +6min resets the clock.
	advance(m, 6*time.Minute)
	p.cpuActive["ws1"] = true
	if _, err := m.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	p.cpuActive["ws1"] = false

	// +14min from start is only 8min after the reset: must survive.
	advance(m, 14*time.Minute)
	ids, err := m.Tick(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatal("idle clock was not reset by activity")
	}
	if _, running := rt.running["ws1"]; !running {
		t.Fatal("workspace stopped too early")
	}

	// +17min from start (11min after reset): hibernates.
	advance(m, 17*time.Minute)
	ids, err = m.Tick(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("expected hibernate after full idle window post-reset, got %v", ids)
	}
}
