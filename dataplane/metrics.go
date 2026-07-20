package dataplane

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MetricsOptions controls EnrichMetrics. Empty paths skip activity attachment.
type MetricsOptions struct {
	LedgerPath           string
	TerminalActivityPath string
	// SkipBusy skips the /busy probe (useful when callers only want cheap
	// docker stats + disk). Default is to probe running workspaces.
	SkipBusy bool
}

// EnrichMetrics fills optional WorkspaceStatus metric fields in place:
// docker CPU/memory (best-effort), storage disk usage, last activity from
// configured JSONL logs, and the agent /busy probe. Failures leave fields
// nil — list endpoints stay useful when a probe is unavailable.
func EnrichMetrics(ctx context.Context, storage Storage, statuses []WorkspaceStatus, opt MetricsOptions) {
	if len(statuses) == 0 {
		return
	}
	attachActivity(statuses, opt.LedgerPath, opt.TerminalActivityPath)
	attachDisk(ctx, storage, statuses)
	_ = FillDockerStats(ctx, statuses)
	if !opt.SkipBusy {
		attachBusy(ctx, statuses)
	}
}

func attachActivity(statuses []WorkspaceStatus, ledgerPath, termPath string) {
	merged := map[string]time.Time{}
	for _, path := range []string{ledgerPath, termPath} {
		if path == "" {
			continue
		}
		times, err := LastActivityTimes(path)
		if err != nil {
			continue
		}
		for id, t := range times {
			if t.After(merged[id]) {
				merged[id] = t
			}
		}
	}
	for i := range statuses {
		if t, ok := merged[statuses[i].ID]; ok {
			tt := t
			statuses[i].LastActivityAt = &tt
		}
	}
}

func attachDisk(ctx context.Context, storage Storage, statuses []WorkspaceStatus) {
	usager, ok := storage.(WorkspaceDiskUsager)
	if !ok || usager == nil {
		return
	}
	for i := range statuses {
		u, err := usager.DiskUsage(ctx, statuses[i].ID)
		if err != nil {
			continue
		}
		usedMB := float64(u.UsedBytes) / (1024 * 1024)
		statuses[i].DiskUsedMB = &usedMB
		if u.QuotaBytes > 0 {
			q := float64(u.QuotaBytes) / (1024 * 1024 * 1024)
			statuses[i].DiskQuotaGB = &q
		}
	}
}

// FillDockerStats populates CPUPercent and Memory* for running sandboxes via
// one `docker stats --no-stream` call. Safe no-op when docker is absent or
// no containers are running.
func FillDockerStats(ctx context.Context, statuses []WorkspaceStatus) error {
	var names []string
	index := map[string]int{} // container name → status index
	for i, st := range statuses {
		if !strings.EqualFold(st.State, "running") {
			continue
		}
		name := containerName(st.ID)
		names = append(names, name)
		index[name] = i
		// docker ps Names can be "ws-foo" or "/ws-foo"; also index by status Container.
		if st.Container != "" {
			index[strings.TrimPrefix(st.Container, "/")] = i
		}
	}
	if len(names) == 0 {
		return nil
	}
	args := append([]string{"stats", "--no-stream", "--format",
		"{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}"}, names...)
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 3 {
			continue
		}
		name := strings.TrimPrefix(parts[0], "/")
		i, ok := index[name]
		if !ok {
			continue
		}
		if pct, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(parts[1]), "%"), 64); err == nil {
			statuses[i].CPUPercent = &pct
		}
		used, limit, ok := parseDockerMem(parts[2])
		if !ok {
			continue
		}
		statuses[i].MemoryUsedMB = &used
		if limit > 0 {
			statuses[i].MemoryLimitMB = &limit
		}
	}
	return nil
}

// parseDockerMem parses docker's "45.3MiB / 2GiB" MemUsage field into MB.
func parseDockerMem(s string) (usedMB, limitMB float64, ok bool) {
	s = strings.TrimSpace(s)
	left, right, cut := strings.Cut(s, "/")
	if !cut {
		return 0, 0, false
	}
	used, err1 := parseDockerSizeMB(strings.TrimSpace(left))
	limit, err2 := parseDockerSizeMB(strings.TrimSpace(right))
	if err1 != nil {
		return 0, 0, false
	}
	if err2 != nil {
		return used, 0, true
	}
	return used, limit, true
}

func parseDockerSizeMB(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "N/A") {
		return 0, fmt.Errorf("empty size")
	}
	// Units docker stats emits: B, KiB, MiB, GiB, TiB (and sometimes KB/MB/GB).
	units := []struct {
		suffix string
		mult   float64 // to bytes
	}{
		{"TiB", 1024 * 1024 * 1024 * 1024},
		{"GiB", 1024 * 1024 * 1024},
		{"MiB", 1024 * 1024},
		{"KiB", 1024},
		{"TB", 1e12},
		{"GB", 1e9},
		{"MB", 1e6},
		{"KB", 1e3},
		{"B", 1},
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			n, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), 64)
			if err != nil {
				return 0, err
			}
			return n * u.mult / (1024 * 1024), nil
		}
	}
	return 0, fmt.Errorf("unparseable size %q", s)
}

func attachBusy(ctx context.Context, statuses []WorkspaceStatus) {
	probes := &DockerProbes{}
	var wg sync.WaitGroup
	for i := range statuses {
		if !strings.EqualFold(statuses[i].State, "running") {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			busy, err := probes.AgentBusy(ctx, statuses[i].ID)
			if err != nil {
				return // leave nil — unknown, not "busy"
			}
			statuses[i].AgentBusy = &busy
		}(i)
	}
	wg.Wait()
}
