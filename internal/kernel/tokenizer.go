package kernel

import (
	"selfmind/internal/kernel/llm"
	"github.com/tiktoken-go/tokenizer"
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
		total += te.Count(m.Content)
		total += 4 // role overhead approximation
	}
	return total
}
