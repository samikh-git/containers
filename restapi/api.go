// Package restapi exposes the router's workspace lifecycle over HTTP so a
// browser — or any HTTP client — can drive the local-mode data plane
// (DESIGN §4: code and control flow browser ↔ router, directly).
//
//	GET    /api/workspaces               list sandbox statuses (?metrics=1 for CPU/mem/disk/busy)
//	POST   /api/workspaces               provision (body: spec, "id" required)
//	POST   /api/workspaces/{id}/fanout   snapshot + n branches (body: {"n":5, …spec})
//	POST   /api/workspaces/{id}/down     stop, volume preserved
//	POST   /api/workspaces/{id}/hibernate graceful stop + snapshot + revoke
//	DELETE /api/workspaces/{id}          destroy workspace, snapshots, branches
//	GET    /api/usage                    cost/usage summary over the audit ledger
//	GET    /api/keys                     gateway routes -> key present?
//	PUT    /api/keys/{route}             set/rotate a provider key ({"key":"…"}; "" clears)
//	GET    /api/routes                   gateway route metadata + key status
//	GET    /api/defaults                 serve-flag defaults for provision UI
//	ANY    /api/workspaces/{id}/opencode/{path…}  reverse proxy to the
//	       sandbox's OpenCode server (chat sessions, SSE events, …)
//	GET    /api/workspaces/{id}/terminal  WebSocket → sandbox termbridge
//	       (web terminal; see terminal.go)
//	CRUD   /api/workspaces/{id}/files/{path…}  workspace file API for the
//	       web editor (see files.go)
//	GET    /healthz
//	GET    /                             end-user app (frontend/, embedded)
//	GET    /admin                        operator console (frontend admin.html, Kumo)
package restapi

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"containerization/dataplane"
)

// The end-user app and operator admin console (frontend/ builds into dist/
// via `npm run build`; vite's outDir points here). Embedded so the router
// remains a single binary.
//
//go:embed all:dist
var appDist embed.FS

// opTimeout matches the CLI's per-operation budget.
const opTimeout = 5 * time.Minute

// wsIDPattern keeps ids safe as dataset path components, container names and
// URL segments. Same alphabet the CLI implicitly assumes; the REST surface
// must enforce it because the caller is a browser, not an operator.
var wsIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// KeyAdmin manages provider keys at the policy gateway. Keys pass through the
// router to the gateway admin API; the router never stores them (§9).
type KeyAdmin interface {
	SetRouteKey(ctx context.Context, route, key string) error
	RouteKeyStatus(ctx context.Context) (map[string]bool, error)
}

// RouteLister is the optional KeyAdmin capability behind GET /api/routes:
// full route metadata (kind, upstream, model allowlist, key status — never
// key values), so the operator UI can render a provider panel.
type RouteLister interface {
	Routes(ctx context.Context) (json.RawMessage, error)
}

// EndpointResolver is the optional Runtime capability the OpenCode proxy
// needs: a host-dialable address for a workspace's agent server.
type EndpointResolver interface {
	AgentEndpoint(ctx context.Context, id string) (string, error)
}

