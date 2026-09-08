package restapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"containerization/dataplane"
)

// newTestServer wires a real router over DirStorage in a temp root with the
// NoneRuntime — the same storage-only configuration the CLI runs in dev.
func newTestServer(t *testing.T, token string) (*httptest.Server, string) {
	t.Helper()
	root := t.TempDir()
	s := &Server{
		Router: &dataplane.Router{
			Storage: &dataplane.DirStorage{Root: root},
			Leases:  &dataplane.FileLeaseAuthority{Path: filepath.Join(root, "leases.json")},
			Runtime: dataplane.NoneRuntime{},
		},
		Defaults: dataplane.WorkspaceSpec{Image: "opencode-sandbox:v1", CPUs: 2, MemoryMB: 2048, QuotaGB: 10},
		Token:    token,
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, root
}

func do(t *testing.T, method, url, body, token string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var decoded map[string]any
	json.NewDecoder(resp.Body).Decode(&decoded)
	return resp, decoded
}

func TestUpListDestroy(t *testing.T) {
	ts, root := newTestServer(t, "")

	resp, body := do(t, "POST", ts.URL+"/api/workspaces", `{"id":"ws1"}`, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("up: got %d, want 201 (%v)", resp.StatusCode, body)
	}
	mount := body["mount"].(string)
	if _, err := os.Stat(mount); err != nil {
		t.Fatalf("mount path %s not created: %v", mount, err)
	}

	resp, _ = do(t, "GET", ts.URL+"/api/workspaces", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: got %d, want 200", resp.StatusCode)
	}

	resp, _ = do(t, "DELETE", ts.URL+"/api/workspaces/ws1", "", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("destroy: got %d, want 204", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(root, "workspaces", "ws1")); !os.IsNotExist(err) {
		t.Fatalf("workspace dir survived destroy: %v", err)
	}
}

func TestFanout(t *testing.T) {
	ts, root := newTestServer(t, "")

	if resp, body := do(t, "POST", ts.URL+"/api/workspaces", `{"id":"ws1"}`, ""); resp.StatusCode != 201 {
		t.Fatalf("up: got %d (%v)", resp.StatusCode, body)
	}
	resp, body := do(t, "POST", ts.URL+"/api/workspaces/ws1/fanout", `{"n":2}`, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("fanout: got %d, want 201 (%v)", resp.StatusCode, body)
	}
	branches := body["branches"].([]any)
	if len(branches) != 2 {
		t.Fatalf("fanout: got %d branches, want 2", len(branches))
	}
	for _, b := range []string{"b1", "b2"} {
		if _, err := os.Stat(filepath.Join(root, "workspaces", "ws1", "branches", b)); err != nil {
			t.Errorf("branch %s missing: %v", b, err)
		}
	}

	// n out of range must not touch storage.
	resp, _ = do(t, "POST", ts.URL+"/api/workspaces/ws1/fanout", `{"n":0}`, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("fanout n=0: got %d, want 400", resp.StatusCode)
	}
}

func TestDownAndHibernate(t *testing.T) {
	ts, root := newTestServer(t, "")
	do(t, "POST", ts.URL+"/api/workspaces", `{"id":"ws1"}`, "")

	if resp, _ := do(t, "POST", ts.URL+"/api/workspaces/ws1/down", `{}`, ""); resp.StatusCode != 200 {
		t.Fatalf("down: got %d, want 200", resp.StatusCode)
	}
	if resp, _ := do(t, "POST", ts.URL+"/api/workspaces/ws1/hibernate", `{}`, ""); resp.StatusCode != 200 {
		t.Fatalf("hibernate: got %d, want 200", resp.StatusCode)
	}
	// Hibernate must have left a snapshot behind.
	snaps, err := os.ReadDir(filepath.Join(root, "workspaces", "ws1", "snapshots"))
	if err != nil || len(snaps) == 0 {
		t.Fatalf("hibernate left no snapshot: %v", err)
	}
}

func TestInvalidIDs(t *testing.T) {
	ts, _ := newTestServer(t, "")
	for _, payload := range []string{`{"id":""}`, `{"id":"../etc"}`, `{"id":"a b"}`, `{"id":"-x"}`} {
		resp, _ := do(t, "POST", ts.URL+"/api/workspaces", payload, "")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("up %s: got %d, want 400", payload, resp.StatusCode)
		}
	}
	// Path-segment ids are constrained by the same pattern.
	resp, _ := do(t, "POST", ts.URL+"/api/workspaces/a%20b/down", `{}`, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("down bad id: got %d, want 400", resp.StatusCode)
	}
}

func TestAuth(t *testing.T) {
	ts, _ := newTestServer(t, "secret")

	if resp, _ := do(t, "GET", ts.URL+"/api/workspaces", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", resp.StatusCode)
	}
	if resp, _ := do(t, "GET", ts.URL+"/api/workspaces", "", "wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", resp.StatusCode)
	}
	if resp, _ := do(t, "GET", ts.URL+"/api/workspaces", "", "secret"); resp.StatusCode != http.StatusOK {
		t.Fatalf("right token: got %d, want 200", resp.StatusCode)
	}

	// The UI page and health check stay open; they hold no state.
	// /admin uses a client-side token gate (browsers can't send Bearer on navigation).
	if resp, _ := do(t, "GET", ts.URL+"/", "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("ui: got %d, want 200", resp.StatusCode)
	}
	if resp, _ := do(t, "GET", ts.URL+"/admin", "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("admin: got %d, want 200", resp.StatusCode)
	}
	if resp, _ := do(t, "GET", ts.URL+"/healthz", "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: got %d, want 200", resp.StatusCode)
	}
}

