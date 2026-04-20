package kernel

import (
	"context"
	"fmt"
	"selfmind/internal/kernel/memory"
	"time"
)

// SkillStore wraps MemoryManager to track skill usage metrics and prune low-value skills.
type SkillStore struct {
	mem *memory.MemoryManager
}

// NewSkillStore creates a SkillStore backed by the given MemoryManager.
func NewSkillStore(mem *memory.MemoryManager) *SkillStore {
	return &SkillStore{mem: mem}
}

// RecordCall increments the call counter for a skill after a successful execution.
func (s *SkillStore) RecordCall(ctx context.Context, tenantID, skillName string) error {
	return s.mem.RecordSkillCall(ctx, tenantID, skillName)
}

// RecordFailure increments the failure counter for a skill after a failed execution.
func (s *SkillStore) RecordFailure(ctx context.Context, tenantID, skillName string) error {
	return s.mem.RecordSkillFailure(ctx, tenantID, skillName)
}

// RecordResult records a skill execution result. success=true means the skill completed without error.
func (s *SkillStore) RecordResult(ctx context.Context, tenantID, skillName string, success bool) error {
	if success {
		return s.mem.RecordSkillCall(ctx, tenantID, skillName)
	}
	return s.mem.RecordSkillFailure(ctx, tenantID, skillName)
}

// Prune removes low-value skill metrics (call_count < 3 AND idle > thresholdDays).
// Returns the number of records pruned.
func (s *SkillStore) Prune(ctx context.Context, tenantID string, thresholdDays int) (int, error) {
	return s.mem.PruneSkills(ctx, tenantID, thresholdDays)
}

// GetStats returns all skill metrics for a tenant, sorted by last_used descending.
func (s *SkillStore) GetStats(ctx context.Context, tenantID string) ([]memory.SkillMetric, error) {
	metrics, err := s.mem.ListSkillMetrics(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	// Sort by last_used descending (most recently used first)
	for i := 0; i < len(metrics)-1; i++ {
		for j := i + 1; j < len(metrics); j++ {
			if metrics[j].LastUsed.After(metrics[i].LastUsed) {
				metrics[i], metrics[j] = metrics[j], metrics[i]
			}
		}
	}
	return metrics, nil
}

// FormatStats returns a human-readable summary of skill metrics.
func (s *SkillStore) FormatStats(ctx context.Context, tenantID string) (string, error) {
	metrics, err := s.GetStats(ctx, tenantID)
	if err != nil {
		return "", err
	}
	if len(metrics) == 0 {
		return "暂无技能调用记录。", nil
	}

	var lines []string
	lines = append(lines, "## 技能调用统计")
	lines = append(lines, fmt.Sprintf("%-30s %6s %6s %s", "技能名", "调用", "失败", "最近使用"))
	lines = append(lines, "---")

	for _, m := range metrics {
		ago := time.Since(m.LastUsed).Truncate(time.Hour)
		rate := float64(m.FailCount) / float64(m.CallCount+1)
		flag := ""
		if rate > 0.3 && m.CallCount > 3 {
			flag = " ⚠️ 高失败率"
		}
		lines = append(lines, fmt.Sprintf("%-30s %6d %6d %s ago%s",
			m.SkillName, m.CallCount, m.FailCount, ago, flag))
	}
	return fmt.Sprintf("共 %d 个技能有调用记录：\n\n%s", len(metrics), joinLines(lines)), nil
}

// PruneWithDefaults runs prune with the standard threshold (30 days idle, < 3 calls).
func (s *SkillStore) PruneWithDefaults(ctx context.Context, tenantID string) (int, error) {
	return s.Prune(ctx, tenantID, 30)
}

// RecordSkillResultFn returns a function suitable for the dispatcher middleware to
// record skill execution results. The returned closure captures skillName and tenantID.
func (s *SkillStore) RecordSkillResultFn(tenantID, skillName string) func(success bool) {
	return func(success bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Best-effort; swallow errors to not block tool execution
		_ = s.RecordResult(ctx, tenantID, skillName, success)
	}
}

// joinLines is a helper to join strings without importing strings package.
func joinLines(lines []string) string {
	result := ""
	for i, l := range lines {
		if i > 0 {
			result += "\n"
		}
		result += l
	}
	return result
}