// Server is the REST control surface over one Router. Mutating operations are
// serialized with a single mutex — the same one-at-a-time semantics the CLI
// gives, so concurrent browser clicks cannot interleave storage/lease steps.
type Server struct {
	Router *dataplane.Router
	// Keys is the gateway admin client for provider-key management; nil when
	// no gateway is configured (key endpoints then return 409).
	Keys KeyAdmin
	// Defaults fills spec fields a request omits (image, cpus, memory, quota,
	// profile, gateway, route, network) — the serve command's flags, exactly
	// as they'd default for the CLI.
	Defaults dataplane.WorkspaceSpec
	// Token, when set, is required as "Authorization: Bearer <Token>" on
	// every /api request. MANDATORY when the listener is not loopback-bound.
	Token string
	// TerminalActivity records web-terminal input for the idle monitor's
	// fourth condition (terminal.go); nil disables recording.
	TerminalActivity *dataplane.TerminalActivityLog
	// LedgerPath, when set, is the gateway audit JSONL used (with
	// TerminalActivity) to attach LastActivityAt on workspace list.
	LedgerPath string

	mu sync.Mutex
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/workspaces", s.auth(s.list))
	mux.HandleFunc("POST /api/workspaces", s.auth(s.up))
	mux.HandleFunc("POST /api/workspaces/{id}/fanout", s.auth(s.fanout))
	mux.HandleFunc("POST /api/workspaces/{id}/down", s.auth(s.down))
	mux.HandleFunc("POST /api/workspaces/{id}/hibernate", s.auth(s.hibernate))
	mux.HandleFunc("DELETE /api/workspaces/{id}", s.auth(s.destroy))
	mux.HandleFunc("GET /api/keys", s.auth(s.keyStatus))
	mux.HandleFunc("PUT /api/keys/{route}", s.auth(s.setKey))
	mux.HandleFunc("GET /api/routes", s.auth(s.routes))
	mux.HandleFunc("GET /api/defaults", s.auth(s.defaults))
	mux.HandleFunc("GET /api/usage", s.auth(s.usage))
	// No method in the pattern: OpenCode's API is GET/POST/DELETE and may
	// grow; the proxy forwards them all.
	mux.HandleFunc("/api/workspaces/{id}/opencode/{rest...}", s.auth(s.opencode))
	// Terminal auth is inside the handler: WebSocket clients can't set
	// headers, so the token may arrive as a query parameter (terminal.go).
	mux.HandleFunc("GET /api/workspaces/{id}/terminal", s.terminal)
	s.registerFileRoutes(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	// End-user app + admin console: static assets from the embedded vite
	// build. Admin is a separate MPA entry (admin.html); everything else
	// falls through to the hash-routed SPA (index.html).
	app, _ := fs.Sub(appDist, "dist")
	assets := http.FileServer(http.FS(app))
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, _ *http.Request) {
		page, err := fs.ReadFile(app, "admin.html")
		if err != nil {
			http.Error(w, "admin UI not built: run `npm run build` in frontend/", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	})
	// Method-less on purpose: a "GET /" pattern is ambiguous against the
	// method-less opencode proxy pattern above (ServeMux precedence).
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			assets.ServeHTTP(w, r)
			return
		}
		page, err := fs.ReadFile(app, "index.html")
		if err != nil {
			http.Error(w, "frontend not built: run `npm run build` in frontend/ before building the router", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	})
	return mux
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Token != "" && r.Header.Get("Authorization") != "Bearer "+s.Token {
			slog.Warn("api auth failed", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
			httpError(w, http.StatusUnauthorized, "authorization required")
			return
		}
		next(w, r)
	}
}

// specRequest is the JSON shape of a provision/fanout request. Zero-valued
// fields inherit Server.Defaults.
type specRequest struct {
	ID         string  `json:"id"`
	N          int     `json:"n"` // fanout only
	Image      string  `json:"image"`
	CPUs       float64 `json:"cpus"`
	MemoryMB   int64   `json:"memory_mb"`
	QuotaGB    int64   `json:"quota_gb"`
	ProfileDir string  `json:"profile_dir"`
	GatewayURL string  `json:"gateway_url"`
	Route      string  `json:"route"`
	Network    string  `json:"network"`
}

func (s *Server) buildSpec(req specRequest, id string) dataplane.WorkspaceSpec {
	spec := s.Defaults
	spec.ID = id
	if req.Image != "" {
		spec.Image = req.Image
	}
	if req.CPUs > 0 {
		spec.CPUs = req.CPUs
	}
	if req.MemoryMB > 0 {
		spec.MemoryMB = req.MemoryMB
	}
	if req.QuotaGB > 0 {
		spec.QuotaGB = req.QuotaGB
	}
	if req.ProfileDir != "" {
		spec.ProfileDir = req.ProfileDir
	}
	if req.GatewayURL != "" {
		spec.GatewayURL = req.GatewayURL
	}
	if req.Route != "" {
		spec.Route = req.Route
	}
	if req.Network != "" {
		spec.Network = req.Network
	}
	return spec
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	statuses, err := s.Router.Runtime.Status(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if statuses == nil {
		statuses = []dataplane.WorkspaceStatus{}
	}
	// Full metrics (docker stats, /busy, disk) are opt-in: the end-user
	// home page polls this endpoint every few seconds and must stay snappy.
	if r.URL.Query().Get("metrics") == "1" || r.URL.Query().Get("metrics") == "true" {
		opt := dataplane.MetricsOptions{LedgerPath: s.LedgerPath}
		if s.TerminalActivity != nil {
			opt.TerminalActivityPath = s.TerminalActivity.Path
		}
		dataplane.EnrichMetrics(r.Context(), s.Router.Storage, statuses, opt)
	}
	writeJSON(w, http.StatusOK, statuses)
}

func (s *Server) defaults(w http.ResponseWriter, _ *http.Request) {
	d := s.Defaults
	writeJSON(w, http.StatusOK, map[string]any{
		"image":       d.Image,
		"cpus":        d.CPUs,
		"memory_mb":   d.MemoryMB,
		"quota_gb":    d.QuotaGB,
		"profile_dir": d.ProfileDir,
		"gateway_url": d.GatewayURL,
		"route":       d.Route,
		"network":     d.Network,
	})
}

func (s *Server) up(w http.ResponseWriter, r *http.Request) {
	var req specRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if !wsIDPattern.MatchString(req.ID) {
		httpError(w, http.StatusBadRequest, "id must match "+wsIDPattern.String())
		return
	}
	ctx, cancel := opContext(r)
	defer cancel()

	s.mu.Lock()
	mount, err := s.Router.Up(ctx, s.buildSpec(req, req.ID))
	s.mu.Unlock()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": req.ID, "mount": mount})
}

func (s *Server) fanout(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req specRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.N < 1 || req.N > 100 {
		httpError(w, http.StatusBadRequest, "n must be between 1 and 100")
		return
	}
	ctx, cancel := opContext(r)
	defer cancel()

	s.mu.Lock()
	branches, err := s.Router.FanOut(ctx, s.buildSpec(req, id), req.N)
	s.mu.Unlock()
	if branches == nil {
		branches = []string{}
	}
	if err != nil {
		// FanOut is not atomic: report what did come up alongside the error.
		slog.Error("fanout failed", "workspace", id, "err", err, "branches_up", len(branches))
		writeJSON(w, http.StatusInternalServerError,
			map[string]any{"error": err.Error(), "branches": branches})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"branches": branches})
}

