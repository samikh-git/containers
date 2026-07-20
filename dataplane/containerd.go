//go:build linux

package dataplane

// ContainerdRuntime runs sandboxes against containerd directly — no docker
// CLI, no dockerd hop (PERFORMANCE.md item 2). It is the Linux data-plane
// runtime; DockerRuntime remains the dev-path runtime (Docker Desktop for
// Mac cannot reach the VM's containerd socket). Security posture matches
// DockerRuntime exactly (DESIGN §8): read-only rootfs, all capabilities
// dropped, no-new-privileges, tmpfs /tmp, pids limit, no network by default.
//
// Networking: containerd has no built-in networking, so a named network is
// a CNI conflist at /etc/cni/net.d/<name>.conflist (see dataplane-vm/
// provision.sh, which writes an isolated bridge conf for "wsnet"). The
// sandbox's IP is recorded as a container label at setup time; that is what
// AgentEndpoint and the /busy probe dial — the router runs on the same host
// and reaches CNI bridge IPs directly.
//
// Images: dockerd's image store is not containerd's. A locally built image
// must be imported once into this runtime's namespace:
//
//	docker save opencode-sandbox:v1 | ctr -n dataplane images import -

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/api/types/runc/options"

	v1stats "github.com/containerd/cgroups/v3/cgroup1/stats"
	v2stats "github.com/containerd/cgroups/v3/cgroup2/stats"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	gocni "github.com/containerd/go-cni"
	"github.com/containerd/typeurl/v2"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

const (
	labelIP  = "platform.agent-ip"
	labelNet = "platform.network"

	// RuncShim is the containerd runtime handler. gVisor is driven through
	// this same shim with BinaryName=runsc (the OCIBinary field) — the exact
	// mechanism dockerd uses for daemon.json "runtimes". The dedicated
	// gVisor shim (io.containerd.runsc.v1) hangs task creation against
	// containerd 2.x (containerd#11708 is the same incompatibility on the
	// delete path; verified here: `runsc create` succeeds, the sandbox
	// boots, but the shim's ttrpc connection drops and Create never
	// returns) — do not switch back to it without re-verifying in the VM.
	RuncShim = "io.containerd.runc.v2"

	// RunscBinary is the gVisor OCI runtime path provision.sh installs.
	RunscBinary = "/usr/local/bin/runsc"
)

type ContainerdRuntime struct {
	// Address of the containerd socket. Default: defaults.DefaultAddress
	// ("/run/containerd/containerd.sock" — the one Docker CE installs).
	Address string
	// Namespace keeps sandbox containers out of dockerd's "moby" namespace.
	// Default "dataplane".
	Namespace string
	// Shim is the runtime handler. Default RuncShim (see its doc comment on
	// why gVisor does NOT get its own shim here).
	Shim string
	// OCIBinary overrides the shim's OCI runtime binary — RunscBinary for
	// gVisor sandboxes, empty for plain runc.
	OCIBinary string
	// CNIConfDir holds <network>.conflist files. Default "/etc/cni/net.d".
	CNIConfDir string
	// CPUFloorPercent below which the sandbox counts as CPU-idle. Default 5.
	CPUFloorPercent float64

	mu      sync.Mutex
	client  *containerd.Client
	cpuPrev map[string]cpuSample
}

type cpuSample struct {
	usageNS uint64
	at      time.Time
}

// conn returns the lazily dialed containerd client.
func (r *ContainerdRuntime) conn() (*containerd.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client != nil {
		return r.client, nil
	}
	addr := r.Address
	if addr == "" {
		addr = defaults.DefaultAddress
	}
	c, err := containerd.New(addr, containerd.WithDefaultNamespace(r.namespace()))
	if err != nil {
		return nil, fmt.Errorf("containerd dial %s: %w", addr, err)
	}
	r.client = c
	return c, nil
}

