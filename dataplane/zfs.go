package dataplane

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// sandboxUID/GID must match the sandbox image's fixed runtime user (the
// "agent" user, uid 1000, created in sandbox-image/Dockerfile). Fresh ZFS
// datasets mount root:root; on a real Linux bind mount (unlike Docker
// Desktop's more permissive host/VM translation) that leaves the sandbox's
// uid-1000 process unable to write /workspace at all. Verified on real
// hardware: the entrypoint's own mkdir of $HOME failed with EACCES before
// this fix existed.
const (
	sandboxUID = 1000
	sandboxGID = 1000
)

// ZFSStorage implements Storage on a ZFS pool — the production backend inside
// the data plane image (DESIGN §7). It shells out to the zfs CLI on purpose:
// that is the battle-tested administration surface, and it keeps us off cgo.
//
// Layout:
//
//	<pool>/workspaces/<wsID>                    workspace dataset → /workspace
//	<pool>/workspaces/<wsID>@<snap>             snapshots
//	<pool>/workspaces/<wsID>/branches/<branch>  clones (agent branches)
type ZFSStorage struct {
	Pool string // e.g. "tank"
}

func (z *ZFSStorage) ds(wsID string) string {
	return fmt.Sprintf("%s/workspaces/%s", z.Pool, wsID)
}

func (z *ZFSStorage) branchDS(wsID, branchID string) string {
	return fmt.Sprintf("%s/branches/%s", z.ds(wsID), branchID)
}

func mountPathOf(dataset string) string { return "/" + dataset }

func zfsRun(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "zfs", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("zfs %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func zfsExists(ctx context.Context, name string) bool {
	return exec.CommandContext(ctx, "zfs", "list", name).Run() == nil
}

func (z *ZFSStorage) EnsureWorkspace(ctx context.Context, wsID string, quotaGB int64) (string, error) {
	ds := z.ds(wsID)
	fresh := !zfsExists(ctx, ds)
	if fresh {
		if err := zfsRun(ctx, "create", "-p", ds); err != nil {
			return "", err
		}
	}
	// Applied on every call so a quota change in desired state converges.
	if quotaGB > 0 {
		if err := zfsRun(ctx, "set", fmt.Sprintf("quota=%dG", quotaGB), ds); err != nil {
			return "", err
		}
	}
	mount := mountPathOf(ds)
	if fresh {
		// A fresh dataset mounts root:root; the sandbox runs as uid 1000
		// (sandbox-image's "agent" user) and needs to own /workspace to
		// write anything at all, including its own $HOME setup.
		if err := os.Chown(mount, sandboxUID, sandboxGID); err != nil {
			return "", fmt.Errorf("chown %s: %w", mount, err)
		}
	}
	return mount, nil
}

func (z *ZFSStorage) Snapshot(ctx context.Context, wsID, name string) error {
	return zfsRun(ctx, "snapshot", z.ds(wsID)+"@"+name)
}

func (z *ZFSStorage) CloneBranch(ctx context.Context, wsID, snapshot, branchID string) (string, error) {
	dst := z.branchDS(wsID, branchID)
	if zfsExists(ctx, dst) {
		return mountPathOf(dst), nil
	}
	// zfs clone has no "-p": unlike EnsureWorkspace's "create -p", the
	// destination's parent dataset must already exist. "branches" is a pure
	// namespace holder — never mounted into a sandbox itself — so create it
	// (idempotently) before every clone rather than tracking whether this
	// is the workspace's first branch.
	if err := zfsRun(ctx, "create", "-p", z.ds(wsID)+"/branches"); err != nil {
		return "", err
	}
	src := z.ds(wsID) + "@" + snapshot
	if err := zfsRun(ctx, "clone", src, dst); err != nil {
		return "", err
	}
	// Clones inherit their snapshot's on-disk ownership, so no chown here —
	// correct as long as the parent workspace was itself chowned above.
	return mountPathOf(dst), nil
}

func (z *ZFSStorage) ListBranches(ctx context.Context, wsID string) ([]string, error) {
	parent := z.ds(wsID) + "/branches"
	out, err := exec.CommandContext(ctx, "zfs", "list", "-H", "-o", "name", "-r", parent).Output()
	if err != nil {
		return nil, nil // no branches dataset yet
	}
	var branches []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" || line == parent {
			continue
		}
		branches = append(branches, filepath.Base(line))
	}
	return branches, nil
}

func (z *ZFSStorage) DestroyBranch(ctx context.Context, wsID, branchID string) error {
	return zfsRun(ctx, "destroy", z.branchDS(wsID, branchID))
}

func (z *ZFSStorage) DestroyWorkspace(ctx context.Context, wsID string) error {
	// -r takes snapshots and branch clones with it.
	return zfsRun(ctx, "destroy", "-r", z.ds(wsID))
}

func (z *ZFSStorage) WorkspaceExists(ctx context.Context, wsID string) bool {
	return zfsExists(ctx, z.ds(wsID))
}

// DiskUsage reports ZFS used bytes and quota (if set) for the workspace
// dataset. Quota "none" yields QuotaBytes=0.
func (z *ZFSStorage) DiskUsage(ctx context.Context, wsID string) (DiskUsage, error) {
	ds := z.ds(wsID)
	out, err := exec.CommandContext(ctx, "zfs", "get", "-H", "-p", "-o", "value", "used,quota", ds).Output()
	if err != nil {
		return DiskUsage{}, fmt.Errorf("zfs get used,quota %s: %w", ds, err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 1 {
		return DiskUsage{}, fmt.Errorf("zfs get used,quota %s: empty", ds)
	}
	used, err := strconv.ParseUint(strings.TrimSpace(lines[0]), 10, 64)
	if err != nil {
		return DiskUsage{}, fmt.Errorf("zfs used %s: %w", lines[0], err)
	}
	var quota uint64
	if len(lines) >= 2 {
		q := strings.TrimSpace(lines[1])
		if q != "" && !strings.EqualFold(q, "none") {
			quota, _ = strconv.ParseUint(q, 10, 64)
		}
	}
	return DiskUsage{UsedBytes: used, QuotaBytes: quota}, nil
}

// CloneWorkspaceFrom births dstWsID as a CoW clone of srcWsID@snapshot —
// the warm-start storage step. The clone inherits mountpoint and the
// uid-1000 ownership of its origin (see EnsureWorkspace's chown), so no
// fixup is needed here. Note the origin dataset cannot be destroyed while
// clones of it live; the warm pool owns that lifecycle.
func (z *ZFSStorage) CloneWorkspaceFrom(ctx context.Context, srcWsID, snapshot, dstWsID string) (string, error) {
	dst := z.ds(dstWsID)
	if zfsExists(ctx, dst) {
		return "", fmt.Errorf("workspace %s already exists — cannot adopt a warm clone", dstWsID)
	}
	// The workspaces/ parent may not exist yet on a fresh pool; create -p
	// is a no-op when it does.
	if err := zfsRun(ctx, "create", "-p", z.Pool+"/workspaces"); err != nil {
		return "", err
	}
	if err := zfsRun(ctx, "clone", z.ds(srcWsID)+"@"+snapshot, dst); err != nil {
		return "", err
	}
	return mountPathOf(dst), nil
}
