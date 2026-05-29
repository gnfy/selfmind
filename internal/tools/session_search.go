package tools

import (
	"fmt"
	"strings"

	"selfmind/internal/kernel/memory"
)

// SessionSearchTool searches and browses cross-session history.
type SessionSearchTool struct {
	BaseTool
	searchFn   func(query string, limit int) (interface{}, error)
	recentFn   func(limit int) (interface{}, error)
	messagesFn func(sessionID string, aroundMessageID, window int) (interface{}, error)
}

func NewSessionSearchTool() *SessionSearchTool {
	return &SessionSearchTool{
		BaseTool: BaseTool{
			name:        "session_search",
			description: "Search past sessions, browse recent sessions, or expand messages around a session message.",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"query": {
						Type:        "string",
						Description: "Search text. Omit to browse recent sessions.",
					},
					"limit": {
						Type:        "integer",
						Description: "Maximum result count. Default 10.",
						Default:     10,
					},
					"session_id": {
						Type:        "string",
						Description: "Session id to expand.",
					},
					"around_message_id": {
						Type:        "integer",
						Description: "Message id to center expansion around.",
					},
					"window": {
						Type:        "integer",
						Description: "Messages before/after around_message_id. Default 10.",
						Default:     10,
					},
				},
				Required: []string{},
			},
		},
	}
}

func (t *SessionSearchTool) RegisterSearchFn(fn func(query string, limit int) (interface{}, error)) {
	t.searchFn = fn
}

func (t *SessionSearchTool) RegisterAccessFns(
	searchFn func(query string, limit int) (interface{}, error),
	recentFn func(limit int) (interface{}, error),
	messagesFn func(sessionID string, aroundMessageID, window int) (interface{}, error),
) {
	t.searchFn = searchFn
	t.recentFn = recentFn
	t.messagesFn = messagesFn
}

func (t *SessionSearchTool) Execute(args map[string]interface{}) (string, error) {
	query, _ := args["query"].(string)
	sessionID, _ := args["session_id"].(string)
	limit := intArg(args, "limit", 10)
	window := intArg(args, "window", 10)
	around := intArg(args, "around_message_id", 0)

	if sessionID != "" {
		if t.messagesFn == nil {
			return "", fmt.Errorf("session message browsing not initialized")
		}
		raw, err := t.messagesFn(sessionID, around, window)
		if err != nil {
			return "", err
		}
		return formatSessionMessages(sessionID, raw), nil
	}

	if strings.TrimSpace(query) == "" {
		if t.recentFn == nil {
			return "", fmt.Errorf("recent session browsing not initialized")
		}
		raw, err := t.recentFn(limit)
		if err != nil {
			return "", err
		}
		return formatSessions("Recent sessions", raw), nil
	}

	if t.searchFn == nil {
		return "", fmt.Errorf("session search not initialized")
	}
	raw, err := t.searchFn(query, limit)
	if err != nil {
		return "", fmt.Errorf("search failed: %w", err)
	}
	return formatSessions(fmt.Sprintf("Results for %q", query), raw), nil
}

func intArg(args map[string]interface{}, key string, fallback int) int {
	switch v := args[key].(type) {
	case int:
		if v > 0 {
			return v
		}
	case int64:
		if v > 0 {
			return int(v)
		}
	case float64:
		if v > 0 {
			return int(v)
		}
	}
	return fallback
}

func formatSessions(title string, raw interface{}) string {
	sessions := normalizeSessions(raw)
	if len(sessions) == 0 {
		return title + ": none."
	}
	var out strings.Builder
	out.WriteString(fmt.Sprintf("%s (%d)\n\n", title, len(sessions)))
	for i, s := range sessions {
		snippet := s.Summary
		if snippet == "" {
			snippet = s.Content
		}
		if len(snippet) > 240 {
			snippet = snippet[:240] + "..."
		}
		out.WriteString(fmt.Sprintf("%d. SessionID: %s\n   Channel: %s\n   %s\n\n", i+1, s.SessionID, s.Channel, snippet))
	}
	return out.String()
}

func normalizeSessions(raw interface{}) []memory.FTS5Session {
	switch v := raw.(type) {
	case []memory.FTS5Session:
		return v
	case []interface{}:
		var sessions []memory.FTS5Session
		for _, item := range v {
			if s, ok := item.(memory.FTS5Session); ok {
				sessions = append(sessions, s)
			}
		}
		return sessions
	default:
		return nil
	}
}
func formatSessionMessages(sessionID string, raw interface{}) string {
	messages := normalizeSessionMessages(raw)
	if len(messages) == 0 {
		return fmt.Sprintf("No messages found for session %s.", sessionID)
	}
	var out strings.Builder
	out.WriteString(fmt.Sprintf("Session %s messages (%d)\n\n", sessionID, len(messages)))
	for _, m := range messages {
		content := m.Content
		if len(content) > 800 {
			content = content[:800] + "..."
		}
		out.WriteString(fmt.Sprintf("[%d] %s: %s\n\n", m.MessageID, m.Role, content))
	}
	return out.String()
}

func normalizeSessionMessages(raw interface{}) []memory.SessionMessage {
	switch v := raw.(type) {
	case []memory.SessionMessage:
		return v
	case []interface{}:
		var messages []memory.SessionMessage
		for _, item := range v {
			if msg, ok := item.(memory.SessionMessage); ok {
				messages = append(messages, msg)
			}
		}
		return messages
	default:
		return nil
	}
}
