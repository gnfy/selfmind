package task

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Task 表示一个全局任务
type Task struct {
	ID         int64
	UnifiedUID string
	Title      string
	Status     string // pending | in_progress | done | cancelled
	CreatedAt  time.Time
}

// Message 表示任务上下文中的单条消息
type Message struct {
	ID        int64
	TaskID    int64
	Channel   string // 'cli' | 'wechat' | 'dingtalk' | 'web'
	Role      string // 'user' | 'assistant' | 'tool'
	Content   string
}

// Manager 管理全局任务
type Manager struct {
	baseDir string
}

// NewManager 创建一个任务管理器
func NewManager(baseDir string) *Manager {
	return &Manager{baseDir: baseDir}
}

// getDB 获取 tasks.db 的数据库连接
func (m *Manager) getDB() (*sql.DB, error) {
	dbPath := filepath.Join(m.baseDir, "tasks.db")
	db, err := sql.Open("sqlite", dbPath+"?_journal=WAL&_sync=NORMAL")
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS tasks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		unified_uid TEXT NOT NULL,
		title TEXT NOT NULL,
		status TEXT DEFAULT 'in_progress',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE IF NOT EXISTS task_context (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id INTEGER NOT NULL,
		channel TEXT NOT NULL,
		role TEXT NOT NULL,
		content TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (task_id) REFERENCES tasks(id)
	);
	CREATE TABLE IF NOT EXISTS current_task (
		unified_uid TEXT PRIMARY KEY,
		task_id INTEGER NOT NULL,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (task_id) REFERENCES tasks(id)
	);
	CREATE TABLE IF NOT EXISTS casual_summaries (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		unified_uid TEXT NOT NULL,
		channel TEXT NOT NULL,
		summary TEXT NOT NULL,
		timestamp DATETIME DEFAULT CURRENT_TIMESTAMP
	);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// CreateTask 创建一个新的全局任务
func (m *Manager) CreateTask(ctx context.Context, unifiedUID, title string) (int64, error) {
	db, err := m.getDB()
	if err != nil {
		return 0, err
	}
	defer db.Close()

	res, err := db.ExecContext(ctx,
		"INSERT INTO tasks (unified_uid, title, status) VALUES (?, ?, 'in_progress')",
		unifiedUID, title,
	)
	if err != nil {
		return 0, fmt.Errorf("create task: %w", err)
	}

	taskID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	// 自动设置为当前任务
	err = m.setCurrentTask(ctx, db, unifiedUID, taskID)
	if err != nil {
		return 0, err
	}

	return taskID, nil
}

// setCurrentTask 内部方法：设置当前任务指针
func (m *Manager) setCurrentTask(ctx context.Context, db *sql.DB, unifiedUID string, taskID int64) error {
	_, err := db.ExecContext(ctx,
		"INSERT OR REPLACE INTO current_task (unified_uid, task_id) VALUES (?, ?)",
		unifiedUID, taskID,
	)
	return err
}

// SetCurrentTask 设置当前进行中任务指针
func (m *Manager) SetCurrentTask(ctx context.Context, unifiedUID string, taskID int64) error {
	db, err := m.getDB()
	if err != nil {
		return err
	}
	defer db.Close()
	return m.setCurrentTask(ctx, db, unifiedUID, taskID)
}

// GetCurrentTask 获取当前任务的完整上下文
func (m *Manager) GetCurrentTask(ctx context.Context, unifiedUID string) (*Task, []Message, error) {
	db, err := m.getDB()
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()

	// 先查当前任务指针
	var taskID int64
	err = db.QueryRowContext(ctx,
		"SELECT task_id FROM current_task WHERE unified_uid = ?",
		unifiedUID,
	).Scan(&taskID)
	if err == sql.ErrNoRows {
		return nil, nil, nil // 没有当前任务
	}
	if err != nil {
		return nil, nil, fmt.Errorf("get current task pointer: %w", err)
	}

	// 查任务详情
	var task Task
	var createdAt string
	err = db.QueryRowContext(ctx,
		"SELECT id, unified_uid, title, status, created_at FROM tasks WHERE id = ?",
		taskID,
	).Scan(&task.ID, &task.UnifiedUID, &task.Title, &task.Status, &createdAt)
	if err != nil {
		return nil, nil, fmt.Errorf("get task: %w", err)
	}
	task.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)

	// 查任务上下文
	rows, err := db.QueryContext(ctx,
		"SELECT id, task_id, channel, role, content FROM task_context WHERE task_id = ? ORDER BY created_at ASC",
		taskID,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("get task context: %w", err)
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var msg Message
		if err := rows.Scan(&msg.ID, &msg.TaskID, &msg.Channel, &msg.Role, &msg.Content); err != nil {
			return nil, nil, err
		}
		messages = append(messages, msg)
	}

	return &task, messages, nil
}

