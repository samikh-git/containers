//go:build linux

package dataplane

// GVisorRuntime drives gVisor (runsc) directly against OCI bundles — no
// containerd task, no shim. It exists for one reason: checkpoint/restore.
// The warm-start path (PERFORMANCE.md, warm pool) needs `runsc checkpoint`
// and `runsc restore` with a rewritten bundle (new /workspace bind source,
// new netns), which neither dockerd nor containerd's task API expose for
// gVisor. Everything empirically verified in the dataplane VM:
//
//   - restore accepts a different /workspace bind source as long as its
//     CONTENT matches the checkpoint moment (hence ZFS clone of the golden
//     snapshot);
//   - restore into a fresh netns works: runsc re-scrapes the veth's IP at
//     restore time and the netstack comes up on the new address (~200ms to
//     the agent listening, vs ~1.4s cold);
//   - a netns is single-use: runsc takes the veth's address over and does
//     not put it back, so every sandbox gets a fresh netns + CNI ADD.
//
// Security posture matches the other runtimes (DESIGN §8): read-only
// rootfs, all capabilities dropped, no-new-privileges, tmpfs /tmp, pids
// limit, no network unless named. The rootfs is a single containerd
// snapshotter VIEW of the image, shared read-only by every sandbox — the
// image contract says all writes go to /workspace and /tmp, so no
// per-container snapshot is needed at all (and skipping it is part of why
// this path is fast).
//
// containerd is still used, but only as the image store (resolve, unpack,
// mount a read-only view); container lifecycle is runsc's.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/errdefs"
	gocni "github.com/containerd/go-cni"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

type GVisorRuntime struct {
	// RunscPath is the runsc binary. Default RunscBinary.
	RunscPath string
	// StateDir holds everything this runtime owns on disk:
	//   <StateDir>/runsc/          runsc's own state root (--root)
	//   <StateDir>/bundles/<name>/ OCI bundle (config.json, boot log)
	//   <StateDir>/state/<name>.json  our per-sandbox record (labels, IP, netns)
	//   <StateDir>/rootfs/<ref>/   shared read-only image rootfs mounts
	StateDir string
	// ContainerdAddress/Namespace locate the image store. Defaults match
	// ContainerdRuntime ("/run/containerd/containerd.sock", "dataplane").
	ContainerdAddress   string
	ContainerdNamespace string
	// CNIConfDir holds <network>.conflist files. Default "/etc/cni/net.d".
	CNIConfDir string
	// CPUFloorPercent below which the sandbox counts as CPU-idle. Default 5.
	CPUFloorPercent float64

	mu      sync.Mutex
	client  *containerd.Client
	cpuPrev map[string]cpuSample
}

