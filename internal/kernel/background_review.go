package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/log"
	"selfmind/internal/platform/textutil"
)

type BackgroundReviewEngine struct {
	mem             *memory.MemoryManager
	backend         AgentBackend
	provider        llm.Provider
	config          EvolutionConfig
	maxIterations   int
	maxRetries      int
	useMemoryFence  bool
	notifyCh        chan string
	controlTenantID string
	// enqueue, when set, hands a serialized ReviewJobPayload to the durable
	// maintenance queue instead of spawning an immediate goroutine (W7).
	enqueue func(tenantID, payloadJSON string) bool
}

func NewBackgroundReviewEngine(mem *memory.MemoryManager, backend AgentBackend, provider llm.Provider, cfg EvolutionConfig, maxIter, maxRetries int) *BackgroundReviewEngine {
	if maxIter <= 0 || maxIter > 8 {
		maxIter = 8
	}
	if maxRetries <= 0 {
		maxRetries = 1
	}
	return &BackgroundReviewEngine{
		mem:           mem,
		backend:       backend,
		provider:      provider,
		config:        cfg,
		maxIterations: maxIter,
		maxRetries:    maxRetries,
	}
}

func (e *BackgroundReviewEngine) SetBackend(backend AgentBackend) {
	e.backend = backend
}

func (e *BackgroundReviewEngine) SetNotifyChannel(ch chan string) {
	e.notifyCh = ch
}

func (e *BackgroundReviewEngine) SetUseMemoryFence(enabled bool) {
	e.useMemoryFence = enabled
}

// SetControlTenantID separates daemon-owned Skill/Catalog assets from the
// person partition used for memory and session review evidence.
func (e *BackgroundReviewEngine) SetControlTenantID(tenantID string) {
	e.controlTenantID = strings.TrimSpace(tenantID)
}

// SetEnqueue installs the durable-job hand-off (execution-quality W7). When
// set, SpawnReview serializes a bounded snapshot and enqueues it instead of
// spawning a goroutine — the daemon maintenance worker executes it with
// dedup, rate limiting, and bounded retries, and a crash no longer loses the
// review. Nil (tests, eval, CLI-only) keeps the immediate in-process path.
func (e *BackgroundReviewEngine) SetEnqueue(fn func(tenantID, payloadJSON string) bool) {
	e.enqueue = fn
}

// ReviewJobPayload is the durable snapshot of one requested review — the
// bounded message tail plus flags, everything ExecuteReview needs to run
// later in the maintenance worker.
type ReviewJobPayload struct {
	Channel      string          `json:"channel"`
	Messages     []ReviewMessage `json:"messages"`
	ReviewMemory bool            `json:"review_memory"`
	ReviewSkills bool            `json:"review_skills"`
}

type ReviewMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

const (
	reviewPayloadTailMessages = 8
	reviewPayloadMessageChars = 2000
)

func reviewPayloadSnapshot(messages []llm.Message) []ReviewMessage {
	// Skip the system prompt (the review agent builds its own) and keep a
	// bounded recent tail — the same window the review prompt cares about.
	var tail []ReviewMessage
	for _, m := range messages {
		if m.Role == "system" {
			continue
		}
		content := m.Content
		if len(content) > reviewPayloadMessageChars {
			content = textutil.Truncate(content, reviewPayloadMessageChars)
		}
		tail = append(tail, ReviewMessage{Role: m.Role, Content: content})
	}
	if len(tail) > reviewPayloadTailMessages {
		tail = tail[len(tail)-reviewPayloadTailMessages:]
	}
	return tail
}

func (e *BackgroundReviewEngine) SpawnReview(tenantID, channel string, messages []llm.Message, reviewMemory, reviewSkills bool) {
	if e == nil || !e.config.Enabled || e.provider == nil || e.backend == nil {
		return
	}
	// Skill evolution is owned by the durable cohort curator. The legacy
	// conversation reviewer may inspect skills, but it must never propose or
	// apply an active-skill mutation.
	reviewSkills = false
	if !reviewMemory {
		return
	}
	if e.enqueue != nil {
		payload := ReviewJobPayload{
			Channel:      channel,
			Messages:     reviewPayloadSnapshot(messages),
			ReviewMemory: reviewMemory,
			ReviewSkills: reviewSkills,
		}
		if raw, err := json.Marshal(payload); err == nil && e.enqueue(tenantID, string(raw)) {
			return
		}
		// Enqueue failure falls through to the immediate path — a review is
		// best-effort learning either way, but never silently dropped here.
	}
	snapshot := append([]llm.Message(nil), messages...)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		e.ExecuteReview(ctx, tenantID, channel, snapshot, reviewMemory, reviewSkills)
	}()
}

