package restapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"containerization/dataplane"
)

func guardServer(t *testing.T, allowed ...string) *httptest.Server {
	t.Helper()
	root := t.TempDir()
	s := &Server{
		Router: &dataplane.Router{
			Storage: &dataplane.DirStorage{Root: root},
			Leases:  &dataplane.FileLeaseAuthority{Path: filepath.Join(root, "leases.json")},
			Runtime: &dataplane.NoneRuntime{},
		},
		AllowedHosts: allowed,
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// The whole point: with no token configured (the local-mode default), a page
// on another origin must not be able to drive the API with the operator's
// browser.
func TestCrossOriginRefused(t *testing.T) {
	ts := guardServer(t)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/workspaces",
		strings.NewReader(`{"id":"pwned"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for a cross-origin create, got %d", resp.StatusCode)
	}
}

// DNS rebinding: the connection lands on loopback but the page still names
// itself in Host.
func TestRebindingHostRefused(t *testing.T) {
	ts := guardServer(t)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/workspaces", nil)
	req.Host = "evil.example"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("expected 421 for an unrecognized Host, got %d", resp.StatusCode)
	}
}

// A configured external name (tunnel hostname) is accepted for both headers.
func TestAllowedHostAccepted(t *testing.T) {
	ts := guardServer(t, "app.example.com")
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/workspaces", nil)
	req.Host = "app.example.com"
	req.Header.Set("Origin", "https://app.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for the configured host, got %d", resp.StatusCode)
	}
}

// Simple-request CSRF shape: a form POST needs no preflight, so it must be
// refused on content type even before the origin check would catch it.
func TestNonJSONBodyRefused(t *testing.T) {
	ts := guardServer(t)
	resp, err := http.Post(ts.URL+"/api/workspaces", "text/plain", strings.NewReader(`{"id":"pwned"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415 for a non-JSON body, got %d", resp.StatusCode)
	}
}
