package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// SQLiteProvider 实现 StorageProvider 接口
// 所有 DB 操作通过单 worker goroutine 串行访问，彻底避免 database/sql 连接池冲突
type SQLiteProvider struct {
	baseDir   string
	opCh      chan dbOp       // 操作通道
	resultCh  chan dbResult   // 结果通道
	stopCh    chan struct{}
}

// dbOp 表示一个待执行的数据库操作
type dbOp struct {
	method string
	args   []interface{}
	result chan<- dbResult
}

// dbResult 操作结果
type dbResult struct {
	val interface{}
	err error
}

// NewSQLiteProvider 初始化 SQLite 存储
func NewSQLiteProvider(baseDir string) (*SQLiteProvider, error) {
	p := &SQLiteProvider{
		baseDir:  baseDir,
		opCh:     make(chan dbOp, 64),
		resultCh: make(chan dbResult, 64),
		stopCh:   make(chan struct{}),
	}
	go p.worker()
	return p, nil
}

// worker 在独立 goroutine 中处理所有 DB 操作
func (p *SQLiteProvider) worker() {
	var db *sql.DB
	var dbTenant string

	for {
		select {
		case op := <-p.opCh:
			tenantID := op.args[0].(string)
			if db == nil || dbTenant != tenantID {
				if db != nil {
					db.Close()
				}
				var err error
				dbPath := filepath.Join(p.baseDir, tenantID, "memory.db")
				os.MkdirAll(filepath.Dir(dbPath), 0755)
				db, err = sql.Open("sqlite", dbPath)
				if err != nil {
					op.result <- dbResult{err: fmt.Errorf("open db: %w", err)}
					continue
				}
				db.SetMaxOpenConns(1)
				dbTenant = tenantID

				// 立即初始化所有必要的表结构，防止读取时报错
				p.initSchema(db)
			}

			var res dbResult
			switch op.method {
			case "SaveTrajectory":
				channel := op.args[1].(string)
				content := op.args[2].([]byte)
				_, err := db.Exec(`INSERT INTO trajectories (channel, content) VALUES (?, ?)`, channel, string(content))
				res = dbResult{err: err}

			case "GetLatestContext":
				channel := op.args[1].(string)
				rows, err := db.Query(`SELECT content FROM trajectories WHERE channel = ? ORDER BY created_at DESC LIMIT 10`, channel)
				if err != nil {
					res = dbResult{err: err}
				} else {
					var results [][]byte
					for rows.Next() {
						var content string
						rows.Scan(&content)
						results = append(results, []byte(content))
					}
					rows.Close()
					res = dbResult{val: results}
				}

			case "IndexSession":
				sess := op.args[1].(FTS5Session)
				_, err := db.Exec(
					`INSERT INTO sessions_fts (session_id, channel, content, summary, timestamp) VALUES (?, ?, ?, ?, ?)`,
					sess.SessionID, sess.Channel, sess.Content, sess.Summary, sess.Timestamp,
				)
				res = dbResult{err: err}

			case "SearchSessions":
				query := op.args[1].(string)
				limit := op.args[2].(int)
				safeQuery := strings.ReplaceAll(query, `"`, `""`)
				ftsQuery := fmt.Sprintf(`session_id:%s* OR content:%s* OR summary:%s*`, safeQuery, safeQuery, safeQuery)
				rows, err := db.Query(
					`SELECT session_id, channel, content, summary, timestamp
					 FROM sessions_fts
					 WHERE sessions_fts MATCH ?
					 ORDER BY rank
					 LIMIT ?`,
					ftsQuery, limit,
				)
				if err != nil {
					res = dbResult{err: err}
				} else {
					var results []FTS5Session
					for rows.Next() {
						var s FTS5Session
						rows.Scan(&s.SessionID, &s.Channel, &s.Content, &s.Summary, &s.Timestamp)
						results = append(results, s)
					}
					rows.Close()
					res = dbResult{val: results}
				}

			case "SaveCheckpoint":
				channel := op.args[1].(string)
				name := op.args[2].(string)
				messages := op.args[3].([]byte)
				_, err := db.Exec(
					`INSERT OR REPLACE INTO checkpoints (name, channel, messages) VALUES (?, ?, ?)`,
					name, channel, string(messages),
				)
				res = dbResult{err: err}

			case "ListCheckpoints":
				channel := op.args[1].(string)
				rows, err := db.Query(
					`SELECT id, name, channel, messages, created_at FROM checkpoints
					 WHERE channel = ? ORDER BY created_at DESC`,
					channel,
				)
				if err != nil {
					res = dbResult{err: err}
				} else {
					var results []Checkpoint
					for rows.Next() {
						var cp Checkpoint
						var msgs string
						if err := rows.Scan(&cp.ID, &cp.Name, &cp.Channel, &msgs, &cp.CreatedAt); err != nil {
							continue
						}
						cp.Messages = []byte(msgs)
						results = append(results, cp)
					}
					rows.Close()
					res = dbResult{val: results}
				}

			case "LoadCheckpoint":
				channel := op.args[1].(string)
				name := op.args[2].(string)
				var msgs string
				err := db.QueryRow(
					`SELECT messages FROM checkpoints WHERE channel = ? AND name = ?`,
					channel, name,
				).Scan(&msgs)
				if err != nil {
					res = dbResult{val: nil, err: err}
				} else {
					res = dbResult{val: []byte(msgs)}
				}

			case "DeleteCheckpoint":
				channel := op.args[1].(string)
				name := op.args[2].(string)
				_, err := db.Exec(`DELETE FROM checkpoints WHERE channel = ? AND name = ?`, channel, name)
				res = dbResult{err: err}

			case "AddFact":
				target := op.args[1].(string)
				content := op.args[2].(string)
				id := uuid.New().String()
				_, err := db.Exec(`INSERT INTO facts (id, target, content) VALUES (?, ?, ?)`, id, target, content)
				res = dbResult{err: err}

			case "GetFacts":
				target := op.args[1].(string)
				rows, err := db.Query(`SELECT id, target, content, created_at FROM facts WHERE target = ?`, target)
				if err != nil {
					res = dbResult{err: err}
				} else {
					var results []Fact
					for rows.Next() {
						var f Fact
						var createdAt string
						if err := rows.Scan(&f.ID, &f.Target, &f.Content, &createdAt); err != nil {
							continue
						}
						f.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
						results = append(results, f)
					}
					rows.Close()
					res = dbResult{val: results}
				}

			case "RemoveFact":
				id := op.args[1].(string)
				_, err := db.Exec(`DELETE FROM facts WHERE id = ?`, id)
				res = dbResult{err: err}

			case "SetPermission":
				toolName := op.args[1].(string)
				allowed := op.args[2].(bool)
				_, err := db.Exec(`INSERT OR REPLACE INTO permissions (tool_name, allowed) VALUES (?, ?)`, toolName, allowed)
				res = dbResult{err: err}

			case "GetPermission":
				toolName := op.args[1].(string)
				var allowed bool
				err := db.QueryRow(`SELECT allowed FROM permissions WHERE tool_name = ?`, toolName).Scan(&allowed)
				if err != nil {
					res = dbResult{val: true, err: nil} // Default to allowed
				} else {
					res = dbResult{val: allowed, err: nil}
				}

			case "SetSecret":
				keyName := op.args[1].(string)
				val := op.args[2].(string)
				_, err := db.Exec(`INSERT OR REPLACE INTO secrets (key_name, value) VALUES (?, ?)`, keyName, val)
				res = dbResult{err: err}

			case "GetSecret":
				keyName := op.args[1].(string)
				var val string
				err := db.QueryRow(`SELECT value FROM secrets WHERE key_name = ?`, keyName).Scan(&val)
				if err != nil {
					res = dbResult{val: "", err: err}
				} else {
					res = dbResult{val: val, err: nil}
				}

			case "SaveProcess":
				proc := op.args[1].(ProcessRecord)
				_, err := db.Exec(`INSERT OR REPLACE INTO processes (id, command, cwd, pid, status, started_at) VALUES (?, ?, ?, ?, ?, ?)`,
					proc.ID, proc.Command, proc.CWD, proc.PID, proc.Status, proc.StartedAt)
				res = dbResult{err: err}

			case "UpdateProcessStatus":
				id := op.args[1].(string)
				status := op.args[2].(string)
				exitCode := op.args[3].(int)
				_, err := db.Exec(`UPDATE processes SET status = ?, exit_code = ?, finished_at = CURRENT_TIMESTAMP WHERE id = ?`,
					status, exitCode, id)
				res = dbResult{err: err}

			case "ListProcesses":
				rows, err := db.Query(`SELECT id, command, cwd, pid, status, exit_code, started_at, finished_at FROM processes ORDER BY started_at DESC`)
				if err != nil {
					res = dbResult{err: err}
				} else {
					var results []ProcessRecord
					for rows.Next() {
						var p ProcessRecord
						var started, finished sql.NullString
						rows.Scan(&p.ID, &p.Command, &p.CWD, &p.PID, &p.Status, &p.ExitCode, &started, &finished)
						if started.Valid {
							p.StartedAt, _ = time.Parse("2006-01-02 15:04:05", started.String)
						}
						if finished.Valid {
							p.FinishedAt, _ = time.Parse("2006-01-02 15:04:05", finished.String)
						}
						results = append(results, p)
					}
					rows.Close()
					res = dbResult{val: results}
				}

			case "GetProcess":
				id := op.args[1].(string)
				var p ProcessRecord
				var started, finished sql.NullString
				err := db.QueryRow(`SELECT id, command, cwd, pid, status, exit_code, started_at, finished_at FROM processes WHERE id = ?`, id).
					Scan(&p.ID, &p.Command, &p.CWD, &p.PID, &p.Status, &p.ExitCode, &started, &finished)
				if err != nil {
					res = dbResult{err: err}
				} else {
					if started.Valid {
						p.StartedAt, _ = time.Parse("2006-01-02 15:04:05", started.String)
					}
					if finished.Valid {
						p.FinishedAt, _ = time.Parse("2006-01-02 15:04:05", finished.String)
					}
					res = dbResult{val: &p}
				}
			}

			if op.result != nil {
				op.result <- res
			}

		case <-p.stopCh:
			if db != nil {
				db.Close()
			}
			close(p.resultCh)
			return
		}
	}
}

