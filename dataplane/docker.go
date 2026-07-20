package dataplane

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// DockerRuntime runs sandboxes via the docker CLI. Exec-based on purpose for
// v1: zero dependencies, trivially swappable for the SDK or containerd later
// behind the Runtime interface (§15). Security posture per DESIGN §8:
// read-only rootfs, all capabilities dropped, no-new-privileges, tmpfs /tmp,
// no network by default (the gateway/egress proxy wiring lands with §9).
type DockerRuntime struct {
	// OCIRuntime is "runsc" inside the data plane image; empty ("") uses the
	// Docker default (runc) for dev hosts without gVisor.
	OCIRuntime string
}

const (
	labelWS  = "platform.workspace-id"
	labelGen = "platform.generation"

	// agentPort is where OpenCode serves inside every sandbox (§7/§8; the
	// image EXPOSEs it and the entrypoint binds it).
	agentPort = "4321"
	// termPort is the sandbox's termbridge (web terminal PTY bridge) — same
	// reachability rules as agentPort.
	termPort = "4322"
)

func containerName(wsID string) string { return "ws-" + wsID }

// networkOrNone: empty network means NO network — the strictest default is
// the default (§8). A real network must be asked for explicitly.
func networkOrNone(n string) string {
	if n == "" {
		return "none"
	}
	return n
}

func docker(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", strings.Join(args[:min(len(args), 3)], " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (r *DockerRuntime) EnsureWorkspace(ctx context.Context, spec WorkspaceSpec, mountPath string) error {
	args := []string{
		"run", "-d",
		"--name", containerName(spec.ID),
		"--label", labelWS + "=" + spec.ID,
		"--label", labelGen + "=" + strconv.FormatUint(spec.Generation, 10),
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--tmpfs", "/tmp:size=512m,mode=1777",
		"--network", networkOrNone(spec.Network),
		"--pids-limit", "512",
		"--mount", fmt.Sprintf("type=bind,src=%s,dst=/workspace", mountPath),
	}
	if r.OCIRuntime != "" {
		args = append(args, "--runtime", r.OCIRuntime)
	}
	if spec.CPUs > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(spec.CPUs, 'f', -1, 64))
	}
	if spec.MemoryMB > 0 {
		args = append(args, "--memory", fmt.Sprintf("%dm", spec.MemoryMB))
	}
	if spec.ProfileDir != "" {
		args = append(args, "--mount",
			fmt.Sprintf("type=bind,src=%s,dst=/etc/agent-profile,readonly", spec.ProfileDir))
	}
	if spec.GatewayURL != "" {
		args = append(args, "--env", "OPENCODE_BASE_URL="+spec.GatewayURL)
	}
	if spec.SessionToken != "" {
		args = append(args, "--env", "OPENCODE_SESSION_TOKEN="+spec.SessionToken)
	}
	// Publish the agent port on loopback so the router (and through it, the
	// browser UI) can reach OpenCode. Loopback-only: this is the dev-path
	// analogue of the router's veth (§8), not an exposure of the sandbox.
	// Internal networks cannot publish ports — there the router dials the
	// container IP directly (AgentEndpoint), which works on a native Linux
	// daemon. Docker Desktop for Mac cannot reach container IPs, so a sandbox
	// that should be browser-reachable in dev needs a non-internal network.
	if spec.Network != "" && !r.networkInternal(ctx, spec.Network) {
		args = append(args, "-p", "127.0.0.1::"+agentPort, "-p", "127.0.0.1::"+termPort)
	}
	args = append(args, spec.Image)

	warmPaths := []string{mountPath}
	if spec.ProfileDir != "" {
		warmPaths = append(warmPaths, spec.ProfileDir)
	}
	// Optimistic create: the common path (Up has already stopped any prior
	// holder) is a single daemon round trip. Idempotency lives in the
	// conflict fallback — same generation already running → done; anything
	// else under the name → replace and retry once.
	_, err := dockerRunWithMountFallback(ctx, spec.Image, warmPaths, args)
	if err == nil || !strings.Contains(err.Error(), "is already in use") {
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
	_, err = dockerRunWithMountFallback(ctx, spec.Image, warmPaths, args)
	return err
}

