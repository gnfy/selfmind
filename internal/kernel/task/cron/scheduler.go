package cron

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/log"
	"selfmind/internal/tools"
)

// CronJob represents a scheduled job stored in SQLite.
type CronJob struct {
	ID       int64
	Name     string
	CronExpr string
	Prompt   string
	TenantID string
	Channel  string // "cli", "telegram", "weixin", etc.
	// Platform and DeliverTo identify where a proactive result is sent. For a
	// "summarize my work and message me" job, Platform is e.g. "weixin" and
	// DeliverTo is the platform user/chat id. Empty for local-only jobs.
	Platform  string
	DeliverTo string
	// Web allows a job to enable web tools for this turn (e.g. a market summary
	// that must look things up), overriding the default web-off policy.
	Web bool
	// Once disables the schedule after its first execution. A calendar-shaped
	// cron expression alone is still recurring (for example, yearly), so
	// one-time reminders need an explicit durable bit.
	Once      bool
	Enabled   bool
	LastRun   *time.Time
	NextRun   *time.Time
	CreatedAt time.Time
}

// Scheduler manages cron jobs backed by SQLite.
type Scheduler struct {
	db       *sql.DB
	mem      *memory.MemoryManager
	pruner   SkillPruner // optional; used for skill-pruner jobs
	executor JobExecutor // optional; runs a job's prompt as a real agent turn
	parser   cron.Parser
	cron     *cron.Cron
	mu       sync.RWMutex
	// entries maps a SQLite job id to its live cron entry id. robfig/cron
	// assigns its own entry ids, which must NOT be assumed equal to our SQLite
	// ids — this map is the source of truth for unscheduling.
	entries map[int64]cron.EntryID
	stopCh  chan struct{}
}

// SkillPruner is the interface for skill pruning, implemented by SkillStore.
type SkillPruner interface {
	Prune(ctx context.Context, tenantID string, thresholdDays int) (int, error)
}

// JobExecutor runs a cron job's prompt as a real agent turn and delivers the
// result to the job's channel. It is injected by the gateway layer (which owns
// ProcessMessage + delivery) so the scheduler stays decoupled from httpapi.
// When no executor is set, runJob falls back to indexing a memory marker.
type JobExecutor interface {
	RunCronJob(ctx context.Context, job CronJob) error
}

// NewScheduler creates a new cron scheduler backed by a dedicated SQLite DB.
func NewScheduler(db *sql.DB, mem *memory.MemoryManager) *Scheduler {
	return &Scheduler{
		db:      db,
		mem:     mem,
		parser:  cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow),
		cron:    cron.New(cron.WithParser(cron.NewParser(cron.Minute|cron.Hour|cron.Dom|cron.Month|cron.Dow)), cron.WithLocation(time.Local)),
		entries: make(map[int64]cron.EntryID),
		stopCh:  make(chan struct{}),
	}
}

// SetSkillPruner configures the skill pruner for skill-pruner cron jobs.
// Must be called before Scheduler.Start().
func (s *Scheduler) SetSkillPruner(pruner SkillPruner) {
	s.pruner = pruner
}

// SetExecutor installs the agent-run executor. Must be called before Start() so
// scheduled jobs actually run the agent and deliver results instead of falling
// back to the marker-only path.
func (s *Scheduler) SetExecutor(e JobExecutor) {
	s.executor = e
}

// SetTimezone rebuilds the cron engine to evaluate schedules in the named
// location (e.g. "Asia/Shanghai"). An empty name keeps the system local zone.
// Must be called before AddJob/Start (it discards any scheduled entries), so
// "0 8 * * *" means 08:00 in the configured zone, not UTC.
func (s *Scheduler) SetTimezone(name string) error {
	loc := time.Local
	if strings.TrimSpace(name) != "" {
		parsed, err := time.LoadLocation(strings.TrimSpace(name))
		if err != nil {
			return fmt.Errorf("load timezone %q: %w", name, err)
		}
		loc = parsed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cron = cron.New(cron.WithParser(cron.NewParser(cron.Minute|cron.Hour|cron.Dom|cron.Month|cron.Dow)), cron.WithLocation(loc))
	s.entries = make(map[int64]cron.EntryID)
	return nil
}

// InitSchema creates the cron_jobs table if it doesn't exist.
func (s *Scheduler) InitSchema(ctx context.Context) error {
	schema := `
CREATE TABLE IF NOT EXISTS cron_jobs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL,
    cron_expr  TEXT NOT NULL,
    prompt     TEXT NOT NULL,
    tenant_id  TEXT NOT NULL,
    channel    TEXT NOT NULL DEFAULT 'cli',
    enabled    INTEGER NOT NULL DEFAULT 1,
	once       INTEGER NOT NULL DEFAULT 0,
    last_run   INTEGER,
    next_run   INTEGER,
    created_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
);`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return err
	}
	// Idempotent migration for proactive-delivery columns. SQLite has no
	// "ADD COLUMN IF NOT EXISTS", so probe the table and add what is missing.
	return s.migrateColumns(ctx, map[string]string{
		"platform":   "TEXT NOT NULL DEFAULT ''",
		"deliver_to": "TEXT NOT NULL DEFAULT ''",
		"web":        "INTEGER NOT NULL DEFAULT 0",
		"once":       "INTEGER NOT NULL DEFAULT 0",
	})
}

