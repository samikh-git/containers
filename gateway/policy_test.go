package gateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func postPath(t *testing.T, s *Server, token, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", token)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w
}

// A session token is permission to talk to a model, not permission to use
// the organization's key against the provider's whole API.
func TestNonInferencePathBlocked(t *testing.T) {
	var reached string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = r.URL.Path
		fmt.Fprint(w, `{}`)
	}))
	defer upstream.Close()

	s, _ := testServer(t, upstream.URL, "anthropic")
	for _, p := range []string{
		"/v1/organizations/me",
		"/v1/organizations/api_keys",
		"/v1/files",
		"/v1/messages/../organizations/me",
	} {
		w := postPath(t, s, "ws1-token", p, `{"model":"claude-sonnet-5"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("path %q: got %d, want 403", p, w.Code)
		}
	}
	if reached != "" {
		t.Fatalf("a blocked path still reached the upstream: %q", reached)
	}

	// The endpoint the route exists for still works.
	if w := postPath(t, s, "ws1-token", "/v1/messages", `{"model":"claude-sonnet-5"}`); w.Code != http.StatusOK {
		t.Fatalf("inference path blocked: %d %s", w.Code, w.Body.String())
	}
}

// A route may widen the allowlist when it genuinely fronts something else.
func TestAllowedPathsOverride(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{}`)
	}))
	defer upstream.Close()

	ledger, err := OpenLedger(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ledger.Close() })
	t.Setenv("K", "k")
	s := NewServer(&Config{
		Routes: map[string]Route{
			"r1": {Kind: "openai", BaseURL: upstream.URL, KeyEnv: "K",
				AllowedPaths: []string{"/v1/custom/*"}},
		},
		Sessions: map[string]Session{"tok": {Workspace: "ws", Route: "r1"}},
	}, ledger)

	if w := postPath(t, s, "tok", "/v1/custom/thing", `{}`); w.Code != http.StatusOK {
		t.Fatalf("explicitly allowed path blocked: %d", w.Code)
	}
	if w := postPath(t, s, "tok", "/v1/chat/completions", `{}`); w.Code != http.StatusForbidden {
		t.Fatalf("path outside an explicit allowlist must be blocked, got %d", w.Code)
	}
}

// A leaked session token must not be able to spend without limit.
func TestRateLimitPerWorkspace(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{}`)
	}))
	defer upstream.Close()

	ledger, err := OpenLedger(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ledger.Close() })
	t.Setenv("K", "k")
	s := NewServer(&Config{
		Routes: map[string]Route{
			"r1": {Kind: "anthropic", BaseURL: upstream.URL, KeyEnv: "K", MaxRequestsPerMinute: 2},
		},
		Sessions: map[string]Session{
			"a": {Workspace: "ws-a", Route: "r1"},
			"b": {Workspace: "ws-b", Route: "r1"},
		},
	}, ledger)

	for i := range 2 {
		if w := postPath(t, s, "a", "/v1/messages", `{"model":"m"}`); w.Code != http.StatusOK {
			t.Fatalf("request %d under the cap: got %d", i, w.Code)
		}
	}
	if w := postPath(t, s, "a", "/v1/messages", `{"model":"m"}`); w.Code != http.StatusTooManyRequests {
		t.Fatalf("over the cap: got %d, want 429", w.Code)
	}
	// The cap is per workspace: a busy neighbour must not throttle anyone else.
	if w := postPath(t, s, "b", "/v1/messages", `{"model":"m"}`); w.Code != http.StatusOK {
		t.Fatalf("another workspace was throttled by its neighbour: %d", w.Code)
	}
}

func TestRateWindowRolls(t *testing.T) {
	l := newRateLimiter()
	now := time.Now()
	if !l.allow("k", 1, now) || l.allow("k", 1, now.Add(time.Second)) {
		t.Fatal("limiter must allow exactly one request inside the window")
	}
	if !l.allow("k", 1, now.Add(time.Minute+time.Second)) {
		t.Fatal("limiter must reset once the window has passed")
	}
}

// The unkeyed chain detects corruption; only the keyed chain detects an
// editor who can also recompute hashes.
func TestKeyedLedgerDetectsRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l, err := OpenLedgerWithKey(path, []byte("chain-key"))
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := l.Append(Event{Workspace: "ws", Outcome: "forwarded"}); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()

	if n, err := VerifyWithKey(path, []byte("chain-key")); err != nil || n != 3 {
		t.Fatalf("keyed verify of an untouched ledger: n=%d err=%v", n, err)
	}
	// Someone with the file but not the key rewrites history: they can
	// recompute a plain SHA-256 chain, but not this one.
	forged, err := OpenLedgerWithKey(path+".forged", nil)
	if err != nil {
		t.Fatal(err)
	}
	forged.Append(Event{Workspace: "ws", Outcome: "forwarded"})
	forged.Close()
	if _, err := VerifyWithKey(path+".forged", []byte("chain-key")); err == nil {
		t.Fatal("a chain forged without the key must not verify with it")
	}
}