// image resolves the spec's image ref, tolerating the docker.io/library/
// prefix that `ctr images import` adds to bare docker-built tags, and
// unpacks it for the snapshotter if needed.
func (r *ContainerdRuntime) image(ctx context.Context, client *containerd.Client, ref string) (containerd.Image, error) {
	img, err := client.GetImage(ctx, ref)
	if err != nil && errdefs.IsNotFound(err) && !strings.Contains(ref, "/") {
		img, err = client.GetImage(ctx, "docker.io/library/"+ref)
	}
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, fmt.Errorf("image %s not in containerd namespace %q — import it once with: docker save %s | ctr -n %s images import -",
				ref, r.namespace(), ref, r.namespace())
		}
		return nil, err
	}
	unpacked, err := img.IsUnpacked(ctx, defaults.DefaultSnapshotter)
	if err != nil {
		return nil, err
	}
	if !unpacked {
		if err := img.Unpack(ctx, defaults.DefaultSnapshotter); err != nil {
			return nil, fmt.Errorf("unpack %s: %w", ref, err)
		}
	}
	return img, nil
}

func (r *ContainerdRuntime) namespace() string {
	if r.Namespace == "" {
		return "dataplane"
	}
	return r.Namespace
}

func (r *ContainerdRuntime) shim() string {
	if r.Shim == "" {
		return RuncShim
	}
	return r.Shim
}

// runtimeOptions carries the OCI binary override to the runc shim, the same
// way dockerd's daemon.json "runtimes" entries do.
func (r *ContainerdRuntime) runtimeOptions() *options.Options {
	if r.OCIBinary == "" {
		return nil
	}
	return &options.Options{BinaryName: r.OCIBinary}
}

func (r *ContainerdRuntime) cni(network string) (gocni.CNI, error) {
	confDir := r.CNIConfDir
	if confDir == "" {
		confDir = "/etc/cni/net.d"
	}
	return gocni.New(
		// Debian/Ubuntu package dir and the upstream-tarball dir.
		gocni.WithPluginDir([]string{"/opt/cni/bin", "/usr/lib/cni"}),
		gocni.WithConfListFile(confDir+"/"+network+".conflist"),
	)
}

func (r *ContainerdRuntime) EnsureWorkspace(ctx context.Context, spec WorkspaceSpec, mountPath string) error {
	client, err := r.conn()
	if err != nil {
		return err
	}
	// Optimistic create, mirroring DockerRuntime: the common path (Up has
	// already stopped any prior holder) creates straight away; idempotency
	// lives in the already-exists fallback — same generation running →
	// done; anything else under the name → replace and retry once.
	err = r.create(ctx, client, spec, mountPath)
	if err == nil || !errdefs.IsAlreadyExists(err) {
		return err
	}
	statuses, serr := r.Status(ctx)
	if serr != nil {
		return serr
	}
	for _, s := range statuses {
		if s.ID == spec.ID && s.Generation == spec.Generation && s.State == "running" {
			return nil
		}
	}
	if err := r.StopWorkspace(ctx, spec.ID); err != nil {
		return err
	}
	return r.create(ctx, client, spec, mountPath)
}

