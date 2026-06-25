package eval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	appcore "selfmind/internal/app"
	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/kernel"
	"selfmind/internal/platform/config"
	"selfmind/internal/platform/log"
)

type RunOptions struct {
	ConfigPath    string
	TenantID      string
	Provider      string
	Model         string
	Workspace     string
	OutputPath    string
	RecordContent bool
}

type RunResult struct {
	CaseID          string        `json:"case_id"`
	Status          string        `json:"status"`
	OutputPath      string        `json:"output_path"`
	Duration        time.Duration `json:"duration"`
	ToolCalls       int           `json:"tool_calls"`
	ActionToolCalls int           `json:"action_tool_calls"`
	ToolErrors      int           `json:"tool_errors"`
	InputTokens     int           `json:"input_tokens"`
	OutputTokens    int           `json:"output_tokens"`
	Checks          []CheckResult `json:"checks"`
}

type runtimeHarness struct {
	cfg          *config.Config
	mem          interface{ Close() error }
	controlStore *control.Store
	cronStop     func()
	server       *httpapi.Server
	tenantID     string
	provider     string
	model        string
}

func RunCaseFile(ctx context.Context, path string, opts RunOptions) (*RunResult, error) {
	c, err := LoadCase(path)
	if err != nil {
		return nil, err
	}
	return RunCase(ctx, c, opts)
}

func RunCase(ctx context.Context, c *Case, opts RunOptions) (*RunResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c == nil {
		return nil, fmt.Errorf("case is nil")
	}
	workspace, err := resolveWorkspace(c, opts.Workspace)
	if err != nil {
		return nil, err
	}
	outPath := opts.OutputPath
	if strings.TrimSpace(outPath) == "" {
		outPath = DefaultRunPath(c, firstNonEmpty(opts.Provider, c.Provider, "default"))
	}
	rec, err := NewRecorder(outPath, opts.RecordContent || c.RecordContent)
	if err != nil {
		return nil, err
	}
	defer rec.Close()

	h, err := newRuntimeHarness(opts, c)
	if err != nil {
		return nil, err
	}
	defer h.Close()

	start := time.Now()
	rec.StartCase(c, h.provider, h.model, workspace)
	var taskIDs []string
	var inputTokens, outputTokens int
	var lastStatus string
	var lastOutcome string
	for i, turn := range c.Turns {
		turnStart := time.Now()
		rec.StartTurn(i, turn.Input)
		runCtx := httpapi.WithStreamObserver(ctx, rec.ObserveStreamEvent)
		resp, status := h.server.ProcessMessage(runCtx, api.MessageRequest{
			TenantID:       h.tenantID,
			Platform:       "eval",
			PlatformUserID: "eval-" + c.ID,
			DisplayName:    "SelfMind Eval",
			Channel:        firstNonEmpty(c.Channel, "cli"),
			Content:        turn.Input,
			ClientCWD:      workspace,
		})
		if resp.Task != nil {
			taskIDs = append(taskIDs, resp.Task.ID)
		}
		if resp.Outcome != nil {
			lastOutcome = resp.Outcome.Status
		}
		inputTokens += resp.Usage.InputTokens
		outputTokens += resp.Usage.OutputTokens
		lastStatus = "completed"
		if resp.Error != "" || status >= 400 {
			lastStatus = "failed"
		}
		rec.FinishTurn(i, status, resp.Content, resp.Error, resp.Usage.InputTokens, resp.Usage.OutputTokens, turnStart)
	}
	snap := rec.Snapshot()
	snap.TaskIDs = taskIDs
	snap.Workspace = workspace
	snap.ExpectedWorkspace = workspace
	snap.DurationSeconds = time.Since(start).Seconds()
	// For eval status checks, prefer the request-level result. Long-lived tasks
	// may remain "running" after a successful turn, while the smoke case is only
	// asking whether this interaction completed.
	snap.OutcomeStatus = firstNonEmpty(lastStatus, lastOutcome)
	checks := EvaluateCase(c, snap)
	status := "passed"
	if lastStatus == "failed" || !ChecksPassed(checks) {
		status = "failed"
	}
	rec.FinishCase(status, checks, inputTokens, outputTokens)
	return &RunResult{
		CaseID:          c.ID,
		Status:          status,
		OutputPath:      outPath,
		Duration:        time.Since(start),
		ToolCalls:       snap.ToolCalls,
		ActionToolCalls: snap.ActionToolCalls,
		ToolErrors:      snap.ToolErrors,
		InputTokens:     inputTokens,
		OutputTokens:    outputTokens,
		Checks:          checks,
	}, nil
}

