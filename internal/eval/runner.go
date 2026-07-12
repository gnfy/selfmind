package eval

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	appcore "selfmind/internal/app"
	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/config"
	"selfmind/internal/platform/log"
	"selfmind/internal/tools"
)

type RunOptions struct {
	ConfigPath     string
	TenantID       string
	Provider       string
	Model          string
	Workspace      string
	OutputPath     string
	RecordContent  bool
	ProgressWriter io.Writer
	TurnTimeout    time.Duration
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
	mem          *memory.MemoryManager
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
	repeat := c.Repeat
	if repeat < 1 {
		repeat = 1
	}
	if repeat == 1 {
		return runSingle(ctx, c, opts, 0, 1)
	}
	// Sampled run for non-deterministic real-model scenarios: pass when the
	// fraction of passing samples meets PassRate (default: all must pass).
	passRate := c.PassRate
	if passRate <= 0 {
		passRate = 1.0
	}
	var last *RunResult
	var firstErr error
	passed := 0
	for s := 0; s < repeat; s++ {
		r, err := runSingle(ctx, c, opts, s, repeat)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		last = r
		if r.Status == "passed" {
			passed++
		}
	}
	if last == nil {
		return nil, firstErr
	}
	rate := float64(passed) / float64(repeat)
	ok := rate >= passRate
	last.Status = "failed"
	if ok {
		last.Status = "passed"
	}
	last.Checks = append(last.Checks, CheckResult{
		Name:    fmt.Sprintf("pass_rate>=%.2f", passRate),
		OK:      ok,
		Message: fmt.Sprintf("%d/%d samples passed (%.0f%%)", passed, repeat, rate*100),
		Score:   rate,
	})
	return last, nil
}