func (r *ContainerdRuntime) create(ctx context.Context, client *containerd.Client, spec WorkspaceSpec, mountPath string) (err error) {
	img, err := r.image(ctx, client, spec.Image)
	if err != nil {
		return err
	}
	name := containerName(spec.ID)

	mounts := []specs.Mount{
		{Destination: "/workspace", Type: "bind", Source: mountPath,
			Options: []string{"rbind", "rw"}},
		{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs",
			Options: []string{"size=512m", "mode=1777", "nosuid", "nodev"}},
	}
	if spec.ProfileDir != "" {
		mounts = append(mounts, specs.Mount{
			Destination: "/etc/agent-profile", Type: "bind", Source: spec.ProfileDir,
			Options: []string{"rbind", "ro"},
		})
	}
	var env []string
	if spec.GatewayURL != "" {
		env = append(env, "OPENCODE_BASE_URL="+spec.GatewayURL)
	}
	if spec.SessionToken != "" {
		env = append(env, "OPENCODE_SESSION_TOKEN="+spec.SessionToken)
	}

	specOpts := []oci.SpecOpts{
		oci.WithImageConfig(img),
		oci.WithRootFSReadonly(),
		oci.WithCapabilities(nil), // drop ALL
		oci.WithNoNewPrivileges,
		oci.WithMounts(mounts),
		withPidsLimit(512),
	}
	if len(env) > 0 {
		specOpts = append(specOpts, oci.WithEnv(env))
	}
	if spec.CPUs > 0 {
		specOpts = append(specOpts, oci.WithCPUCFS(int64(spec.CPUs*100000), 100000))
	}
	if spec.MemoryMB > 0 {
		specOpts = append(specOpts, oci.WithMemoryLimit(uint64(spec.MemoryMB)*1024*1024))
	}
	// No network option needed for the none case: the default OCI spec puts
	// the container in a fresh network namespace with only loopback — the
	// strictest default is the default (§8), same as docker --network none.

	labels := map[string]string{
		labelWS:  spec.ID,
		labelGen: strconv.FormatUint(spec.Generation, 10),
	}
	if spec.Network != "" {
		labels[labelNet] = spec.Network
	}

	container, err := client.NewContainer(ctx, name,
		containerd.WithImage(img),
		containerd.WithNewSnapshot(name+"-snap", img),
		containerd.WithRuntime(r.shim(), r.runtimeOptions()),
		containerd.WithContainerLabels(labels),
		containerd.WithNewSpec(specOpts...),
	)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = container.Delete(context.WithoutCancel(ctx), containerd.WithSnapshotCleanup)
		}
	}()

	task, err := container.NewTask(ctx, cio.NullIO)
	if err != nil {
		return fmt.Errorf("task create %s: %w", name, err)
	}
	defer func() {
		if err != nil {
			_, _ = task.Delete(context.WithoutCancel(ctx), containerd.WithProcessKill)
		}
	}()

	// CNI attach happens between task create (netns now exists) and start.
	if spec.Network != "" {
		cni, cerr := r.cni(spec.Network)
		if cerr != nil {
			return fmt.Errorf("network %s: %w", spec.Network, cerr)
		}
		netns := fmt.Sprintf("/proc/%d/ns/net", task.Pid())
		result, cerr := cni.Setup(ctx, name, netns)
		if cerr != nil {
			return fmt.Errorf("cni setup %s: %w", spec.Network, cerr)
		}
		if ip := firstIP(result); ip != "" {
			labels[labelIP] = ip
			if _, lerr := container.SetLabels(ctx, labels); lerr != nil {
				return lerr
			}
		}
	}

	if err = task.Start(ctx); err != nil {
		return fmt.Errorf("task start %s: %w", name, err)
	}
	return nil
}

func firstIP(result *gocni.Result) string {
	for name, iface := range result.Interfaces {
		if name == "lo" {
			continue
		}
		for _, ipc := range iface.IPConfigs {
			if ipc.IP != nil {
				return ipc.IP.String()
			}
		}
	}
	return ""
}

