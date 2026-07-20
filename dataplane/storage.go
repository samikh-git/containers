package dataplane

import "context"

// Storage is the workspace persistence layer (DESIGN §7). The production
// backend is ZFS inside the data plane image; DirStorage is a development
// backend so the full flow runs on hosts without ZFS (e.g. macOS).
//
// All operations are idempotent where the design demands it: EnsureWorkspace
// on an existing workspace succeeds, Snapshot with an existing name fails
// (snapshots are immutable history, silently reusing one would corrupt the
// RPO story), CloneBranch on an existing branch succeeds and returns it.
type Storage interface {
	// EnsureWorkspace creates the workspace's dataset if absent and returns
	// its mount path.
	EnsureWorkspace(ctx context.Context, wsID string, quotaGB int64) (mountPath string, err error)

	// Snapshot records an immutable point-in-time state of the workspace.
	Snapshot(ctx context.Context, wsID, name string) error

	// CloneBranch creates (or returns) a writable copy-on-write branch from
	// a snapshot and returns its mount path.
	CloneBranch(ctx context.Context, wsID, snapshot, branchID string) (mountPath string, err error)

	// ListBranches returns the branch IDs of a workspace.
	ListBranches(ctx context.Context, wsID string) ([]string, error)

	// DestroyBranch removes one branch (and its data).
	DestroyBranch(ctx context.Context, wsID, branchID string) error

	// DestroyWorkspace removes the workspace, its snapshots and branches.
	DestroyWorkspace(ctx context.Context, wsID string) error
}

// WorkspaceExister is the optional Storage capability the warm-start path
// needs: restore-from-checkpoint is only valid for a workspace that does
// not exist yet (its content must exactly match the golden snapshot), so
// the router has to tell "fresh" apart from "resume" before claiming a
// pool slot.
type WorkspaceExister interface {
	WorkspaceExists(ctx context.Context, wsID string) bool
}

// SnapshotCloner is the optional Storage capability behind warm starts: a
// new workspace born as a copy-on-write clone of another workspace's
// snapshot (the warm pool's golden content), rather than as an empty
// dataset. Fails if the destination already exists — adopting a clone into
// an existing workspace would silently discard its data.
type SnapshotCloner interface {
	CloneWorkspaceFrom(ctx context.Context, srcWsID, snapshot, dstWsID string) (mountPath string, err error)
}

// DiskUsage is volume consumption for one workspace (ops metrics).
type DiskUsage struct {
	UsedBytes  uint64
	QuotaBytes uint64 // 0 = unlimited / not enforced / unknown
}

// WorkspaceDiskUsager is the optional Storage capability behind admin disk
// columns. Missing or failing implementations leave Disk* fields nil.
type WorkspaceDiskUsager interface {
	DiskUsage(ctx context.Context, wsID string) (DiskUsage, error)
}
