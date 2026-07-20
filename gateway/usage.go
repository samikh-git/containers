package gateway

import (
	"math"
	"sort"
	"strings"
	"time"
)

// ModelPrice is list-price USD per million tokens. Used for operator cost
// estimates in the admin console — not billing.
type ModelPrice struct {
	Prefix   string  // matched against the request model (longest prefix wins)
	InputPM  float64 // USD per 1M input tokens
	OutputPM float64 // USD per 1M output tokens
}

// DefaultPrices are approximate public list prices for common models.
// Unknown models fall back to UnknownModelPrice so totals stay visible.
var DefaultPrices = []ModelPrice{
	{Prefix: "claude-opus-4", InputPM: 15, OutputPM: 75},
	{Prefix: "claude-sonnet-4", InputPM: 3, OutputPM: 15},
	{Prefix: "claude-haiku-4", InputPM: 1, OutputPM: 5},
	{Prefix: "claude-3-5-sonnet", InputPM: 3, OutputPM: 15},
	{Prefix: "claude-3-5-haiku", InputPM: 0.80, OutputPM: 4},
	{Prefix: "claude-3-opus", InputPM: 15, OutputPM: 75},
	{Prefix: "claude-3-sonnet", InputPM: 3, OutputPM: 15},
	{Prefix: "claude-3-haiku", InputPM: 0.25, OutputPM: 1.25},
	{Prefix: "claude-sonnet", InputPM: 3, OutputPM: 15},
	{Prefix: "claude-opus", InputPM: 15, OutputPM: 75},
	{Prefix: "claude-haiku", InputPM: 1, OutputPM: 5},
	{Prefix: "gpt-4.1-mini", InputPM: 0.40, OutputPM: 1.60},
	{Prefix: "gpt-4.1-nano", InputPM: 0.10, OutputPM: 0.40},
	{Prefix: "gpt-4.1", InputPM: 2, OutputPM: 8},
	{Prefix: "gpt-4o-mini", InputPM: 0.15, OutputPM: 0.60},
	{Prefix: "gpt-4o", InputPM: 2.50, OutputPM: 10},
	{Prefix: "o3-mini", InputPM: 1.10, OutputPM: 4.40},
	{Prefix: "o3", InputPM: 2, OutputPM: 8},
	{Prefix: "o1-mini", InputPM: 1.10, OutputPM: 4.40},
	{Prefix: "o1", InputPM: 15, OutputPM: 60},
	{Prefix: "gemini-2.5-pro", InputPM: 1.25, OutputPM: 10},
	{Prefix: "gemini-2.5-flash", InputPM: 0.15, OutputPM: 0.60},
	{Prefix: "gemini-2.0-flash", InputPM: 0.10, OutputPM: 0.40},
}

// UnknownModelPrice is the estimate used when no DefaultPrices prefix matches.
var UnknownModelPrice = ModelPrice{InputPM: 3, OutputPM: 15}

// PriceFor returns the best matching ModelPrice for a model id.
func PriceFor(model string, table []ModelPrice) ModelPrice {
	if table == nil {
		table = DefaultPrices
	}
	model = strings.ToLower(strings.TrimSpace(model))
	// Strip provider prefixes like "anthropic/" or "openai/".
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	best := UnknownModelPrice
	bestLen := -1
	for _, p := range table {
		pref := strings.ToLower(p.Prefix)
		if strings.HasPrefix(model, pref) && len(pref) > bestLen {
			best = p
			bestLen = len(pref)
		}
	}
	return best
}

// EstimateUSD returns estimated USD cost for the given token counts.
func EstimateUSD(model string, inputTokens, outputTokens int64, table []ModelPrice) float64 {
	p := PriceFor(model, table)
	usd := float64(inputTokens)*p.InputPM/1e6 + float64(outputTokens)*p.OutputPM/1e6
	return roundUSD(usd)
}

func roundUSD(v float64) float64 {
	return math.Round(v*1e6) / 1e6
}