// migrateColumns adds any of the named columns that the cron_jobs table is
// missing, leaving existing rows intact.
func (s *Scheduler) migrateColumns(ctx context.Context, columns map[string]string) error {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info(cron_jobs)")
	if err != nil {
		return err
	}
	existing := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		existing[name] = true
	}
	rows.Close()
	for name, def := range columns {
		if existing[name] {
			continue
		}
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE cron_jobs ADD COLUMN %s %s", name, def)); err != nil {
			return fmt.Errorf("add column %s: %w", name, err)
		}
	}
	return nil
}

// AddJob inserts a new cron job and schedules it.
func (s *Scheduler) AddJob(ctx context.Context, job *CronJob) (int64, error) {
	// Validate cron expression
	if _, err := s.parser.Parse(job.CronExpr); err != nil {
		return 0, fmt.Errorf("invalid cron expr %q: %w", job.CronExpr, err)
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO cron_jobs (name, cron_expr, prompt, tenant_id, channel, platform, deliver_to, web, once, enabled)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.Name, job.CronExpr, job.Prompt, job.TenantID, job.Channel,
		job.Platform, job.DeliverTo, btoi(job.Web), btoi(job.Once), btoi(job.Enabled))
	if err != nil {
		return 0, fmt.Errorf("insert cron job: %w", err)
	}
	id, _ := res.LastInsertId()
	job.ID = id

	if job.Enabled {
		if err := s.scheduleJob(id, job); err != nil {
			return id, err
		}
	}
	return id, nil
}