func newRuntimeHarness(opts RunOptions, c *Case) (*runtimeHarness, error) {
	cfg, err := config.LoadConfig(config.Options{Path: opts.ConfigPath, CreateIfMissing: true})
	if err != nil {
		return nil, err
	}
	provider := firstNonEmpty(opts.Provider, c.Provider)
	model := firstNonEmpty(opts.Model, c.Model)
	if provider != "" {
		cfg.Model.Provider = provider
	}
	if model != "" {
		cfg.Model.Default = model
	}
	// Eval runs are explicit foreground checks. Avoid starting unrelated cron
	// jobs while preserving the same agent/tool/runtime path.
	cfg.Cron.Enabled = false
	log.Init(log.Options{Level: cfg.Agent.LogLevel})

	mem, dataDir, err := appcore.InitStorage(cfg)
	if err != nil {
		return nil, err
	}
	tenantID := firstNonEmpty(opts.TenantID, os.Getenv("SELF_TENANT_ID"))
	if tenantID == "" {
		tenantID = fmt.Sprintf("eval-%s-%d", sanitizeFilePart(c.ID), time.Now().UnixNano())
	}
	agent, err := appcore.InitAgent(mem, cfg, tenantID)
	if err != nil {
		if mem != nil {
			_ = mem.Close()
		}
		return nil, err
	}
	skillStore := kernel.NewSkillStore(mem)
	disp, err := appcore.InitTools(mem, cfg, agent, skillStore, tenantID)
	if err != nil {
		if mem != nil {
			_ = mem.Close()
		}
		return nil, err
	}
	agent.SetBackend(disp)
	gwDeps, err := appcore.InitGateway(dataDir, mem, agent, cfg, skillStore)
	if err != nil {
		if mem != nil {
			_ = mem.Close()
		}
		return nil, err
	}
	appcore.RegisterCronTool(disp, gwDeps.CronScheduler)
	controlStore, err := control.OpenStore(dataDir)
	if err != nil {
		appcore.StopCron(gwDeps.CronScheduler)
		if mem != nil {
			_ = mem.Close()
		}
		return nil, err
	}
	appcore.InitMCP(disp, cfg)
	displayProvider, displayModel, _ := appcore.ResolveModelDisplay(cfg)
	server := &httpapi.Server{
		Control:         controlStore,
		Gateway:         gwDeps.Gateway,
		DefaultTenantID: tenantID,
	}
	return &runtimeHarness{
		cfg:          cfg,
		mem:          mem,
		controlStore: controlStore,
		cronStop:     func() { appcore.StopCron(gwDeps.CronScheduler) },
		server:       server,
		tenantID:     tenantID,
		provider:     firstNonEmpty(displayProvider, provider, "default"),
		model:        firstNonEmpty(displayModel, model, "default"),
	}, nil
}

func (h *runtimeHarness) Close() {
	if h == nil {
		return
	}
	if h.cronStop != nil {
		h.cronStop()
	}
	if h.controlStore != nil {
		_ = h.controlStore.Close()
	}
	if h.mem != nil {
		_ = h.mem.Close()
	}
}

func resolveWorkspace(c *Case, override string) (string, error) {
	raw := firstNonEmpty(override, c.Workspace)
	if raw == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		raw = cwd
	}
	raw = os.ExpandEnv(raw)
	if strings.HasPrefix(raw, "~/") || raw == "~" {
		home, _ := os.UserHomeDir()
		if raw == "~" {
			raw = home
		} else {
			raw = filepath.Join(home, raw[2:])
		}
	}
	if !filepath.IsAbs(raw) {
		abs, err := filepath.Abs(raw)
		if err != nil {
			return "", err
		}
		raw = abs
	}
	return filepath.Clean(raw), nil
}

func DefaultRunPath(c *Case, provider string) string {
	caseID := "case"
	suite := "default"
	if c != nil {
		caseID = sanitizeFilePart(c.ID)
		suite = sanitizeFilePart(firstNonEmpty(c.Suite, filepath.Base(filepath.Dir(c.Path())), "default"))
	}
	provider = sanitizeFilePart(firstNonEmpty(provider, "default"))
	date := time.Now().Format("2006-01-02")
	return filepath.Join("evalruns", date, suite+"-"+caseID+"-"+provider+".jsonl")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func sanitizeFilePart(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-_.")
	if out == "" {
		return "default"
	}
	return out
}