// AppendContext 向当前任务追加一条上下文消息
func (m *Manager) AppendContext(ctx context.Context, unifiedUID, channel, role, content string) error {
	db, err := m.getDB()
	if err != nil {
		return err
	}
	defer db.Close()

	// 获取当前任务指针
	var taskID int64
	err = db.QueryRowContext(ctx,
		"SELECT task_id FROM current_task WHERE unified_uid = ?",
		unifiedUID,
	).Scan(&taskID)
	if err == sql.ErrNoRows {
		return nil // 没有当前任务，不追加
	}
	if err != nil {
		return fmt.Errorf("get current task for append: %w", err)
	}

	_, err = db.ExecContext(ctx,
		"INSERT INTO task_context (task_id, channel, role, content) VALUES (?, ?, ?, ?)",
		taskID, channel, role, content,
	)
	if err != nil {
		return fmt.Errorf("append context: %w", err)
	}

	// 更新 tasks.updated_at
	_, err = db.ExecContext(ctx,
		"UPDATE tasks SET updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		taskID,
	)
	return err
}

// UpdateTaskStatus 更新任务状态
func (m *Manager) UpdateTaskStatus(ctx context.Context, unifiedUID string, taskID int64, status string) error {
	db, err := m.getDB()
	if err != nil {
		return err
	}
	defer db.Close()

	result, err := db.ExecContext(ctx,
		"UPDATE tasks SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND unified_uid = ?",
		status, taskID, unifiedUID,
	)
	if err != nil {
		return fmt.Errorf("update task status: %w", err)
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("task not found or not owned by user")
	}
	return nil
}

// ListTasks 列出用户所有任务
func (m *Manager) ListTasks(ctx context.Context, unifiedUID string) ([]Task, error) {
	db, err := m.getDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx,
		"SELECT id, unified_uid, title, status, created_at FROM tasks WHERE unified_uid = ? ORDER BY updated_at DESC",
		unifiedUID,
	)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()

	var tasks []Task
	for rows.Next() {
		var t Task
		var createdAt string
		if err := rows.Scan(&t.ID, &t.UnifiedUID, &t.Title, &t.Status, &createdAt); err != nil {
			return nil, err
		}
		t.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
		tasks = append(tasks, t)
	}
	return tasks, nil
}

// SaveCasualSummary 保存闲聊摘要（不进入 trajectory，也不进入 task_context）
// 用于跨会话的上下文感知，例如用户之前闲聊时提到"周末想去爬山"
func (m *Manager) SaveCasualSummary(ctx context.Context, unifiedUID, channel, summary string) error {
	db, err := m.getDB()
	if err != nil {
		return err
	}
	defer db.Close()

	// 保留最近 20 条闲聊摘要，按 unified_uid 查
	_, err = db.ExecContext(ctx,
		"INSERT INTO casual_summaries (unified_uid, channel, summary) VALUES (?, ?, ?)",
		unifiedUID, channel, summary,
	)
	if err != nil {
		return fmt.Errorf("save casual summary: %w", err)
	}

	// 只保留最近 20 条
	_, _ = db.ExecContext(ctx,
		`DELETE FROM casual_summaries WHERE unified_uid = ? AND id NOT IN (
			SELECT id FROM casual_summaries WHERE unified_uid = ? ORDER BY timestamp DESC LIMIT 20
		)`,
		unifiedUID, unifiedUID,
	)
	return nil
}

// GetCasualSummaries 获取最近 N 条闲聊摘要（供 Agent 感知用户状态）
func (m *Manager) GetCasualSummaries(ctx context.Context, unifiedUID string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 5
	}
	db, err := m.getDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx,
		"SELECT summary FROM casual_summaries WHERE unified_uid = ? ORDER BY timestamp DESC LIMIT ?",
		unifiedUID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get casual summaries: %w", err)
	}
	defer rows.Close()

	var summaries []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		summaries = append(summaries, s)
	}
	return summaries, nil
}
