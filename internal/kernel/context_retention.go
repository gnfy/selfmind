package kernel

import (
	"context"

	"selfmind/internal/kernel/llm"
)

type contextInputKey struct{}

// The gateway-decorated current input is known before the spine is composed.
// Match that exact input, not the oldest historical user message, as the start
// of this Run's instructions. All subsequent user guidance remains protected.
func withContextInput(ctx context.Context, input string) context.Context {
	return context.WithValue(ctx, contextInputKey{}, input)
}

func protectedContextMessages(ctx context.Context, messages []llm.Message) []bool {
	start := 0
	input, scoped := ctx.Value(contextInputKey{}).(string)
	// A recovered ledger predates this turn's continuation text. Its user
	// constraints still apply; legacy checkpoints have no finer provenance,
	// so retain their user messages conservatively instead of treating them
	// as disposable work-spine history.
	_, resumed := ctx.Value(loopResumeMessagesKey{}).([]llm.Message)
	if scoped && !resumed {
		for i, message := range messages {
			if message.Role == "user" && message.Content == input {
				start = i
				break
			}
		}
	}
	protected := make([]bool, len(messages))
	firstUser, lastUser := -1, -1
	for i, message := range messages {
		if message.Role == "user" && !isCompactionSummary(message) {
			if firstUser < 0 {
				firstUser = i
			}
			lastUser = i
		}
	}
	for i, message := range messages {
		protected[i] = message.Role == "system" ||
			(message.Role == "user" && !isCompactionSummary(message) &&
				((scoped && i >= start) || (!scoped && (i == firstUser || i == lastUser))))
	}
	return protected
}

// Remove an assistant's call batch together with its contiguous tool results.
// This avoids relying on transport sanitizers to discard useful orphaned
// results after a one-message-at-a-time trim. Preserve the newest group too.
func (c *ContextEngine) trimContext(ctx context.Context, messages []llm.Message, max int) []llm.Message {
	out := append([]llm.Message(nil), messages...)
	for c.countMessages(out) > max {
		protected := protectedContextMessages(ctx, out)
		removed := false
		for start := 0; start < len(out); {
			end := start + 1
			if len(out[start].ToolCalls) > 0 {
				for end < len(out) && out[end].Role == "tool" {
					end++
				}
			}
			keep := end == len(out)
			for i := start; i < end; i++ {
				keep = keep || protected[i]
			}
			if !keep {
				out = append(out[:start], out[end:]...)
				removed = true
				break
			}
			start = end
		}
		if !removed {
			break // provider preflight reports oversized mandatory context
		}
	}
	return out
}
