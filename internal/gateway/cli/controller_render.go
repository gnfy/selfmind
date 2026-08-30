package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
	"selfmind/internal/ui/components"
)

// Pure formatting and rendering helpers for the CLI transcript and status line.
// Extracted from controller.go (see AGENTS.md) — no behavior change.

func formatUsage(usage, limit int) string {
	if limit <= 0 {
		return fmt.Sprintf("%s run · ctx ?", compactCount(usage))
	}
	return fmt.Sprintf("%s run · %s ctx", compactCount(usage), formatContextLimit(limit))
}

// formatUsageSession adds the session-cumulative token count for cost
// awareness: "12.0K run · 340.0K session · 1.05M ctx".
func formatUsageSession(run, session, limit int) string {
	ctx := "ctx ?"
	if limit > 0 {
		ctx = formatContextLimit(limit) + " ctx"
	}
	return fmt.Sprintf("%s run · %s session · %s", compactCount(run), compactCount(session), ctx)
}

// formatContextLimit shows the context window with more precision than
// compactCount: a 1,050,000-token window reads "1.05M" instead of a rounded
// "1.1M". Sub-million values keep the compact K form.
func formatContextLimit(n int) string {
	if n >= 1_000_000 {
		s := fmt.Sprintf("%.2f", float64(n)/1_000_000)
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
		return s + "M"
	}
	return compactCount(n)
}

// resolveUIModelMeta returns a compact, real model-runtime descriptor for the
// status line (reasoning effort and/or service tier). It returns "" when none
// is configured, so the status line shows nothing rather than a placeholder.
func resolveUIModelMeta(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	rt, err := modelruntime.NewResolver(cfg).Resolve(context.Background(), modelruntime.Selection{})
	if err != nil {
		return ""
	}
	var parts []string
	if e := strings.TrimSpace(rt.ReasoningEffort); e != "" {
		parts = append(parts, e)
	}
	if t := strings.TrimSpace(rt.ServiceTier); t != "" {
		parts = append(parts, t)
	}
	return strings.Join(parts, " ")
}

func resolveUITokenLimit(cfg *config.Config, providerName, modelName string) int {
	if cfg != nil {
		rt, err := modelruntime.NewResolver(cfg).Resolve(context.Background(), modelruntime.Selection{})
		if err == nil && rt.ContextLength > 0 {
			return rt.ContextLength
		}
	}
	return modelruntime.KnownContextLength(providerName, modelName)
}

func compactCount(n int) string {
	if n >= 1_000_000 {
		if n%1_000_000 == 0 {
			return fmt.Sprintf("%dM", n/1_000_000)
		}
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1000 {
		if n%1000 == 0 {
			return fmt.Sprintf("%dK", n/1000)
		}
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}

func formatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// formatElapsedCompact renders a running duration codex-style: "0s", "59s",
// "1m 23s", "1h 02m 05s".
func formatElapsedCompact(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d.Seconds())
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %02dm %02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm %02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

func valueOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func renderProgressBar(progress float64, width int) string {
	filled := int(progress * float64(width))
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", width-filled) + "]"
}

func renderMarkdown(s string, width int) string {
	return components.RenderMarkdown(s, width)
}
