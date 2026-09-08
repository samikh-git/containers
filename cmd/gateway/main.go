// Command gateway runs the policy enforcement point (DESIGN §9): the single
// door between agent sandboxes and model endpoints. Provider keys live in
// THIS process's environment — never in sandboxes.
//
//	gateway --config gateway.json --ledger audit.jsonl
//	gateway --verify audit.jsonl        # check the hash chain and exit
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"containerization/gateway"
	"containerization/internal/applog"
)

func main() {
	config := flag.String("config", "gateway.json", "gateway config file")
	ledgerPath := flag.String("ledger", "audit.jsonl", "audit ledger file (JSONL, hash-chained)")
	verify := flag.String("verify", "", "verify a ledger file's hash chain and exit")
	admin := flag.String("admin", "127.0.0.1:8444", "admin API listen address (sessions register/revoke; keep on localhost)")
	logLevel := flag.String("log-level", envOr("LOG_LEVEL", "info"), "log level: debug|info|warn|error")
	flag.Parse()
	applog.Configure(*logLevel)

	if *verify != "" {
		n, err := gateway.Verify(*verify)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ledger INVALID after %d events: %v\n", n, err)
			os.Exit(1)
		}
		fmt.Printf("ledger OK: %d events, chain intact\n", n)
		return
	}

	cfg, err := gateway.LoadConfig(*config)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	for name, r := range cfg.Routes {
		if r.KeyEnv != "" && os.Getenv(r.KeyEnv) == "" {
			fmt.Fprintf(os.Stderr, "warning: route %q: %s is not set — requests on it will fail upstream auth\n", name, r.KeyEnv)
		}
	}

	ledger, err := gateway.OpenLedger(*ledgerPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	defer ledger.Close()

	srv := gateway.NewServer(cfg, ledger)
	listen := cfg.Listen
	if listen == "" {
		listen = ":8443"
	}

	adminToken := os.Getenv("GATEWAY_ADMIN_TOKEN")
	if adminToken == "" && !strings.HasPrefix(*admin, "127.0.0.1:") && !strings.HasPrefix(*admin, "localhost:") {
		fmt.Fprintln(os.Stderr, "error: admin listener is not loopback-bound; GATEWAY_ADMIN_TOKEN is required")
		os.Exit(1)
	}
	go func() {
		fmt.Printf("admin API on %s\n", *admin)
		adminSrv := &http.Server{
			Addr:              *admin,
			Handler:           &gateway.AdminHandler{Server: srv, Token: adminToken},
			ReadHeaderTimeout: 20 * time.Second,
		}
		if err := adminSrv.ListenAndServe(); err != nil {
			fmt.Fprintln(os.Stderr, "admin listener error:", err)
			os.Exit(1)
		}
	}()

	fmt.Printf("policy gateway listening on %s (%d routes, %d seed sessions), ledger at %s\n",
		listen, len(cfg.Routes), len(cfg.Sessions), *ledgerPath)
	// ReadHeaderTimeout, not ReadTimeout: this listener faces the sandboxes,
	// so a stalled handshake must not pin a connection, but a long streaming
	// completion must not be cut off either.
	gw := &http.Server{Addr: listen, Handler: srv, ReadHeaderTimeout: 20 * time.Second}
	if err := gw.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
