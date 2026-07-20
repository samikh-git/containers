package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testServer(t *testing.T, upstream string, kind string) (*Server, string) {
	t.Helper()
	ledgerPath := filepath.Join(t.TempDir(), "audit.jsonl")
	ledger, err := OpenLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ledger.Close() })

	t.Setenv("TEST_PROVIDER_KEY", "real-provider-key")
	cfg := &Config{
		Routes: map[string]Route{
			"r1": {Kind: kind, BaseURL: upstream, KeyEnv: "TEST_PROVIDER_KEY",
				AllowedModels: []string{"claude-*", "exact-model"}},
		},
		Sessions: map[string]Session{
			"ws1-token": {Workspace: "ws1", Route: "r1"},
		},
	}
	return NewServer(cfg, ledger), ledgerPath
}

func post(t *testing.T, s *Server, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("x-api-key", token)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w
}

func TestKeySwapAndForward(t *testing.T) {
	var gotKey, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	s, _ := testServer(t, upstream.URL, "anthropic")
	w := post(t, s, "ws1-token", `{"model":"claude-sonnet-5"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if gotKey != "real-provider-key" {
		t.Fatalf("upstream did not receive the provider key, got %q", gotKey)
	}
	if gotAuth != "" {
		t.Fatalf("session token leaked upstream in Authorization: %q", gotAuth)
	}
}

func TestUnknownTokenRejected(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream must not be reached without a valid session")
	}))
	defer upstream.Close()

	s, _ := testServer(t, upstream.URL, "anthropic")
	if w := post(t, s, "wrong-token", `{"model":"claude-sonnet-5"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", w.Code)
	}
	if w := post(t, s, "", `{"model":"claude-sonnet-5"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status %d, want 401", w.Code)
	}
}

func TestModelAllowlist(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{}`)
	}))
	defer upstream.Close()
	s, _ := testServer(t, upstream.URL, "anthropic")

	if w := post(t, s, "ws1-token", `{"model":"gpt-9"}`); w.Code != http.StatusForbidden {
		t.Fatalf("disallowed model: status %d, want 403", w.Code)
	}
	if w := post(t, s, "ws1-token", `{"model":"exact-model"}`); w.Code != http.StatusOK {
		t.Fatalf("exact allowed model rejected: %d", w.Code)
	}
	if w := post(t, s, "ws1-token", `{"model":"claude-opus-5"}`); w.Code != http.StatusOK {
		t.Fatalf("prefix allowed model rejected: %d", w.Code)
	}
}

func TestSecretTripwire(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("request with secret must not reach upstream")
	}))
	defer upstream.Close()
	s, ledgerPath := testServer(t, upstream.URL, "anthropic")

	body := `{"model":"claude-sonnet-5","messages":[{"content":"key is AKIAIOSFODNN7EXAMPLE"}]}`
	if w := post(t, s, "ws1-token", body); w.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", w.Code)
	}

	events := readLedger(t, ledgerPath)
	last := events[len(events)-1]
	if last.Outcome != "blocked" || !strings.Contains(last.Reason, "secret pattern") {
		t.Fatalf("block not recorded: %+v", last)
	}
	if strings.Contains(last.Reason+last.Model, "AKIAIOSFODNN7EXAMPLE") {
		t.Fatal("the secret itself was written into the ledger")
	}
}

func TestStreamingPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: chunk-%d\n\n", i)
			f.Flush()
		}
	}))
	defer upstream.Close()
	s, ledgerPath := testServer(t, upstream.URL, "anthropic")

	w := post(t, s, "ws1-token", `{"model":"claude-sonnet-5","stream":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if got := w.Body.String(); !strings.Contains(got, "chunk-0") || !strings.Contains(got, "chunk-2") {
		t.Fatalf("stream not passed through: %q", got)
	}

	events := readLedger(t, ledgerPath)
	last := events[len(events)-1]
	if last.Outcome != "forwarded" || last.RespBytes == 0 {
		t.Fatalf("forwarded event wrong: %+v", last)
	}
}

func TestOpenAIAuthConvention(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{}`)
	}))
	defer upstream.Close()
	s, _ := testServer(t, upstream.URL, "openai")

	// OpenAI-style client sends Bearer <session token>.
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"claude-sonnet-5"}`))
	req.Header.Set("Authorization", "Bearer ws1-token")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if gotAuth != "Bearer real-provider-key" {
		t.Fatalf("provider key not swapped in: %q", gotAuth)
	}
}

// TestMultiRouteModelSelection verifies a workspace carrying two routes: the
// model decides which upstream forwards it. Claude models pin to the Anthropic
// route; everything else falls through to the catch-all (empty allowlist).
func TestMultiRouteModelSelection(t *testing.T) {
	var hit string
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = "anthropic"
		fmt.Fprint(w, `{}`)
	}))
	defer anthropic.Close()
	openrouter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = "openrouter"
		fmt.Fprint(w, `{}`)
	}))
	defer openrouter.Close()

	ledger, err := OpenLedger(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ledger.Close() })
	t.Setenv("K", "k")
	cfg := &Config{
		Routes: map[string]Route{
			"anthropic":  {Kind: "anthropic", BaseURL: anthropic.URL, KeyEnv: "K", AllowedModels: []string{"claude-*"}},
			"openrouter": {Kind: "openai", BaseURL: openrouter.URL, KeyEnv: "K"}, // no allowlist: catch-all
		},
		Sessions: map[string]Session{
			"tok": {Workspace: "ws", Routes: []string{"anthropic", "openrouter"}},
		},
	}
	s := NewServer(cfg, ledger)

	cases := map[string]string{
		"claude-sonnet-5":             "anthropic",
		"anthropic/claude-3.5-sonnet": "openrouter", // OpenRouter's namespaced id, not "claude-*"
		"openai/gpt-4o":               "openrouter",
		"meta-llama/llama-3.1-70b":    "openrouter",
	}
	for model, want := range cases {
		hit = ""
		if w := post(t, s, "tok", `{"model":"`+model+`"}`); w.Code != http.StatusOK {
			t.Fatalf("model %q: status %d: %s", model, w.Code, w.Body.String())
		}
		if hit != want {
			t.Fatalf("model %q routed to %q, want %q", model, hit, want)
		}
	}
}

// TestSingleRouteStillBlocks confirms the per-model change didn't weaken the
// single-route allowlist: a lone route with an allowlist still 403s a miss.
func TestSingleRouteStillBlocks(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream must not be reached for a disallowed model")
	}))
	defer upstream.Close()
	s, _ := testServer(t, upstream.URL, "anthropic")
	if w := post(t, s, "ws1-token", `{"model":"gpt-9"}`); w.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", w.Code)
	}
}

func TestRuntimeKeyViaAdmin(t *testing.T) {
	var gotKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	s, _ := testServer(t, upstream.URL, "anthropic")
	admin := &AdminHandler{Server: s}

	// Key status before: env key present (testServer sets TEST_PROVIDER_KEY).
	w := httptest.NewRecorder()
	admin.ServeHTTP(w, httptest.NewRequest("GET", "/keys", nil))
	var status map[string]bool
	json.Unmarshal(w.Body.Bytes(), &status)
	if !status["r1"] {
		t.Fatalf("route r1 should report a key from env: %v", status)
	}

	// Set a runtime key; it must take precedence over the env key.
	w = httptest.NewRecorder()
	admin.ServeHTTP(w, httptest.NewRequest("PUT", "/keys/r1",
		strings.NewReader(`{"key":"runtime-key"}`)))
	if w.Code != http.StatusNoContent {
		t.Fatalf("set key: status %d: %s", w.Code, w.Body.String())
	}
	if resp := post(t, s, "ws1-token", `{"model":"claude-sonnet-5"}`); resp.Code != http.StatusOK {
		t.Fatalf("forward: status %d", resp.Code)
	}
	if gotKey != "runtime-key" {
		t.Fatalf("upstream got %q, want the runtime key", gotKey)
	}

	// Clearing the runtime key falls back to the env key.
	w = httptest.NewRecorder()
	admin.ServeHTTP(w, httptest.NewRequest("PUT", "/keys/r1", strings.NewReader(`{"key":""}`)))
	if w.Code != http.StatusNoContent {
		t.Fatalf("clear key: status %d", w.Code)
	}
	post(t, s, "ws1-token", `{"model":"claude-sonnet-5"}`)
	if gotKey != "real-provider-key" {
		t.Fatalf("upstream got %q, want fallback env key", gotKey)
	}

	// Unknown route is rejected.
	w = httptest.NewRecorder()
	admin.ServeHTTP(w, httptest.NewRequest("PUT", "/keys/nope", strings.NewReader(`{"key":"k"}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown route: status %d, want 400", w.Code)
	}
}