// UsageBucket is one aggregation row (workspace, model, or route).
type UsageBucket struct {
	Key          string  `json:"key"`
	Requests     int64   `json:"requests"`
	Forwarded    int64   `json:"forwarded"`
	Blocked      int64   `json:"blocked"`
	Errors       int64   `json:"errors"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	ReqBytes     int64   `json:"req_bytes"`
	RespBytes    int64   `json:"resp_bytes"`
	CostUSD      float64 `json:"cost_usd"`
	PricedReqs   int64   `json:"priced_requests"` // requests that had token counts
}

// UsageSummary is the operator cost/usage view over a ledger window.
type UsageSummary struct {
	WindowStart time.Time     `json:"window_start"`
	WindowEnd   time.Time     `json:"window_end"`
	LedgerPath  string        `json:"ledger_path,omitempty"`
	Available   bool          `json:"available"` // false when no ledger configured/readable
	Note        string        `json:"note,omitempty"`
	Total       UsageBucket   `json:"total"`
	ByWorkspace []UsageBucket `json:"by_workspace"`
	ByModel     []UsageBucket `json:"by_model"`
	ByRoute     []UsageBucket `json:"by_route"`
}

// SummarizeUsage aggregates events in [since, until]. Events outside the
// window are skipped. Cost is estimated from token counts × DefaultPrices;
// requests without tokens contribute to counts/bytes but not CostUSD.
func SummarizeUsage(events []Event, since, until time.Time, ledgerPath string) UsageSummary {
	sum := UsageSummary{
		WindowStart: since,
		WindowEnd:   until,
		LedgerPath:  ledgerPath,
		Available:   true,
		Note:        "cost_usd is an operator estimate from public list prices; not an invoice",
		Total:       UsageBucket{Key: "total"},
	}
	byWS := map[string]*UsageBucket{}
	byModel := map[string]*UsageBucket{}
	byRoute := map[string]*UsageBucket{}

	add := func(m map[string]*UsageBucket, key string, e Event, cost float64) {
		if key == "" {
			key = "(none)"
		}
		b, ok := m[key]
		if !ok {
			b = &UsageBucket{Key: key}
			m[key] = b
		}
		accumulate(b, e, cost)
	}

	for _, e := range events {
		if e.Time.Before(since) || e.Time.After(until) {
			continue
		}
		var cost float64
		if e.InputTokens > 0 || e.OutputTokens > 0 {
			cost = EstimateUSD(e.Model, e.InputTokens, e.OutputTokens, nil)
		}
		accumulate(&sum.Total, e, cost)
		add(byWS, e.Workspace, e, cost)
		add(byModel, e.Model, e, cost)
		add(byRoute, e.Route, e, cost)
	}

	sum.ByWorkspace = sortedBuckets(byWS)
	sum.ByModel = sortedBuckets(byModel)
	sum.ByRoute = sortedBuckets(byRoute)
	sum.Total.CostUSD = roundUSD(sum.Total.CostUSD)
	return sum
}

func accumulate(b *UsageBucket, e Event, cost float64) {
	b.Requests++
	switch e.Outcome {
	case "forwarded":
		b.Forwarded++
	case "blocked":
		b.Blocked++
	case "upstream_error", "auth_failed":
		b.Errors++
	}
	b.InputTokens += e.InputTokens
	b.OutputTokens += e.OutputTokens
	b.ReqBytes += e.ReqBytes
	b.RespBytes += e.RespBytes
	if e.InputTokens > 0 || e.OutputTokens > 0 {
		b.PricedReqs++
		b.CostUSD = roundUSD(b.CostUSD + cost)
	}
}

func sortedBuckets(m map[string]*UsageBucket) []UsageBucket {
	out := make([]UsageBucket, 0, len(m))
	for _, b := range m {
		out = append(out, *b)
	}
	// Cost desc, then requests desc, then key asc — useful for the admin table.
	sort.Slice(out, func(i, j int) bool {
		if out[i].CostUSD != out[j].CostUSD {
			return out[i].CostUSD > out[j].CostUSD
		}
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Key < out[j].Key
	})
	return out
}