// RunReviewFromPayload executes one durable review job (the maintenance
// worker path). The returned summary doubles as the job's result hash input.
func (e *BackgroundReviewEngine) RunReviewFromPayload(ctx context.Context, tenantID, payloadJSON string) (string, error) {
	if e == nil || !e.config.Enabled || e.provider == nil || e.backend == nil {
		return "", fmt.Errorf("background review engine is not configured")
	}
	var payload ReviewJobPayload
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		return "", fmt.Errorf("invalid review payload: %w", err)
	}
	// Safely drain jobs created before skill review was separated from memory
	// maintenance. They must not reach a model or mutate an active skill.
	payload.ReviewSkills = false
	if !payload.ReviewMemory {
		return "review skipped: legacy skill review is disabled", nil
	}
	messages := make([]llm.Message, 0, len(payload.Messages))
	for _, m := range payload.Messages {
		messages = append(messages, llm.Message{Role: m.Role, Content: m.Content})
	}
	return e.executeReview(ctx, tenantID, payload.Channel, messages, payload.ReviewMemory, payload.ReviewSkills)
}

// ExecuteReview runs one review synchronously and returns the user-facing
// summary. Shared by the legacy in-process goroutine and the durable worker.
func (e *BackgroundReviewEngine) ExecuteReview(ctx context.Context, tenantID, channel string, snapshot []llm.Message, reviewMemory, reviewSkills bool) string {
	msg, _ := e.executeReview(ctx, tenantID, channel, snapshot, reviewMemory, reviewSkills)
	return msg
}

// executeReview preserves the provider error for the durable worker. The
// legacy public method intentionally remains best-effort, while queued jobs
// must see quota failures so they can be blocked on the shared route circuit
// instead of being incorrectly marked complete.
func (e *BackgroundReviewEngine) executeReview(ctx context.Context, tenantID, channel string, snapshot []llm.Message, reviewMemory, reviewSkills bool) (string, error) {
	reviewSkills = false
	if !reviewMemory {
		return "review skipped: legacy skill review is disabled", nil
	}
	invocationScope := ToolInvocationScope{
		ControlTenantID:   e.controlTenantID,
		PersonID:          tenantID,
		ExecutionScopeKey: "background-review:" + tenantID,
		SkillMutationMode: SkillMutationNone,
	}
	if invocationScope.ControlTenantID == "" {
		invocationScope.ControlTenantID = "default"
	}
	ctx = WithToolInvocationScope(ctx, invocationScope)
	restricted := &restrictedReviewBackend{
		inner: e.backend,
		allowed: map[string]bool{
			"memory":         true,
			"skills_list":    true,
			"skill_view":     true,
			"session_search": true,
		},
	}
	reviewAgent := NewAgent(e.mem, restricted, e.provider, backgroundReviewSoul(reviewMemory, reviewSkills), e.maxIterations, e.maxRetries, nil)
	reviewAgent.SetUseMemoryFence(e.useMemoryFence)
	resp, _, err := reviewAgent.RunConversation(ctx, tenantID, channel+":background_review", buildBackgroundReviewPrompt(snapshot, reviewMemory, reviewSkills))
	msg := summarizeReviewResult(resp, err)
	// Hallucinated-compliance guard: models sometimes emit the "skill
	// updated: <name>" summary WITHOUT ever calling skill_manage. Never
	// forward a change claim that reality does not confirm — verify each
	// claimed skill through the same restricted backend before notifying.
	if err == nil {
		if unverified := unverifiedSkillClaims(restricted, tenantID, resp, invocationScope); len(unverified) > 0 {
			log.Warn("background review claimed a skill change that did not happen",
				"claimed_skills", strings.Join(unverified, ", "),
				"tenant", tenantID,
				"channel", channel)
			msg = fmt.Sprintf("claimed a skill change that did not happen (skill %s not found after review; skill_manage change not verified); recorded as no-change",
				quoteNames(unverified))
		}
	}
	if e.notifyCh != nil {
		select {
		case e.notifyCh <- "review:" + msg:
		default:
		}
	}
	return msg, err
}

