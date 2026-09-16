package kernel

import (
	"context"
	"fmt"

	"selfmind/internal/kernel/llm"
)

func (c *ContextEngine) inputBudget() int {
	budget := c.maxTokens - c.reserveTokens
	if budget <= 0 {
		budget = c.maxTokens
	}
	if budget < 1 {
		budget = 1
	}
	return budget
}

// PrepareRequest budgets the same message and tool payload that will be sent.
// Mandatory instructions are never discarded to make a request fit.
func (c *ContextEngine) PrepareRequest(ctx context.Context, messages []llm.Message, tools []llm.ToolDefinition) ([]llm.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	toolTokens := c.tokenizer.CountTools(tools)
	budget := c.inputBudget()
	messages = c.truncateMessages(ctx, messages, budget-toolTokens)
	return c.fitRequest(ctx, messages, tools)
}

// fitRequest is the deterministic final check after late steering. It must
// not call a summarizer: the input snapshot has already been taken.
func (c *ContextEngine) fitRequest(ctx context.Context, messages []llm.Message, tools []llm.ToolDefinition) ([]llm.Message, error) {
	toolTokens := c.tokenizer.CountTools(tools)
	budget := c.inputBudget()
	if c.countMessages(messages)+toolTokens <= budget {
		return messages, ctx.Err()
	}
	messages = c.trimContext(ctx, messages, budget-toolTokens)
	if used := c.countMessages(messages) + toolTokens; used > budget {
		return nil, fmt.Errorf("required instructions and latest tool output exceed the input budget (%d estimated tokens; %d available). Shorten the input or reduce the active tool catalog before retrying", used, budget)
	}
	return messages, ctx.Err()
}
