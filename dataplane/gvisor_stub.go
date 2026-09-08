//go:build !linux

package dataplane

import (
	"context"
	"errors"
	"time"
)

// GVisorRuntime and CheckpointPool are linux-only (raw runsc against OCI
// bundles on the data-plane host); these stubs keep non-linux builds of
// cmd/router compiling. Use the docker runtimes on macOS.

type GVisorRuntime struct {
	RunscPath           string
	StateDir            string
	ContainerdAddress   string
	ContainerdNamespace string
	CNIConfDir          string
	CPUFloorPercent     float64
}

var errGVisorLinuxOnly = errors.New("gvisor runtime requires linux (use --runtime runsc|runc on this host)")

func (g *GVisorRuntime) EnsureWorkspace(context.Context, WorkspaceSpec, string) error {
	return errGVisorLinuxOnly
}
func (g *GVisorRuntime) StopWorkspace(context.Context, string) error { return errGVisorLinuxOnly }
func (g *GVisorRuntime) Status(context.Context) ([]WorkspaceStatus, error) {
	return nil, errGVisorLinuxOnly
}
func (g *GVisorRuntime) RestoreWorkspace(context.Context, WorkspaceSpec, string, string) error {
	return errGVisorLinuxOnly
}
func (g *GVisorRuntime) CheckpointWorkspace(context.Context, string, string) error {
	return errGVisorLinuxOnly
}
func (g *GVisorRuntime) HibernateCheckpoint(context.Context, string, string) error {
	return errGVisorLinuxOnly
}
func (g *GVisorRuntime) HibernateImage(string) (string, string, bool) { return "", "", false }
func (g *GVisorRuntime) ClearHibernate(string) error                  { return nil }
func (g *GVisorRuntime) AgentListening(context.Context, string) bool { return false }
func (g *GVisorRuntime) AgentReady(context.Context, string) bool     { return false }
func (g *GVisorRuntime) AgentEndpoint(context.Context, string) (string, error) {
	return "", errGVisorLinuxOnly
}
func (g *GVisorRuntime) TerminalEndpoint(context.Context, string) (string, error) {
	return "", errGVisorLinuxOnly
}
func (g *GVisorRuntime) CPUActive(context.Context, string) (bool, error) {
	return true, errGVisorLinuxOnly
}
func (g *GVisorRuntime) AgentBusy(context.Context, string) (bool, error) {
	return true, errGVisorLinuxOnly
}

type CheckpointPool struct {
	Dir         string
	Storage     Storage
	Runtime     *GVisorRuntime
	Template    WorkspaceSpec
	BootTimeout time.Duration
}

type SlotInfo struct {
	ID        string    `json:"id"`
	Image     string    `json:"image"`
	Network   string    `json:"network"`
	CreatedAt time.Time `json:"created_at"`
}

func (p *CheckpointPool) Fill(context.Context, int) (int, error) { return 0, errGVisorLinuxOnly }
func (p *CheckpointPool) Claim(context.Context, WorkspaceSpec) (*WarmSlot, bool) {
	return nil, false
}
func (p *CheckpointPool) Consume(*WarmSlot)                     {}
func (p *CheckpointPool) Release(context.Context, *WarmSlot)    {}
func (p *CheckpointPool) Status() ([]SlotInfo, error)           { return nil, errGVisorLinuxOnly }
func (p *CheckpointPool) Drain(context.Context) error           { return errGVisorLinuxOnly }
func (p *CheckpointPool) EnableAutoRefill(context.Context, int) {}
func (p *CheckpointPool) StopAutoRefill()                       {}
func (p *CheckpointPool) RequestRefill()                        {}