type restrictedReviewBackend struct {
	inner   AgentBackend
	allowed map[string]bool
}

func (b *restrictedReviewBackend) Dispatch(name string, args map[string]interface{}) (string, error) {
	if !b.allowed[name] {
		return "", fmt.Errorf("background review cannot use tool %s", name)
	}
	return b.inner.Dispatch(name, args)
}

func (b *restrictedReviewBackend) ToolExecutionMetadata(name string, args map[string]interface{}) ToolExecutionMetadata {
	if b == nil || !b.allowed[name] {
		return ToolExecutionMetadata{}
	}
	provider, ok := b.inner.(ToolExecutionMetadataProvider)
	if !ok {
		return ToolExecutionMetadata{}
	}
	return provider.ToolExecutionMetadata(name, args)
}

func (b *restrictedReviewBackend) GetToolDefinitions() []map[string]interface{} {
	defs := b.inner.GetToolDefinitions()
	filtered := make([]map[string]interface{}, 0, len(defs))
	for _, d := range defs {
		name := toolDefinitionName(d)
		if b.allowed[name] {
			filtered = append(filtered, d)
		}
	}
	return filtered
}

func toolDefinitionName(d map[string]interface{}) string {
	if name, ok := d["name"].(string); ok {
		return name
	}
	if fn, ok := d["function"].(map[string]interface{}); ok {
		if name, ok := fn["name"].(string); ok {
			return name
		}
	}
	return ""
}

func toolDefinitionDescription(d map[string]interface{}) string {
	if desc, ok := d["description"].(string); ok {
		return desc
	}
	if fn, ok := d["function"].(map[string]interface{}); ok {
		if desc, ok := fn["description"].(string); ok {
			return desc
		}
	}
	return ""
}

func toolDefinitionParameters(d map[string]interface{}) map[string]interface{} {
	if params, ok := d["parameters"].(map[string]interface{}); ok {
		return params
	}
	if fn, ok := d["function"].(map[string]interface{}); ok {
		if params, ok := fn["parameters"].(map[string]interface{}); ok {
			return params
		}
	}
	return nil
}

// skillClaimPattern conservatively matches the explicit change-claim format
// the review prompt asks for ("skill created: <name>", "skill updated: <name>",
// "skill patched: <name>"). It intercepts ONLY clear claims: the verb must be
// followed by a colon (ASCII or full-width) and a plausible skill name token.
// Ordinary prose ("Nothing to save.", discussion of skills without a claim)
// never matches and passes through unchanged.
// The name must start and end with an alphanumeric so trailing sentence
// punctuation ("skill updated: foo.") is not swallowed into the name.
var skillClaimPattern = regexp.MustCompile(`(?i)\bskill\s+(created|updated|patched)\s*[:：]\s*` + "[`\"']?" + `([A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?)`)

// extractSkillChangeClaims returns the deduplicated skill names the review
// response claims to have created/updated/patched. Memory claims
// ("memory saved: <topic>") are deliberately NOT intercepted: the claimed
// topic is free text that cannot be cheaply and reliably matched against
// stored facts, and a fuzzy check would risk rejecting honest reviews. Skill
// claims name a concrete on-disk asset, so they are verifiable.
func extractSkillChangeClaims(resp string) []string {
	matches := skillClaimPattern.FindAllStringSubmatch(resp, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(matches))
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		name := m[2]
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// unverifiedSkillClaims verifies every claimed skill change against reality by
// loading the named skill through the SAME restricted backend the review agent
// used (skill_view is on its allowlist). Existence is the baseline check for
// created/updated/patched claims: a hallucinated claim with no tool call leaves
// no skill on disk. Any claim that cannot be confirmed (missing skill, or a
// backend without skill_view) is treated as unverified — this check never
// fails open into crediting an unconfirmed change.
func unverifiedSkillClaims(backend AgentBackend, tenantID, resp string, scopes ...ToolInvocationScope) []string {
	var failed []string
	for _, name := range extractSkillChangeClaims(resp) {
		args := map[string]interface{}{
			"name":       name,
			"_tenant_id": tenantID,
		}
		if len(scopes) > 0 {
			args["_invocation_scope"] = scopes[0]
		}
		_, err := backend.Dispatch("skill_view", args)
		if err != nil {
			failed = append(failed, name)
		}
	}
	return failed
}

func quoteNames(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, fmt.Sprintf("%q", n))
	}
	return strings.Join(quoted, ", ")
}

