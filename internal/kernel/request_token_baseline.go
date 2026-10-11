package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/json"

	"selfmind/internal/kernel/llm"
)

// One Run owns one baseline. Native usage describes exactly its submitted
// prompt, including cache reads; output and cache counts are never added again.
// Only an unchanged request prefix may carry that measurement forward.
type requestTokenBaseline struct {
	key, pendingKey            string
	hashes, pending            [][32]byte
	input, local, pendingLocal int
}
type requestTokenBaselineKey struct{}

func tokenBaselineFromContext(ctx context.Context) *requestTokenBaseline {
	value, _ := ctx.Value(requestTokenBaselineKey{}).(*requestTokenBaseline)
	return value
}

func messageHashes(messages []llm.Message) [][32]byte {
	out := make([][32]byte, len(messages))
	for i, message := range messages {
		// Local retention metadata is not part of the provider request.
		message.ContextOrigin, message.ControlCondition = "", ""
		raw, _ := json.Marshal(message)
		out[i] = sha256.Sum256(raw)
	}
	return out
}

func (b *requestTokenBaseline) configure(key string) {
	if b == nil {
		return
	}
	if b.key != key {
		b.input, b.local, b.hashes = 0, 0, nil
	}
	b.key = key
}

func (b *requestTokenBaseline) estimate(messages []llm.Message, local int) (int, bool) {
	if b == nil || b.key == "" || b.input <= 0 || len(messages) < len(b.hashes) {
		return local, false
	}
	hashes := messageHashes(messages[:len(b.hashes)])
	for i := range hashes {
		if hashes[i] != b.hashes[i] {
			return local, false
		}
	}
	return max(local, b.input+local-b.local), true
}

func (b *requestTokenBaseline) submitted(messages []llm.Message, local int) {
	if b == nil {
		return
	}
	b.pendingKey, b.pending, b.pendingLocal = b.key, messageHashes(messages), local
}

func (b *requestTokenBaseline) observed(status string, usage llm.UsageStats) {
	if b == nil {
		return
	}
	if status != "succeeded" || usage.InputTokens <= 0 || b.pendingKey == "" {
		b.input, b.hashes = 0, nil
		return
	}
	b.key, b.hashes, b.input, b.local = b.pendingKey, b.pending, usage.InputTokens, b.pendingLocal
}

func (c *ContextEngine) requestTokens(ctx context.Context, messages []llm.Message, tools []llm.ToolDefinition) (int, bool) {
	local := c.countMessages(messages) + c.tokenizer.CountTools(tools)
	return tokenBaselineFromContext(ctx).estimate(messages, local)
}
