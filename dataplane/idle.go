package dataplane

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Idle detection & scale-to-zero (DESIGN §7). A workspace is idle only when
// ALL of these hold for the whole idle window:
//
//  1. no model traffic (gateway ledger activity),
//  2. no meaningful CPU inside the sandbox,
//  3. the agent reports not-busy (/busy endpoint contract),
//  4. no web-terminal input (TerminalActivityLog — a human typing; §7's
//     "no WebSocket activity"). Terminal *output* is not a condition: real
//     work shows up as CPU, and a silent watched terminal is
//     indistinguishable from an abandoned tab.
//
// The failure direction is deliberate: any probe error or ambiguity counts
// as ACTIVE. Killing work we misjudged is worse than a container idling.

// ActivityProbes answers "is this sandbox doing something right now".
type ActivityProbes interface {
	CPUActive(ctx context.Context, wsID string) (bool, error)
	AgentBusy(ctx context.Context, wsID string) (bool, error)
}

// IdleMonitor tracks per-workspace activity and hibernates workspaces that
// have been fully idle for IdleAfter. Run Tick on an interval well shorter
// than IdleAfter.
type IdleMonitor struct {
	Router     *Router
	Probes     ActivityProbes
	IdleAfter  time.Duration
	LedgerPath string // gateway audit ledger; "" disables the model-traffic condition
	// TerminalActivityPath is the JSONL file the serve process's terminal
	// proxy appends input events to (TerminalActivityLog); "" disables the
	// terminal condition.
	TerminalActivityPath string

	lastActive map[string]time.Time
	now        func() time.Time // test seam
}

func NewIdleMonitor(router *Router, probes ActivityProbes, idleAfter time.Duration, ledgerPath string) *IdleMonitor {
	return &IdleMonitor{
		Router:     router,
		Probes:     probes,
		IdleAfter:  idleAfter,
		LedgerPath: ledgerPath,
		lastActive: map[string]time.Time{},
		now:        time.Now,
	}
}

// Tick evaluates every running workspace once and hibernates the ones whose
// idle window has fully elapsed. Returns the ids it hibernated.
func (m *IdleMonitor) Tick(ctx context.Context) ([]string, error) {
	statuses, err := m.Router.Runtime.Status(ctx)
	if err != nil {
		return nil, err
	}

	modelActivity := map[string]time.Time{}
	if m.LedgerPath != "" {
		modelActivity, err = LastActivityTimes(m.LedgerPath)
		if err != nil {
			// Unreadable ledger → cannot rule out activity → treat all as
			// active this tick (fail toward keeping things running).
			fmt.Fprintf(os.Stderr, "idle: ledger unreadable, skipping tick: %v\n", err)
			return nil, nil
		}
	}
	termActivity := map[string]time.Time{}
	if m.TerminalActivityPath != "" {
		termActivity, err = LastActivityTimes(m.TerminalActivityPath)
		if err != nil {
			// Same failure direction as the ledger.
			fmt.Fprintf(os.Stderr, "idle: terminal activity unreadable, skipping tick: %v\n", err)
			return nil, nil
		}
	}

	now := m.now()
	var running []WorkspaceStatus
	for _, st := range statuses {
		if st.State != "running" {
			delete(m.lastActive, st.ID)
			continue
		}
		if _, seen := m.lastActive[st.ID]; !seen {
			m.lastActive[st.ID] = now // first sighting starts the clock
		}
		running = append(running, st)
	}

	// Probe workspaces concurrently: CPUActive alone blocks ~1.5-2s per
	// workspace (docker stats samples two intervals), so a sequential sweep
	// makes tick latency scale with workspace count. Probes only read;
	// lastActive is touched only from this goroutine.
	active := make([]bool, len(running))
	var wg sync.WaitGroup
	for i, st := range running {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			active[i] = m.isActive(ctx, id, modelActivity, termActivity)
		}(i, st.ID)
	}
	wg.Wait()

	var hibernated []string
	for i, st := range running {
		if active[i] {
			m.lastActive[st.ID] = now
			continue
		}

		if now.Sub(m.lastActive[st.ID]) >= m.IdleAfter {
			if err := m.Router.Hibernate(ctx, st.ID); err != nil {
				fmt.Fprintf(os.Stderr, "idle: hibernate %s: %v\n", st.ID, err)
				continue
			}
			delete(m.lastActive, st.ID)
			hibernated = append(hibernated, st.ID)
		}
	}
	return hibernated, nil
}