func TestDefaults(t *testing.T) {
	ts, _ := newTestServer(t, "")
	resp, body := do(t, "GET", ts.URL+"/api/defaults", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("defaults: got %d (%v)", resp.StatusCode, body)
	}
	if body["image"] != "opencode-sandbox:v1" {
		t.Fatalf("image: got %v", body["image"])
	}
	if body["cpus"] != float64(2) {
		t.Fatalf("cpus: got %v", body["cpus"])
	}
}

type fakeKeyAdmin struct {
	keys map[string]string
}

func (f *fakeKeyAdmin) SetRouteKey(_ context.Context, route, key string) error {
	if route == "nope" {
		return errors.New("route not defined")
	}
	f.keys[route] = key
	return nil
}

func (f *fakeKeyAdmin) RouteKeyStatus(context.Context) (map[string]bool, error) {
	status := map[string]bool{"tier-a": false}
	for r := range f.keys {
		status[r] = true
	}
	return status, nil
}

func (f *fakeKeyAdmin) Routes(context.Context) (json.RawMessage, error) {
	return json.RawMessage(`{"tier-a":{"kind":"anthropic","base_url":"https://api.anthropic.com","key_set":false}}`), nil
}

func TestKeyEndpoints(t *testing.T) {
	ts, _ := newTestServer(t, "")

	// No gateway configured → 409 with a pointed message.
	resp, body := do(t, "GET", ts.URL+"/api/keys", "", "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("keys without gateway: got %d, want 409 (%v)", resp.StatusCode, body)
	}
	resp, body = do(t, "GET", ts.URL+"/api/routes", "", "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("routes without gateway: got %d, want 409 (%v)", resp.StatusCode, body)
	}
}

