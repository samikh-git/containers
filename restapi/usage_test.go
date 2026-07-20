package restapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"containerization/dataplane"
)

func TestUsageEndpointNoLedger(t *testing.T) {
	ts, _ := newTestServer(t, "")
	resp, body := do(t, "GET", ts.URL+"/api/usage", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if body["available"] != false {
		t.Fatalf("expected available=false without ledger, got %#v", body["available"])
	}
}

func TestUsageEndpointWithLedger(t *testing.T) {
	ledger := filepath.Join(t.TempDir(), "audit.jsonl")
	now := time.Now().UTC()
	line, err := json.Marshal(map[string]any{
		"seq": 1, "prev": "", "hash": "x",
		"time":      now.Add(-1 * time.Hour).Format(time.RFC3339Nano),
		"workspace": "ws1", "route": "r1", "model": "claude-sonnet-4-5",
		"outcome": "forwarded", "input_tokens": 1000, "output_tokens": 200,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledger, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	s := &Server{
		Router: &dataplane.Router{
			Storage: &dataplane.DirStorage{Root: root},
			Leases:  &dataplane.FileLeaseAuthority{Path: filepath.Join(root, "leases.json")},
			Runtime: dataplane.NoneRuntime{},
		},
		Token:      "sekrit",
		LedgerPath: ledger,
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp, body := do(t, "GET", ts.URL+"/api/usage?window=24h", "", "sekrit")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body=%v", resp.StatusCode, body)
	}
	if body["available"] != true {
		t.Fatalf("available: %#v", body["available"])
	}
	total, _ := body["total"].(map[string]any)
	if total == nil {
		t.Fatalf("missing total: %#v", body)
	}
	if total["requests"].(float64) != 1 {
		t.Fatalf("requests: %#v", total["requests"])
	}
	if total["input_tokens"].(float64) != 1000 {
		t.Fatalf("input_tokens: %#v", total["input_tokens"])
	}
	if total["cost_usd"].(float64) <= 0 {
		t.Fatalf("expected positive cost, got %#v", total["cost_usd"])
	}
}

func TestUsageRequiresAuth(t *testing.T) {
	ts, _ := newTestServer(t, "sekrit")
	if resp, _ := do(t, "GET", ts.URL+"/api/usage", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: got %d", resp.StatusCode)
	}
}
