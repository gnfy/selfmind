package cli

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
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

func wrapText(s string, width int) string {
	if width <= 0 {
		return s
	}
	var result []string
	for _, line := range strings.Split(s, "\n") {
		if runewidth.StringWidth(stripANSI(line)) <= width {
			result = append(result, line)
			continue
		}
		var cur strings.Builder
		curWidth := 0
		words := strings.Fields(line)
		for _, w := range words {
			wWidth := runewidth.StringWidth(stripANSI(w))
			if curWidth+wWidth+1 <= width {
				if cur.Len() > 0 {
					cur.WriteString(" ")
					curWidth += 1
				}
				cur.WriteString(w)
				curWidth += wWidth
			} else {
				if cur.Len() > 0 {
					result = append(result, cur.String())
				}
				cur.Reset()
				cur.WriteString(w)
				curWidth = wWidth
			}
		}
		if cur.Len() > 0 {
			result = append(result, cur.String())
		}
	}
	return strings.Join(result, "\n")
}

var (
	inlineCodeRegex   = regexp.MustCompile("`.*?`")
	inlineBoldRegex   = regexp.MustCompile(`\*\*.*?\*\*`)
	inlineItalicRegex = regexp.MustCompile(`\*[^* ][^* \n]*\*`)
	inlineLinkRegex   = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
)

func renderMarkdown(s string, width int) string {
	if width < 8 {
		width = 8
	}
	codeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	var result strings.Builder
	lines := strings.Split(s, "\n")
	inCodeBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inCodeBlock = !inCodeBlock
			continue
		}
		if inCodeBlock {
			result.WriteString(codeStyle.Render(line) + "\n")
			continue
		}
		line = inlineCodeRegex.ReplaceAllStringFunc(line, func(match string) string {
			return lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Render(match[1 : len(match)-1])
		})
		line = inlineBoldRegex.ReplaceAllStringFunc(line, func(match string) string {
			return lipgloss.NewStyle().Bold(true).Render(match[2 : len(match)-2])
		})
		line = inlineItalicRegex.ReplaceAllStringFunc(line, func(match string) string {
			return lipgloss.NewStyle().Italic(true).Render(match[1 : len(match)-1])
		})
		line = inlineLinkRegex.ReplaceAllString(line, "$1 ($2)")
		result.WriteString(wrapText(line, width) + "\n")
	}
	return result.String()
}