func TestRoutesInfoViaAdmin(t *testing.T) {
	s, _ := testServer(t, "http://upstream.test", "anthropic")
	admin := &AdminHandler{Server: s}

	get := func() map[string]RouteInfo {
		w := httptest.NewRecorder()
		admin.ServeHTTP(w, httptest.NewRequest("GET", "/routes", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("routes: status %d: %s", w.Code, w.Body.String())
		}
		var info map[string]RouteInfo
		if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
			t.Fatal(err)
		}
		return info
	}

	// Env key present (testServer sets TEST_PROVIDER_KEY).
	r1 := get()["r1"]
	if r1.Kind != "anthropic" || r1.BaseURL != "http://upstream.test" {
		t.Fatalf("route config not reported: %+v", r1)
	}
	if !r1.KeySet || r1.KeySource != "env" {
		t.Fatalf("want key from env, got %+v", r1)
	}

	// Runtime key takes over as the reported source.
	w := httptest.NewRecorder()
	admin.ServeHTTP(w, httptest.NewRequest("PUT", "/keys/r1", strings.NewReader(`{"key":"rk"}`)))
	if w.Code != http.StatusNoContent {
		t.Fatalf("set key: status %d", w.Code)
	}
	if r1 = get()["r1"]; !r1.KeySet || r1.KeySource != "runtime" {
		t.Fatalf("want runtime key source, got %+v", r1)
	}

	// No env, no runtime key -> not set. RouteInfo must never carry a value.
	t.Setenv("TEST_PROVIDER_KEY", "")
	admin.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest("PUT", "/keys/r1", strings.NewReader(`{"key":""}`)))
	if r1 = get()["r1"]; r1.KeySet || r1.KeySource != "" {
		t.Fatalf("want no key, got %+v", r1)
	}
}

func TestLedgerChainAndTamperDetection(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "audit.jsonl")
	l, err := OpenLedger(p)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := l.Append(Event{Workspace: "ws1", Outcome: "forwarded"}); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()

	if n, err := Verify(p); err != nil || n != 5 {
		t.Fatalf("verify: n=%d err=%v", n, err)
	}

	// Reopen resumes the chain.
	l2, err := OpenLedger(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := l2.Append(Event{Workspace: "ws1", Outcome: "blocked"}); err != nil {
		t.Fatal(err)
	}
	l2.Close()
	if n, err := Verify(p); err != nil || n != 6 {
		t.Fatalf("verify after resume: n=%d err=%v", n, err)
	}

	// Tamper with an early event: chain must break.
	b, _ := os.ReadFile(p)
	tampered := strings.Replace(string(b), `"workspace":"ws1"`, `"workspace":"evil"`, 1)
	if err := os.WriteFile(p, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(p); err == nil {
		t.Fatal("tampered ledger passed verification")
	}
}

func readLedger(t *testing.T, path string) []Event {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("bad ledger line: %v", err)
		}
		events = append(events, e)
	}
	if len(events) == 0 {
		t.Fatal("ledger empty")
	}
	return events
}
