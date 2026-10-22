package cron

import (
	"context"
	"fmt"
	"strings"

	"selfmind/internal/kernel"
	"selfmind/internal/tools"
)

// JobRequest is the endpoint and prompt a Run asked the cron tool to
// schedule, exactly as the model sent them.
type JobRequest struct {
	Platform  string
	DeliverTo string
	Channel   string
	Prompt    string
}

// JobEndpoint is where a job acts and delivers: one bound account and that
// account's own chat.
type JobEndpoint struct {
	TenantID  string
	Platform  string
	DeliverTo string
	Channel   string
}

// JobAuthority decides, from a Run's trusted invocation scope, what the Run
// may schedule and which stored jobs it may see or change. The gateway
// implements it over the identity store; this package never resolves
// identities itself.
type JobAuthority interface {
	// Bind returns the endpoint a new job is stored with: an account of the
	// Run's own person and that account's own chat. It refuses another
	// person's account, an unbound account, a shared chat, and a prompt that
	// begins with a command, with an error meant for the model, and never
	// substitutes another person's endpoint.
	Bind(ctx context.Context, scope kernel.ToolInvocationScope, request JobRequest) (JobEndpoint, error)
	// Standing reports whether the Run's person may see or change a stored
	// job and, when the job does not run, why.
	Standing(ctx context.Context, scope kernel.ToolInvocationScope, job CronJob) (JobStanding, error)
}

// JobStanding is how one stored job reads to the person of a Run.
type JobStanding struct {
	// Owned reports whether the person may see and change the job.
	Owned bool
	// Blocked says why the job does not run; empty when it runs.
	Blocked string
}

// AddedReceipt is what the cron tool answers when it stores a job. The Run
// that records it created the job, which is how a row stored without its
// creator is traced back to one.
func AddedReceipt(id int64, name string) string {
	return fmt.Sprintf("Cron job %d (%s) added.", id, name)
}

// CronTool is the model-facing cron tool. Every call acts for the person of
// the Run that makes it.
type CronTool struct {
	sched     *Scheduler
	authority JobAuthority
}

// ToolAdapter wraps a CronTool to satisfy the tools.Tool interface.
type ToolAdapter struct {
	CronTool *CronTool
}

func (a *ToolAdapter) Name() string { return "cron" }
func (a *ToolAdapter) Description() string {
	return "Manage scheduled jobs and reminders. Use once=true for a one-time reminder; otherwise schedules recur. A job runs as you and delivers to your own chat."
}
func (a *ToolAdapter) Execute(args map[string]interface{}) (string, error) {
	return a.CronTool.Execute(tools.ContextFromArgs(args), args)
}
func (a *ToolAdapter) Schema() tools.ToolSchema {
	return tools.ToolSchema{
		Type: "object",
		Properties: map[string]tools.PropertyDef{
			"action":     {Type: "string", Description: "Action: list, add, remove, enable"},
			"name":       {Type: "string"},
			"cron":       {Type: "string", Description: "Cron expression, e.g. 0 9 * * *"},
			"prompt":     {Type: "string", Description: "The task to run when the job fires, described in words; text that begins with a SelfMind command is refused"},
			"job_id":     {Type: "integer"},
			"enabled":    {Type: "boolean"},
			"tenantID":   {Type: "string"},
			"channel":    {Type: "string", Description: "Delivery channel, e.g. cli, weixin, telegram; omit to use the direct chat of deliver_to"},
			"platform":   {Type: "string", Description: "Platform of one of your own accounts to deliver on, e.g. weixin; omit to deliver where this request came from"},
			"deliver_to": {Type: "string", Description: "Your own account id on that platform for proactive delivery"},
			"web":        {Type: "boolean", Description: "Allow web tools for this job (e.g. market/news lookups)"},
			"once":       {Type: "boolean", Description: "Run once, then disable automatically (for one-time reminders)"},
		},
	}
}

