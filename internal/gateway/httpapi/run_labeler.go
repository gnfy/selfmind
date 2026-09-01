package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/platform/log"
	"selfmind/internal/platform/textutil"
)

// ErrPostRunBatchShape marks a semantically malformed aggregate response.
// Only this class is safe to bisect: replaying a batch after provider/network
// failure duplicates the same expensive input without isolating bad content.
var ErrPostRunBatchShape = errors.New("post-run batch response shape is invalid")

// PostRunAnalysis is the one bounded maintenance result produced after an
// eligible run. Memory decisions are applied by the app-layer analyzer that
// owns the memory backend; reference proposals become search hints only.
//
// TaskDecision is DECODE COMPATIBILITY for frozen v2 proposals migrated by
// MigrateMaintenanceJobsToVersion: the KEEP/MOVE/NEW/INBOX routing was removed
// with simplification P2 (every root run owns its task; child runs inherit
// through the parent edge), so a decoded value is audited and ignored, never
// applied. New analyzer output does not produce it.
type PostRunAnalysis struct {
	TaskDecision   string                  `json:"task_decision,omitempty"`
	TaskReferences []TaskReferenceProposal `json:"task_references,omitempty"`
	UserFacts      []string                `json:"user_facts,omitempty"`
	MemoryFacts    []string                `json:"memory_facts,omitempty"`
	Decisions      []MemoryDecision        `json:"memory_decisions,omitempty"`
}

// TaskReferenceProposal is a model-proposed human-facing address for the
// completed work. It is never execution authority: deterministic validation
// and evidence policy decide whether it remains shadow/candidate or may later
// participate in exact routing.
type TaskReferenceProposal struct {
	Class      string  `json:"class"` // literal | entity | descriptive
	Value      string  `json:"value"`
	Confidence float64 `json:"confidence,omitempty"`
}

