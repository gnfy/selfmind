package httpapi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/control"
)

// modelsDiagReply exposes the physical cost of background maintenance routes.
// It deliberately excludes prompts and response bodies; operators only need
// routing, outcomes, and aggregate token usage to diagnose fallback churn.
func (d *Server) modelsDiagReply(ctx context.Context, identity *control.IdentityContext) (string, error) {
	if d == nil || d.Control == nil || identity == nil {
		return "Maintenance model diagnostics unavailable.", nil
	}
	usage, err := d.Control.MaintenanceProviderUsageSince(ctx, identity.TenantID, time.Now().Add(-24*time.Hour))
	if err != nil {
		return "", err
	}
	if len(usage) == 0 {
		return "Maintenance models (24h)\nNo provider calls recorded.", nil
	}

	var sb strings.Builder
	sb.WriteString("Maintenance models (24h)\n")
	for _, item := range usage {
		provider := strings.TrimSpace(item.Provider)
		if provider == "" {
			provider = "unknown"
		}
		model := strings.TrimSpace(item.Model)
		if model != "" {
			provider += "/" + model
		}
		role := strings.TrimSpace(item.Role)
		if role == "" {
			role = "maintenance"
		}
		fmt.Fprintf(&sb, "- %s [%s]: calls %d (ok %d, failed %d, circuit %d)\n",
			provider, role, item.Calls, item.Succeeded, item.Failed, item.CircuitOpen)
		fmt.Fprintf(&sb, "  tokens: input %s, output %s, cache read %s, cache write %s\n",
			formatMaintenanceTokens(item.InputTokens), formatMaintenanceTokens(item.OutputTokens),
			formatMaintenanceTokens(item.CacheReadInputTokens), formatMaintenanceTokens(item.CacheCreationInputTokens))
	}
	return strings.TrimSpace(sb.String()), nil
}

func formatMaintenanceTokens(value int64) string {
	switch {
	case value >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(value)/1_000_000)
	case value >= 1_000:
		return fmt.Sprintf("%.1fK", float64(value)/1_000)
	default:
		return fmt.Sprintf("%d", value)
	}
}
