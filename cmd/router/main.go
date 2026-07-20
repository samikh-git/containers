// Command router is the local-mode data plane CLI (DESIGN §3, §4): storage,
// lease fencing and sandboxes on one machine, no control plane.
//
//	router up      --id ws1 [--image opencode-sandbox:v1]
//	router fanout  --id ws1 --n 5
//	router status
//	router down    --id ws1
//	router destroy --id ws1
//	router serve   [--listen 127.0.0.1:8400]   REST API + browser UI
//
// Backends:
//
//	--storage zfs --pool tank      production (inside the data plane image)
//	--storage dir --root <path>    development (macOS or any host)
//	--runtime runsc|runc|none      gVisor, plain Docker, or storage-only
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"containerization/dataplane"
	"containerization/restapi"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]

	// `pool` takes a subcommand before its flags: router pool fill --n 4 …
	poolCmd := ""
	if cmd == "pool" {
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "usage: router pool {fill|status|drain} [flags]")
			os.Exit(2)
		}
		poolCmd, args = args[0], args[1:]
	}

	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	var (
		id        = fs.String("id", "", "workspace id")
		n         = fs.Int("n", 3, "number of agent branches (fanout)")
		image     = fs.String("image", "opencode-sandbox:v1", "sandbox image")
		storage   = fs.String("storage", defaultStorage(), "storage backend: zfs|dir")
		pool      = fs.String("pool", "tank", "zfs pool (storage=zfs)")
		root      = fs.String("root", defaultRoot(), "data root (storage=dir)")
		rt        = fs.String("runtime", defaultRuntime(), "sandbox runtime: runsc|runc|gvisor|containerd|containerd-runc|none (gvisor = raw runsc + warm pool; containerd*/gvisor linux only)")
		cpus      = fs.Float64("cpus", 2, "cpu limit per sandbox")
		memMB     = fs.Int64("memory-mb", 2048, "memory limit per sandbox (MB)")
		quotaGB   = fs.Int64("quota-gb", 10, "volume quota (GB, zfs only)")
		profile   = fs.String("profile", "", "capability profile dir (mounted ro)")
		gateway   = fs.String("gateway", "", "policy gateway URL (OPENCODE_BASE_URL)")
		gwAdmin   = fs.String("gateway-admin", "http://127.0.0.1:8444", "gateway admin API URL (session register/revoke)")
		route     = fs.String("route", "", "gateway route(s) = model tier(s); comma-separated for per-model routing (e.g. tier-a-anthropic,tier-b-openrouter)")
		network   = fs.String("network", "", "docker network for sandboxes (create with --internal; empty = no network)")
		idleAfter = fs.Duration("idle", 10*time.Minute, "watch: hibernate after this long fully idle")
		interval  = fs.Duration("interval", 30*time.Second, "watch: probe interval")
		ledger    = fs.String("ledger", "", "gateway audit ledger path — watch: model-traffic activity; serve: LastActivityAt + /api/usage cost metrics")
		listen    = fs.String("listen", "127.0.0.1:8400", "serve: REST API listen address")
		activity  = fs.String("activity", "", "terminal activity JSONL — serve appends, watch reads (default <root>/terminal-activity.jsonl)")
		poolN     = fs.Int("pool-n", 2, "pool fill / serve auto-refill: target ready warm slots (0 disables serve auto-refill)")
	)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *activity == "" {
		*activity = filepath.Join(*root, "terminal-activity.jsonl")
	}

	router := &dataplane.Router{
		Storage: buildStorage(*storage, *pool, *root),
		Leases:  &dataplane.FileLeaseAuthority{Path: filepath.Join(*root, "leases.json")},
		Runtime: buildRuntime(*rt, *root),
	}
	if *gateway != "" {
		router.Sessions = dataplane.NewGatewayAdminClient(*gwAdmin, os.Getenv("GATEWAY_ADMIN_TOKEN"))
	}

	spec := dataplane.WorkspaceSpec{
		ID: *id, Image: *image,
		CPUs: *cpus, MemoryMB: *memMB, QuotaGB: *quotaGB,
		ProfileDir: *profile, GatewayURL: *gateway,
		Route: *route, Network: *network,
	}

	// Warm pool: only the gvisor runtime can checkpoint/restore. The pool
	// is wired whenever that runtime is selected — Claim on an empty pool
	// is a cheap no, so this costs nothing when unfilled.
	var warmPool *dataplane.CheckpointPool
	if gv, ok := router.Runtime.(*dataplane.GVisorRuntime); ok {
		warmPool = &dataplane.CheckpointPool{
			Dir:      filepath.Join(*root, "warmpool"),
			Storage:  router.Storage,
			Runtime:  gv,
			Template: spec, // image/profile/gateway/network identity for slots
		}
		router.Pool = warmPool
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	switch cmd {
	case "up":
		requireID(*id)
		mount, err := router.Up(ctx, spec)
		exitOn(err)
		fmt.Printf("workspace %s up, volume at %s\n", *id, mount)

	case "fanout":
		requireID(*id)
		start := time.Now()
		branches, err := router.FanOut(ctx, spec, *n)
		for _, b := range branches {
			fmt.Printf("branch %s up\n", b)
		}
		exitOn(err)
		fmt.Printf("%d branches in %s\n", len(branches), time.Since(start).Round(time.Millisecond))

	case "status":
		statuses, err := router.Runtime.Status(ctx)
		exitOn(err)
		if len(statuses) == 0 {
			fmt.Println("no sandboxes")
			return
		}
		for _, s := range statuses {
			fmt.Printf("%-24s gen=%-4d %-10s %s\n", s.ID, s.Generation, s.State, s.Container)
		}

	case "down":
		requireID(*id)
		exitOn(router.Down(ctx, *id))
		fmt.Printf("workspace %s stopped (volume preserved)\n", *id)

	case "watch":
		// Idle detection & scale-to-zero (DESIGN §7): three-condition
		// check, hibernate on a fully idle window. Runs until interrupted.
		// The containerd runtime probes itself (metrics API, no process
		// spawn); the docker runtimes shell out via DockerProbes.
		var probes dataplane.ActivityProbes = &dataplane.DockerProbes{}
		if p, ok := router.Runtime.(dataplane.ActivityProbes); ok {
			probes = p
		}
		monitor := dataplane.NewIdleMonitor(router, probes, *idleAfter, *ledger)
		monitor.TerminalActivityPath = *activity
		fmt.Printf("watching: hibernate after %s idle, probing every %s\n", *idleAfter, *interval)
		watchCtx := context.Background() // not the 5-minute op timeout
		exitOn(monitor.Watch(watchCtx, *interval))

	case "destroy":
		requireID(*id)
		exitOn(router.Destroy(ctx, *id))
		fmt.Printf("workspace %s destroyed\n", *id)

	case "pool":
		// Warm-start pool management (PERFORMANCE.md): pre-booted sandbox
		// checkpoints restored in ~200ms at `up` time. gvisor runtime only.
		if warmPool == nil {
			fmt.Fprintln(os.Stderr, "the warm pool requires --runtime gvisor")
			os.Exit(2)
		}
		switch poolCmd {
		case "fill":
			// Slot boots wait on the agent coming up; allow more than the
			// default op timeout for larger pools.
			fillCtx, cancelFill := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancelFill()
			start := time.Now()
			built, err := warmPool.Fill(fillCtx, *poolN)
			fmt.Printf("%d slot(s) built in %s\n", built, time.Since(start).Round(time.Millisecond))
			exitOn(err)
		case "status":
			slots, err := warmPool.Status()
			exitOn(err)
			if len(slots) == 0 {
				fmt.Println("no warm slots")
				return
			}
			for _, s := range slots {
				fmt.Printf("%-10s image=%-24s network=%-8s built %s\n",
					s.ID, s.Image, s.Network, s.CreatedAt.Format(time.RFC3339))
			}
		case "drain":
			exitOn(warmPool.Drain(ctx))
			fmt.Println("pool drained")
		default:
			fmt.Fprintln(os.Stderr, "usage: router pool {fill|status|drain} [flags]")
			os.Exit(2)
		}

	case "serve":
		// REST API + embedded browser UI over the same router core. The
		// flag values above become the defaults for spec fields a request
		// omits, exactly as they'd default on the CLI.
		serveCtx, stopServe := context.WithCancel(context.Background())
		defer stopServe()
		if warmPool != nil && *poolN > 0 {
			// Keep warm inventory topped up so UI/API creates stay on the
			// restore path. CLI `up` does not enable this — only serve.
			warmPool.EnableAutoRefill(serveCtx, *poolN)
			defer warmPool.StopAutoRefill()
			fmt.Printf("warm pool auto-refill enabled (target %d)\n", *poolN)
		}
		srv := &restapi.Server{
			Router: router,
			Defaults: dataplane.WorkspaceSpec{
				Image: *image, CPUs: *cpus, MemoryMB: *memMB, QuotaGB: *quotaGB,
				ProfileDir: *profile, GatewayURL: *gateway,
				Route: *route, Network: *network,
			},
			Token:            os.Getenv("ROUTER_API_TOKEN"),
			TerminalActivity: &dataplane.TerminalActivityLog{Path: *activity},
			LedgerPath:       *ledger,
		}
		if *gateway != "" {
			srv.Keys = dataplane.NewGatewayAdminClient(*gwAdmin, os.Getenv("GATEWAY_ADMIN_TOKEN"))
		}
		if srv.Token == "" && !loopback(*listen) {
			fmt.Fprintln(os.Stderr, "refusing to serve on a non-loopback address without ROUTER_API_TOKEN")
			os.Exit(2)
		}
		fmt.Printf("REST API and web UI on http://%s\n", *listen)
		exitOn(http.ListenAndServe(*listen, srv.Handler()))

	default:
		usage()
	}
}