// dockerRunWithMountFallback handles a Docker Desktop for Mac quirk: a
// strict "--mount type=bind" source that was JUST created (e.g. by
// os.MkdirAll a moment earlier) can be reported "does not exist" by the
// daemon — and stays that way indefinitely; empirically this does not
// resolve with waiting (still failing after 60s of retries in testing). A
// short-syntax "-v host:container" bind against the IDENTICAL path succeeds
// immediately and, crucially, unblocks a subsequent strict "--mount" against
// the same path (verified reliably across repeated fresh delete+recreate
// cycles). So: try the real run first; only on that specific failure, warm
// each bind-mounted host path with one throwaway "-v" run, then retry once.
//
// This never fires on the actual production target (DESIGN §7): a native
// Linux daemon talks to the filesystem directly, no VM/virtiofs boundary in
// the way, so the first attempt just succeeds and this fallback never runs.
func dockerRunWithMountFallback(ctx context.Context, image string, hostPaths []string, args []string) (string, error) {
	out, err := docker(ctx, args...)
	if err == nil || !strings.Contains(err.Error(), "bind source path does not exist") {
		return out, err
	}
	fmt.Fprintln(os.Stderr, "note: Docker Desktop hasn't synced a freshly created bind-mount path yet — warming it up...")
	for _, p := range hostPaths {
		if _, werr := docker(ctx, "run", "--rm", "--entrypoint", "true", "-v", p+":/warm", image); werr != nil {
			return out, fmt.Errorf("mount warm-up for %s: %w (original error: %v)", p, werr, err)
		}
	}
	return docker(ctx, args...)
}

// networkInternal reports whether a docker network was created --internal.
// Unknown networks count as internal: failing closed means we never ask the
// daemon for a port publish it would reject the whole run over.
func (r *DockerRuntime) networkInternal(ctx context.Context, network string) bool {
	out, err := docker(ctx, "network", "inspect", "-f", "{{.Internal}}", network)
	if err != nil {
		return true
	}
	return out != "false"
}

// AgentEndpoint returns a host-dialable "host:port" for the workspace's
// OpenCode server: the loopback-published port when one exists (the Docker
// Desktop dev path), otherwise the container IP (native Linux daemon, where
// the host can reach sandbox IPs directly — including on internal networks).
func (r *DockerRuntime) AgentEndpoint(ctx context.Context, id string) (string, error) {
	if out, err := docker(ctx, "port", containerName(id), agentPort+"/tcp"); err == nil && out != "" {
		return strings.TrimSpace(strings.Split(out, "\n")[0]), nil
	}
	out, err := docker(ctx, "inspect", "-f",
		`{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}`, containerName(id))
	if err != nil {
		return "", fmt.Errorf("workspace %s: %w", id, err)
	}
	if ip, _, _ := strings.Cut(strings.TrimSpace(out), " "); ip != "" {
		return ip + ":" + agentPort, nil
	}
	return "", fmt.Errorf("workspace %s: sandbox has no network — start it with --network to reach the agent", id)
}

// TerminalEndpoint is AgentEndpoint for the termbridge port (web terminal).
func (r *DockerRuntime) TerminalEndpoint(ctx context.Context, id string) (string, error) {
	if out, err := docker(ctx, "port", containerName(id), termPort+"/tcp"); err == nil && out != "" {
		return strings.TrimSpace(strings.Split(out, "\n")[0]), nil
	}
	out, err := docker(ctx, "inspect", "-f",
		`{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}`, containerName(id))
	if err != nil {
		return "", fmt.Errorf("workspace %s: %w", id, err)
	}
	if ip, _, _ := strings.Cut(strings.TrimSpace(out), " "); ip != "" {
		return ip + ":" + termPort, nil
	}
	return "", fmt.Errorf("workspace %s: sandbox has no network — start it with --network to reach the terminal", id)
}

func (r *DockerRuntime) StopWorkspace(ctx context.Context, id string) error {
	// Graceful drain per §7: SIGTERM (checkpoint hook) → grace → SIGKILL.
	if _, err := docker(ctx, "stop", "--time", "30", containerName(id)); err != nil {
		// Container may not exist; removal below is what must succeed.
		_ = err
	}
	_, err := docker(ctx, "rm", "-f", containerName(id))
	if err != nil && strings.Contains(err.Error(), "No such container") {
		return nil
	}
	return err
}

func (r *DockerRuntime) Status(ctx context.Context) ([]WorkspaceStatus, error) {
	out, err := docker(ctx, "ps", "-a",
		"--filter", "label="+labelWS,
		"--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	var statuses []WorkspaceStatus
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row struct {
			Names  string `json:"Names"`
			State  string `json:"State"`
			Labels string `json:"Labels"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		st := WorkspaceStatus{State: row.State, Container: row.Names}
		for _, kv := range strings.Split(row.Labels, ",") {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			switch k {
			case labelWS:
				st.ID = v
			case labelGen:
				st.Generation, _ = strconv.ParseUint(v, 10, 64)
			}
		}
		if st.ID != "" {
			statuses = append(statuses, st)
		}
	}
	return statuses, nil
}
