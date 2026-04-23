package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

// TurnExtractor performs lightweight fact extraction after each assistant turn.
// It is cheaper than FactExtractor because it only looks at the current turn,
// not the entire conversation history.
type TurnExtractor struct {
	provider  llm.Provider
	enabled   bool
	interval  int // extract every N assistant turns
	minChars  int // skip turns shorter than this with no tool calls

	turnCounter   int           // number of assistant turns since last extraction
	lastExtracted time.Time
}

// NewTurnExtractor creates a turn-level extractor.
func NewTurnExtractor(provider llm.Provider, enabled bool, interval, minChars int) *TurnExtractor {
	if interval <= 0 {
		interval = 5
	}
	if minChars <= 0 {
		minChars = 80
	}
	return &TurnExtractor{
		provider: provider,
		enabled:  enabled && provider != nil,
		interval: interval,
		minChars: minChars,
	}
}

const turnExtractPrompt = `Analyze the conversation turn below and extract durable facts worth remembering for future sessions.

Rules:
- Only extract facts likely to be useful later (preferences, conventions, tech choices, project structure).
- Do NOT extract passwords, API keys, or transient status.
- Do NOT extract one-off task details.
- If no durable facts, return empty arrays.

Turn:
User: %s
Assistant: %s

Respond ONLY with JSON: {"user_facts":["..."], "memory_facts":["..."]}`

// extractedTurnFacts is the JSON structure returned by the LLM.
type extractedTurnFacts struct {
	UserFacts   []string `json:"user_facts"`
	MemoryFacts []string `json:"memory_facts"`
}

// ShouldExtract determines whether this turn warrants extraction based on frequency and content filters.
func (te *TurnExtractor) ShouldExtract(turn memory.MessagePair, hasToolCalls bool) bool {
	if !te.enabled {
		return false
	}

	te.turnCounter++
	if te.turnCounter < te.interval {
		return false
	}

	contentLen := len(turn.User) + len(turn.Assistant)
	if !hasToolCalls && contentLen < te.minChars {
		return false
	}

	// Skip pure greetings / casual chatter
	combined := turn.User + " " + turn.Assistant
	if isCasualChat(combined) {
		return false
	}

	return true
}

// Extract performs lightweight fact extraction for a single turn.
// It runs asynchronously so it never blocks the conversation loop.
func (te *TurnExtractor) Extract(ctx context.Context, tenantID string, mem *memory.MemoryManager, turn memory.MessagePair) {
	if mem == nil {
		return
	}

	// Run in background to avoid blocking
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		prompt := fmt.Sprintf(turnExtractPrompt, truncate(turn.User, 800), truncate(turn.Assistant, 800))
		resp, err := te.provider.ChatCompletion(bgCtx, []llm.Message{
			{Role: "user", Content: prompt},
		})
		if err != nil {
			return
		}

		var facts extractedTurnFacts
		if err := json.Unmarshal([]byte(resp), &facts); err != nil {
			// Try to extract JSON from markdown code block
			cleaned := extractJSON(resp)
			if err2 := json.Unmarshal([]byte(cleaned), &facts); err2 != nil {
				return
			}
		}

		existingUser, _ := mem.GetFacts(bgCtx, tenantID, "user")
		existingMem, _ := mem.GetFacts(bgCtx, tenantID, "memory")

		for _, f := range facts.UserFacts {
			f = strings.TrimSpace(f)
			if f == "" || isDuplicateFact(f, existingUser) {
				continue
			}
			_ = mem.AddFact(bgCtx, tenantID, "user", f)
		}
		for _, f := range facts.MemoryFacts {
			f = strings.TrimSpace(f)
			if f == "" || isDuplicateFact(f, existingMem) {
				continue
			}
			_ = mem.AddFact(bgCtx, tenantID, "memory", f)
		}
	}()
}

// ResetCounter resets the turn counter (e.g., after a successful extraction or new session).
func (te *TurnExtractor) ResetCounter() {
	te.turnCounter = 0
}

// casualChatPatterns are simple heuristics to skip trivial turns.
var casualChatPatterns = []string{"你好", "您好", "hello", "hi", "hey", "谢谢", "thanks", "再见", "bye", "goodbye", "在吗", "在吗？"}

func isCasualChat(text string) bool {
	lower := strings.ToLower(text)
	// If the turn is very short and contains only greeting words
	if len(text) < 30 {
		for _, p := range casualChatPatterns {
			if strings.Contains(lower, p) {
				return true
			}
		}
	}
	return false
}

func isDuplicateFact(newFact string, existing []memory.Fact) bool {
	newLower := strings.ToLower(newFact)
	for _, ef := range existing {
		existingLower := strings.ToLower(ef.Content)
		if strings.Contains(existingLower, newLower) || strings.Contains(newLower, existingLower) {
			return true
		}
	}
	return false
}