func backgroundReviewSoul(reviewMemory, reviewSkills bool) string {
	var focus []string
	if reviewMemory {
		focus = append(focus, "durable user/project memory")
	}
	if reviewSkills {
		focus = append(focus, "reusable procedural skills")
	}
	return "You are SelfMind's background learning reviewer. You run after the user-facing answer is complete. Use only the available tools to save durable learning. Focus: " + strings.Join(focus, " and ") + ". Never state that a skill was created/updated or a memory was saved unless you actually made that change with a successful tool call in this turn; claims are verified against the toolchain. If nothing is worth saving, answer exactly: Nothing to save."
}

func buildBackgroundReviewPrompt(messages []llm.Message, reviewMemory, reviewSkills bool) string {
	var sb strings.Builder
	sb.WriteString("Review this completed conversation and decide whether SelfMind should learn anything durable.\n\n")
	if reviewMemory {
		sb.WriteString("Memory rules:\n")
		sb.WriteString("- Save user preferences, stable project facts, recurring corrections, and environment conventions with the memory tool.\n")
		sb.WriteString("- Do not save temporary task status, one-off outcomes, PR numbers, file counts, or facts likely to be stale within a week.\n")
		sb.WriteString("- Do not save transient tool failures, provider outages, or guesses about a tool being broken unless the user confirms a durable rule.\n")
		sb.WriteString("- If the user corrects style, naming, environment, or workflow expectations, save the durable preference in memory.\n")
	}
	if reviewSkills {
		sb.WriteString("Skill rules:\n")
		sb.WriteString("- Save reusable workflows, non-trivial debugging paths, or user workflow corrections with skill_manage.\n")
		sb.WriteString("- Prefer skills_list/skill_view and patch of an existing skill before creating a new class-level skill.\n")
		sb.WriteString("- Put session-specific detail in references/ via skill_manage action=write_file.\n")
		sb.WriteString("- Do not create duplicate skills for the same workflow. Search first, then patch the closest existing skill.\n")
		sb.WriteString("- When creating skills, set source=agent-created and include concise front matter name/description.\n")
		sb.WriteString("- If a used skill was incomplete, stale, or contradicted by the user, patch that skill instead of writing a new one.\n")
		sb.WriteString("- Avoid archiving/deleting skills during ordinary review unless the evidence is strong; pinned or manual skills should not be archived by review.\n")
		sb.WriteString("- Do not encode secrets, one-off file paths, issue numbers, or temporary command output in SKILL.md; put durable examples in references/ only when they generalize.\n")
	}
	sb.WriteString("\nConversation snapshot:\n")
	for _, m := range messages {
		content := m.Content
		if len(content) > 1200 {
			content = textutil.HeadTail(content, 600, "\n...[truncated]...\n")
		}
		sb.WriteString(fmt.Sprintf("\n[%s]\n%s\n", m.Role, content))
	}
	sb.WriteString("\nUse tools if there is durable learning. Otherwise reply: Nothing to save.\n")
	sb.WriteString("After tool use, summarize exactly what changed in one short sentence, for example: memory saved: <topic>, skill updated: <name>, or skill created: <name>.\n")
	sb.WriteString("Never state \"skill created:\", \"skill updated:\", \"skill patched:\", or \"memory saved:\" unless the corresponding tool call succeeded in THIS turn; these claims are verified against the toolchain and a false claim is discarded. If you decide not to change anything, reply exactly: Nothing to save.")
	return sb.String()
}

func summarizeReviewResult(resp string, err error) string {
	if err != nil {
		return "review skipped: " + err.Error()
	}
	resp = strings.TrimSpace(resp)
	if resp == "" || strings.EqualFold(resp, "Nothing to save.") || strings.EqualFold(resp, "Nothing to save") {
		return "review skipped: nothing durable"
	}
	if len(resp) > 200 {
		resp = textutil.TruncateBytes(resp, 200) + "..."
	}
	return "learning review: " + resp
}
