package tools

import "selfmind/internal/kernel"

// Ensure *Dispatcher implements kernel.ToolBackend and kernel.AgentBackend.
var _ kernel.ToolBackend = (*Dispatcher)(nil)
var _ kernel.AgentBackend = (*Dispatcher)(nil)