func (m *IdleMonitor) isActive(ctx context.Context, wsID string, modelActivity, termActivity map[string]time.Time) bool {
	// Condition 1: model traffic within the window.
	if t, ok := modelActivity[wsID]; ok && m.now().Sub(t) < m.IdleAfter {
		return true
	}
	// Condition 4: a human typed into the web terminal within the window.
	// Checked before the probes because it is a map lookup, not a docker
	// round trip.
	if t, ok := termActivity[wsID]; ok && m.now().Sub(t) < m.IdleAfter {
		return true
	}
	// Condition 2: CPU. Errors count as active.
	if active, err := m.Probes.CPUActive(ctx, wsID); err != nil || active {
		return true
	}
	// Condition 3: the agent's own word. Errors count as busy.
	if busy, err := m.Probes.AgentBusy(ctx, wsID); err != nil || busy {
		return true
	}
	return false
}

// LastActivityTimes extracts the newest event time per workspace from an
// append-only JSONL file of {workspace, time, …} events — the gateway audit
// ledger and the terminal activity log share this shape. A plain scan is
// correct (and cheap at local-mode sizes; the SIEM-shipper era replaces
// this with a live feed).
func LastActivityTimes(path string) (map[string]time.Time, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]time.Time{}, nil
		}
		return nil, err
	}
	out := map[string]time.Time{}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e struct {
			Workspace string    `json:"workspace"`
			Time      time.Time `json:"time"`
		}
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		if e.Workspace != "" && e.Time.After(out[e.Workspace]) {
			out[e.Workspace] = e.Time
		}
	}
	return out, nil
}

// DockerProbes implements ActivityProbes against the docker CLI.
type DockerProbes struct {
	// CPUFloorPercent below which the sandbox counts as CPU-idle. Default 5.
	CPUFloorPercent float64
}

func (p *DockerProbes) floor() float64 {
	if p.CPUFloorPercent <= 0 {
		return 5.0
	}
	return p.CPUFloorPercent
}

func (p *DockerProbes) CPUActive(ctx context.Context, wsID string) (bool, error) {
	out, err := exec.CommandContext(ctx, "docker", "stats", "--no-stream",
		"--format", "{{.CPUPerc}}", containerName(wsID)).Output()
	if err != nil {
		return true, fmt.Errorf("docker stats %s: %w", wsID, err)
	}
	pct, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(string(out)), "%"), 64)
	if err != nil {
		return true, fmt.Errorf("docker stats %s: unparseable %q", wsID, out)
	}
	return pct >= p.floor(), nil
}

// AgentBusy asks the harness inside the sandbox. The /busy contract (§7):
// 200 with body "true" or "false". Anything else — no endpoint, non-200,
// garbled body — counts as busy.
func (p *DockerProbes) AgentBusy(ctx context.Context, wsID string) (bool, error) {
	out, err := exec.CommandContext(ctx, "docker", "exec", containerName(wsID),
		"curl", "-sf", "--max-time", "3", "http://127.0.0.1:4321/busy").Output()
	if err != nil {
		return true, fmt.Errorf("busy probe %s: %w", wsID, err)
	}
	switch strings.TrimSpace(string(out)) {
	case "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return true, fmt.Errorf("busy probe %s: unexpected body %q", wsID, out)
	}
}

// Watch runs Tick on the interval until the context ends.
func (m *IdleMonitor) Watch(ctx context.Context, interval time.Duration) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if ids, err := m.Tick(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "idle: tick: %v\n", err)
			} else {
				for _, id := range ids {
					fmt.Printf("hibernated %s (idle > %s)\n", id, m.IdleAfter)
				}
			}
		}
	}
}