// EnsureJob inserts the job if none with the same name+tenant exists, otherwise
// updates the existing row in place and reschedules it. This makes built-in job
// registration (skill-pruner, canary) idempotent across restarts instead of
// inserting a duplicate row every time the gateway boots.
func (s *Scheduler) EnsureJob(ctx context.Context, job *CronJob) (int64, error) {
	if _, err := s.parser.Parse(job.CronExpr); err != nil {
		return 0, fmt.Errorf("invalid cron expr %q: %w", job.CronExpr, err)
	}
	var id int64
	err := s.db.QueryRowContext(ctx,
		"SELECT id FROM cron_jobs WHERE name = ? AND tenant_id = ?", job.Name, job.TenantID).Scan(&id)
	if err == sql.ErrNoRows {
		return s.AddJob(ctx, job)
	}
	if err != nil {
		return 0, err
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE cron_jobs SET cron_expr = ?, prompt = ?, channel = ?, platform = ?, deliver_to = ?, web = ?, once = ?, enabled = ?
		WHERE id = ?`,
		job.CronExpr, job.Prompt, job.Channel, job.Platform, job.DeliverTo, btoi(job.Web), btoi(job.Once), btoi(job.Enabled), id); err != nil {
		return id, err
	}
	job.ID = id
	if job.Enabled {
		if err := s.scheduleJob(id, job); err != nil {
			return id, err
		}
	}
	return id, nil
}

// ListJobs returns all cron jobs for a tenant.
func (s *Scheduler) ListJobs(ctx context.Context, tenantID string) ([]CronJob, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, cron_expr, prompt, tenant_id, channel, platform, deliver_to, web, once, enabled,
		       last_run, next_run, created_at
		FROM cron_jobs WHERE tenant_id = ? ORDER BY id`,
		tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []CronJob
	for rows.Next() {
		var j CronJob
		var web, once int
		var lastRun, nextRun sql.NullInt64
		var createdAt int64
		if err := rows.Scan(&j.ID, &j.Name, &j.CronExpr, &j.Prompt, &j.TenantID,
			&j.Channel, &j.Platform, &j.DeliverTo, &web, &once, &j.Enabled, &lastRun, &nextRun, &createdAt); err != nil {
			return nil, err
		}
		j.Web = itob(web)
		j.Once = itob(once)
		if lastRun.Valid {
			t := time.Unix(lastRun.Int64, 0)
			j.LastRun = &t
		}
		if nextRun.Valid {
			t := time.Unix(nextRun.Int64, 0)
			j.NextRun = &t
		}
		j.CreatedAt = time.Unix(createdAt, 0)
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// RemoveJob deletes a cron job and unschedules it.
func (s *Scheduler) RemoveJob(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if entryID, ok := s.entries[id]; ok {
		s.cron.Remove(entryID)
		delete(s.entries, id)
	}
	_, err := s.db.ExecContext(ctx, "DELETE FROM cron_jobs WHERE id = ?", id)
	return err
}

// EnableJob enables or disables a cron job.
func (s *Scheduler) EnableJob(ctx context.Context, id int64, enabled bool) error {
	// Reload job to get cron expr
	row := s.db.QueryRowContext(ctx,
		"SELECT id, name, cron_expr, prompt, tenant_id, channel, platform, deliver_to, web, once FROM cron_jobs WHERE id = ?", id)
	var j CronJob
	var web, once int
	if err := row.Scan(&j.ID, &j.Name, &j.CronExpr, &j.Prompt, &j.TenantID, &j.Channel, &j.Platform, &j.DeliverTo, &web, &once); err != nil {
		return err
	}
	j.Web = itob(web)
	j.Once = itob(once)
	j.Enabled = enabled

	s.mu.Lock()
	defer s.mu.Unlock()

	if enabled {
		if err := s.scheduleJobLocked(id, &j); err != nil {
			return err
		}
	} else if entryID, ok := s.entries[id]; ok {
		s.cron.Remove(entryID)
		delete(s.entries, id)
	}
	_, err := s.db.ExecContext(ctx, "UPDATE cron_jobs SET enabled = ? WHERE id = ?", btoi(enabled), id)
	return err
}

// Start begins the cron scheduler loop.
func (s *Scheduler) Start(ctx context.Context) error {
	// Load and schedule all enabled jobs
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, cron_expr, prompt, tenant_id, channel, platform, deliver_to, web, once FROM cron_jobs WHERE enabled = 1`)
	if err != nil {
		return err
	}
	defer rows.Close()

	s.mu.Lock()
	for rows.Next() {
		var j CronJob
		var web, once int
		if err := rows.Scan(&j.ID, &j.Name, &j.CronExpr, &j.Prompt, &j.TenantID, &j.Channel, &j.Platform, &j.DeliverTo, &web, &once); err != nil {
			s.mu.Unlock()
			return err
		}
		j.Web = itob(web)
		j.Once = itob(once)
		if err := s.scheduleJobLocked(j.ID, &j); err != nil {
			log.Debug("cron: failed to schedule job", "job_id", j.ID, "job_name", j.Name, "error", err)
		}
	}
	s.mu.Unlock()

	s.cron.Start()
	return nil
}

// Stop halts the cron scheduler and closes the SQLite DB.
func (s *Scheduler) Stop(ctx context.Context) {
	close(s.stopCh)
	s.cron.Stop()
	if s.db != nil {
		s.db.Close()
	}
}

// scheduleJobLocked adds a job to the cron scheduler (caller must hold s.mu).
func (s *Scheduler) scheduleJobLocked(id int64, job *CronJob) error {
	// Replace any existing entry for this job id so re-enabling/rescheduling
	// does not leak duplicate cron entries.
	if prev, ok := s.entries[id]; ok {
		s.cron.Remove(prev)
		delete(s.entries, id)
	}
	entryID, err := s.cron.AddFunc(job.CronExpr, func() {
		s.runJob(context.Background(), job)
	})
	if err != nil {
		return fmt.Errorf("add cron func: %w", err)
	}
	s.entries[id] = entryID
	return nil
}

// scheduleJob is the same but acquires the write lock.
func (s *Scheduler) scheduleJob(id int64, job *CronJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scheduleJobLocked(id, job)
}

// runJob executes a cron job by spawning an agent task.
func (s *Scheduler) runJob(ctx context.Context, job *CronJob) {
	if job.Prompt == "" {
		return
	}
	if job.Once {
		defer s.completeOneTimeJob(job.ID)
	}

	log.Debug("cron: running job %d (%s) for tenant %s", job.ID, job.Name, job.TenantID)

	// Update last_run
	now := time.Now()
	_, _ = s.db.ExecContext(ctx,
		"UPDATE cron_jobs SET last_run = ? WHERE id = ?",
		now.Unix(), job.ID)

	// Handle built-in skill prune job
	if strings.HasPrefix(job.Prompt, "skill_prune:") {
		tenantID := strings.TrimPrefix(job.Prompt, "skill_prune:")
		tenantID = strings.TrimSpace(tenantID)
		if s.pruner != nil {
			pruned, err := s.pruner.Prune(ctx, tenantID, 30)
			if err != nil {
				log.Debug("cron: skill prune failed for %s: %v", tenantID, err)
			} else {
				log.Debug("cron: pruned %d low-value skill records for tenant %s", pruned, tenantID)
			}
		}
		log.Debug("cron: job %d (%s) completed", job.ID, job.Name)
		return
	}

	// Preferred path: run the prompt as a real agent turn and deliver the
	// result to the job's channel. The executor (gateway layer) owns the run +
	// delivery; the scheduler stays decoupled.
	if s.executor != nil {
		if err := s.executor.RunCronJob(ctx, *job); err != nil {
			log.Debug("cron: executor failed for job %d (%s): %v", job.ID, job.Name, err)
		}
		log.Debug("cron: job %d (%s) completed", job.ID, job.Name)
		return
	}

	// Fallback (no executor, e.g. CLI-only mode): index a marker so the agent
	// picks the job up on the next conversation instead of silently dropping it.
	if s.mem != nil {
		trajectoryData := fmt.Sprintf(`[cron job "%s" triggered at %s] %s`,
			job.Name, now.Format(time.RFC3339), job.Prompt)
		_ = s.mem.IndexSession(ctx, job.TenantID, job.Channel,
			fmt.Sprintf("cron-%d-%d", job.ID, now.Unix()), []byte(trajectoryData))
	}

	log.Debug("cron: job %d (%s) completed", job.ID, job.Name)
}

// completeOneTimeJob durably disables a one-shot schedule and removes its live
// cron entry after the first attempted execution. The job remains in history
// for diagnostics instead of being deleted.
func (s *Scheduler) completeOneTimeJob(id int64) {
	if s == nil || s.db == nil || id <= 0 {
		return
	}
	_, _ = s.db.ExecContext(context.Background(), "UPDATE cron_jobs SET enabled = 0 WHERE id = ?", id)
	s.mu.Lock()
	if entryID, ok := s.entries[id]; ok {
		s.cron.Remove(entryID)
		delete(s.entries, id)
	}
	s.mu.Unlock()
}

// btoi converts bool to int (SQLite integer).
func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// itob converts int (SQLite integer) to bool.
func itob(i int) bool {
	return i != 0
}

// CronTool provides /cron slash commands via the tools layer.
// It is registered as a built-in tool with the dispatcher.
type CronTool struct {
	sched *Scheduler
}

// ToolAdapter wraps a CronTool to satisfy the tools.Tool interface.
type ToolAdapter struct {
	CronTool *CronTool
}

func (a *ToolAdapter) Name() string { return "cron" }
func (a *ToolAdapter) Description() string {
	return "Manage scheduled jobs and reminders. Use once=true for a one-time reminder; otherwise schedules recur."
}
func (a *ToolAdapter) Execute(args map[string]interface{}) (string, error) {
	return a.CronTool.Execute(context.Background(), args)
}
func (a *ToolAdapter) Schema() tools.ToolSchema {
	return tools.ToolSchema{
		Type: "object",
		Properties: map[string]tools.PropertyDef{
			"action":     {Type: "string", Description: "Action: list, add, remove, enable"},
			"name":       {Type: "string"},
			"cron":       {Type: "string", Description: "Cron expression, e.g. 0 9 * * *"},
			"prompt":     {Type: "string"},
			"job_id":     {Type: "integer"},
			"enabled":    {Type: "boolean"},
			"tenantID":   {Type: "string"},
			"channel":    {Type: "string", Description: "Delivery channel, e.g. cli, weixin, telegram"},
			"platform":   {Type: "string", Description: "Platform to deliver the result on, e.g. weixin"},
			"deliver_to": {Type: "string", Description: "Recipient id (platform user/chat) for proactive delivery"},
			"web":        {Type: "boolean", Description: "Allow web tools for this job (e.g. market/news lookups)"},
			"once":       {Type: "boolean", Description: "Run once, then disable automatically (for one-time reminders)"},
		},
	}
}

// NewCronTool creates a cron tool wrapping a scheduler.
func NewCronTool(sched *Scheduler) *CronTool {
	return &CronTool{sched: sched}
}

// ToolDefinition returns the OpenAPI-style tool definition for /cron.
func (ct *CronTool) ToolDefinition() map[string]interface{} {
	return map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "cron",
			"description": "Manage scheduled jobs and reminders. Use once=true for a one-time reminder; otherwise schedules recur.",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"action": map[string]interface{}{
						"type":        "string",
						"description": "Action: list, add, remove, enable",
					},
					"name":       map[string]interface{}{"type": "string"},
					"cron":       map[string]interface{}{"type": "string", "description": "Cron expression, e.g. 0 9 * * *"},
					"prompt":     map[string]interface{}{"type": "string", "description": "Prompt sent to the agent when the job fires"},
					"job_id":     map[string]interface{}{"type": "integer", "description": "Job ID to remove or enable"},
					"enabled":    map[string]interface{}{"type": "boolean"},
					"tenantID":   map[string]interface{}{"type": "string"},
					"channel":    map[string]interface{}{"type": "string", "description": "Delivery channel, e.g. cli, weixin"},
					"platform":   map[string]interface{}{"type": "string", "description": "Platform to deliver on, e.g. weixin"},
					"deliver_to": map[string]interface{}{"type": "string", "description": "Recipient id for proactive delivery"},
					"web":        map[string]interface{}{"type": "boolean", "description": "Allow web tools for this job"},
					"once":       map[string]interface{}{"type": "boolean", "description": "Run once, then disable automatically (for one-time reminders)"},
				},
			},
		},
	}
}

// Execute runs a cron sub-command.
func (ct *CronTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	action, _ := args["action"].(string)
	tenantID, _ := args["tenantID"].(string)
	if tenantID == "" {
		tenantID = "default"
	}

	switch action {
	case "list":
		jobs, err := ct.sched.ListJobs(ctx, tenantID)
		if err != nil {
			return "", err
		}
		if len(jobs) == 0 {
			return "No cron jobs found.", nil
		}
		var lines []string
		for _, j := range jobs {
			enabled := "disabled"
			if j.Enabled {
				enabled = "enabled"
			}
			lastRun := "never"
			if j.LastRun != nil {
				lastRun = j.LastRun.Format("2006-01-02 15:04")
			}
			nextRun := "—"
			if j.NextRun != nil {
				nextRun = j.NextRun.Format("2006-01-02 15:04")
			}
			lines = append(lines, fmt.Sprintf("[%d] %s | %s | %s | last:%s | next:%s",
				j.ID, j.Name, j.CronExpr, enabled, lastRun, nextRun))
		}
		return "Cron jobs:\n" + strings.Join(lines, "\n"), nil

	case "add":
		name, _ := args["name"].(string)
		cronExpr, _ := args["cron"].(string)
		prompt, _ := args["prompt"].(string)
		channel, _ := args["channel"].(string)
		platform, _ := args["platform"].(string)
		deliverTo, _ := args["deliver_to"].(string)
		web, _ := args["web"].(bool)
		once, _ := args["once"].(bool)
		if deliverTo != "" && (channel == "" || strings.EqualFold(channel, platform)) {
			channel = deliverTo
		}
		if channel == "" {
			channel = firstNonEmptyCron(platform, "cli")
		}
		if name == "" || cronExpr == "" || prompt == "" {
			return "", fmt.Errorf("name, cron, and prompt are required for add")
		}
		job := &CronJob{
			Name:      name,
			CronExpr:  cronExpr,
			Prompt:    prompt,
			TenantID:  tenantID,
			Channel:   channel,
			Platform:  platform,
			DeliverTo: deliverTo,
			Web:       web,
			Once:      once,
			Enabled:   true,
		}
		id, err := ct.sched.AddJob(ctx, job)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Cron job %d (%s) added.", id, name), nil

	case "remove":
		id, ok := args["job_id"].(float64)
		if !ok {
			return "", fmt.Errorf("job_id required")
		}
		if err := ct.sched.RemoveJob(ctx, int64(id)); err != nil {
			return "", err
		}
		return fmt.Sprintf("Cron job %d removed.", int64(id)), nil

	case "enable":
		id, _ := args["job_id"].(float64)
		enabled, _ := args["enabled"].(bool)
		if err := ct.sched.EnableJob(ctx, int64(id), enabled); err != nil {
			return "", err
		}
		state := "disabled"
		if enabled {
			state = "enabled"
		}
		return fmt.Sprintf("Cron job %d %s.", int64(id), state), nil

	default:
		return "", fmt.Errorf("unknown cron action: %s", action)
	}
}

func firstNonEmptyCron(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