// gvisorState is the on-disk record for one sandbox — the same role
// container labels play on the docker/containerd paths. Rebuilt state
// (Status) always comes from here plus `runsc state`, never from memory.
type gvisorState struct {
	Name       string    `json:"name"`
	Workspace  string    `json:"workspace"`
	Generation uint64    `json:"generation"`
	Network    string    `json:"network,omitempty"`
	IP         string    `json:"ip,omitempty"`
	Netns      string    `json:"netns,omitempty"` // full path, e.g. /var/run/netns/gv-ws-x
	Bundle     string    `json:"bundle"`
	Restored   bool      `json:"restored,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

func (g *GVisorRuntime) runscPath() string {
	if g.RunscPath == "" {
		return RunscBinary
	}
	return g.RunscPath
}

func (g *GVisorRuntime) runscRoot() string  { return filepath.Join(g.StateDir, "runsc") }
func (g *GVisorRuntime) bundleDir(name string) string {
	return filepath.Join(g.StateDir, "bundles", name)
}
func (g *GVisorRuntime) statePath(name string) string {
	return filepath.Join(g.StateDir, "state", name+".json")
}

// runsc runs one runsc command with this runtime's state root.
func (g *GVisorRuntime) runsc(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"--root", g.runscRoot()}, args...)
	out, err := exec.CommandContext(ctx, g.runscPath(), full...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("runsc %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// runscDetached is runsc() for `run -detach` / `restore -detach`: the
// detached sandbox inherits and KEEPS the parent's stdio, so piping it
// (CombinedOutput) blocks the caller until the sandbox exits — a hang, not
// a start. Stdio goes to a log file in the bundle instead, which is also
// where the container's own stdout/stderr usefully end up.
func (g *GVisorRuntime) runscDetached(ctx context.Context, bundle string, args ...string) error {
	logPath := filepath.Join(bundle, "console.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	full := append([]string{"--root", g.runscRoot()}, args...)
	cmd := exec.CommandContext(ctx, g.runscPath(), full...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Run(); err != nil {
		tail, _ := os.ReadFile(logPath)
		if len(tail) > 512 {
			tail = tail[len(tail)-512:]
		}
		return fmt.Errorf("runsc %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(tail)))
	}
	return nil
}

func (g *GVisorRuntime) conn() (*containerd.Client, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.client != nil {
		return g.client, nil
	}
	addr := g.ContainerdAddress
	if addr == "" {
		addr = defaults.DefaultAddress
	}
	ns := g.ContainerdNamespace
	if ns == "" {
		ns = "dataplane"
	}
	c, err := containerd.New(addr, containerd.WithDefaultNamespace(ns))
	if err != nil {
		return nil, fmt.Errorf("containerd dial %s: %w", addr, err)
	}
	g.client = c
	return c, nil
}

// ensureRootfs resolves the image and materializes its rootfs as a plain
// directory at a stable path, shared read-only by every sandbox of that
// image (the image contract sends all writes to /workspace and /tmp, so no
// per-container copy is needed). A plain directory, not a snapshotter view
// mount: runsc's gofer cannot start on a containerd overlayfs view as root
// (verified in the VM — "cannot read client sync file: EOF"), so the view
// is mounted once, copied out, and unmounted. One-time cost per image.
// Returns the rootfs path and the image's config (entrypoint, env, cwd).
func (g *GVisorRuntime) ensureRootfs(ctx context.Context, ref string) (string, ocispec.ImageConfig, error) {
	var cfg ocispec.ImageConfig
	client, err := g.conn()
	if err != nil {
		return "", cfg, err
	}
	img, err := client.GetImage(ctx, ref)
	if err != nil && errdefs.IsNotFound(err) && !strings.Contains(ref, "/") {
		img, err = client.GetImage(ctx, "docker.io/library/"+ref)
	}
	if err != nil {
		if errdefs.IsNotFound(err) {
			ns := g.ContainerdNamespace
			if ns == "" {
				ns = "dataplane"
			}
			return "", cfg, fmt.Errorf("image %s not in containerd namespace %q — import it once with: docker save %s | ctr -n %s images import -",
				ref, ns, ref, ns)
		}
		return "", cfg, err
	}

	desc, err := img.Config(ctx)
	if err != nil {
		return "", cfg, err
	}
	blob, err := content.ReadBlob(ctx, client.ContentStore(), desc)
	if err != nil {
		return "", cfg, err
	}
	var imageSpec ocispec.Image
	if err := json.Unmarshal(blob, &imageSpec); err != nil {
		return "", cfg, fmt.Errorf("image config %s: %w", ref, err)
	}
	cfg = imageSpec.Config

	safe := strings.NewReplacer("/", "_", ":", "_", "@", "_").Replace(img.Name())
	target := filepath.Join(g.StateDir, "rootfs", safe)
	// Extraction is atomic via rename: a completed rootfs always has /usr.
	if _, err := os.Stat(filepath.Join(target, "usr")); err == nil {
		return target, cfg, nil
	}

	unpacked, err := img.IsUnpacked(ctx, defaults.DefaultSnapshotter)
	if err != nil {
		return "", cfg, err
	}
	if !unpacked {
		if err := img.Unpack(ctx, defaults.DefaultSnapshotter); err != nil {
			return "", cfg, fmt.Errorf("unpack %s: %w", ref, err)
		}
	}
	diffIDs, err := img.RootFS(ctx)
	if err != nil {
		return "", cfg, err
	}
	sn := client.SnapshotService(defaults.DefaultSnapshotter)
	key := "gvisor-rootfs-" + safe
	mounts, err := sn.View(ctx, key, identity.ChainID(diffIDs).String())
	if errdefs.IsAlreadyExists(err) {
		mounts, err = sn.Mounts(ctx, key)
	}
	if err != nil {
		return "", cfg, fmt.Errorf("rootfs view %s: %w", ref, err)
	}
	defer func() { _ = sn.Remove(context.WithoutCancel(ctx), key) }()

	viewDir := target + ".view"
	tmpDir := target + ".extract"
	_ = os.RemoveAll(tmpDir)
	if err := os.MkdirAll(viewDir, 0o755); err != nil {
		return "", cfg, err
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return "", cfg, err
	}
	if err := mount.All(mounts, viewDir); err != nil {
		return "", cfg, fmt.Errorf("rootfs mount %s: %w", ref, err)
	}
	defer func() {
		_ = mount.UnmountAll(viewDir, 0)
		_ = os.Remove(viewDir)
	}()
	// -a preserves ownership/modes/links; source contents into tmpDir.
	if out, err := exec.CommandContext(ctx, "cp", "-a", viewDir+"/.", tmpDir).CombinedOutput(); err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", cfg, fmt.Errorf("rootfs extract %s: %w: %s", ref, err, strings.TrimSpace(string(out)))
	}
	if err := os.Rename(tmpDir, target); err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", cfg, err
	}
	return target, cfg, nil
}

func (g *GVisorRuntime) cni(network string) (gocni.CNI, error) {
	confDir := g.CNIConfDir
	if confDir == "" {
		confDir = "/etc/cni/net.d"
	}
	return gocni.New(
		gocni.WithPluginDir([]string{"/opt/cni/bin", "/usr/lib/cni"}),
		gocni.WithConfListFile(confDir+"/"+network+".conflist"),
	)
}

// setupNetwork creates a fresh named netns and runs CNI ADD in it. Fresh
// every time — runsc consumes the veth's address (see package comment), so
// namespaces are strictly single-use.
func (g *GVisorRuntime) setupNetwork(ctx context.Context, name, network string) (netnsPath, ip string, err error) {
	nsName := "gv-" + name
	if out, err := exec.CommandContext(ctx, "ip", "netns", "add", nsName).CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("netns add %s: %w: %s", nsName, err, strings.TrimSpace(string(out)))
	}
	netnsPath = "/var/run/netns/" + nsName
	defer func() {
		if err != nil {
			_ = exec.Command("ip", "netns", "del", nsName).Run()
		}
	}()
	cni, err := g.cni(network)
	if err != nil {
		return "", "", fmt.Errorf("network %s: %w", network, err)
	}
	result, err := cni.Setup(ctx, name, netnsPath)
	if err != nil {
		return "", "", fmt.Errorf("cni setup %s: %w", network, err)
	}
	return netnsPath, firstIP(result), nil
}

// teardownNetwork is best-effort: CNI DEL (releases the IPAM lease even if
// the netns is gone), then netns removal.
func (g *GVisorRuntime) teardownNetwork(ctx context.Context, st *gvisorState) {
	if st.Network != "" {
		if cni, err := g.cni(st.Network); err == nil {
			_ = cni.Remove(ctx, st.Name, st.Netns)
		}
	}
	if st.Netns != "" {
		_ = exec.CommandContext(ctx, "ip", "netns", "del", filepath.Base(st.Netns)).Run()
	}
}

// buildSpec assembles the sandbox's OCI spec. Restore REQUIRES the mount
// table's shape to match the checkpoint moment (sources may change,
// destinations/types may not), so slot boot and claim restore both come
// through here with the same recipe.
func (g *GVisorRuntime) buildSpec(spec WorkspaceSpec, mountPath, rootfs string, img ocispec.ImageConfig, netnsPath string) *specs.Spec {
	env := append([]string{}, img.Env...)
	if spec.GatewayURL != "" {
		env = append(env, "OPENCODE_BASE_URL="+spec.GatewayURL)
	}
	if spec.SessionToken != "" {
		env = append(env, "OPENCODE_SESSION_TOKEN="+spec.SessionToken)
	}
	args := append(append([]string{}, img.Entrypoint...), img.Cmd...)
	if len(args) == 0 {
		args = []string{"/usr/local/bin/entrypoint.sh"}
	}
	cwd := img.WorkingDir
	if cwd == "" {
		cwd = "/workspace"
	}

	mounts := []specs.Mount{
		{Destination: "/proc", Type: "proc", Source: "proc"},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs",
			Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts",
			Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"}},
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm",
			Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs",
			Options: []string{"nosuid", "noexec", "nodev", "ro"}},
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

	namespaces := []specs.LinuxNamespace{
		{Type: specs.PIDNamespace},
		{Type: specs.IPCNamespace},
		{Type: specs.UTSNamespace},
		{Type: specs.MountNamespace},
	}
	if netnsPath != "" {
		namespaces = append(namespaces, specs.LinuxNamespace{Type: specs.NetworkNamespace, Path: netnsPath})
	} else {
		// No network asked for: an empty fresh netns (loopback only) — the
		// strictest default is the default (§8).
		namespaces = append(namespaces, specs.LinuxNamespace{Type: specs.NetworkNamespace})
	}

	pids := int64(512)
	resources := &specs.LinuxResources{Pids: &specs.LinuxPids{Limit: &pids}}
	if spec.CPUs > 0 {
		quota := int64(spec.CPUs * 100000)
		period := uint64(100000)
		resources.CPU = &specs.LinuxCPU{Quota: &quota, Period: &period}
	}
	if spec.MemoryMB > 0 {
		limit := spec.MemoryMB * 1024 * 1024
		resources.Memory = &specs.LinuxMemory{Limit: &limit}
	}

	noNewPrivs := true
	return &specs.Spec{
		Version: specs.Version,
		Process: &specs.Process{
			User:            specs.User{UID: sandboxUID, GID: sandboxGID},
			Args:            args,
			Env:             env,
			Cwd:             cwd,
			NoNewPrivileges: noNewPrivs,
			Capabilities:    &specs.LinuxCapabilities{}, // drop ALL
			Rlimits: []specs.POSIXRlimit{
				{Type: "RLIMIT_NOFILE", Hard: 65536, Soft: 65536},
			},
		},
		Root: &specs.Root{Path: rootfs, Readonly: true},
		// Constant hostname on purpose: restore keeps the checkpoint's UTS
		// state, so a per-workspace hostname would silently diverge between
		// cold and warm starts.
		Hostname: "sandbox",
		Mounts:   mounts,
		Linux: &specs.Linux{
			Namespaces: namespaces,
			Resources:  resources,
		},
	}
}

// prepare builds the bundle (rootfs, network, config.json) for a boot or a
// restore, returning the state record to persist. Callers own cleanup on
// later failure via g.cleanup(st).
func (g *GVisorRuntime) prepare(ctx context.Context, spec WorkspaceSpec, mountPath string) (*gvisorState, error) {
	rootfs, imgCfg, err := g.ensureRootfs(ctx, spec.Image)
	if err != nil {
		return nil, err
	}
	name := containerName(spec.ID)
	st := &gvisorState{
		Name:       name,
		Workspace:  spec.ID,
		Generation: spec.Generation,
		Network:    spec.Network,
		Bundle:     g.bundleDir(name),
		CreatedAt:  time.Now().UTC(),
	}
	if spec.Network != "" {
		st.Netns, st.IP, err = g.setupNetwork(ctx, name, spec.Network)
		if err != nil {
			return nil, err
		}
	}
	oci := g.buildSpec(spec, mountPath, rootfs, imgCfg, st.Netns)
	if err := os.MkdirAll(st.Bundle, 0o755); err != nil {
		g.teardownNetwork(ctx, st)
		return nil, err
	}
	b, _ := json.MarshalIndent(oci, "", " ")
	if err := os.WriteFile(filepath.Join(st.Bundle, "config.json"), b, 0o644); err != nil {
		g.teardownNetwork(ctx, st)
		return nil, err
	}
	return st, nil
}

func (g *GVisorRuntime) writeState(st *gvisorState) error {
	if err := os.MkdirAll(filepath.Join(g.StateDir, "state"), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(st)
	return os.WriteFile(g.statePath(st.Name), b, 0o644)
}

func (g *GVisorRuntime) readState(name string) (*gvisorState, error) {
	b, err := os.ReadFile(g.statePath(name))
	if err != nil {
		return nil, err
	}
	var st gvisorState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// cleanup removes everything prepare/boot created for a sandbox.
func (g *GVisorRuntime) cleanup(ctx context.Context, st *gvisorState) {
	_, _ = g.runsc(ctx, "delete", "-force", st.Name)
	g.teardownNetwork(ctx, st)
	_ = os.RemoveAll(st.Bundle)
	_ = os.Remove(g.statePath(st.Name))
}

func (g *GVisorRuntime) EnsureWorkspace(ctx context.Context, spec WorkspaceSpec, mountPath string) error {
	// Idempotency: same generation running → done; anything else under the
	// name → replace.
	if st, err := g.readState(containerName(spec.ID)); err == nil {
		if st.Generation == spec.Generation && g.runscStatus(ctx, st.Name) == "running" {
			return nil
		}
		if err := g.StopWorkspace(ctx, spec.ID); err != nil {
			return err
		}
	}
	st, err := g.prepare(ctx, spec, mountPath)
	if err != nil {
		return err
	}
	if err := g.runscDetached(ctx, st.Bundle, "run", "-detach", "--bundle", st.Bundle, st.Name); err != nil {
		g.cleanup(ctx, st)
		return err
	}
	if err := g.writeState(st); err != nil {
		g.cleanup(ctx, st)
		return err
	}
	return nil
}

// RestoreWorkspace starts spec's sandbox from a runsc checkpoint image —
// the warm-start compute step. The bundle is rebuilt with the new
// /workspace source and a fresh netns; runsc adopts the new veth's IP.
// mountPath's CONTENT must match the checkpoint moment (a clone of the
// golden snapshot — the caller's responsibility).
func (g *GVisorRuntime) RestoreWorkspace(ctx context.Context, spec WorkspaceSpec, mountPath, imageDir string) error {
	if st, err := g.readState(containerName(spec.ID)); err == nil {
		_ = st
		if err := g.StopWorkspace(ctx, spec.ID); err != nil {
			return err
		}
	}
	st, err := g.prepare(ctx, spec, mountPath)
	if err != nil {
		return err
	}
	st.Restored = true
	if err := g.runscDetached(ctx, st.Bundle, "restore", "-detach", "--image-path", imageDir, "--bundle", st.Bundle, st.Name); err != nil {
		g.cleanup(ctx, st)
		return err
	}
	if err := g.writeState(st); err != nil {
		g.cleanup(ctx, st)
		return err
	}
	return nil
}

// CheckpointWorkspace checkpoints a running sandbox to imageDir and tears
// the (now stopped) sandbox down, keeping the workspace volume. Used by the
// warm pool to turn a booted golden sandbox into slot inventory, and by
// process hibernate (same machinery, image kept under StateDir/hibernate).
func (g *GVisorRuntime) CheckpointWorkspace(ctx context.Context, wsID, imageDir string) error {
	name := containerName(wsID)
	st, err := g.readState(name)
	if err != nil {
		return fmt.Errorf("checkpoint %s: no such sandbox: %w", wsID, err)
	}
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		return err
	}
	if _, err := g.runsc(ctx, "checkpoint", "--image-path", imageDir, name); err != nil {
		return err
	}
	// checkpoint leaves the container stopped; remove it and its netns.
	g.cleanup(ctx, st)
	return nil
}

type hibernateMeta struct {
	Token     string    `json:"token"`
	CreatedAt time.Time `json:"created_at"`
}

func (g *GVisorRuntime) hibernateRoot(wsID string) string {
	return filepath.Join(g.StateDir, "hibernate", wsID)
}

// HibernateCheckpoint implements ProcessHibernator: runsc checkpoint into
// StateDir (outside the /workspace bind) and record the frozen session
// token for resume. meta.json is written last so a crash mid-checkpoint
// never yields a half-usable image.
func (g *GVisorRuntime) HibernateCheckpoint(ctx context.Context, wsID, token string) error {
	root := g.hibernateRoot(wsID)
	_ = os.RemoveAll(root)
	ckpt := filepath.Join(root, "ckpt")
	if err := g.CheckpointWorkspace(ctx, wsID, ckpt); err != nil {
		_ = os.RemoveAll(root)
		return err
	}
	meta := hibernateMeta{Token: token, CreatedAt: time.Now().UTC()}
	b, err := json.MarshalIndent(meta, "", " ")
	if err != nil {
		_ = os.RemoveAll(root)
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "meta.json"), b, 0o600); err != nil {
		_ = os.RemoveAll(root)
		return err
	}
	return nil
}

// HibernateImage implements ProcessHibernator.
func (g *GVisorRuntime) HibernateImage(wsID string) (imageDir, token string, ok bool) {
	root := g.hibernateRoot(wsID)
	b, err := os.ReadFile(filepath.Join(root, "meta.json"))
	if err != nil {
		return "", "", false
	}
	var meta hibernateMeta
	if json.Unmarshal(b, &meta) != nil || meta.Token == "" {
		return "", "", false
	}
	ckpt := filepath.Join(root, "ckpt")
	if st, err := os.Stat(ckpt); err != nil || !st.IsDir() {
		return "", "", false
	}
	return ckpt, meta.Token, true
}

// ClearHibernate implements ProcessHibernator.
func (g *GVisorRuntime) ClearHibernate(wsID string) error {
	return os.RemoveAll(g.hibernateRoot(wsID))
}

// AgentListening reports whether the agent has bound its port, checked from
// inside the sandbox (/proc/net/tcp), so it works pre- and post-restore and
// without relying on host reachability. Port 4321 = 0x10E1; state 0A =
// LISTEN.
func (g *GVisorRuntime) AgentListening(ctx context.Context, wsID string) bool {
	_, err := g.runsc(ctx, "exec", containerName(wsID),
		"grep", "-qE", ":10E1 .* 0A", "/proc/net/tcp", "/proc/net/tcp6")
	return err == nil
}

// AgentReady reports whether the agent actually SERVES — an HTTP 200 from
// the harness, not just a bound socket. The distinction matters for the
// warm pool: OpenCode binds its port early and keeps initializing, so a
// checkpoint taken at listen time restores into that remaining init. A
// checkpoint taken at ready time restores a harness that answers
// immediately. Falls back to the listen probe when the sandbox has no
// network to dial.
func (g *GVisorRuntime) AgentReady(ctx context.Context, wsID string) bool {
	ip, err := g.agentIP(wsID)
	if err != nil {
		return g.AgentListening(ctx, wsID)
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, "http://"+ip+":"+agentPort+"/app", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode == http.StatusOK
}

func (g *GVisorRuntime) runscStatus(ctx context.Context, name string) string {
	out, err := g.runsc(ctx, "state", name)
	if err != nil {
		return ""
	}
	var s struct {
		Status string `json:"status"`
	}
	if json.Unmarshal([]byte(out), &s) != nil {
		return ""
	}
	return s.Status
}

func (g *GVisorRuntime) StopWorkspace(ctx context.Context, id string) error {
	name := containerName(id)
	st, err := g.readState(name)
	if err != nil {
		if os.IsNotExist(err) {
			// Still try a delete in case runsc knows the name without our
			// state file (crashed mid-create).
			_, _ = g.runsc(ctx, "delete", "-force", name)
			return nil
		}
		return err
	}
	// Graceful drain per §7: SIGTERM (checkpoint hook) → grace → SIGKILL.
	if g.runscStatus(ctx, name) == "running" {
		_, _ = g.runsc(ctx, "kill", name, "TERM")
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if s := g.runscStatus(ctx, name); s != "running" {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
		if g.runscStatus(ctx, name) == "running" {
			_, _ = g.runsc(ctx, "kill", name, "KILL")
		}
	}
	g.cleanup(ctx, st)
	g.mu.Lock()
	delete(g.cpuPrev, id)
	g.mu.Unlock()
	return nil
}

func (g *GVisorRuntime) Status(ctx context.Context) ([]WorkspaceStatus, error) {
	entries, err := os.ReadDir(filepath.Join(g.StateDir, "state"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var statuses []WorkspaceStatus
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".json")
		st, err := g.readState(name)
		if err != nil {
			continue
		}
		state := g.runscStatus(ctx, name)
		switch state {
		case "running", "created":
			// keep as-is
		case "":
			state = "exited"
		default:
			state = "exited"
		}
		statuses = append(statuses, WorkspaceStatus{
			ID: st.Workspace, Generation: st.Generation, State: state, Container: name,
		})
	}
	return statuses, nil
}

func (g *GVisorRuntime) agentIP(id string) (string, error) {
	st, err := g.readState(containerName(id))
	if err != nil {
		return "", fmt.Errorf("workspace %s: %w", id, err)
	}
	if st.IP == "" {
		return "", fmt.Errorf("workspace %s: sandbox has no network — start it with --network to reach the agent", id)
	}
	return st.IP, nil
}

func (g *GVisorRuntime) AgentEndpoint(_ context.Context, id string) (string, error) {
	ip, err := g.agentIP(id)
	if err != nil {
		return "", err
	}
	return ip + ":" + agentPort, nil
}

func (g *GVisorRuntime) TerminalEndpoint(_ context.Context, id string) (string, error) {
	ip, err := g.agentIP(id)
	if err != nil {
		return "", err
	}
	return ip + ":" + termPort, nil
}

// CPUActive implements ActivityProbes via `runsc events --stats` (one-shot
// cgroup-style stats), compared against the previous tick — same scheme as
// ContainerdRuntime.CPUActive.
func (g *GVisorRuntime) CPUActive(ctx context.Context, wsID string) (bool, error) {
	out, err := g.runsc(ctx, "events", "--stats", containerName(wsID))
	if err != nil {
		return true, fmt.Errorf("stats %s: %w", wsID, err)
	}
	var ev struct {
		Data struct {
			CPU struct {
				Usage struct {
					Total uint64 `json:"total"`
				} `json:"usage"`
			} `json:"cpu"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &ev); err != nil {
		return true, fmt.Errorf("stats %s: %w", wsID, err)
	}
	usageNS := ev.Data.CPU.Usage.Total

	now := time.Now()
	g.mu.Lock()
	if g.cpuPrev == nil {
		g.cpuPrev = map[string]cpuSample{}
	}
	prev, seen := g.cpuPrev[wsID]
	g.cpuPrev[wsID] = cpuSample{usageNS: usageNS, at: now}
	g.mu.Unlock()

	if !seen || !now.After(prev.at) || usageNS < prev.usageNS {
		return true, nil // no baseline yet — active until proven idle
	}
	pct := float64(usageNS-prev.usageNS) / float64(now.Sub(prev.at).Nanoseconds()) * 100
	floor := g.CPUFloorPercent
	if floor <= 0 {
		floor = 5.0
	}
	return pct >= floor, nil
}

// AgentBusy dials the sandbox's CNI IP from the host — same contract as the
// containerd runtime's probe.
func (g *GVisorRuntime) AgentBusy(ctx context.Context, wsID string) (bool, error) {
	ip, err := g.agentIP(wsID)
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
