// Package dataplane implements the workspace router's core: storage-backed
// workspaces with lease fencing, and sandboxed agent containers on top of
// them (DESIGN.md §5, §7, §8).
package dataplane

import (
	"context"
	"time"
)

// WorkspaceSpec is the desired state for one workspace, as the control plane
// (or, in local mode, the CLI) declares it. Applying a spec is idempotent:
// EnsureWorkspace at the same Generation is always a no-op.
type WorkspaceSpec struct {
	ID         string
	Generation uint64
	Image      string
	CPUs       float64
	MemoryMB   int64
	QuotaGB    int64
	// ProfileDir is the host path of the capability-profile bundle (§10),
	// mounted read-only at /etc/agent-profile. Empty means dev mode: the
	// sandbox entrypoint will refuse to start OpenCode without it, which is
	// the contract working as intended.
	ProfileDir string
	// GatewayURL is injected as OPENCODE_BASE_URL. Never a provider URL,
	// never accompanied by a key (§9).
	GatewayURL string
	// SessionToken is the sandbox's only credential: minted by the router,
	// registered at the gateway, injected as OPENCODE_SESSION_TOKEN.
	SessionToken string
	// Route names the gateway route(s) (model tier, §9) for this workspace.
	// A comma-separated list registers several routes; the gateway then picks
	// among them per request by model, so one workspace can offer models from
	// several providers (e.g. "tier-a-anthropic,tier-b-openrouter").
	Route string
	// Network is the docker network for the sandbox. Empty means "none"
	// (no connectivity at all). For gateway access, create an INTERNAL
	// network (docker network create --internal wsnet) with the gateway
	// container attached — internal networks have no route to the outside
	// world, so the gateway remains the only reachable destination.
	Network string
}

// WorkspaceStatus is actual state, rebuilt from runtime labels — never from
// router memory — so a restarted router reconciles from reality (§5).
// Optional metrics fields are filled by EnrichMetrics for ops UIs; Status()
// implementations leave them nil.
type WorkspaceStatus struct {
	ID         string
	Generation uint64
	State      string // e.g. "running", "exited", "stopped"
	Container  string

	// CPUPercent is the sandbox's recent CPU usage (host percent, not
	// normalized to the container's CFS quota). Nil when unavailable.
	CPUPercent *float64 `json:",omitempty"`
	// MemoryUsedMB / MemoryLimitMB are working-set usage and the cgroup
	// limit. Limit may be nil when the sandbox has no memory cap.
	MemoryUsedMB  *float64 `json:",omitempty"`
	MemoryLimitMB *float64 `json:",omitempty"`
	// DiskUsedMB / DiskQuotaGB are volume consumption (ZFS used / quota, or
	// dir-backend du). Quota nil means unlimited / not enforced.
	DiskUsedMB  *float64 `json:",omitempty"`
	DiskQuotaGB *float64 `json:",omitempty"`
	// LastActivityAt is the newest of model-ledger and terminal-activity
	// events for this workspace, when those logs are configured.
	LastActivityAt *time.Time `json:",omitempty"`
	// AgentBusy is the sandbox /busy probe when the runtime can answer it.
	AgentBusy *bool `json:",omitempty"`
}

// Runtime is the seam of DESIGN §15. v1 ships exactly one real
// implementation (Docker+runsc); NoneRuntime exists for storage-only
// operation and tests.
type Runtime interface {
	EnsureWorkspace(ctx context.Context, spec WorkspaceSpec, mountPath string) error
	StopWorkspace(ctx context.Context, id string) error
	Status(ctx context.Context) ([]WorkspaceStatus, error)
}

// NoneRuntime performs no container operations. Used on hosts where only the
// storage layer is being exercised (e.g. macOS dev outside the Linux VM).
type NoneRuntime struct{}

func (NoneRuntime) EnsureWorkspace(context.Context, WorkspaceSpec, string) error { return nil }
func (NoneRuntime) StopWorkspace(context.Context, string) error                  { return nil }
func (NoneRuntime) Status(context.Context) ([]WorkspaceStatus, error)            { return nil, nil }
