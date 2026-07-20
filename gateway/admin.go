package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
)

// AdminHandler is the router-facing control surface: register a session
// token when a workspace is provisioned, revoke by workspace when it is
// stopped. It must only ever be bound to localhost (or, later, an mTLS
// channel from the router) — it is not part of the sandbox-reachable
// surface.
//
//	POST   /sessions                {"token":"…","workspace":"…","route":"…"}
//	DELETE /sessions/{token}
//	DELETE /sessions?workspace=ws1   (revoke all tokens of a workspace)
//	PUT    /keys/{route}             {"key":"…"} — set/rotate ("" clears) a provider key
//	GET    /keys                     route -> key-present (never key values)
//	GET    /routes                   route -> config + key status (never key values)
type AdminHandler struct {
	Server *Server
	// Token, when set, is required as "Authorization: Bearer <Token>" on
	// every admin request. MANDATORY when the admin listener is reachable
	// from anywhere a sandbox could be (e.g. the gateway container on the
	// sandbox network): without it, a sandbox could mint its own sessions.
	Token string
}

func (a *AdminHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if a.Token != "" && r.Header.Get("Authorization") != "Bearer "+a.Token {
		http.Error(w, "admin authorization required", http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/sessions":
		var req struct {
			Token     string   `json:"token"`
			Workspace string   `json:"workspace"`
			Route     string   `json:"route"`
			Routes    []string `json:"routes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
			req.Token == "" || req.Workspace == "" || (req.Route == "" && len(req.Routes) == 0) {
			http.Error(w, "token, workspace and at least one route are required", http.StatusBadRequest)
			return
		}
		if err := a.Server.AddSession(req.Token, Session{Workspace: req.Workspace, Route: req.Route, Routes: req.Routes}); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)

	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/sessions/"):
		token := strings.TrimPrefix(r.URL.Path, "/sessions/")
		a.Server.RemoveSession(token)
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodDelete && r.URL.Path == "/sessions":
		ws := r.URL.Query().Get("workspace")
		if ws == "" {
			http.Error(w, "workspace query parameter required", http.StatusBadRequest)
			return
		}
		n := a.Server.RemoveWorkspaceSessions(ws)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int{"revoked": n})

	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/keys/"):
		route := strings.TrimPrefix(r.URL.Path, "/keys/")
		var req struct {
			Key string `json:"key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || route == "" {
			http.Error(w, "route path segment and key body required", http.StatusBadRequest)
			return
		}
		if err := a.Server.SetRouteKey(route, req.Key); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodGet && r.URL.Path == "/keys":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(a.Server.RouteKeyStatus())

	case r.Method == http.MethodGet && r.URL.Path == "/routes":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(a.Server.RoutesInfo())

	default:
		http.NotFound(w, r)
	}
}