func buildStorage(kind, pool, root string) dataplane.Storage {
	switch kind {
	case "zfs":
		return &dataplane.ZFSStorage{Pool: pool}
	case "dir":
		return &dataplane.DirStorage{Root: root}
	default:
		fmt.Fprintf(os.Stderr, "unknown storage backend %q\n", kind)
		os.Exit(2)
		return nil
	}
}

func buildRuntime(kind, root string) dataplane.Runtime {
	switch kind {
	case "runsc":
		return &dataplane.DockerRuntime{OCIRuntime: "runsc"}
	case "runc":
		return &dataplane.DockerRuntime{}
	case "gvisor":
		// Raw runsc against OCI bundles — no dockerd, no containerd task.
		// The only runtime with checkpoint/restore, hence the only one the
		// warm pool works with; also the fastest cold boot (no snapshot
		// per container: one shared read-only rootfs view).
		return &dataplane.GVisorRuntime{StateDir: filepath.Join(root, "gvisor")}
	case "containerd":
		// Direct containerd client, gVisor via the runc shim's BinaryName
		// override (the dedicated runsc shim hangs against containerd 2.x —
		// see dataplane.RuncShim). No docker CLI or dockerd hop
		// (PERFORMANCE.md item 2).
		return &dataplane.ContainerdRuntime{OCIBinary: dataplane.RunscBinary}
	case "containerd-runc":
		return &dataplane.ContainerdRuntime{}
	case "none":
		return dataplane.NoneRuntime{}
	default:
		fmt.Fprintf(os.Stderr, "unknown runtime %q\n", kind)
		os.Exit(2)
		return nil
	}
}

// Dev-friendly defaults: ZFS+runsc inside the Linux data plane image, plain
// directories and no gVisor elsewhere.
func defaultStorage() string {
	if _, err := os.Stat("/sbin/zfs"); err == nil {
		return "zfs"
	}
	return "dir"
}

func defaultRuntime() string {
	if _, err := os.Stat("/usr/local/bin/runsc"); err == nil {
		return "runsc"
	}
	return "runc"
}

func defaultRoot() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".workspace-data")
}

// loopback reports whether addr binds only to a loopback interface. Anything
// else (0.0.0.0, a LAN address, a bare ":port") is reachable off-host and
// must carry ROUTER_API_TOKEN — same rule as the gateway admin listener.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func requireID(id string) {
	if id == "" {
		fmt.Fprintln(os.Stderr, "--id is required")
		os.Exit(2)
	}
}

func exitOn(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: router {up|fanout|status|down|destroy|watch|serve|pool} [flags]")
	os.Exit(2)
}
