package dataplane

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// DirStorage implements Storage with plain directories — the development
// backend for hosts without ZFS. Snapshots and clones are directory copies;
// on APFS (macOS) and reflink-capable Linux filesystems the copy is
// copy-on-write and near-instant, which keeps the fan-out flow realistic
// even in dev. Quotas are not enforced (accepted dev-mode gap).
//
// Layout under Root:
//
//	<root>/workspaces/<wsID>/data                workspace → /workspace
//	<root>/workspaces/<wsID>/snapshots/<snap>    snapshot copies
//	<root>/workspaces/<wsID>/branches/<branch>   branch copies
type DirStorage struct {
	Root string
}

func (d *DirStorage) wsDir(wsID string) string {
	return filepath.Join(d.Root, "workspaces", wsID)
}

func (d *DirStorage) EnsureWorkspace(_ context.Context, wsID string, _ int64) (string, error) {
	data := filepath.Join(d.wsDir(wsID), "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		return "", err
	}
	return data, nil
}

func (d *DirStorage) Snapshot(ctx context.Context, wsID, name string) error {
	dst := filepath.Join(d.wsDir(wsID), "snapshots", name)
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("snapshot %s@%s already exists", wsID, name)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return cowCopy(ctx, filepath.Join(d.wsDir(wsID), "data"), dst)
}

func (d *DirStorage) CloneBranch(ctx context.Context, wsID, snapshot, branchID string) (string, error) {
	dst := filepath.Join(d.wsDir(wsID), "branches", branchID)
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	src := filepath.Join(d.wsDir(wsID), "snapshots", snapshot)
	if _, err := os.Stat(src); err != nil {
		return "", fmt.Errorf("snapshot %s@%s not found", wsID, snapshot)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	if err := cowCopy(ctx, src, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func (d *DirStorage) ListBranches(_ context.Context, wsID string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(d.wsDir(wsID), "branches"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var branches []string
	for _, e := range entries {
		if e.IsDir() {
			branches = append(branches, e.Name())
		}
	}
	return branches, nil
}

func (d *DirStorage) DestroyBranch(_ context.Context, wsID, branchID string) error {
	return os.RemoveAll(filepath.Join(d.wsDir(wsID), "branches", branchID))
}

func (d *DirStorage) DestroyWorkspace(_ context.Context, wsID string) error {
	return os.RemoveAll(d.wsDir(wsID))
}

func (d *DirStorage) WorkspaceExists(_ context.Context, wsID string) bool {
	_, err := os.Stat(filepath.Join(d.wsDir(wsID), "data"))
	return err == nil
}

// DiskUsage reports approximate bytes under the workspace data directory
// via `du -sk`. Quotas are not enforced on the dir backend (QuotaBytes=0).
func (d *DirStorage) DiskUsage(ctx context.Context, wsID string) (DiskUsage, error) {
	data := filepath.Join(d.wsDir(wsID), "data")
	if _, err := os.Stat(data); err != nil {
		return DiskUsage{}, err
	}
	out, err := exec.CommandContext(ctx, "du", "-sk", data).Output()
	if err != nil {
		return DiskUsage{}, fmt.Errorf("du %s: %w", data, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 1 {
		return DiskUsage{}, fmt.Errorf("du %s: empty", data)
	}
	kb, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return DiskUsage{}, err
	}
	return DiskUsage{UsedBytes: kb * 1024}, nil
}

// CloneWorkspaceFrom is the dev-backend warm-start clone: a CoW directory
// copy of another workspace's snapshot into a brand-new workspace.
func (d *DirStorage) CloneWorkspaceFrom(ctx context.Context, srcWsID, snapshot, dstWsID string) (string, error) {
	dst := filepath.Join(d.wsDir(dstWsID), "data")
	if _, err := os.Stat(dst); err == nil {
		return "", fmt.Errorf("workspace %s already exists — cannot adopt a warm clone", dstWsID)
	}
	src := filepath.Join(d.wsDir(srcWsID), "snapshots", snapshot)
	if _, err := os.Stat(src); err != nil {
		return "", fmt.Errorf("snapshot %s@%s not found", srcWsID, snapshot)
	}
	if err := os.MkdirAll(d.wsDir(dstWsID), 0o755); err != nil {
		return "", err
	}
	if err := cowCopy(ctx, src, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// cowCopy copies a directory tree, using the platform's copy-on-write
// facility where available: clonefile on macOS/APFS (cp -c), reflinks on
// Linux (cp --reflink=auto). Falls back to a regular copy semantics-wise;
// correctness never depends on the CoW fast path.
func cowCopy(ctx context.Context, src, dst string) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		cmd = exec.CommandContext(ctx, "cp", "-Rc", src, dst)
	} else {
		cmd = exec.CommandContext(ctx, "cp", "-R", "--reflink=auto", src, dst)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("copy %s -> %s: %w: %s", src, dst, err, string(out))
	}
	return nil
}