// MemoryDecision is one intake ruling against nearby existing memory
// (docs/memory-governance.zh-CN.md §3.3): the model proposes, the
// deterministic policy layer in the analyzer decides whether it takes effect.
type MemoryDecision struct {
	Target     string  `json:"target"`        // user; memory is legacy replay compatibility only
	Decision   string  `json:"decision"`      // SKIP | ADD | REINFORCE | SUPERSEDE | CONFLICT
	Ref        string  `json:"ref,omitempty"` // offered neighbor id prefix
	Content    string  `json:"content,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	// Durability is the analyzer's time-validity ruling: durable |
	// time_bounded | episodic. The intake policy layer ENFORCES it in code —
	// episodic content never becomes long-term memory even when the model
	// pairs it with ADD (prompt-only enforcement failed in production:
	// 10/29 facts stored on 2026-07-17 were transient run state).
	Durability string `json:"durability,omitempty"`
	// ValidUntil (RFC3339) is required for time_bounded facts; intake
	// defaults it when missing.
	ValidUntil string `json:"valid_until,omitempty"`
	Category   string `json:"category,omitempty"`
}

type PostRunAnalysisRequest struct {
	Prompt             string
	UserInput          string // original user text; task-reference evidence only
	TurnText           string // raw user input + outcome summary, for neighbor retrieval
	TenantID           string
	PersonID           string
	WorkspaceID        string
	TaskID             string
	RunID              string
	AnalyzerVersion    int
	PromptSnapshotHash string
}

// PostRunAnalyzer performs durable fact extraction plus reference search-hint
// proposals in one explicitly role-routed model call. Implementations live in
// internal/app so the gateway remains provider- and storage-agnostic.
type PostRunAnalyzer interface {
	Analyze(ctx context.Context, req PostRunAnalysisRequest) (PostRunAnalysis, error)
}

// PostRunBatchAnalyzer is the optional batching extension used by the daemon
// maintenance worker. Results are keyed by run id so a model cannot reorder
// decisions and accidentally apply one run's memory or label ruling to
// another. Implementations that do not support batching keep working through
// PostRunAnalyzer, with one call per ready job.
type PostRunBatchAnalyzer interface {
	AnalyzeBatch(ctx context.Context, reqs []PostRunAnalysisRequest) (map[string]PostRunAnalysis, error)
}

type PostRunAnalysisApplier interface {
	Apply(ctx context.Context, req PostRunAnalysisRequest, analysis PostRunAnalysis) error
}

// defaultPostRunAnalyzerTimeout bounds one maintenance call. It runs
// post-finalize on a detached goroutine, so this bound protects the daemon
// from a hung provider, not the user-visible response (which was already
// sent). Cheap-role providers routinely exceeded the old 45s bound on real
// batches (observed live 2026-07-17: repeated "context deadline exceeded"
// burned all five retries and skipped learning for the affected runs), so the
// default is generous and configurable via tasks.maintenance_llm_timeout.
const defaultPostRunAnalyzerTimeout = 2 * time.Minute

// postRunAnalyzerVersion identifies the maintenance algorithm generation for
// the maintenance_jobs idempotency key. Bump it when the analyzer's decision
// semantics change and historic runs should become re-analyzable. v3 removed
// the task-routing half (simplification P2); v4 narrowed memory intake to
// explicitly stated user preferences (simplification P3). Unfinished older
// jobs migrate with their frozen proposals intact, and the current apply path
// deterministically ignores a legacy task_decision and skips legacy
// target="memory" decisions instead of replaying them under new semantics.
const postRunAnalyzerVersion = 4

// maintenanceRetryDelay parks a failed job before it becomes claimable again.
const maintenanceRetryDelay = 10 * time.Minute

type runMaintenanceReplay struct {
	WorkspaceID string
	UserInput   string
	Attach      taskAttach
}

func buildPostRunMaintenancePayload(identity *control.IdentityContext, task *control.Task, run *control.Run, workspaceID, userInput string, outcome api.RunOutcome, attach taskAttach, promptHash string) (string, error) {
	if identity == nil || task == nil || run == nil {
		return "", fmt.Errorf("maintenance replay context is incomplete")
	}
	payload, err := json.Marshal(postRunJobPayload{
		Identity: *identity, Task: *task, Run: *run, WorkspaceID: workspaceID,
		UserInput: userInput, Outcome: outcome, AttachCreated: attach.created, AttachPreLabel: attach.preLabel,
		AttachReason:       string(attach.reason),
		PromptSnapshotHash: strings.TrimSpace(promptHash),
	})
	if err != nil {
		return "", fmt.Errorf("encode maintenance replay payload: %w", err)
	}
	return string(payload), nil
}

func (c *RunCoordinator) materializeRunFinalization(ctx context.Context, identity *control.IdentityContext, task *control.Task, run *control.Run, taskStatus, workspaceID, userInput, channel, assistantContent string, outcome api.RunOutcome, attach taskAttach, handoff control.Handoff, event control.Event) error {
	if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil || task == nil || run == nil {
		return fmt.Errorf("run finalization context is incomplete")
	}
	payload, err := buildPostRunMaintenancePayload(identity, task, run, workspaceID, userInput, outcome, attach, c.srv.PromptSnapshotHash)
	if err != nil {
		return err
	}
	_, err = c.srv.Control.MaterializeRunFinalization(context.WithoutCancel(ctx), control.RunFinalization{
		Identity:   *identity,
		RunID:      run.ID,
		RunStatus:  terminalRunStatus(outcome.Status),
		TaskID:     task.ID,
		TaskStatus: taskStatus,
		Summary:    outcome.Summary,
		VerificationState: func() string {
			if outcome.Verification == nil {
				return "not_applicable"
			}
			return outcome.Verification.State
		}(),
		VerificationRefs: func() []string {
			if outcome.Verification == nil {
				return nil
			}
			refs := make([]string, 0, len(outcome.Verification.Checks))
			for _, check := range outcome.Verification.Checks {
				if strings.TrimSpace(check.Command) != "" {
					refs = append(refs, check.Command)
				}
			}
			return refs
		}(),
		ClaimMismatch:      len(outcome.ClaimMismatches) > 0,
		NextSteps:          outcome.NextSteps,
		Channel:            channel,
		AssistantContent:   assistantContent,
		Handoff:            handoff,
		AnalyzerVersion:    postRunAnalyzerVersion,
		MaintenancePayload: payload,
		Event:              event,
		EffectKey:          attach.effectKey,
	})
	if err == nil {
		c.recordTaskResolution(ctx, identity, run, api.MessageRequest{Content: userInput}, task, attach, task.ID, "unverified", false)
		if c.srv.SelfEvolution.Enabled {
			if _, _, profileErr := c.srv.Control.MaterializeWorkflowProfile(context.WithoutCancel(ctx), identity.TenantID, run.ID, c.srv.SelfEvolution); profileErr != nil {
				log.Warn("workflow profile materialization failed", "run_id", run.ID, "error", profileErr)
			}
			if _, observationErr := c.srv.Control.MaterializeWorkflowObservations(context.WithoutCancel(ctx), identity.TenantID, run.ID); observationErr != nil {
				log.Warn("workflow observation materialization failed", "run_id", run.ID, "error", observationErr)
			} else if c.srv.SkillCurator != nil {
				digests, digestErr := c.srv.Control.ReadySkillEvidenceDigestsForRun(context.WithoutCancel(ctx), identity.TenantID, run.ID)
				if digestErr != nil {
					log.Warn("skill cohort selection failed", "run_id", run.ID, "error", digestErr)
				}
				for _, digest := range digests {
					digest.PromptSnapshotHash = c.srv.PromptSnapshotHash
					payload, marshalErr := json.Marshal(digest)
					if marshalErr != nil {
						continue
					}
					key := "skillcuration_" + digest.EvidenceSetHash[:min(24, len(digest.EvidenceSetHash))]
					if _, enqueueErr := c.srv.Control.EnqueueMaintenanceJob(context.WithoutCancel(ctx), identity.TenantID, key, SkillCurationJobVersion, string(payload)); enqueueErr != nil {
						log.Warn("skill curation enqueue failed", "run_id", run.ID, "error", enqueueErr)
					}
				}
			}
		}
	}
	return err
}

func terminalRunStatus(status string) string {
	return status
}

type postRunJobPayload struct {
	Identity           control.IdentityContext `json:"identity"`
	Task               control.Task            `json:"task"`
	Run                control.Run             `json:"run"`
	WorkspaceID        string                  `json:"workspace_id,omitempty"`
	UserInput          string                  `json:"user_input"`
	Outcome            api.RunOutcome          `json:"outcome"`
	AttachCreated      bool                    `json:"attach_created,omitempty"`
	AttachPreLabel     bool                    `json:"attach_pre_label,omitempty"`
	AttachReason       string                  `json:"attach_reason,omitempty"`
	PromptSnapshotHash string                  `json:"prompt_snapshot_hash,omitempty"`
}

func (d *Server) analyzeFinishedRun(ctx context.Context, identity *control.IdentityContext, task *control.Task, run *control.Run, workspaceID, userInput string, outcome api.RunOutcome, attach taskAttach, promptHash string) {
	prepared := d.preparePostRunAnalysis(ctx, identity, task, run, workspaceID, userInput, outcome, attach, promptHash)
	if prepared == nil {
		return
	}
	// Claim the run's maintenance job before any model work: one run has ONE
	// logical maintenance result. A lost claim means another worker already
	// owns (or finished) this pass.
	maxAttempts := maintenanceMaxAttempts
	if job, _ := d.Control.GetMaintenanceJob(ctx, identity.TenantID, run.ID, postRunAnalyzerVersion); job != nil && strings.TrimSpace(job.ProposalJSON) != "" {
		maxAttempts = 0
	}
	claimed, exhausted, err := d.Control.ClaimMaintenanceJobWithLimit(ctx, identity.TenantID, run.ID, postRunAnalyzerVersion, maxAttempts)
	if err != nil {
		log.Warn("gateway: maintenance job claim failed; skipping analyzer", "run", run.ID, "error", err)
		return
	}
	if !claimed {
		if exhausted {
			log.Warn("gateway: maintenance retry budget exhausted; analyzer not called", "run", run.ID)
		}
		return
	}
	d.analyzeClaimedPostRun(ctx, prepared)
}

type preparedPostRunAnalysis struct {
	identity *control.IdentityContext
	task     *control.Task
	run      *control.Run
	attach   taskAttach
	outcome  api.RunOutcome
	request  PostRunAnalysisRequest
}

func (d *Server) preparePostRunAnalysis(ctx context.Context, identity *control.IdentityContext, task *control.Task, run *control.Run, workspaceID, userInput string, outcome api.RunOutcome, attach taskAttach, promptHash string) *preparedPostRunAnalysis {
	if d == nil || d.Control == nil || identity == nil || task == nil || run == nil {
		return nil
	}
	if d.PostRunAnalyzer == nil {
		return nil
	}
	// Post-run maintenance is memory and reference-hint work only
	// (simplification P2): task routing is gone — every root run owns its
	// task and child runs inherit through the parent edge, so there is no
	// wrong grouping left for a model to repair.
	durableEligible := postRunMemoryEligible(userInput, outcome)
	if !durableEligible && !d.postRunReferenceEligible(ctx, identity, task, userInput) {
		_ = d.Control.SkipMaintenanceJob(ctx, identity.TenantID, run.ID, postRunAnalyzerVersion, "run is not eligible for post-run maintenance")
		return nil
	}
	prompt := buildPostRunAnalysisPrompt(task, userInput, outcome)
	return &preparedPostRunAnalysis{
		identity: identity, task: task, run: run, attach: attach,
		outcome: outcome,
		request: PostRunAnalysisRequest{
			Prompt:             prompt,
			UserInput:          strings.TrimSpace(userInput),
			TurnText:           strings.TrimSpace(userInput + "\n" + outcome.Summary),
			TenantID:           identity.TenantID,
			PersonID:           identity.PersonID,
			WorkspaceID:        workspaceID,
			TaskID:             task.ID,
			RunID:              run.ID,
			AnalyzerVersion:    postRunAnalyzerVersion,
			PromptSnapshotHash: strings.TrimSpace(promptHash),
		},
	}
}

func (d *Server) postRunReferenceEligible(ctx context.Context, identity *control.IdentityContext, task *control.Task, userInput string) bool {
	if d == nil || d.Control == nil || identity == nil || task == nil || strings.TrimSpace(userInput) == "" {
		return false
	}
	refs, err := d.Control.ListTaskReferencesForTask(ctx, identity.TenantID, identity.PersonID, task.ID, 20)
	if err != nil {
		return false
	}
	for _, ref := range refs {
		if (ref.Status == control.TaskReferenceCandidate || ref.Status == control.TaskReferenceActive) &&
			control.TaskReferenceAppearsInText(userInput, ref.RawValue) {
			return true
		}
	}
	return false
}

func (d *Server) analyzeClaimedPostRun(ctx context.Context, prepared *preparedPostRunAnalysis) {
	if prepared == nil {
		return
	}
	req := prepared.request
	var analysis PostRunAnalysis
	job, _ := d.Control.GetMaintenanceJob(ctx, req.TenantID, req.RunID, postRunAnalyzerVersion)
	if job != nil && strings.TrimSpace(job.ProposalJSON) != "" {
		if err := json.Unmarshal([]byte(job.ProposalJSON), &analysis); err != nil {
			_ = d.Control.FailMaintenanceJob(ctx, req.TenantID, req.RunID, postRunAnalyzerVersion, "decode frozen proposal: "+err.Error(), maintenanceRetryDelay)
			return
		}
	} else {
		var err error
		analysis, err = d.PostRunAnalyzer.Analyze(ctx, req)
		if err != nil {
			d.failClaimedPostRun(ctx, prepared, err)
			return
		}
		if !d.saveMaintenanceProposal(ctx, prepared, analysis) {
			return
		}
	}
	d.applyClaimedPostRun(ctx, prepared, analysis)
}

func (d *Server) saveMaintenanceProposal(ctx context.Context, prepared *preparedPostRunAnalysis, analysis PostRunAnalysis) bool {
	proposal, err := json.Marshal(analysis)
	if err == nil {
		err = d.Control.SaveMaintenanceProposal(ctx, prepared.request.TenantID, prepared.request.RunID, postRunAnalyzerVersion, string(proposal), analysisResultHash(analysis))
	}
	if err != nil {
		_ = d.Control.FailMaintenanceJob(ctx, prepared.request.TenantID, prepared.request.RunID, postRunAnalyzerVersion, err.Error(), maintenanceRetryDelay)
		return false
	}
	return true
}

func (d *Server) applyClaimedPostRun(ctx context.Context, prepared *preparedPostRunAnalysis, analysis PostRunAnalysis) {
	req := prepared.request
	if applier, ok := d.PostRunAnalyzer.(PostRunAnalysisApplier); ok {
		if err := applier.Apply(ctx, req, analysis); err != nil {
			_ = d.Control.FailMaintenanceJob(ctx, req.TenantID, req.RunID, postRunAnalyzerVersion, err.Error(), maintenanceRetryDelay)
			return
		}
	}
	// A frozen v2 proposal migrated across the analyzer-version cutover may
	// still carry a legacy task_decision. Routing is gone; audit and ignore it
	// rather than replaying a KEEP/MOVE/NEW/INBOX ruling under new semantics.
	if decision := strings.TrimSpace(analysis.TaskDecision); decision != "" && !strings.EqualFold(decision, "KEEP") {
		d.appendLabelAssignedEvent(ctx, req.TaskID, req.RunID, map[string]interface{}{
			"decision":          "ignored_legacy",
			"legacy_decision":   textutil.Truncate(toOneLine(decision), 120),
			"run_id":            req.RunID,
			"task_id":           req.TaskID,
			"analyzer_version":  postRunAnalyzerVersion,
			"migration_context": "task routing removed by simplification P2",
		})
	}
	if err := d.applyTaskReferenceProposals(ctx, prepared, analysis); err != nil {
		// References improve future routing and recall, but they are not part of
		// the completed run's label transaction. Retrying after MOVE/NEW already
		// ran could duplicate display governance, so this optional projection is
		// deliberately fail-open and remains visible in logs.
		log.Warn("gateway: task reference projection failed; preserving completed maintenance result", "run", req.RunID, "error", err)
	}
	_ = d.Control.CompleteMaintenanceJob(ctx, req.TenantID, req.RunID, postRunAnalyzerVersion, analysisResultHash(analysis))
}

func (d *Server) failClaimedPostRun(ctx context.Context, prepared *preparedPostRunAnalysis, err error) {
	if prepared == nil || err == nil {
		return
	}
	log.Warn("gateway: post-run analyzer failed; keeping execution result unchanged", "run", prepared.request.RunID, "error", err)
	if d.blockPromptRevisionJob(ctx, prepared.request.TenantID, prepared.request.RunID, postRunAnalyzerVersion, err) {
		return
	}
	if llm.IsRetryableError(err) {
		d.failRetryableMaintenanceJob(ctx, prepared.request.TenantID, prepared.request.RunID, postRunAnalyzerVersion, err, maintenanceRetryDelay)
		return
	}
	d.blockMaintenanceProviderJob(ctx, prepared.identity, prepared.task, prepared.run, postRunAnalyzerVersion, err)
}

// analysisResultHash fingerprints one maintenance result so a later retry of
// the same terminal notification can be recognized as already applied.
func analysisResultHash(analysis PostRunAnalysis) string {
	parts := append(append([]string{analysis.TaskDecision}, analysis.UserFacts...), analysis.MemoryFacts...)
	for _, reference := range analysis.TaskReferences {
		parts = append(parts, reference.Class+"|"+reference.Value+"|"+fmt.Sprintf("%.4f", reference.Confidence))
	}
	for _, d := range analysis.Decisions {
		parts = append(parts, d.Target+"|"+d.Decision+"|"+d.Ref+"|"+d.Content)
	}
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(digest[:8])
}

// postRunMemoryEligible is language-neutral: durable outcome structure or a
// sufficiently substantive input/result pair qualifies. The lower bound keeps
// short, natural preference statements (including compact CJK sentences)
// eligible without introducing a language-specific keyword classifier. It
// only controls whether background learning is worth one cheap call; it never
// routes or blocks the user turn.
func postRunMemoryEligible(userInput string, outcome api.RunOutcome) bool {
	if len(outcome.Files) > 0 || len(outcome.Done) > 0 || len(outcome.NextSteps) > 0 || len(outcome.Tests) > 0 || len(outcome.Risks) > 0 {
		return true
	}
	// A failed run with no durable evidence teaches nothing worth one model
	// call — its diagnostics belong in run events, not person memory.
	if strings.EqualFold(strings.TrimSpace(outcome.Status), "failed") {
		return false
	}
	inputRunes := len([]rune(strings.TrimSpace(userInput)))
	summaryRunes := len([]rune(strings.TrimSpace(outcome.Summary)))
	return inputRunes >= 6 && summaryRunes >= 8 && inputRunes+summaryRunes >= 16
}

// appendLabelAssignedEvent writes the auditable label-decision record
// (docs/work-timeline.md: "Label decisions fully recorded"). Best-effort; a
// lost event never affects correctness. The payload carries ids and the
// bounded model reply only — no raw user text.
func (d *Server) appendLabelAssignedEvent(ctx context.Context, taskID, runID string, payload map[string]interface{}) {
	_, _ = d.Control.AppendEvent(ctx, control.Event{
		TaskID:     taskID,
		RunID:      runID,
		Type:       "label.assigned",
		Visibility: "task",
		Payload:    mustJSON(payload),
	})
}

func buildPostRunAnalysisPrompt(task *control.Task, userInput string, outcome api.RunOutcome) string {
	var sb strings.Builder
	sb.WriteString("Analyze this completed run for durable memory and reference hints. Return only the JSON object required by the system prompt.\n\n")
	fmt.Fprintf(&sb, "Current task: %s | %q\n", task.ID, textutil.Truncate(toOneLine(task.Title), 80))
	sb.WriteString("\nMemory rules:\n")
	sb.WriteString("- memory_decisions: judge only durable personal preferences, habits, or corrections explicitly stated by the user against the nearby memories listed after the turn data.\n")
	sb.WriteString("- target is always \"user\". Project facts, repository conventions, commands, and run state are SKIP; they belong in repository instructions or work history.\n")
	sb.WriteString("- decision SKIP: not an explicit personal preference, temporary, speculative, secret, or already fully represented. ADD: a genuinely new explicit preference. REINFORCE: same meaning as the referenced memory (do not rewrite it). SUPERSEDE: the user explicitly changed the referenced preference. CONFLICT: contradicts the referenced preference and both could be true.\n")
	sb.WriteString("- REINFORCE/SUPERSEDE/CONFLICT must set ref to an id from the nearby list. An empty memory_decisions array is correct when this turn adds nothing durable.\n")
	sb.WriteString("\n<turn-data>\n")
	fmt.Fprintf(&sb, "user: %s\n", textutil.Truncate(toOneLine(userInput), 800))
	fmt.Fprintf(&sb, "result_summary: %s\n", textutil.Truncate(toOneLine(outcome.Summary), 800))
	if len(outcome.Done) > 0 {
		fmt.Fprintf(&sb, "done: %s\n", textutil.Truncate(toOneLine(strings.Join(outcome.Done, "; ")), 600))
	}
	if len(outcome.NextSteps) > 0 {
		fmt.Fprintf(&sb, "next_steps: %s\n", textutil.Truncate(toOneLine(strings.Join(outcome.NextSteps, "; ")), 600))
	}
	if len(outcome.Files) > 0 {
		fmt.Fprintf(&sb, "files: %s\n", textutil.Truncate(toOneLine(strings.Join(outcome.Files, "; ")), 600))
	}
	if len(outcome.Tests) > 0 {
		fmt.Fprintf(&sb, "tests: %s\n", textutil.Truncate(toOneLine(strings.Join(outcome.Tests, "; ")), 600))
	}
	sb.WriteString("</turn-data>\n")
	return sb.String()
}
