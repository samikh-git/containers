package dataplane

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"containerization/internal/applog"
)

// Session tokens (DESIGN §9): the only credential a sandbox holds. Minted by
// the router at provision, registered with the gateway's admin API, revoked
// on stop. Worthless anywhere but at the gateway.

// MintToken returns a fresh opaque session token.
func MintToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "st-" + hex.EncodeToString(b), nil
}

// SessionRegistrar registers/revokes sandbox sessions. Nil registrar means
// no gateway is configured (storage-only dev flows).
type SessionRegistrar interface {
	Register(ctx context.Context, token, workspace string, routes []string) error
	RevokeWorkspace(ctx context.Context, workspace string) error
}

// GatewayAdminClient talks to cmd/gateway's admin listener.
type GatewayAdminClient struct {
	AdminURL string // e.g. http://127.0.0.1:8444
	Token    string // GATEWAY_ADMIN_TOKEN; required for non-loopback admin
	Client   *http.Client
}

func NewGatewayAdminClient(adminURL, token string) *GatewayAdminClient {
	return &GatewayAdminClient{
		AdminURL: adminURL,
		Token:    token,
		Client:   &http.Client{Timeout: 5 * time.Second},
	}
}

func (g *GatewayAdminClient) auth(req *http.Request) {
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
}

func (g *GatewayAdminClient) Register(ctx context.Context, token, workspace string, routes []string) error {
	body, _ := json.Marshal(map[string]any{
		"token": token, "workspace": workspace, "routes": routes,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		g.AdminURL+"/sessions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	g.auth(req)
	resp, err := g.Client.Do(req)
	if err != nil {
		slog.Error("session register failed",
			"workspace", workspace, "token", applog.TokenPrefix(token),
			"admin", g.AdminURL, "err", err)
		return fmt.Errorf("gateway admin: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		slog.Error("session register rejected",
			"workspace", workspace, "token", applog.TokenPrefix(token),
			"admin", g.AdminURL, "status", resp.StatusCode, "body", stringsTrim(msg))
		return fmt.Errorf("gateway admin: register returned %d: %s", resp.StatusCode, stringsTrim(msg))
	}
	slog.Info("session registered",
		"workspace", workspace, "token", applog.TokenPrefix(token), "routes", routes)
	return nil
}

func stringsTrim(b []byte) string {
	return string(bytes.TrimSpace(b))
}

// SetRouteKey sets or rotates a route's provider key at the gateway. The key
// passes through this client straight to the gateway admin API and is never
// stored router-side (§9: keys live in the gateway process only).
func (g *GatewayAdminClient) SetRouteKey(ctx context.Context, route, key string) error {
	body, _ := json.Marshal(map[string]string{"key": key})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		g.AdminURL+"/keys/"+url.PathEscape(route), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	g.auth(req)
	resp, err := g.Client.Do(req)
	if err != nil {
		return fmt.Errorf("gateway admin: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("gateway admin: set key returned %d", resp.StatusCode)
	}
	return nil
}

// RouteKeyStatus reports which gateway routes currently hold a usable key.
func (g *GatewayAdminClient) RouteKeyStatus(ctx context.Context) (map[string]bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.AdminURL+"/keys", nil)
	if err != nil {
		return nil, err
	}
	g.auth(req)
	resp, err := g.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gateway admin: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gateway admin: key status returned %d", resp.StatusCode)
	}
	var status map[string]bool
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, err
	}
	return status, nil
}

// Routes returns the gateway's route metadata (config + key status, never key
// values) as raw JSON. The router relays it to the operator UI verbatim; the
// shape is the gateway's to define (gateway.RouteInfo).
func (g *GatewayAdminClient) Routes(ctx context.Context) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.AdminURL+"/routes", nil)
	if err != nil {
		return nil, err
	}
	g.auth(req)
	resp, err := g.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gateway admin: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gateway admin: routes returned %d", resp.StatusCode)
	}
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (g *GatewayAdminClient) RevokeWorkspace(ctx context.Context, workspace string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		g.AdminURL+"/sessions?workspace="+url.QueryEscape(workspace), nil)
	if err != nil {
		return err
	}
	g.auth(req)
	resp, err := g.Client.Do(req)
	if err != nil {
		slog.Error("session revoke failed", "workspace", workspace, "admin", g.AdminURL, "err", err)
		return fmt.Errorf("gateway admin: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		slog.Error("session revoke rejected",
			"workspace", workspace, "admin", g.AdminURL,
			"status", resp.StatusCode, "body", stringsTrim(msg))
		return fmt.Errorf("gateway admin: revoke returned %d: %s", resp.StatusCode, stringsTrim(msg))
	}
	slog.Info("session revoked", "workspace", workspace)
	return nil
}
