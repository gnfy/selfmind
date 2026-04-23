package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

// FactExtractor automatically extracts durable facts from a completed
// conversation and persists them via the MemoryManager.
type FactExtractor struct {
	provider llm.Provider
	enabled  bool
}

// extractionPrompt is sent to a lightweight LLM to distill facts.
const extractionPrompt = `You are a memory extraction assistant. Analyze the conversation below and extract durable facts that should be remembered for future sessions.

Extract TWO types of facts:
1. USER PREFERENCES: User's personal preferences, habits, communication style, role, expertise level, naming conventions, favorite tools, etc.
2. ENVIRONMENT FACTS: Project conventions, technology choices, directory structures, build commands, API endpoint patterns, deployment workflows, etc.

Rules:
- Only extract facts that are LIKELY to be useful in future conversations (not one-off task details).
- Do NOT extract sensitive data like passwords, API keys, or tokens.
- Do NOT extract transient information (e.g. "the server is down right now").
- Format each fact as a single concise declarative sentence.
- If no durable facts are found, return empty arrays.

Conversation:
%s

Respond ONLY with a JSON object in this exact format (no markdown, no explanation):
{"user_facts":["fact1","fact2"],"memory_facts":["fact1","fact2"]}`

// extractedFacts is the JSON structure returned by the LLM.
type extractedFacts struct {
	UserFacts   []string `json:"user_facts"`
	MemoryFacts []string `json:"memory_facts"`
}

// NewFactExtractor creates a new extractor. If provider is nil, extraction is disabled.
func NewFactExtractor(provider llm.Provider, enabled bool) *FactExtractor {
	return &FactExtractor{
		provider: provider,
		enabled:  enabled && provider != nil,
	}
}

// Extract analyzes the conversation and persists new facts that are not
// already present in the existing fact list.
func (fe *FactExtractor) Extract(ctx context.Context, tenantID string, mem *memory.MemoryManager, messages []llm.Message) error {
	if !fe.enabled || mem == nil || len(messages) < 2 {
		return nil
	}

	// Trigger if the conversation involved tool calls (substantive task)
	// or has at least 2 turns of back-and-forth.
	hasToolCalls := false
	turnCount := 0
	for _, m := range messages {
		if m.Role == "user" || m.Role == "assistant" {
			turnCount++
		}
		if m.Role == "tool" {
			hasToolCalls = true
		}
	}
	if !hasToolCalls && turnCount < 4 {
		// Skip trivial / casual chats to save cost
		return nil
	}

	// 1. Build conversation transcript
	transcript := buildTranscript(messages)
	if len(transcript) < 100 {
		// Too short to extract meaningful facts
		return nil
	}

	// 2. Fetch existing facts for deduplication
	existingUserFacts, _ := mem.GetFacts(ctx, tenantID, "user")
	existingMemFacts, _ := mem.GetFacts(ctx, tenantID, "memory")

	// 3. Call LLM to extract new facts
	prompt := fmt.Sprintf(extractionPrompt, transcript)
	resp, err := fe.provider.ChatCompletion(ctx, []llm.Message{
		{Role: "user", Content: prompt},
	})
	if err != nil {
		return fmt.Errorf("llm extraction failed: %w", err)
	}

	var extracted extractedFacts
	if err := json.Unmarshal([]byte(resp), &extracted); err != nil {
		// Try to extract JSON from markdown code block
		cleaned := extractJSON(resp)
		if err2 := json.Unmarshal([]byte(cleaned), &extracted); err2 != nil {
			return fmt.Errorf("parse extraction response: %w (raw: %s)", err, truncate(resp, 200))
		}
	}

	// 4. Persist new facts with deduplication
	for _, f := range extracted.UserFacts {
		f = strings.TrimSpace(f)
		if f == "" || isDuplicate(f, existingUserFacts) {
			continue
		}
		_ = mem.AddFact(ctx, tenantID, "user", f)
	}
	for _, f := range extracted.MemoryFacts {
		f = strings.TrimSpace(f)
		if f == "" || isDuplicate(f, existingMemFacts) {
			continue
		}
		_ = mem.AddFact(ctx, tenantID, "memory", f)
	}

	return nil
}

// buildTranscript creates a human-readable conversation log for the LLM.
func buildTranscript(messages []llm.Message) string {
	var sb strings.Builder
	for _, m := range messages {
		switch m.Role {
		case "user":
			sb.WriteString("User: ")
			sb.WriteString(m.Content)
			sb.WriteString("\n\n")
		case "assistant":
			sb.WriteString("Assistant: ")
			sb.WriteString(m.Content)
			sb.WriteString("\n\n")
		case "tool":
			// Truncate long tool results
			content := m.Content
			if len(content) > 800 {
				content = content[:400] + "\n...[truncated]...\n" + content[len(content)-400:]
			}
			sb.WriteString("Tool result: ")
			sb.WriteString(content)
			sb.WriteString("\n\n")
		}
	}
	return sb.String()
}

// isDuplicate checks if a new fact is semantically similar to an existing one.
// We use a simple substring containment + edit-distance heuristic.
func isDuplicate(newFact string, existing []memory.Fact) bool {
	newLower := strings.ToLower(newFact)
	for _, ef := range existing {
		existingLower := strings.ToLower(ef.Content)
		// Exact or near-exact match
		if strings.Contains(existingLower, newLower) || strings.Contains(newLower, existingLower) {
			return true
		}
		// Levenshtein ratio > 0.75 considered duplicate
		if levenshteinRatio(newLower, existingLower) > 0.75 {
			return true
		}
	}
	return false
}

// extractJSON tries to pull a JSON object out of a markdown-fenced response.
func extractJSON(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// levenshteinRatio returns the similarity ratio (0.0-1.0) between two strings.
func levenshteinRatio(a, b string) float64 {
	if a == "" && b == "" {
		return 1.0
	}
	if a == "" || b == "" {
		return 0.0
	}
	dist := levenshteinDistance(a, b)
	maxLen := len(a)
	if len(b) > maxLen {
		maxLen = len(b)
	}
	return 1.0 - float64(dist)/float64(maxLen)
}

func levenshteinDistance(a, b string) int {
	// Convert to rune slices for Unicode safety
	ra := []rune(a)
	rb := []rune(b)
	la, lb := len(ra), len(rb)

	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}

	// Use two rows for O(min(la,lb)) space
	if lb < la {
		ra, rb = rb, ra
		la, lb = lb, la
	}

	prev := make([]int, la+1)
	curr := make([]int, la+1)
	for i := 0; i <= la; i++ {
		prev[i] = i
	}

	for j := 1; j <= lb; j++ {
		curr[0] = j
		for i := 1; i <= la; i++ {
			cost := 0
			if ra[i-1] != rb[j-1] {
				cost = 1
			}
			curr[i] = min(prev[i]+1, curr[i-1]+1, prev[i-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[la]
}

func min(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}
