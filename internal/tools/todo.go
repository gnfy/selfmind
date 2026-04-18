package tools

import (
	"encoding/json"
	"fmt"
)

// =============================================================================
// Todo Tool
// =============================================================================

// TodoTool 任务管理工具
type TodoTool struct {
	BaseTool
	store *TodoStore
}

type TodoItem struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"` // pending, in_progress, completed, cancelled
}

type TodoStore struct {
	items map[string]TodoItem
}

func NewTodoStore() *TodoStore {
	return &TodoStore{items: make(map[string]TodoItem)}
}

func (s *TodoStore) Set(todos []TodoItem) {
	s.items = make(map[string]TodoItem)
	for _, t := range todos {
		s.items[t.ID] = t
	}
}

func (s *TodoStore) Get() []TodoItem {
	result := make([]TodoItem, 0, len(s.items))
	for _, t := range s.items {
		result = append(result, t)
	}
	return result
}

func NewTodoTool() *TodoTool {
	return &TodoTool{
		BaseTool: BaseTool{
			name:        "todo",
			description: "管理任务列表",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"todos": {
						Type:        "string",
						Description: "JSON 序列化的任务列表（用于写入）；省略则返回当前列表",
					},
				},
				Required: []string{},
			},
		},
		store: NewTodoStore(),
	}
}

func (t *TodoTool) Execute(args map[string]interface{}) (string, error) {
	if todosJSON, ok := args["todos"].(string); ok && todosJSON != "" {
		var todos []TodoItem
		if err := json.Unmarshal([]byte(todosJSON), &todos); err != nil {
			return "", fmt.Errorf("invalid todos JSON: %w", err)
		}
		t.store.Set(todos)
	}

	items := t.store.Get()
	data, _ := json.MarshalIndent(items, "", "  ")
	return string(data), nil
}
