package tools

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaxChoices is the maximum number of predefined choices (a 5th "Other" option is always appended by the UI)
const MaxChoices = 4

// ClarifyFn is the platform-provided function that handles user interaction.
// Set by RegisterClarifyCallback() called from the controller's Start().
var ClarifyFn func(question string, choices []string) string

// ClarifyTool asks the user a question with optional multiple-choice answers.
type ClarifyTool struct {
	BaseTool
}

func NewClarifyTool() *ClarifyTool {
	return &ClarifyTool{
		BaseTool: BaseTool{
			name:        "clarify",
			description: "Ask the user a question when you need clarification, feedback, or a decision before proceeding. Supports two modes:\n\n1. **Multiple choice** — provide up to 4 choices. The user picks one or types their own answer via a 5th 'Other' option.\n2. **Open-ended** — omit choices entirely. The user types a free-form response.\n\nUse this tool when:\n- The task is ambiguous and you need the user to choose an approach\n- You want post-task feedback ('How did that work out?')\n- A decision has meaningful trade-offs the user should weigh in on\n\nDo NOT use this tool for simple yes/no confirmation of dangerous commands (the terminal tool handles that). Prefer making a reasonable default choice yourself when the decision is low-stakes.",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"question": {
						Type:        "string",
						Description: "The question to present to the user.",
					},
				},
				Required: []string{"question"},
			},
		},
	}
}

func (t *ClarifyTool) Execute(args map[string]interface{}) (string, error) {
	question, ok := args["question"].(string)
	if !ok || strings.TrimSpace(question) == "" {
		return "", fmt.Errorf("question text is required")
	}
	question = strings.TrimSpace(question)

	var choices []string
	if c, ok := args["choices"].([]interface{}); ok && c != nil {
		for _, v := range c {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				choices = append(choices, strings.TrimSpace(s))
			}
		}
		if len(choices) > MaxChoices {
			choices = choices[:MaxChoices]
		}
		if len(choices) == 0 {
			choices = nil
		}
	}

	if ClarifyFn == nil {
		return "", fmt.Errorf("clarify tool is not available in this execution context")
	}

	userResponse := ClarifyFn(question, choices)

	result, err := json.Marshal(map[string]interface{}{
		"question":       question,
		"choices_offered": choices,
		"user_response":  strings.TrimSpace(userResponse),
	})
	if err != nil {
		return "", err
	}
	return string(result), nil
}