func runSingle(ctx context.Context, c *Case, opts RunOptions, sampleIdx, totalSamples int) (*RunResult, error) {
	// Eval runs are data-isolated BY DEFAULT: every case gets a throwaway temp
	// data dir (fresh control.db + memory), so record/replay sessions never
	// write eval-* persons, current_task rows, or runs into the user's real
	// ~/.selfmind data. `shared_data: true` is the explicit opt-out for a case
	// that genuinely needs pre-existing durable state (none exist today — each
	// case creates its own eval-<id> identity). Workspace isolation stays
	// scenario-driven: cases that seed or assert world state also get a scratch
	// workspace, while e.g. `workspace: "."` cases still probe the real repo.
	// VCR cassettes are keyed by case ID under a separate .vcr dir, so data-dir
	// isolation never invalidates recorded cassettes.
	var dataDirOverride string
	var cleanup func()
	workspace, err := resolveWorkspace(c, opts.Workspace)
	if err != nil {
		return nil, err
	}
	isolateWorkspace := needsWorkspaceIsolation(c)
	if !c.SharedData || isolateWorkspace {
		root, err := os.MkdirTemp("", "selfmind-eval-"+sanitizeFilePart(c.ID)+"-")
		if err != nil {
			return nil, err
		}
		cleanup = func() { _ = os.RemoveAll(root) }
		dataDirOverride = filepath.Join(root, "data")
		if isolateWorkspace {
			workspace = filepath.Join(root, "workspace")
			if err := os.MkdirAll(workspace, 0o755); err != nil {
				cleanup()
				return nil, err
			}
			if c.Setup != nil {
				if err := applyFileSeeds(workspace, c.Setup.Files); err != nil {
					cleanup()
					return nil, err
				}
			}
		}
	}
	if cleanup != nil {
		defer cleanup()
	}

	outPath := opts.OutputPath
	if strings.TrimSpace(outPath) == "" {
		outPath = DefaultRunPath(c, firstNonEmpty(opts.Provider, c.Provider, "default"))
	}
	if totalSamples > 1 {
		outPath = strings.TrimSuffix(outPath, ".jsonl") + fmt.Sprintf("-s%d.jsonl", sampleIdx+1)
	}
	rec, err := NewRecorder(outPath, opts.RecordContent || c.RecordContent)
	if err != nil {
		return nil, err
	}
	defer rec.Close()

	h, err := newRuntimeHarness(opts, c, dataDirOverride)
	if err != nil {
		return nil, err
	}
	defer h.Close()

	// Resolve the eval identity up front so setup can seed memory/task, then
	// reuse it for the post-run world-state assertions.
	identity, err := h.controlStore.ResolveOrCreateAccount(ctx, h.tenantID, "eval", "eval-"+c.ID, "SelfMind Eval")
	if err != nil {
		return nil, err
	}
	// Bind the workspace explicitly whenever the data dir is fresh (the
	// default). The gateway's CLI-cwd heuristic only auto-registers a workspace
	// for cli/terminal channels, so IM-channel scenarios (weixin/feishu/qq)
	// would otherwise run with no workspace scope in an empty control.db.
	// Registering it and passing its ID on every turn binds the scope
	// regardless of channel.
	var workspaceID string
	if dataDirOverride != "" {
		if ws, werr := h.controlStore.RegisterWorkspace(ctx, control.Workspace{
			TenantID:      identity.TenantID,
			OwnerPersonID: identity.PersonID,
			Name:          "eval-workspace",
			LocalPath:     workspace,
			AllowedRoots:  []string{workspace},
		}); werr == nil && ws != nil {
			workspaceID = ws.ID
			_ = h.controlStore.SetCurrentWorkspace(ctx, identity.TenantID, identity.PersonID, ws.ID)
		}
	}
	if c.Setup != nil {
		if err := applyStateSeeds(ctx, h.controlStore, h.mem, identity, workspaceID, c.Setup); err != nil {
			return nil, err
		}
	}

	start := time.Now()
	rec.StartCase(c, h.provider, h.model, workspace)
	var taskIDs []string
	var inputTokens, outputTokens int
	var lastStatus string
	var lastOutcome string
	// Track every person the case touched (the case identity plus any per-turn
	// platform_user_id "stranger" identities) so post-case run finalization can
	// sweep exactly the runs this eval created and nothing else.
	seenPersons := map[string]bool{}
	if identity != nil && identity.PersonID != "" {
		seenPersons[identity.PersonID] = true
	}
	// VCR hygiene, once per case execution: reset the per-session call counter
	// so numbering always starts at 0000 (the counter is process-global and a
	// prior run of the same case in this process would otherwise leave a 0001+
	// hole in recordings and break replays); in record mode also wipe the
	// case's previous cassette generation so files never interleave.
	llm.ResetVCRSession(c.ID)
	if llm.VCRRecordMode() {
		if err := llm.WipeVCRSessionRecordings("", c.ID); err != nil {
			return nil, fmt.Errorf("wipe stale cassettes for %s: %w", c.ID, err)
		}
	}
	for i, turn := range c.Turns {
		turnStart := time.Now()
		channel := firstNonEmpty(turn.Channel, c.Channel, "cli")
		rec.StartTurn(i, turn.Input, channel)
		observer := rec.ObserveStreamEvent
		if opts.ProgressWriter != nil {
			observer = func(event llm.StreamEvent) {
				rec.ObserveStreamEvent(event)
				writeLiveProgress(opts.ProgressWriter, c.ID, i, event)
			}
		}
		// Per-turn deadline: a stalled provider stream (bytes stop arriving
		// without an EOF) would otherwise hang the eval indefinitely. The
		// deadline cancels the underlying request so the turn fails cleanly.
		// Tag the turn with a VCR session (the case id) so model calls can be
		// recorded/replayed deterministically when SELFMIND_EVAL_VCR is set.
		vcrCtx := llm.WithVCRSession(httpapi.WithStreamObserver(ctx, observer), c.ID)
		turnCtx, cancelTurn := context.WithTimeout(vcrCtx, turnBudget(c, opts))
		// A per-turn platform_user_id override simulates a different platform user
		// (identity-isolation scenarios); the default keeps the case's identity.
		resp, status := h.server.ProcessMessage(turnCtx, api.MessageRequest{
			TenantID:       h.tenantID,
			Platform:       "eval",
			PlatformUserID: firstNonEmpty(turn.PlatformUserID, "eval-"+c.ID),
			DisplayName:    "SelfMind Eval",
			Channel:        channel,
			Content:        turn.Input,
			ClientCWD:      workspace,
			WorkspaceID:    workspaceID,
			// Eval has no human sitting on the approval waiter. Run autonomously
			// inside the case workspace; workspace scope and the hard deny floor
			// remain active, so dangerous operations are still rejected.
			ApprovalMode: string(tools.ApprovalFullAuto),
		})
		cancelTurn()
		if resp.Identity != nil && resp.Identity.PersonID != "" {
			seenPersons[resp.Identity.PersonID] = true
		}
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
	// Every eval turn is synchronous, so its run must be terminal once
	// ProcessMessage returns. Anything still `running` here is a finalization
	// bug or an interrupted write; force a terminal state so eval never leaves
	// phantom running runs behind (in shared_data mode they would show up in
	// the user's /tasks forever).
	forceFinalized := finalizeLeftoverRuns(ctx, h.controlStore, h.tenantID, seenPersons, 3*time.Second)
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
	if forceFinalized > 0 {
		// Surface (without failing the case) that the harness had to force
		// leftover runs to a terminal state — a signal the gateway's own run
		// finalization regressed.
		checks = append(checks, CheckResult{
			Name:    "run_finalization",
			OK:      true,
			Message: fmt.Sprintf("forced %d leftover running run(s) to interrupted after the case", forceFinalized),
		})
	}

	// World-state predicates: assert on control.db / files / memory — an oracle
	// the model cannot game by phrasing its answer.
	if len(c.AssertState) > 0 {
		subjectTaskID := lastNonEmpty(taskIDs)
		if subjectTaskID == "" {
			if cur, _ := h.controlStore.CurrentTask(ctx, identity.TenantID, identity.PersonID); cur != nil {
				subjectTaskID = cur.ID
			}
		}
		world := CollectWorldState(ctx, h.controlStore, h.mem, identity, subjectTaskID, workspace)
		checks = append(checks, EvaluateStatePredicates(c.AssertState, world)...)
	}

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

// finalizeLeftoverRuns guarantees every run this eval created reached a
// terminal status before the harness moves on. Eval turns are synchronous, so
// under normal operation the sweep finds nothing; the bounded wait only covers
// in-flight finalization writes racing the turn's return. Leftovers past the
// deadline are forced to `interrupted` (run + owning task) so they can never
// linger as phantom `running` rows. The sweep is scoped to the tenant AND the
// persons this case touched, so a `shared_data` case can never stomp a real
// concurrent run.
func finalizeLeftoverRuns(ctx context.Context, store *control.Store, tenantID string, personIDs map[string]bool, wait time.Duration) int {
	if store == nil || len(personIDs) == 0 {
		return 0
	}
	persons := make([]string, 0, len(personIDs))
	for id := range personIDs {
		persons = append(persons, id)
	}
	deadline := time.Now().Add(wait)
	for {
		runs, err := store.ListRunningRuns(ctx, tenantID, persons)
		if err != nil || len(runs) == 0 {
			return 0
		}
		if time.Now().After(deadline) {
			for _, r := range runs {
				_ = store.FinishRun(ctx, r.TenantID, r.ID, "interrupted")
				if r.TaskID != "" {
					_ = store.UpdateTaskStatus(ctx, r.TenantID, r.TaskID, "interrupted",
						"Eval run did not reach a terminal status; finalized by the eval harness.", nil)
				}
			}
			return len(runs)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// turnBudget is the per-turn wall-clock deadline. It uses the case's
// MaxDurationSeconds when set, the explicit option, or a safe default that
// prevents a stalled provider stream from hanging the whole eval.
func turnBudget(c *Case, opts RunOptions) time.Duration {
	if opts.TurnTimeout > 0 {
		return opts.TurnTimeout
	}
	if c != nil && c.Expect.MaxDurationSeconds > 0 {
		return time.Duration(c.Expect.MaxDurationSeconds) * time.Second
	}
	if v := strings.TrimSpace(os.Getenv("SELFMIND_EVAL_TURN_TIMEOUT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 200 * time.Second
}

func lastNonEmpty(values []string) string {
	for i := len(values) - 1; i >= 0; i-- {
		if strings.TrimSpace(values[i]) != "" {
			return strings.TrimSpace(values[i])
		}
	}
	return ""
}

func writeLiveProgress(w io.Writer, caseID string, turnIndex int, event llm.StreamEvent) {
	if w == nil {
		return
	}
	switch event.EventType {
	case "agent.thinking", "agent.step":
		if text := strings.TrimSpace(event.Content); text != "" {
			fmt.Fprintf(w, "  [%s/%d] %s\n", caseID, turnIndex+1, text)
		}
	case "tool.started":
		if event.ToolName != "" {
			fmt.Fprintf(w, "  [%s/%d] tool: %s started\n", caseID, turnIndex+1, event.ToolName)
		}
	case "tool.completed":
		status := "done"
		if event.Err != nil {
			status = "failed"
		}
		fmt.Fprintf(w, "  [%s/%d] tool: %s %s %.1fs\n", caseID, turnIndex+1, event.ToolName, status, event.DurationSeconds)
	case "model_stream_first_token":
		fmt.Fprintf(w, "  [%s/%d] first token\n", caseID, turnIndex+1)
	}
}

func newRuntimeHarness(opts RunOptions, c *Case, dataDirOverride string) (*runtimeHarness, error) {
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
	// Isolated scenarios get a fresh data dir so control.db / memory start clean
	// and never touch the user's real ~/.selfmind data.
	if strings.TrimSpace(dataDirOverride) != "" {
		cfg.Storage.DataDir = dataDirOverride
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
		// Automatic semantic recall (Work Timeline P2): eval runs the same
		// selector path as real input — no eval-only shortcut around recall.
		Recall: httpapi.NewRecallEngine(controlStore, mem, appcore.SemanticRecallExpander(mem, cfg, tenantID)),
		// Tool-output spool (execution-quality W1): same derivation as the
		// daemon runner, so eval exercises the artifact + read-back flow
		// inside its isolated data dir.
		ToolOutputDir: filepath.Join(dataDir, "tool-output"),
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
