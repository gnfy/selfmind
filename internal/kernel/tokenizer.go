package kernel

import (
	"encoding/json"

	"github.com/tiktoken-go/tokenizer"

	"selfmind/internal/kernel/llm"
)

// TokenEstimator wraps tiktoken-go for precise token counting.
// Falls back to heuristic estimation when the codec is unavailable.
type TokenEstimator struct {
	enc tokenizer.Codec
}

// NewTokenEstimator creates an estimator using the cl100k_base encoding
// (used by GPT-4, Claude, and most modern models).
func NewTokenEstimator() *TokenEstimator {
	enc, err := tokenizer.Get(tokenizer.Cl100kBase)
	if err != nil {
		return &TokenEstimator{enc: nil}
	}
	return &TokenEstimator{enc: enc}
}

// Count returns the token count for a single string.
func (te *TokenEstimator) Count(text string) int {
	if te.enc == nil {
		return estimateTokens(text)
	}
	_, ids, _ := te.enc.Encode(text)
	return len(ids)
}

// CountMessages returns the total token count for a list of messages,
// including role overhead (~3-4 tokens per message).
func (te *TokenEstimator) CountMessages(msgs []llm.Message) int {
	total := 0
	for _, m := range msgs {
		total += messageTokenCount(m, te.Count)
	}
	return total
}

func messageTokenCount(m llm.Message, count func(string) int) int {
	total := count(m.Content) + 4
	for _, part := range m.MultiContent {
		total += count(part.Text)
		if part.ImageURL != "" || part.Data != "" {
			total += 256 // provider-dependent image estimate, also used by diagnostics
		}
	}
	for _, call := range m.ToolCalls {
		total += count(call.ID) + count(call.Function) + count(call.Args) + 4
	}
	return total + count(m.ToolCallID)
}

func (te *TokenEstimator) CountTools(tools []llm.ToolDefinition) int {
	if len(tools) == 0 {
		return 0
	}
	raw, _ := json.Marshal(tools)
	return te.Count(string(raw))
}