func TestKeyEndpointsWithGateway(t *testing.T) {
	root := t.TempDir()
	fake := &fakeKeyAdmin{keys: map[string]string{}}
	s := &Server{
		Router: &dataplane.Router{
			Storage: &dataplane.DirStorage{Root: root},
			Leases:  &dataplane.FileLeaseAuthority{Path: filepath.Join(root, "leases.json")},
			Runtime: dataplane.NoneRuntime{},
		},
		Keys: fake,
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, _ := do(t, "PUT", ts.URL+"/api/keys/tier-a", `{"key":"sk-test"}`, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("set key: got %d, want 204", resp.StatusCode)
	}
	if fake.keys["tier-a"] != "sk-test" {
		t.Fatalf("key not forwarded to gateway admin: %v", fake.keys)
	}

	resp, body := do(t, "GET", ts.URL+"/api/keys", "", "")
	if resp.StatusCode != http.StatusOK || body["tier-a"] != true {
		t.Fatalf("key status: got %d %v", resp.StatusCode, body)
	}

	resp, _ = do(t, "PUT", ts.URL+"/api/keys/nope", `{"key":"k"}`, "")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("bad route: got %d, want 502", resp.StatusCode)
	}

	// Route metadata relays the gateway's JSON verbatim.
	resp, routes := do(t, "GET", ts.URL+"/api/routes", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("routes: got %d, want 200", resp.StatusCode)
	}
	if info, ok := routes["tier-a"].(map[string]any); !ok || info["kind"] != "anthropic" {
		t.Fatalf("routes: unexpected payload %v", routes)
	}
}

// endpointRuntime is a NoneRuntime that also resolves agent endpoints — the
// shape DockerRuntime has in production.
type endpointRuntime struct {
	dataplane.NoneRuntime
	endpoint string
}

func (e *endpointRuntime) AgentEndpoint(_ context.Context, id string) (string, error) {
	if id == "gone" {
		return "", errors.New("sandbox has no network")
	}
	return e.endpoint, nil
}

func TestOpencodeProxy(t *testing.T) {
	// Stand-in for the sandbox's opencode server.
	var gotPath, gotAuth, gotBody string
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		fmt.Fprint(w, `{"id":"ses_1"}`)
	}))
	defer agent.Close()

	root := t.TempDir()
	s := &Server{
		Router: &dataplane.Router{
			Storage: &dataplane.DirStorage{Root: root},
			Leases:  &dataplane.FileLeaseAuthority{Path: filepath.Join(root, "leases.json")},
			Runtime: &endpointRuntime{endpoint: strings.TrimPrefix(agent.URL, "http://")},
		},
		Token: "secret",
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, body := do(t, "POST", ts.URL+"/api/workspaces/ws1/opencode/session?directory=/workspace", `{"title":"t"}`, "secret")
	if resp.StatusCode != http.StatusOK || body["id"] != "ses_1" {
		t.Fatalf("proxy: got %d %v", resp.StatusCode, body)
	}
	if gotPath != "/session?directory=/workspace" {
		t.Fatalf("agent saw path %q, want /session with query", gotPath)
	}
	if gotBody != `{"title":"t"}` {
		t.Fatalf("agent saw body %q", gotBody)
	}
	// The router API token must not leak into the sandbox.
	if gotAuth != "" {
		t.Fatalf("router token leaked to agent: %q", gotAuth)
	}

	// Proxy requires auth like every other /api route.
	if resp, _ := do(t, "GET", ts.URL+"/api/workspaces/ws1/opencode/session", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated proxy: got %d, want 401", resp.StatusCode)
	}
	// Unresolvable endpoint → 502.
	if resp, _ := do(t, "GET", ts.URL+"/api/workspaces/gone/opencode/session", "", "secret"); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("gone workspace: got %d, want 502", resp.StatusCode)
	}
}

func TestOpencodeProxyWithoutResolver(t *testing.T) {
	ts, _ := newTestServer(t, "") // NoneRuntime: no endpoint resolution
	resp, _ := do(t, "GET", ts.URL+"/api/workspaces/ws1/opencode/session", "", "")
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("proxy without resolver: got %d, want 501", resp.StatusCode)
	}
}

func TestUIServed(t *testing.T) {
	ts, _ := newTestServer(t, "")
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("ui content-type: %s", ct)
	}
}
