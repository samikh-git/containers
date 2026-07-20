//go:build !linux

package dataplane

import (
	"context"
	"errors"
)

// RuncShim and RunscBinary mirror the linux build so cmd/router compiles
// everywhere; selecting the containerd runtime off-linux fails at first use.
const (
	RuncShim    = "io.containerd.runc.v2"
	RunscBinary = "/usr/local/bin/runsc"
)

// ContainerdRuntime is linux-only (it dials the containerd socket on the
// data-plane host); this stub keeps non-linux builds compiling. Docker
// Desktop's containerd lives inside its VM and is not reachable from macOS —
// use DockerRuntime there.
type ContainerdRuntime struct {
	Address         string
	Namespace       string
	Shim            string
	OCIBinary       string
	CNIConfDir      string
	CPUFloorPercent float64
}

var errContainerdLinuxOnly = errors.New("containerd runtime requires linux (use --runtime runsc|runc on this host)")

func (r *ContainerdRuntime) EnsureWorkspace(context.Context, WorkspaceSpec, string) error {
	return errContainerdLinuxOnly
}
func (r *ContainerdRuntime) StopWorkspace(context.Context, string) error {
	return errContainerdLinuxOnly
}
func (r *ContainerdRuntime) Status(context.Context) ([]WorkspaceStatus, error) {
	return nil, errContainerdLinuxOnly
}
func (r *ContainerdRuntime) AgentEndpoint(context.Context, string) (string, error) {
	return "", errContainerdLinuxOnly
}
func (r *ContainerdRuntime) TerminalEndpoint(context.Context, string) (string, error) {
	return "", errContainerdLinuxOnly
}
func (r *ContainerdRuntime) CPUActive(context.Context, string) (bool, error) {
	return true, errContainerdLinuxOnly
}
func (r *ContainerdRuntime) AgentBusy(context.Context, string) (bool, error) {
	return true, errContainerdLinuxOnly
}
