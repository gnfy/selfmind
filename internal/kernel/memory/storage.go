package memory

import (
	"context"
	"fmt"
	"time"
)

// FTS5Session 用于存储会话摘要供全文搜索使用
type FTS5Session struct {
	SessionID string `json:"session_id"`
	Channel   string `json:"channel"`
	Content   string `json:"content"`
	Summary   string `json:"summary"`
	Timestamp int64  `json:"timestamp"`
}

// ProcessRecord 用于持久化后台进程状态
type ProcessRecord struct {
	ID         string    `json:"id"`
	Command    string    `json:"command"`
	CWD        string    `json:"cwd"`
	PID        int       `json:"pid"`
	Status     string    `json:"status"` // "running", "exited", "failed", "lost"
	ExitCode   int       `json:"exit_code"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// SkillMetric tracks usage metrics for a skill per tenant.
type SkillMetric struct {
	SkillName string    `json:"skill_name"`
	TenantID  string    `json:"tenant_id"`
	CallCount int       `json:"call_count"`
	FailCount int       `json:"fail_count"`
	LastUsed  time.Time `json:"last_used"`
}

// StorageProvider 定义存储后端接口，以支持 SQLite, Postgres, MySQL 等
type StorageProvider interface {
	SaveTrajectory(ctx context.Context, tenantID, channel string, traj []byte) error
	GetLatestContext(ctx context.Context, tenantID, channel string) ([][]byte, error)
	IndexMessagesFromTrajectory(ctx context.Context, tenantID, channel, sessionID string, messagesJSON []byte) error
	SearchSessions(tenantID, query string, limit int) ([]FTS5Session, error)
	// Checkpoint operations
	SaveCheckpoint(ctx context.Context, tenantID, channel, name string, messages []byte) error
	ListCheckpoints(ctx context.Context, tenantID, channel string) ([]Checkpoint, error)
	LoadCheckpoint(ctx context.Context, tenantID, channel, name string) ([]byte, error)
	DeleteCheckpoint(ctx context.Context, tenantID, channel, name string) error

	// Fact (Long-term Memory) operations
	AddFact(ctx context.Context, tenantID string, target, content string) error
	GetFacts(ctx context.Context, tenantID string, target string) ([]Fact, error)
	RemoveFact(ctx context.Context, tenantID string, id string) error

	// Permission operations
	SetPermission(ctx context.Context, tenantID, toolName string, allowed bool) error
	GetPermission(ctx context.Context, tenantID, toolName string) (bool, error)

	// Secret operations
	SetSecret(ctx context.Context, tenantID, keyName, value string) error
	GetSecret(ctx context.Context, tenantID, keyName string) (string, error)

	// Process operations
	SaveProcess(ctx context.Context, tenantID string, proc ProcessRecord) error
	UpdateProcessStatus(ctx context.Context, tenantID, id, status string, exitCode int) error
	ListProcesses(ctx context.Context, tenantID string) ([]ProcessRecord, error)
	GetProcess(ctx context.Context, tenantID, id string) (*ProcessRecord, error)

	// Skill metrics operations
	RecordSkillCall(ctx context.Context, tenantID, skillName string) error
	RecordSkillFailure(ctx context.Context, tenantID, skillName string) error
	ListSkillMetrics(ctx context.Context, tenantID string) ([]SkillMetric, error)
	PruneSkills(ctx context.Context, tenantID string, thresholdDays int) (int, error)
	GetSkillMetric(ctx context.Context, tenantID, skillName string) (*SkillMetric, error)
}

// MemoryManager 管理存储后端并处理租户上下文
type MemoryManager struct {
	provider  StorageProvider
	providers map[string]MemoryProvider
}

func NewMemoryManager(p StorageProvider) *MemoryManager {
	return &MemoryManager{provider: p, providers: make(map[string]MemoryProvider)}
}

// SaveTrajectory 保存一次完整的交互轨迹
func (m *MemoryManager) SaveTrajectory(ctx context.Context, tenantID, channel string, traj []byte) error {
	if m.provider == nil {
		return nil
	}
	return m.provider.SaveTrajectory(ctx, tenantID, channel, traj)
}

// GetLatestContext 获取最近的历史上下文（按渠道隔离）
func (m *MemoryManager) GetLatestContext(ctx context.Context, tenantID, channel string) ([][]byte, error) {
	if m.provider == nil {
		return nil, nil
	}
	return m.provider.GetLatestContext(ctx, tenantID, channel)
}

// IndexSession 将一个会话摘要写入 FTS5 索引
func (m *MemoryManager) IndexSession(ctx context.Context, tenantID, channel, sessionID string, messagesJSON []byte) error {
	if m.provider == nil {
		return nil
	}
	return m.provider.IndexMessagesFromTrajectory(ctx, tenantID, channel, sessionID, messagesJSON)
}

// SearchSessions performs a full-text search on session history.
func (m *MemoryManager) SearchSessions(tenantID, query string, limit int) ([]FTS5Session, error) {
	if m.provider == nil {
		return nil, nil
	}
	return m.provider.SearchSessions(tenantID, query, limit)
}

// Close 关闭存储后端
func (m *MemoryManager) Close() error {
	if m.provider == nil {
		return nil
	}
	if closer, ok := m.provider.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

// SearchFn returns a search function for the session_search tool.
func (m *MemoryManager) SearchFn(tenantID string) func(query string, limit int) (interface{}, error) {
	return func(query string, limit int) (interface{}, error) {
		if m.provider == nil {
			return nil, fmt.Errorf("memory provider not initialized")
		}
		sessions, err := m.provider.SearchSessions(tenantID, query, limit)
		if err != nil {
			return nil, err
		}
		return sessions, nil
	}
}

// SaveCheckpoint saves a named snapshot of the current conversation.
func (m *MemoryManager) SaveCheckpoint(ctx context.Context, tenantID, channel, name string, messages []byte) error {
	if m.provider == nil {
		return fmt.Errorf("memory provider not initialized")
	}
	return m.provider.SaveCheckpoint(ctx, tenantID, channel, name, messages)
}

// ListCheckpoints returns all checkpoints for a tenant/channel.
func (m *MemoryManager) ListCheckpoints(ctx context.Context, tenantID, channel string) ([]Checkpoint, error) {
	if m.provider == nil {
		return nil, fmt.Errorf("memory provider not initialized")
	}
	return m.provider.ListCheckpoints(ctx, tenantID, channel)
}

// LoadCheckpoint retrieves a checkpoint by name and returns its messages.
func (m *MemoryManager) LoadCheckpoint(ctx context.Context, tenantID, channel, name string) ([]byte, error) {
	if m.provider == nil {
		return nil, fmt.Errorf("memory provider not initialized")
	}
	return m.provider.LoadCheckpoint(ctx, tenantID, channel, name)
}

// DeleteCheckpoint removes a named checkpoint.
func (m *MemoryManager) DeleteCheckpoint(ctx context.Context, tenantID, channel, name string) error {
	if m.provider == nil {
		return fmt.Errorf("memory provider not initialized")
	}
	return m.provider.DeleteCheckpoint(ctx, tenantID, channel, name)
}

// AddFact saves a new fact.
func (m *MemoryManager) AddFact(ctx context.Context, tenantID string, target, content string) error {
	if m.provider == nil {
		return fmt.Errorf("memory provider not initialized")
	}
	return m.provider.AddFact(ctx, tenantID, target, content)
}

// GetFacts retrieves all facts for a target.
func (m *MemoryManager) GetFacts(ctx context.Context, tenantID string, target string) ([]Fact, error) {
	if m.provider == nil {
		return nil, fmt.Errorf("memory provider not initialized")
	}
	return m.provider.GetFacts(ctx, tenantID, target)
}

// RemoveFact deletes a fact by ID.
func (m *MemoryManager) RemoveFact(ctx context.Context, tenantID string, id string) error {
	if m.provider == nil {
		return fmt.Errorf("memory provider not initialized")
	}
	return m.provider.RemoveFact(ctx, tenantID, id)
}

// SetPermission sets the permission for a tool.
func (m *MemoryManager) SetPermission(ctx context.Context, tenantID, toolName string, allowed bool) error {
	if m.provider == nil {
		return fmt.Errorf("memory provider not initialized")
	}
	return m.provider.SetPermission(ctx, tenantID, toolName, allowed)
}

// GetPermission retrieves the permission for a tool.
func (m *MemoryManager) GetPermission(ctx context.Context, tenantID, toolName string) (bool, error) {
	if m.provider == nil {
		return true, nil // Default to allowed if no provider
	}
	return m.provider.GetPermission(ctx, tenantID, toolName)
}

// SetSecret saves a sensitive string (like API Key) to the storage.
func (m *MemoryManager) SetSecret(ctx context.Context, tenantID, keyName, value string) error {
	if m.provider == nil {
		return fmt.Errorf("memory provider not initialized")
	}
	return m.provider.SetSecret(ctx, tenantID, keyName, value)
}

// GetSecret retrieves a sensitive string from the storage.
func (m *MemoryManager) GetSecret(ctx context.Context, tenantID, keyName string) (string, error) {
	if m.provider == nil {
		return "", nil
	}
	return m.provider.GetSecret(ctx, tenantID, keyName)
}

// SaveProcess persists process info.
func (m *MemoryManager) SaveProcess(ctx context.Context, tenantID string, proc ProcessRecord) error {
	if m.provider == nil {
		return nil
	}
	return m.provider.SaveProcess(ctx, tenantID, proc)
}

// UpdateProcessStatus updates process status.
func (m *MemoryManager) UpdateProcessStatus(ctx context.Context, tenantID, id, status string, exitCode int) error {
	if m.provider == nil {
		return nil
	}
	return m.provider.UpdateProcessStatus(ctx, tenantID, id, status, exitCode)
}

// ListProcesses returns all processes for a tenant.
func (m *MemoryManager) ListProcesses(ctx context.Context, tenantID string) ([]ProcessRecord, error) {
	if m.provider == nil {
		return nil, nil
	}
	return m.provider.ListProcesses(ctx, tenantID)
}

// GetProcess retrieves process info by ID.
func (m *MemoryManager) GetProcess(ctx context.Context, tenantID, id string) (*ProcessRecord, error) {
	if m.provider == nil {
		return nil, nil
	}
	return m.provider.GetProcess(ctx, tenantID, id)
}

// RecordSkillCall increments the call count for a skill.
func (m *MemoryManager) RecordSkillCall(ctx context.Context, tenantID, skillName string) error {
	if m.provider == nil {
		return nil
	}
	return m.provider.RecordSkillCall(ctx, tenantID, skillName)
}

// RecordSkillFailure increments the failure count for a skill.
func (m *MemoryManager) RecordSkillFailure(ctx context.Context, tenantID, skillName string) error {
	if m.provider == nil {
		return nil
	}
	return m.provider.RecordSkillFailure(ctx, tenantID, skillName)
}

// ListSkillMetrics returns all skill metrics for a tenant.
func (m *MemoryManager) ListSkillMetrics(ctx context.Context, tenantID string) ([]SkillMetric, error) {
	if m.provider == nil {
		return nil, nil
	}
	return m.provider.ListSkillMetrics(ctx, tenantID)
}

// PruneSkills removes low-value skill metrics based on call count and recency thresholds.
func (m *MemoryManager) PruneSkills(ctx context.Context, tenantID string, thresholdDays int) (int, error) {
	if m.provider == nil {
		return 0, nil
	}
	return m.provider.PruneSkills(ctx, tenantID, thresholdDays)
}

// GetSkillMetric returns the metric record for a single skill.
func (m *MemoryManager) GetSkillMetric(ctx context.Context, tenantID, skillName string) (*SkillMetric, error) {
	if m.provider == nil {
		return nil, nil
	}
	return m.provider.GetSkillMetric(ctx, tenantID, skillName)
}