func (s *Server) down(w http.ResponseWriter, r *http.Request) {
	s.stopOp(w, r, s.Router.Down)
}

func (s *Server) hibernate(w http.ResponseWriter, r *http.Request) {
	s.stopOp(w, r, s.Router.Hibernate)
}

func (s *Server) stopOp(w http.ResponseWriter, r *http.Request,
	op func(context.Context, string) error) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx, cancel := opContext(r)
	defer cancel()

	s.mu.Lock()
	err := op(ctx, id)
	s.mu.Unlock()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

func opContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), opTimeout)
}

func (s *Server) destroy(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx, cancel := opContext(r)
	defer cancel()

	s.mu.Lock()
	err := s.Router.Destroy(ctx, id)
	s.mu.Unlock()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) keyStatus(w http.ResponseWriter, r *http.Request) {
	if s.Keys == nil {
		httpError(w, http.StatusConflict, "no policy gateway configured (start serve with --gateway)")
		return
	}
	status, err := s.Keys.RouteKeyStatus(r.Context())
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) routes(w http.ResponseWriter, r *http.Request) {
	lister, ok := s.Keys.(RouteLister)
	if s.Keys == nil || !ok {
		httpError(w, http.StatusConflict, "no policy gateway configured (start serve with --gateway)")
		return
	}
	raw, err := lister.Routes(r.Context())
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(raw)
}

func (s *Server) setKey(w http.ResponseWriter, r *http.Request) {
	if s.Keys == nil {
		httpError(w, http.StatusConflict, "no policy gateway configured (start serve with --gateway)")
		return
	}
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := s.Keys.SetRouteKey(r.Context(), r.PathValue("route"), req.Key); err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// opencode reverse-proxies everything under /api/workspaces/{id}/opencode/
// to the workspace sandbox's OpenCode server. Streaming (SSE /event) passes
// through unbuffered. No opTimeout here: event streams are long-lived by
// design; lifetime is the client's connection.
func (s *Server) opencode(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	resolver, ok := s.Router.Runtime.(EndpointResolver)
	if !ok {
		httpError(w, http.StatusNotImplemented, "runtime cannot resolve agent endpoints (running with --runtime none?)")
		return
	}
	endpoint, err := resolver.AgentEndpoint(r.Context(), id)
	if err != nil {
		slog.Error("opencode endpoint resolve failed", "workspace", id, "err", err)
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	target := &url.URL{Scheme: "http", Host: endpoint}
	rest := r.PathValue("rest")
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = "/" + rest
			pr.Out.URL.RawPath = ""
			pr.Out.Host = target.Host
			// The router API token authenticates to THIS server; it must not
			// leak into the sandbox.
			pr.Out.Header.Del("Authorization")
		},
		FlushInterval: -1, // flush immediately: OpenCode's /event is SSE
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			slog.Error("opencode proxy failed",
				"workspace", id, "endpoint", endpoint, "path", rest, "err", err)
			httpError(w, http.StatusBadGateway, "opencode unreachable: "+err.Error())
		},
	}
	slog.Debug("opencode proxy", "workspace", id, "endpoint", endpoint, "method", r.Method, "path", rest)
	proxy.ServeHTTP(w, r)
}

func pathID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !wsIDPattern.MatchString(id) {
		httpError(w, http.StatusBadRequest, "id must match "+wsIDPattern.String())
		return "", false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status int, msg string) {
	if status >= 500 {
		slog.Error("api error", "status", status, "error", msg)
	} else if status >= 400 {
		slog.Warn("api error", "status", status, "error", msg)
	}
	writeJSON(w, status, map[string]string{"error": msg})
}