func (r *ContainerdRuntime) StopWorkspace(ctx context.Context, id string) error {
	client, err := r.conn()
	if err != nil {
		return err
	}
	name := containerName(id)
	container, err := client.LoadContainer(ctx, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return err
	}
	labels, err := container.Labels(ctx)
	if err != nil {
		return err
	}

	task, err := container.Task(ctx, nil)
	if err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	var netns string
	if task != nil {
		netns = fmt.Sprintf("/proc/%d/ns/net", task.Pid())
		// Graceful drain per §7: SIGTERM (checkpoint hook) → grace → SIGKILL.
		if err := r.drain(ctx, task); err != nil {
			return fmt.Errorf("stop %s: %w", name, err)
		}
	}
	// Best-effort CNI teardown: releases the IPAM lease even when the netns
	// is already gone (plugins tolerate a missing namespace on DEL).
	if network := labels[labelNet]; network != "" {
		if cni, cerr := r.cni(network); cerr == nil {
			_ = cni.Remove(ctx, name, netns)
		}
	}
	if task != nil {
		// Plain delete: drain has already stopped the task. WithProcessKill
		// would make the shim SIGKILL first, and runsc errors ("sandbox is
		// not running") when asked to kill an already-dead sandbox.
		if _, err := task.Delete(ctx); err != nil && !errdefs.IsNotFound(err) {
			return err
		}
	}
	if err := container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	r.mu.Lock()
	delete(r.cpuPrev, id)
	r.mu.Unlock()
	return nil
}

// drain sends SIGTERM, waits up to 30s, then SIGKILLs. A task already
// stopped is fine.
func (r *ContainerdRuntime) drain(ctx context.Context, task containerd.Task) error {
	if st, err := task.Status(ctx); err == nil && st.Status != containerd.Running && st.Status != containerd.Paused {
		return nil
	}
	exited, err := task.Wait(ctx)
	if err != nil {
		return err
	}
	if err := task.Kill(ctx, syscall.SIGTERM); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return err
	}
	select {
	case <-exited:
		return nil
	case <-time.After(30 * time.Second):
	case <-ctx.Done():
	}
	if err := task.Kill(ctx, syscall.SIGKILL, containerd.WithKillAll); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
	}
	return ctx.Err()
}

func (r *ContainerdRuntime) Status(ctx context.Context) ([]WorkspaceStatus, error) {
	client, err := r.conn()
	if err != nil {
		return nil, err
	}
	list, err := client.Containers(ctx, `labels."`+labelWS+`"`)
	if err != nil {
		return nil, err
	}
	var statuses []WorkspaceStatus
	for _, c := range list {
		labels, err := c.Labels(ctx)
		if err != nil || labels[labelWS] == "" {
			continue
		}
		st := WorkspaceStatus{ID: labels[labelWS], Container: c.ID()}
		st.Generation, _ = strconv.ParseUint(labels[labelGen], 10, 64)
		st.State = "created"
		if task, terr := c.Task(ctx, nil); terr == nil {
			if s, serr := task.Status(ctx); serr == nil {
				switch s.Status {
				case containerd.Running, containerd.Paused, containerd.Pausing:
					st.State = "running"
				case containerd.Created:
					st.State = "created"
				default:
					st.State = "exited"
				}
			}
		}
		statuses = append(statuses, st)
	}
	return statuses, nil
}

// AgentEndpoint returns the sandbox's CNI-assigned IP with the agent port —
// the router runs on the same host and dials bridge IPs directly.
func (r *ContainerdRuntime) AgentEndpoint(ctx context.Context, id string) (string, error) {
	ip, err := r.agentIP(ctx, id)
	if err != nil {
		return "", err
	}
	return ip + ":" + agentPort, nil
}

// TerminalEndpoint is AgentEndpoint for the termbridge port (web terminal).
func (r *ContainerdRuntime) TerminalEndpoint(ctx context.Context, id string) (string, error) {
	ip, err := r.agentIP(ctx, id)
	if err != nil {
		return "", err
	}
	return ip + ":" + termPort, nil
}

func (r *ContainerdRuntime) agentIP(ctx context.Context, id string) (string, error) {
	client, err := r.conn()
	if err != nil {
		return "", err
	}
	container, err := client.LoadContainer(ctx, containerName(id))
	if err != nil {
		return "", fmt.Errorf("workspace %s: %w", id, err)
	}
	labels, err := container.Labels(ctx)
	if err != nil {
		return "", err
	}
	if ip := labels[labelIP]; ip != "" {
		return ip, nil
	}
	return "", fmt.Errorf("workspace %s: sandbox has no network — start it with --network to reach the agent", id)
}

