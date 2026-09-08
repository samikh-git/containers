package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"containerization/internal/applog"
)

// Route is one model upstream: a §9 tier decision made concrete. Kind
// selects the auth convention, not a translation layer — the gateway speaks
// exactly two wire conventions and translates nothing.
type Route struct {
	Kind          string   `json:"kind"`     // "anthropic" | "openai"
	BaseURL       string   `json:"base_url"` // e.g. https://api.anthropic.com, http://ollama:11434/v1
	KeyEnv        string   `json:"key_env"`  // env var holding the provider key (host side only)
	AllowedModels []string `json:"allowed_models,omitempty"`
	MaxRequestMB  int64    `json:"max_request_mb,omitempty"` // default 20
	// AllowedPaths bounds which upstream endpoints a session may reach.
	// Trailing "*" wildcards, same syntax as AllowedModels. Empty means the
	// kind's default set (defaultPaths) — inference only. This matters
	// because the gateway attaches the ORGANIZATION's provider key: without
	// it, a sandbox token is also a key for the provider's account
	// management, batch and file APIs, which is a much bigger grant than
	// "this workspace may talk to a model".
	AllowedPaths []string `json:"allowed_paths,omitempty"`
	// MaxRequestsPerMinute caps how fast one workspace may spend against
	// this route. 0 uses defaultRPM; a negative value disables the cap.
	// Spend is the real exposure of a leaked session token, and a cap is
	// the difference between a bounded incident and an unbounded bill.
	MaxRequestsPerMinute int `json:"max_requests_per_minute,omitempty"`
}

// defaultRPM is the per-workspace, per-route request ceiling when a route
// does not set one. High enough that an agent doing real work never notices,
// low enough that a stolen token cannot drain an account overnight.
const defaultRPM = 120

// defaultPaths are the inference endpoints of both conventions. Kind is not
// the discriminator here: it selects the auth header, not the API surface,
// and an OpenAI-kind upstream like OpenRouter serves the Anthropic-shaped
// /v1/messages too. What this list excludes is the point — account
// administration, batches, files, anything that spends or reveals beyond a
// single completion. A route that legitimately fronts more says so with
// allowed_paths.
var defaultPaths = []string{
	"/v1/messages", "/v1/messages/*",
	"/v1/chat/completions", "/v1/completions", "/v1/responses",
	"/v1/embeddings", "/v1/complete",
	"/v1/models", "/v1/models/*",
}

// pathAllowed vets the request path against the route's allowlist.
func (r Route) pathAllowed(p string) bool {
	allowed := r.AllowedPaths
	if len(allowed) == 0 {
		allowed = defaultPaths
	}
	for _, a := range allowed {
		if prefix, ok := strings.CutSuffix(a, "*"); ok {
			if strings.HasPrefix(p, prefix) {
				return true
			}
		} else if a == p {
			return true
		}
	}
	return false
}

// Session maps a sandbox-held token to a workspace and the routes it may use.
// The token is the only credential inside a sandbox; it is worthless anywhere
// but here. A workspace may carry more than one route (e.g. an Anthropic tier
// plus an OpenRouter tier): the gateway picks among them per request by
// matching the model (see selectRoute), so a single picker can offer models
// from several providers while each still forwards to its own upstream (§9,
// per-model tier policy within a workspace).
type Session struct {
	Workspace string `json:"workspace"`
	// Route is the legacy single-route field, kept so existing configs and
	// registrations keep working; Routes supersedes it when present.
	Route  string   `json:"route,omitempty"`
	Routes []string `json:"routes,omitempty"`
}

// routeSet returns the session's candidate routes in precedence order,
// honoring the legacy single Route field when Routes is empty.
func (sess Session) routeSet() []string {
	if len(sess.Routes) > 0 {
		return sess.Routes
	}
	if sess.Route != "" {
		return []string{sess.Route}
	}
	return nil
}

// readCap is the most permissive request-size cap among the session's
// candidate routes (MB). The selected route's own cap is enforced after the
// model is known; this only bounds the initial read.
func (sess Session) readCap(cfg *Config) int64 {
	var max int64
	for _, name := range sess.routeSet() {
		if r, ok := cfg.Routes[name]; ok && r.MaxRequestMB > max {
			max = r.MaxRequestMB
		}
	}
	if max <= 0 {
		max = 20
	}
	return max
}