// NewCronTool creates the cron tool over a scheduler. authority binds every
// call to the person of the calling Run; without it the tool refuses.
func NewCronTool(sched *Scheduler, authority JobAuthority) *CronTool {
	return &CronTool{sched: sched, authority: authority}
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

// Execute runs a cron sub-command for the person of the calling Run. That
// person's tenant is the only tenant it reads or writes, and list, remove,
// and enable reach only that person's jobs.
func (ct *CronTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	scope, ok := tools.InvocationScopeFromArgs(args)
	if !ok || strings.TrimSpace(scope.ControlTenantID) == "" || strings.TrimSpace(scope.PersonID) == "" {
		return "", fmt.Errorf("scheduling needs the authenticated run of a person")
	}
	if ct == nil || ct.sched == nil || ct.authority == nil {
		return "", fmt.Errorf("scheduling is unavailable")
	}
	if tenantID, _ := args["tenantID"].(string); strings.TrimSpace(tenantID) != "" && strings.TrimSpace(tenantID) != scope.ControlTenantID {
		return "", fmt.Errorf("tenant %q is not yours; a job belongs to the tenant of the run that creates it, so omit tenantID", tenantID)
	}

	action, _ := args["action"].(string)
	switch action {
	case "list":
		jobs, err := ct.ownJobs(ctx, scope)
		if err != nil {
			return "", err
		}
		if len(jobs) == 0 {
			return "No cron jobs found.", nil
		}
		var lines []string
		blocked := false
		for _, owned := range jobs {
			j := owned.job
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
			line := fmt.Sprintf("[%d] %s | %s | %s | last:%s | next:%s",
				j.ID, j.Name, j.CronExpr, enabled, lastRun, nextRun)
			if owned.blocked != "" {
				line += " | does not run: " + owned.blocked
				blocked = true
			}
			lines = append(lines, line)
		}
		out := "Cron jobs:\n" + strings.Join(lines, "\n")
		if blocked {
			out += "\nA job that does not run never will as stored: remove it, or add it again for one of your own chats."
		}
		return out, nil

	case "add":
		name, _ := args["name"].(string)
		cronExpr, _ := args["cron"].(string)
		prompt, _ := args["prompt"].(string)
		channel, _ := args["channel"].(string)
		platform, _ := args["platform"].(string)
		deliverTo, _ := args["deliver_to"].(string)
		web, _ := args["web"].(bool)
		once, _ := args["once"].(bool)
		if name == "" || cronExpr == "" || prompt == "" {
			return "", fmt.Errorf("name, cron, and prompt are required for add")
		}
		endpoint, err := ct.authority.Bind(ctx, scope, JobRequest{Platform: platform, DeliverTo: deliverTo, Channel: channel, Prompt: prompt})
		if err != nil {
			return "", err
		}
		job := &CronJob{
			Name:            name,
			CronExpr:        cronExpr,
			Prompt:          prompt,
			TenantID:        endpoint.TenantID,
			Channel:         endpoint.Channel,
			Platform:        endpoint.Platform,
			DeliverTo:       endpoint.DeliverTo,
			CreatorPersonID: scope.PersonID,
			Web:             web,
			Once:            once,
			Enabled:         true,
		}
		id, err := ct.sched.AddJob(ctx, job)
		if err != nil {
			return "", err
		}
		return AddedReceipt(id, name), nil

	case "remove":
		job, err := ct.ownJob(ctx, scope, args)
		if err != nil {
			return "", err
		}
		if err := ct.sched.RemoveJob(ctx, job.ID); err != nil {
			return "", err
		}
		return fmt.Sprintf("Cron job %d removed.", job.ID), nil

	case "enable":
		job, err := ct.ownJob(ctx, scope, args)
		if err != nil {
			return "", err
		}
		enabled, _ := args["enabled"].(bool)
		if err := ct.sched.EnableJob(ctx, job.ID, enabled); err != nil {
			return "", err
		}
		state := "disabled"
		if enabled {
			state = "enabled"
		}
		return fmt.Sprintf("Cron job %d %s.", job.ID, state), nil

	default:
		return "", fmt.Errorf("unknown cron action: %s", action)
	}
}

// ownedJob is one job the calling Run's person may see and change, with why
// it does not run when it does not.
type ownedJob struct {
	job     CronJob
	blocked string
}

// ownJobs lists the jobs of the calling Run's person.
func (ct *CronTool) ownJobs(ctx context.Context, scope kernel.ToolInvocationScope) ([]ownedJob, error) {
	jobs, err := ct.sched.ListJobs(ctx, scope.ControlTenantID)
	if err != nil {
		return nil, err
	}
	var own []ownedJob
	for _, job := range jobs {
		standing, err := ct.authority.Standing(ctx, scope, job)
		if err != nil {
			return nil, err
		}
		if standing.Owned {
			own = append(own, ownedJob{job: job, blocked: standing.Blocked})
		}
	}
	return own, nil
}

// ownJob finds the job named by job_id among the calling person's jobs. Any
// other job reads as not found, whether or not it exists.
func (ct *CronTool) ownJob(ctx context.Context, scope kernel.ToolInvocationScope, args map[string]interface{}) (CronJob, error) {
	var id int64
	switch value := args["job_id"].(type) {
	case float64:
		id = int64(value)
	case int:
		id = int64(value)
	case int64:
		id = value
	default:
		return CronJob{}, fmt.Errorf("job_id required")
	}
	jobs, err := ct.ownJobs(ctx, scope)
	if err != nil {
		return CronJob{}, err
	}
	for _, owned := range jobs {
		if owned.job.ID == id {
			return owned.job, nil
		}
	}
	return CronJob{}, fmt.Errorf("cron job %d not found", id)
}
