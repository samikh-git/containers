// Package applog configures process-wide structured logging (slog) for the
// gateway and router binaries. Keep token values out of logs — use TokenPrefix.
package applog

import (
	"log/slog"
	"os"
	"strings"
)

// Configure sets the default slog handler writing text to stderr.
// level is "debug", "info", "warn", or "error" (default info).
func Configure(level string) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLevel(level),
	})))
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// TokenPrefix returns a short redacted form of a session token for logs.
// Empty tokens are reported as "<empty>" so missing credentials are obvious.
func TokenPrefix(token string) string {
	if token == "" {
		return "<empty>"
	}
	if len(token) <= 10 {
		return token[:min(4, len(token))] + "…"
	}
	return token[:8] + "…"
}