// initSchema 确保所有必要的表结构都已创建
func (p *SQLiteProvider) initSchema(db *sql.DB) {
	// 1. 轨迹表
	db.Exec(`CREATE TABLE IF NOT EXISTS trajectories (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		content TEXT,
		channel TEXT DEFAULT 'cli',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);`)

	// 2. 全文搜索表
	db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS sessions_fts USING fts5(
		session_id UNINDEXED, channel UNINDEXED, content, summary, timestamp UNINDEXED
	);`)

	// 3. 检查点表
	db.Exec(`CREATE TABLE IF NOT EXISTS checkpoints (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		channel TEXT DEFAULT 'cli',
		messages TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(name, channel)
	);`)

	// 4. 事实记忆表
	db.Exec(`CREATE TABLE IF NOT EXISTS facts (
		id TEXT PRIMARY KEY,
		target TEXT,
		content TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);`)

	// 5. 权限与秘密存储表
	db.Exec(`CREATE TABLE IF NOT EXISTS permissions (
		tool_name TEXT PRIMARY KEY,
		allowed BOOLEAN
	);`)
	db.Exec(`CREATE TABLE IF NOT EXISTS secrets (
		key_name TEXT PRIMARY KEY,
		value TEXT
	);`)

	// 6. 后台进程表
	db.Exec(`CREATE TABLE IF NOT EXISTS processes (
		id TEXT PRIMARY KEY,
		command TEXT,
		cwd TEXT,
		pid INTEGER,
		status TEXT,
		exit_code INTEGER DEFAULT 0,
		started_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		finished_at DATETIME
	);`)
}

// call 同步向 worker 发送操作并等待结果
func (p *SQLiteProvider) call(method string, args ...interface{}) (interface{}, error) {
	resultCh := make(chan dbResult, 1)
	p.opCh <- dbOp{method: method, args: args, result: resultCh}
	res := <-resultCh
	return res.val, res.err
}

// Close 关闭 worker goroutine 和所有数据库连接
func (p *SQLiteProvider) Close() error {
	close(p.stopCh)
	<-p.resultCh
	return nil
}

func (p *SQLiteProvider) SaveTrajectory(ctx context.Context, tenantID, channel string, traj []byte) error {
	_, err := p.call("SaveTrajectory", tenantID, channel, traj)
	return err
}

func (p *SQLiteProvider) GetLatestContext(ctx context.Context, tenantID, channel string) ([][]byte, error) {
	val, err := p.call("GetLatestContext", tenantID, channel)
	if err != nil {
		return nil, err
	}
	if val == nil {
		return nil, nil
	}
	return val.([][]byte), nil
}

func (p *SQLiteProvider) IndexSession(tenantID string, sess FTS5Session) error {
	_, err := p.call("IndexSession", tenantID, sess)
	return err
}

func (p *SQLiteProvider) SearchSessions(tenantID, query string, limit int) ([]FTS5Session, error) {
	val, err := p.call("SearchSessions", tenantID, query, limit)
	if err != nil {
		return nil, err
	}
	if val == nil {
		return nil, nil
	}
	return val.([]FTS5Session), nil
}

func (p *SQLiteProvider) IndexMessagesFromTrajectory(ctx context.Context, tenantID, channel, sessionID string, messagesJSON []byte) error {
	var record struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(messagesJSON, &record); err != nil {
		return err
	}

	var summaryParts []string
	msgCount := 0
	for _, m := range record.Messages {
		if m.Role == "user" && msgCount < 2 {
			if len(m.Content) > 80 {
				summaryParts = append(summaryParts, m.Content[:80]+"...")
			} else {
				summaryParts = append(summaryParts, m.Content)
			}
			msgCount++
		}
	}
	summary := strings.Join(summaryParts, " | ")

	var contentBuilder strings.Builder
	for _, m := range record.Messages {
		contentBuilder.WriteString(m.Role + ": " + m.Content + "\n")
	}

	sess := FTS5Session{
		SessionID: sessionID,
		Channel:   channel,
		Content:   contentBuilder.String(),
		Summary:   summary,
		Timestamp: time.Now().Unix(),
	}
	return p.IndexSession(tenantID, sess)
}

func (p *SQLiteProvider) AddFact(ctx context.Context, tenantID string, target, content string) error {
	_, err := p.call("AddFact", tenantID, target, content)
	return err
}

func (p *SQLiteProvider) GetFacts(ctx context.Context, tenantID string, target string) ([]Fact, error) {
	val, err := p.call("GetFacts", tenantID, target)
	if err != nil {
		return nil, err
	}
	if val == nil {
		return nil, nil
	}
	return val.([]Fact), nil
}

func (p *SQLiteProvider) RemoveFact(ctx context.Context, tenantID string, id string) error {
	_, err := p.call("RemoveFact", tenantID, id)
	return err
}

func (p *SQLiteProvider) SetPermission(ctx context.Context, tenantID, toolName string, allowed bool) error {
	_, err := p.call("SetPermission", tenantID, toolName, allowed)
	return err
}

func (p *SQLiteProvider) GetPermission(ctx context.Context, tenantID, toolName string) (bool, error) {
	val, err := p.call("GetPermission", tenantID, toolName)
	if err != nil {
		return true, nil
	}
	return val.(bool), nil
}

func (p *SQLiteProvider) SetSecret(ctx context.Context, tenantID, keyName, value string) error {
	_, err := p.call("SetSecret", tenantID, keyName, value)
	return err
}

func (p *SQLiteProvider) GetSecret(ctx context.Context, tenantID, keyName string) (string, error) {
	val, err := p.call("GetSecret", tenantID, keyName)
	if err != nil {
		return "", err
	}
	return val.(string), nil
}

func (p *SQLiteProvider) SaveProcess(ctx context.Context, tenantID string, proc ProcessRecord) error {
	_, err := p.call("SaveProcess", tenantID, proc)
	return err
}

func (p *SQLiteProvider) UpdateProcessStatus(ctx context.Context, tenantID, id, status string, exitCode int) error {
	_, err := p.call("UpdateProcessStatus", tenantID, id, status, exitCode)
	return err
}

func (p *SQLiteProvider) ListProcesses(ctx context.Context, tenantID string) ([]ProcessRecord, error) {
	val, err := p.call("ListProcesses", tenantID)
	if err != nil {
		return nil, err
	}
	if val == nil {
		return nil, nil
	}
	return val.([]ProcessRecord), nil
}

func (p *SQLiteProvider) GetProcess(ctx context.Context, tenantID, id string) (*ProcessRecord, error) {
	val, err := p.call("GetProcess", tenantID, id)
	if err != nil {
		return nil, err
	}
	if val == nil {
		return nil, nil
	}
	return val.(*ProcessRecord), nil
}
