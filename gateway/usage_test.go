package gateway

import (
	"testing"
	"time"
)

func TestEstimateUSD(t *testing.T) {
	// claude-sonnet-4 → $3 / $15 per MTok
	got := EstimateUSD("claude-sonnet-4-5", 1_000_000, 1_000_000, nil)
	want := 3.0 + 15.0
	if got != want {
		t.Fatalf("EstimateUSD = %v, want %v", got, want)
	}
}

func TestPriceForLongestPrefix(t *testing.T) {
	p := PriceFor("claude-sonnet-4-5-20250929", nil)
	if p.InputPM != 3 || p.OutputPM != 15 {
		t.Fatalf("wrong price: %+v", p)
	}
	p = PriceFor("anthropic/claude-haiku-4-5", nil)
	if p.InputPM != 1 {
		t.Fatalf("prefix strip failed: %+v", p)
	}
}

func TestSummarizeUsage(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	events := []Event{
		{Time: now.Add(-1 * time.Hour), Workspace: "ws1", Route: "r1", Model: "claude-sonnet-4-5",
			Outcome: "forwarded", InputTokens: 1000, OutputTokens: 500, ReqBytes: 100, RespBytes: 50},
		{Time: now.Add(-30 * time.Minute), Workspace: "ws1", Route: "r1", Model: "claude-sonnet-4-5",
			Outcome: "blocked", ReqBytes: 20},
		{Time: now.Add(-2 * time.Hour), Workspace: "ws2", Route: "r2", Model: "gpt-4o",
			Outcome: "forwarded", InputTokens: 2000, OutputTokens: 100},
		{Time: now.Add(-48 * time.Hour), Workspace: "old", Route: "r1", Model: "claude-sonnet-4-5",
			Outcome: "forwarded", InputTokens: 99999, OutputTokens: 99999}, // outside window
	}
	sum := SummarizeUsage(events, now.Add(-24*time.Hour), now, "audit.jsonl")
	if sum.Total.Requests != 3 {
		t.Fatalf("requests = %d, want 3", sum.Total.Requests)
	}
	if sum.Total.Forwarded != 2 || sum.Total.Blocked != 1 {
		t.Fatalf("outcomes: %+v", sum.Total)
	}
	if sum.Total.InputTokens != 3000 || sum.Total.OutputTokens != 600 {
		t.Fatalf("tokens: %+v", sum.Total)
	}
	if sum.Total.CostUSD <= 0 {
		t.Fatalf("expected positive cost, got %v", sum.Total.CostUSD)
	}
	if len(sum.ByWorkspace) != 2 {
		t.Fatalf("workspaces: %+v", sum.ByWorkspace)
	}
	if sum.ByWorkspace[0].Key != "ws1" && sum.ByWorkspace[0].Key != "ws2" {
		t.Fatalf("unexpected first workspace: %+v", sum.ByWorkspace[0])
	}
}
