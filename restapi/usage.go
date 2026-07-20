package restapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"containerization/gateway"
)

// usage serves GET /api/usage — operator cost/usage summary over the gateway
// audit ledger. Query params:
//
//	window — duration lookback (default 24h); accepted units: h, m, or bare
//	         hours as an integer ("24"). Cap 30d.
//	since  — RFC3339 start (overrides window when set)
//	until  — RFC3339 end (default now)
func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	if s.LedgerPath == "" {
		writeJSON(w, http.StatusOK, gateway.UsageSummary{
			Available: false,
			Note:      "no ledger configured — pass --ledger to router serve",
			Total:     gateway.UsageBucket{Key: "total"},
		})
		return
	}

	until := time.Now().UTC()
	if v := r.URL.Query().Get("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			httpError(w, http.StatusBadRequest, "until must be RFC3339")
			return
		}
		until = t.UTC()
	}
	since := until.Add(-24 * time.Hour)
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			httpError(w, http.StatusBadRequest, "since must be RFC3339")
			return
		}
		since = t.UTC()
	} else if v := r.URL.Query().Get("window"); v != "" {
		d, err := parseUsageWindow(v)
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		since = until.Add(-d)
	}

	events, err := gateway.ReadEvents(s.LedgerPath)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "ledger unreadable: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, gateway.SummarizeUsage(events, since, until, s.LedgerPath))
}

func parseUsageWindow(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 24 * time.Hour, nil
	}
	if d, err := time.ParseDuration(v); err == nil {
		if d <= 0 || d > 30*24*time.Hour {
			return 0, errWindowRange
		}
		return d, nil
	}
	// Bare integer = hours (admin UI convenience).
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || n > 30*24 {
		return 0, errWindowRange
	}
	return time.Duration(n) * time.Hour, nil
}

type windowError string

func (e windowError) Error() string { return string(e) }

const errWindowRange windowError = "window must be a positive duration ≤ 720h (30d)"