// CPUActive implements ActivityProbes via containerd's metrics API — no
// process spawn, no docker stats double-sample delay (PERFORMANCE.md item
// 4). Usage is compared against the previous tick's sample; the first
// sighting has no baseline and counts as active (fail toward keeping
// things running, same direction as every other probe).
func (r *ContainerdRuntime) CPUActive(ctx context.Context, wsID string) (bool, error) {
	client, err := r.conn()
	if err != nil {
		return true, err
	}
	container, err := client.LoadContainer(ctx, containerName(wsID))
	if err != nil {
		return true, fmt.Errorf("metrics %s: %w", wsID, err)
	}
	task, err := container.Task(ctx, nil)
	if err != nil {
		return true, fmt.Errorf("metrics %s: %w", wsID, err)
	}
	m, err := task.Metrics(ctx)
	if err != nil {
		return true, fmt.Errorf("metrics %s: %w", wsID, err)
	}
	data, err := typeurl.UnmarshalAny(m.Data)
	if err != nil {
		return true, fmt.Errorf("metrics %s: %w", wsID, err)
	}
	var usageNS uint64
	switch v := data.(type) {
	case *v2stats.Metrics:
		if v.CPU == nil {
			return true, fmt.Errorf("metrics %s: no cpu stats", wsID)
		}
		usageNS = v.CPU.UsageUsec * 1000
	case *v1stats.Metrics:
		if v.CPU == nil || v.CPU.Usage == nil {
			return true, fmt.Errorf("metrics %s: no cpu stats", wsID)
		}
		usageNS = v.CPU.Usage.Total
	default:
		return true, fmt.Errorf("metrics %s: unexpected type %T", wsID, data)
	}

	now := time.Now()
	r.mu.Lock()
	if r.cpuPrev == nil {
		r.cpuPrev = map[string]cpuSample{}
	}
	prev, seen := r.cpuPrev[wsID]
	r.cpuPrev[wsID] = cpuSample{usageNS: usageNS, at: now}
	r.mu.Unlock()

	if !seen || !now.After(prev.at) || usageNS < prev.usageNS {
		return true, nil // no baseline yet — active until proven idle
	}
	pct := float64(usageNS-prev.usageNS) / float64(now.Sub(prev.at).Nanoseconds()) * 100
	return pct >= r.floor(), nil
}

func (r *ContainerdRuntime) floor() float64 {
	if r.CPUFloorPercent <= 0 {
		return 5.0
	}
	return r.CPUFloorPercent
}

// AgentBusy implements the /busy contract (§7) by dialing the sandbox IP
// from the host — no docker exec. A sandbox without a network cannot be
// probed and counts as busy (fail-active).
func (r *ContainerdRuntime) AgentBusy(ctx context.Context, wsID string) (bool, error) {
	ip, err := r.agentIP(ctx, wsID)
	if err != nil {
		return true, fmt.Errorf("busy probe %s: %w", wsID, err)
	}
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, "http://"+ip+":"+agentPort+"/busy", nil)
	if err != nil {
		return true, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return true, fmt.Errorf("busy probe %s: %w", wsID, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil || resp.StatusCode != http.StatusOK {
		return true, fmt.Errorf("busy probe %s: status %d", wsID, resp.StatusCode)
	}
	switch strings.TrimSpace(string(body)) {
	case "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return true, fmt.Errorf("busy probe %s: unexpected body %q", wsID, body)
	}
}

func withPidsLimit(limit int64) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		if s.Linux == nil {
			s.Linux = &specs.Linux{}
		}
		if s.Linux.Resources == nil {
			s.Linux.Resources = &specs.LinuxResources{}
		}
		s.Linux.Resources.Pids = &specs.LinuxPids{Limit: &limit}
		return nil
	}
}
