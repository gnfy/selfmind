package control

import (
	"context"
	"time"
)

const (
	MaintenanceProviderCallSucceeded   = "succeeded"
	MaintenanceProviderCallFailed      = "failed"
	MaintenanceProviderCallCircuitOpen = "circuit_open"
)

// MaintenanceProviderCall is one physical provider attempt in the background
// maintenance chain. It intentionally stores only redacted route metadata and
// aggregate usage; prompts, credentials, and response bodies never enter this
// diagnostics table.
type MaintenanceProviderCall struct {
	TenantID                 string
	Role                     string
	Provider                 string
	Model                    string
	RouteID                  string
	CandidateIndex           int
	Status                   string
	TriggerClass             string
	FinishReason             string
	ErrorClass               string
	InputTokens              int
	OutputTokens             int
	CacheReadInputTokens     int
	CacheMissInputTokens     int
	CacheCreationInputTokens int
	ReasoningOutputTokens    int
	CacheUsageReported       bool
	BatchSize                int
	LatencyMS                int64
	CreatedAt                time.Time
}

type MaintenanceProviderUsage struct {
	Role                     string
	Provider                 string
	Model                    string
	Calls                    int
	Succeeded                int
	Failed                   int
	CircuitOpen              int
	InputTokens              int64
	OutputTokens             int64
	CacheReadInputTokens     int64
	CacheMissInputTokens     int64
	CacheCreationInputTokens int64
	ReasoningOutputTokens    int64
	CacheUsageReportedCalls  int
}

func (s *Store) RecordMaintenanceProviderCall(ctx context.Context, call MaintenanceProviderCall) error {
	if s == nil || s.db == nil {
		return nil
	}
	if call.BatchSize <= 0 {
		call.BatchSize = 1
	}
	if call.CreatedAt.IsZero() {
		call.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO maintenance_provider_calls
		(tenant_id, role, provider, model, route_id, candidate_index, status,
		 trigger_class, finish_reason, error_class, input_tokens, output_tokens,
		 cache_read_input_tokens, cache_miss_input_tokens, cache_creation_input_tokens,
		 reasoning_output_tokens, cache_usage_reported, batch_size, latency_ms, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		normalizeTenant(call.TenantID), boundedText(call.Role, 80), boundedText(call.Provider, 120),
		boundedText(call.Model, 160), boundedText(call.RouteID, 160), call.CandidateIndex,
		boundedText(call.Status, 40), boundedText(call.TriggerClass, 40),
		boundedText(call.FinishReason, 80), boundedText(call.ErrorClass, 40),
		maxInt(call.InputTokens, 0), maxInt(call.OutputTokens, 0),
		maxInt(call.CacheReadInputTokens, 0), maxInt(call.CacheMissInputTokens, 0), maxInt(call.CacheCreationInputTokens, 0),
		maxInt(call.ReasoningOutputTokens, 0), boolInt(call.CacheUsageReported),
		call.BatchSize, maxInt64(call.LatencyMS, 0), call.CreatedAt.Unix())
	return err
}

func (s *Store) MaintenanceProviderUsageSince(ctx context.Context, tenantID string, since time.Time) ([]MaintenanceProviderUsage, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT role, provider, model, COUNT(*),
		SUM(CASE WHEN status = ? THEN 1 ELSE 0 END),
		SUM(CASE WHEN status = ? THEN 1 ELSE 0 END),
		SUM(CASE WHEN status = ? THEN 1 ELSE 0 END),
		SUM(input_tokens), SUM(output_tokens), SUM(cache_read_input_tokens), SUM(cache_miss_input_tokens),
		SUM(cache_creation_input_tokens), SUM(reasoning_output_tokens), SUM(cache_usage_reported)
		FROM maintenance_provider_calls WHERE tenant_id = ? AND created_at >= ?
		GROUP BY role, provider, model ORDER BY SUM(input_tokens + output_tokens) DESC, provider, model`,
		MaintenanceProviderCallSucceeded, MaintenanceProviderCallFailed, MaintenanceProviderCallCircuitOpen,
		normalizeTenant(tenantID), since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MaintenanceProviderUsage
	for rows.Next() {
		var item MaintenanceProviderUsage
		if err := rows.Scan(&item.Role, &item.Provider, &item.Model, &item.Calls,
			&item.Succeeded, &item.Failed, &item.CircuitOpen, &item.InputTokens, &item.OutputTokens,
			&item.CacheReadInputTokens, &item.CacheMissInputTokens, &item.CacheCreationInputTokens,
			&item.ReasoningOutputTokens, &item.CacheUsageReportedCalls); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) PruneMaintenanceProviderCalls(ctx context.Context, olderThan time.Duration) (int, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	if olderThan <= 0 {
		olderThan = 30 * 24 * time.Hour
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM maintenance_provider_calls WHERE created_at < ?`, time.Now().Add(-olderThan).Unix())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func maxInt(value, minimum int) int {
	if value < minimum {
		return minimum
	}
	return value
}

func maxInt64(value, minimum int64) int64 {
	if value < minimum {
		return minimum
	}
	return value
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
