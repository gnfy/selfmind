package kernel

import (
	"encoding/json"
	"hash/maphash"
	"sync"

	"github.com/tiktoken-go/tokenizer"

	"selfmind/internal/kernel/llm"
)

// Long texts are counted once. One request recounts the same history several
// times (compaction check, request budget, diagnostics), and encoding is
// superlinear in a long run without spaces: 32 KB of base64 or minified code
// took 290 ms per count. The cache keys each text by a hash and its length, so
// it holds no text, and starts over after tokenCountCacheEntries texts.
const (
	tokenCountCacheMinBytes = 512
	tokenCountCacheEntries  = 4096
)

type tokenCountKey struct {
	hash   uint64
	length int
}

// TokenEstimator wraps tiktoken-go for precise token counting.
// Falls back to heuristic estimation when the codec is unavailable.
type TokenEstimator struct {
	enc tokenizer.Codec

	mu      sync.Mutex
	seed    maphash.Seed
	counted map[tokenCountKey]int
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
	if len(text) < tokenCountCacheMinBytes {
		_, ids, _ := te.enc.Encode(text)
		return len(ids)
	}
	te.mu.Lock()
	if te.counted == nil {
		te.seed, te.counted = maphash.MakeSeed(), make(map[tokenCountKey]int)
	}
	key := tokenCountKey{hash: maphash.String(te.seed, text), length: len(text)}
	n, ok := te.counted[key]
	te.mu.Unlock()
	if ok {
		return n
	}
	_, ids, _ := te.enc.Encode(text)
	te.mu.Lock()
	if len(te.counted) >= tokenCountCacheEntries {
		te.counted = make(map[tokenCountKey]int)
	}
	te.counted[key] = len(ids)
	te.mu.Unlock()
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
