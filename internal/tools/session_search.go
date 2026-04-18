package tools

import (
	"fmt"
	"strings"
)

// =============================================================================
// Session Search Tool (FTS5)
// =============================================================================

// SessionSearchTool 全文本搜索会话历史
type SessionSearchTool struct {
	BaseTool
	searchFn func(query string, limit int) (interface{}, error)
}

type SessionMatch struct {
	TenantID  string `json:"tenant_id"`
	Channel   string `json:"channel"`
	SessionID string `json:"session_id"`
	Content   string `json:"content"`
	Snippet   string `json:"snippet"`
}

func NewSessionSearchTool() *SessionSearchTool {
	return &SessionSearchTool{
		BaseTool: BaseTool{
			name:        "session_search",
			description: "在会话历史中全文搜索",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"query": {
						Type:        "string",
						Description: "搜索关键词",
					},
					"limit": {
						Type:        "integer",
						Description: "最大结果数，默认 10",
						Default:     10,
					},
					"channel": {
						Type:        "string",
						Description: "可选：限定渠道（cli/wechat/dingtalk）",
					},
				},
				Required: []string{"query"},
			},
		},
	}
}

// RegisterSearchFn registers the search function (called by memory module).
func (t *SessionSearchTool) RegisterSearchFn(fn func(query string, limit int) (interface{}, error)) {
	t.searchFn = fn
}

func (t *SessionSearchTool) Execute(args map[string]interface{}) (string, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return "", fmt.Errorf("query is required")
	}
	limit := 10
	if l, ok := args["limit"].(int); ok {
		limit = l
	}

	if t.searchFn == nil {
		return "", fmt.Errorf("session search not initialized")
	}

	raw, err := t.searchFn(query, limit)
	if err != nil {
		return "", fmt.Errorf("search failed: %w", err)
	}

	sessions, ok := raw.([]interface{})
	if !ok {
		return "", fmt.Errorf("unexpected result type: %T", raw)
	}

	if len(sessions) == 0 {
		return fmt.Sprintf("No results found for: %s", query), nil
	}

	var out strings.Builder
	out.WriteString(fmt.Sprintf("Found %d results for: %s\n\n", len(sessions), query))
	for i, s := range sessions {
		var sessionID, content, summary string
		switch v := s.(type) {
		case map[string]interface{}:
			if v["session_id"] != nil {
				sessionID = v["session_id"].(string)
			}
			if v["content"] != nil {
				content = v["content"].(string)
			}
			if v["summary"] != nil {
				summary = v["summary"].(string)
			}
		}
		snippet := summary
		if snippet == "" && content != "" {
			if len(content) > 120 {
				snippet = content[:120] + "..."
			} else {
				snippet = content
			}
		}
		out.WriteString(fmt.Sprintf("%d. SessionID: %s\n   %s\n\n", i+1, sessionID, snippet))
	}
	return out.String(), nil
}
