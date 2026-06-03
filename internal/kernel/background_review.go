package kernel

import (
	"context"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

type BackgroundReviewEngine struct {
	mem            *memory.MemoryManager
	backend        AgentBackend
	provider       llm.Provider
	config         EvolutionConfig
	maxIterations  int
	maxRetries     int
	useMemoryFence bool
	notifyCh       chan string
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

func (e *BackgroundReviewEngine) SpawnReview(tenantID, channel string, messages []llm.Message, reviewMemory, reviewSkills bool) {
	if e == nil || !e.config.Enabled || e.provider == nil || e.backend == nil {
		return
	}
	if !reviewMemory && !reviewSkills {
		return
	}
	snapshot := append([]llm.Message(nil), messages...)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		restricted := &restrictedReviewBackend{
			inner: e.backend,
			allowed: map[string]bool{
				"memory":         true,
				"skill_manage":   true,
				"session_search": true,
			},
		}
		reviewAgent := NewAgent(e.mem, restricted, e.provider, backgroundReviewSoul(reviewMemory, reviewSkills), e.maxIterations, e.maxRetries, nil)
		reviewAgent.SetUseMemoryFence(e.useMemoryFence)
		resp, _, err := reviewAgent.RunConversation(ctx, tenantID, channel+":background_review", buildBackgroundReviewPrompt(snapshot, reviewMemory, reviewSkills))
		if e.notifyCh == nil {
			return
		}
		msg := summarizeReviewResult(resp, err)
		select {
		case e.notifyCh <- "review:" + msg:
		default:
		}
	}()
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

func backgroundReviewSoul(reviewMemory, reviewSkills bool) string {
	var focus []string
	if reviewMemory {
		focus = append(focus, "durable user/project memory")
	}
	if reviewSkills {
		focus = append(focus, "reusable procedural skills")
	}
	return "You are SelfMind's background learning reviewer. You run after the user-facing answer is complete. Use only the available tools to save durable learning. Focus: " + strings.Join(focus, " and ") + ". If nothing is worth saving, answer exactly: Nothing to save."
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
		sb.WriteString("- Prefer search/read/patch of an existing skill before creating a new class-level skill.\n")
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
			content = content[:600] + "\n...[truncated]...\n" + content[len(content)-600:]
		}
		sb.WriteString(fmt.Sprintf("\n[%s]\n%s\n", m.Role, content))
	}
	sb.WriteString("\nUse tools if there is durable learning. Otherwise reply: Nothing to save.\n")
	sb.WriteString("After tool use, summarize exactly what changed in one short sentence, for example: memory saved: <topic>, skill updated: <name>, or skill created: <name>.")
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
		resp = resp[:200] + "..."
	}
	return "learning review: " + resp
}