// Config is the gateway's static configuration (local mode: a JSON file; in
// team/enterprise mode the same structure arrives from the control plane as
// desired state, DESIGN §5 model_routes).
type Config struct {
	Listen   string             `json:"listen"`
	Routes   map[string]Route   `json:"routes"`
	Sessions map[string]Session `json:"sessions"` // token -> session
}

func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	for name, r := range c.Routes {
		if r.Kind != "anthropic" && r.Kind != "openai" {
			return nil, fmt.Errorf("route %s: unknown kind %q (anthropic|openai)", name, r.Kind)
		}
		if r.BaseURL == "" {
			return nil, fmt.Errorf("route %s: base_url required", name)
		}
	}
	for tok, s := range c.Sessions {
		set := s.routeSet()
		if len(set) == 0 {
			return nil, fmt.Errorf("session %s…: no route", tok[:min(len(tok), 8)])
		}
		for _, name := range set {
			if _, ok := c.Routes[name]; !ok {
				return nil, fmt.Errorf("session %s…: route %q not defined", tok[:min(len(tok), 8)], name)
			}
		}
	}
	return &c, nil
}

// secretTripwires are best-effort outbound scanners (§9: a tripwire, not
// DLP — the real controls are tier choice and the provider agreement).
var secretTripwires = []*regexp.Regexp{
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),                   // AWS access key id
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`), // PEM private keys
	regexp.MustCompile(`ghp_[A-Za-z0-9]{36}`),                // GitHub PAT
	regexp.MustCompile(`sk-ant-[A-Za-z0-9-]{20,}`),           // Anthropic API key
	regexp.MustCompile(`xox[bap]-[0-9A-Za-z-]{10,}`),         // Slack tokens
}

// Server is the policy enforcement point: authenticate token → resolve
// route → policy checks → swap key → forward → record.
type Server struct {
	Config *Config
	Ledger *Ledger
	// Client used for upstream calls; overridable in tests.
	Client *http.Client

	mu       sync.RWMutex
	sessions map[string]Session // seeded from Config, mutated via AdminHandler
	keys     map[string]string  // route -> provider key set at runtime (admin API)
	limiter  *rateLimiter
}

func NewServer(cfg *Config, ledger *Ledger) *Server {
	sessions := make(map[string]Session, len(cfg.Sessions))
	for tok, s := range cfg.Sessions {
		sessions[tok] = s
	}
	return &Server{
		Config:   cfg,
		Ledger:   ledger,
		Client:   &http.Client{Timeout: 10 * time.Minute}, // model streams are long
		sessions: sessions,
		keys:     make(map[string]string),
		limiter:  newRateLimiter(),
	}
}

// SetRouteKey sets (or, with an empty key, clears) a route's provider key at
// runtime. Keys set here take precedence over the route's key_env, so a
// gateway can start with no key in its environment and receive one via the
// admin API — the key still never leaves this process (§9).
func (s *Server) SetRouteKey(route, key string) error {
	if _, ok := s.Config.Routes[route]; !ok {
		return fmt.Errorf("route %q not defined", route)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if key == "" {
		delete(s.keys, route)
	} else {
		s.keys[route] = key
	}
	return nil
}

// RouteInfo is the admin-facing view of one route: its configuration plus
// where its key comes from ("runtime", "env" or "" when absent). Never the
// key value itself — that stays inside this process (§9).
type RouteInfo struct {
	Kind          string   `json:"kind"`
	BaseURL       string   `json:"base_url"`
	KeyEnv        string   `json:"key_env,omitempty"`
	AllowedModels []string `json:"allowed_models,omitempty"`
	KeySet        bool     `json:"key_set"`
	KeySource     string   `json:"key_source,omitempty"` // "runtime" | "env"
}

// RoutesInfo reports every configured route with its key status, for the
// operator UI's provider-key panel.
func (s *Server) RoutesInfo() map[string]RouteInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]RouteInfo, len(s.Config.Routes))
	for name, r := range s.Config.Routes {
		info := RouteInfo{
			Kind: r.Kind, BaseURL: r.BaseURL,
			KeyEnv: r.KeyEnv, AllowedModels: r.AllowedModels,
		}
		if _, ok := s.keys[name]; ok {
			info.KeySource = "runtime"
		} else if r.KeyEnv != "" && os.Getenv(r.KeyEnv) != "" {
			info.KeySource = "env"
		}
		info.KeySet = info.KeySource != ""
		out[name] = info
	}
	return out
}

// RouteKeyStatus reports, per route, whether a usable key is present (runtime
// or environment). Never the key itself.
func (s *Server) RouteKeyStatus() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	status := make(map[string]bool, len(s.Config.Routes))
	for name, r := range s.Config.Routes {
		_, runtime := s.keys[name]
		status[name] = runtime || (r.KeyEnv != "" && os.Getenv(r.KeyEnv) != "")
	}
	return status
}

func (s *Server) routeKey(route string) string {
	s.mu.RLock()
	key, ok := s.keys[route]
	s.mu.RUnlock()
	if ok {
		return key
	}
	return os.Getenv(s.Config.Routes[route].KeyEnv)
}

// selectRoute picks which of a session's candidate routes forwards a request,
// by matching the request's model against each route's allowlist in order. A
// route with a non-empty allowlist wins as soon as the model matches; a route
// with an empty allowlist is a catch-all and becomes the fallback (lowest
// precedence), so a workspace can pin specific models to one provider and send
// everything else to another. ok=false means no candidate accepts the model —
// the caller turns that into a 403, preserving the single-route allowlist block.
func (s *Server) selectRoute(sess Session, model string) (string, Route, bool) {
	var fallback string
	for _, name := range sess.routeSet() {
		r, ok := s.Config.Routes[name]
		if !ok {
			continue
		}
		if len(r.AllowedModels) == 0 {
			if fallback == "" {
				fallback = name
			}
			continue
		}
		if modelAllowed(r.AllowedModels, model) {
			return name, r, true
		}
	}
	if fallback != "" {
		return fallback, s.Config.Routes[fallback], true
	}
	return "", Route{}, false
}

func (s *Server) lookupSession(token string) (Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[token]
	return sess, ok
}

// AddSession registers a token. The route must exist; sessions are the only
// mutable state the gateway holds, and revocation must be instant (§10's
// "pull it fleet-wide in one action" applies to model access too).
func (s *Server) AddSession(token string, sess Session) error {
	set := sess.routeSet()
	if len(set) == 0 {
		return fmt.Errorf("session has no route")
	}
	for _, name := range set {
		if _, ok := s.Config.Routes[name]; !ok {
			return fmt.Errorf("route %q not defined", name)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[token] = sess
	return nil
}

func (s *Server) RemoveSession(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}

// RemoveWorkspaceSessions revokes every token of a workspace and returns
// how many were revoked.
func (s *Server) RemoveWorkspaceSessions(workspace string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for tok, sess := range s.sessions {
		if sess.Workspace == workspace {
			delete(s.sessions, tok)
			n++
		}
	}
	return n
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		return
	}

	token := bearerOrAPIKey(r)
	sess, ok := s.lookupSession(token)
	if !ok {
		reason := "unknown session token"
		if token == "" {
			reason = "missing session token"
		}
		slog.Warn("gateway auth failed",
			"reason", reason,
			"token", applog.TokenPrefix(token),
			"method", r.Method,
			"path", r.URL.Path,
			"remote", r.RemoteAddr,
			"sessions", s.sessionCount(),
		)
		s.record(Event{Outcome: "auth_failed", Reason: reason, Status: http.StatusUnauthorized})
		http.Error(w, "unknown session token", http.StatusUnauthorized)
		return
	}
	ev := Event{Workspace: sess.Workspace}

	// Bounded read under the most permissive candidate cap; the selected
	// route's own cap is enforced below, once we know which route forwards it.
	body, err := io.ReadAll(io.LimitReader(r.Body, sess.readCap(s.Config)<<20+1))
	if err != nil {
		slog.Warn("gateway read error", "workspace", sess.Workspace, "err", err)
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	// Route selection is per-model: the workspace may carry several tiers and
	// the request's model decides which one forwards it. No accepting route is
	// itself a policy event (§9).
	ev.Model = modelOf(body)
	routeName, route, ok := s.selectRoute(sess, ev.Model)
	if !ok {
		ev.Outcome, ev.Reason, ev.Status = "blocked", "model not accepted by any route", http.StatusForbidden
		s.record(ev)
		slog.Warn("gateway blocked request",
			"workspace", sess.Workspace, "model", ev.Model, "reason", ev.Reason, "status", ev.Status)
		http.Error(w, fmt.Sprintf("model %q not allowed for this workspace", ev.Model), http.StatusForbidden)
		return
	}
	ev.Route, ev.Upstream = routeName, route.BaseURL

	// Which endpoint, not just which model. The provider key we are about to
	// attach is the organization's, and it opens far more than inference.
	upPath := path.Clean("/" + r.URL.Path)
	if !route.pathAllowed(upPath) {
		ev.Outcome, ev.Reason, ev.Status = "blocked", "path not allowed on this route", http.StatusForbidden
		s.record(ev)
		slog.Warn("gateway blocked request",
			"workspace", sess.Workspace, "route", routeName, "path", upPath,
			"reason", ev.Reason, "status", ev.Status)
		http.Error(w, fmt.Sprintf("path %q not allowed on this route", upPath), http.StatusForbidden)
		return
	}

	// Spend ceiling per workspace and route.
	if !s.limiter.allow(sess.Workspace+"\x00"+routeName, rpmOf(route), time.Now()) {
		ev.Outcome, ev.Reason, ev.Status = "blocked", "rate limit exceeded", http.StatusTooManyRequests
		s.record(ev)
		slog.Warn("gateway rate limited",
			"workspace", sess.Workspace, "route", routeName, "limit_rpm", rpmOf(route))
		w.Header().Set("Retry-After", "60")
		http.Error(w, "rate limit exceeded for this workspace", http.StatusTooManyRequests)
		return
	}

	// Size cap is the selected route's policy, so exceeding it is an event.
	maxBytes := route.MaxRequestMB
	if maxBytes <= 0 {
		maxBytes = 20
	}
	if int64(len(body)) > maxBytes<<20 {
		ev.Outcome, ev.Reason, ev.Status = "blocked", "request exceeds size cap", http.StatusRequestEntityTooLarge
		s.record(ev)
		slog.Warn("gateway blocked request",
			"workspace", sess.Workspace, "route", routeName, "reason", ev.Reason, "status", ev.Status)
		http.Error(w, "request exceeds size cap", http.StatusRequestEntityTooLarge)
		return
	}
	ev.ReqBytes = int64(len(body))

	// Secret tripwires.
	for _, re := range secretTripwires {
		if re.Match(body) {
			ev.Outcome, ev.Reason, ev.Status = "blocked", "secret pattern in outbound prompt: "+re.String(), http.StatusForbidden
			s.record(ev)
			slog.Warn("gateway blocked request",
				"workspace", sess.Workspace, "route", routeName, "reason", "secret pattern detected", "status", ev.Status)
			http.Error(w, "outbound request blocked: secret pattern detected", http.StatusForbidden)
			return
		}
	}

	// Forward: same path, upstream base, provider key swapped in.
	up, err := http.NewRequestWithContext(r.Context(), r.Method,
		strings.TrimRight(route.BaseURL, "/")+upPath, strings.NewReader(string(body)))
	if err != nil {
		slog.Error("gateway bad upstream request", "workspace", sess.Workspace, "route", routeName, "err", err)
		http.Error(w, "bad upstream request", http.StatusInternalServerError)
		return
	}
	copyProxyHeaders(up.Header, r.Header)
	key := s.routeKey(routeName)
	if key == "" {
		slog.Warn("gateway route has no provider key",
			"workspace", sess.Workspace, "route", routeName, "key_env", route.KeyEnv)
	}
	switch route.Kind {
	case "anthropic":
		up.Header.Set("x-api-key", key)
		if up.Header.Get("anthropic-version") == "" {
			up.Header.Set("anthropic-version", "2023-06-01")
		}
	case "openai":
		up.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := s.Client.Do(up)
	if err != nil {
		ev.Outcome, ev.Reason, ev.Status = "upstream_error", err.Error(), http.StatusBadGateway
		ev.DurationMS = time.Since(start).Milliseconds()
		s.record(ev)
		slog.Error("gateway upstream unreachable",
			"workspace", sess.Workspace, "route", routeName, "upstream", route.BaseURL, "err", err)
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Stream through, counting bytes and flushing (SSE-friendly). Token
	// counts are parsed best-effort from the two wire formats we own (§9).
	// Headers are copied minus the hop-by-hop set and minus anything that
	// would let an upstream plant state in the sandbox (cookies) — the
	// gateway is a policy door, not a transparent pipe.
	for k, vs := range resp.Header {
		if skipResponseHeader(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	n, inTok, outTok := streamCopyAndTap(w, resp.Body, route.Kind)

	ev.Outcome = "forwarded"
	ev.Status = resp.StatusCode
	ev.RespBytes = n
	ev.InputTokens = inTok
	ev.OutputTokens = outTok
	ev.DurationMS = time.Since(start).Milliseconds()
	s.record(ev)

	attrs := []any{
		"workspace", sess.Workspace,
		"route", routeName,
		"model", ev.Model,
		"status", resp.StatusCode,
		"duration_ms", ev.DurationMS,
		"req_bytes", ev.ReqBytes,
		"resp_bytes", n,
	}
	if resp.StatusCode >= 400 {
		slog.Warn("gateway upstream error response", attrs...)
	} else {
		slog.Info("gateway forwarded", attrs...)
	}
}

// rpmOf resolves a route's request ceiling: 0 means "use the default", and a
// negative value is an explicit opt-out.
func rpmOf(r Route) int {
	if r.MaxRequestsPerMinute == 0 {
		return defaultRPM
	}
	return r.MaxRequestsPerMinute
}

func (s *Server) sessionCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessions)
}

func (s *Server) record(e Event) {
	if err := s.Ledger.Append(e); err != nil {
		// v1 local mode is fail-open with a loud stderr; the org-policy
		// fail-closed mode arrives with the buffered shipper (§11).
		slog.Error("gateway ledger write failed", "err", err)
	}
}

// bearerOrAPIKey extracts the session token from either auth convention:
// Anthropic-style clients send x-api-key, OpenAI-style send a Bearer token.
func bearerOrAPIKey(r *http.Request) string {
	if k := r.Header.Get("x-api-key"); k != "" {
		return k
	}
	auth := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(auth, "Bearer "); ok {
		return t
	}
	return ""
}

func modelOf(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &m)
	return m.Model
}

// modelAllowed supports exact names and trailing-* prefixes ("claude-*").
func modelAllowed(allowed []string, model string) bool {
	for _, a := range allowed {
		if p, ok := strings.CutSuffix(a, "*"); ok {
			if strings.HasPrefix(model, p) {
				return true
			}
		} else if a == model {
			return true
		}
	}
	return false
}

// hopByHopHeaders are per-connection headers that must not be relayed
// (RFC 9110 §7.6.1); Set-Cookie is here because nothing in a model response
// has any business setting state in the sandbox.
var hopByHopHeaders = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Proxy-Authorization": true, "Te": true, "Trailer": true,
	"Transfer-Encoding": true, "Upgrade": true,
	"Set-Cookie": true,
}

func skipResponseHeader(k string) bool {
	return hopByHopHeaders[http.CanonicalHeaderKey(k)]
}

// copyProxyHeaders forwards content/accept headers but never the sandbox's
// credentials (the whole point is that they stop here).
func copyProxyHeaders(dst, src http.Header) {
	for _, k := range []string{"Content-Type", "Accept", "Anthropic-Version", "Anthropic-Beta"} {
		if v := src.Get(k); v != "" {
			dst.Set(k, v)
		}
	}
}

func streamCopy(w http.ResponseWriter, r io.Reader) int64 {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	var n int64
	for {
		k, err := r.Read(buf)
		if k > 0 {
			if _, werr := w.Write(buf[:k]); werr != nil {
				return n
			}
			n += int64(k)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return n
		}
	}
}
