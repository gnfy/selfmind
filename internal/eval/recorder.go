package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/tools"
)

type JSONLEvent struct {
	Time            string                 `json:"time"`
	Type            string                 `json:"type"`
	CaseID          string                 `json:"case_id,omitempty"`
	TurnIndex       int                    `json:"turn_index,omitempty"`
	Channel         string                 `json:"channel,omitempty"`
	Provider        string                 `json:"provider,omitempty"`
	Model           string                 `json:"model,omitempty"`
	Workspace       string                 `json:"workspace,omitempty"`
	Status          string                 `json:"status,omitempty"`
	HTTPStatus      int                    `json:"http_status,omitempty"`
	DurationMS      int64                  `json:"duration_ms,omitempty"`
	ElapsedMS       int64                  `json:"elapsed_ms,omitempty"`
	Tool            string                 `json:"tool,omitempty"`
	ToolCallID      string                 `json:"tool_call_id,omitempty"`
	OK              *bool                  `json:"ok,omitempty"`
	Error           string                 `json:"error,omitempty"`
	ErrorCategory   string                 `json:"error_category,omitempty"`
	Message         string                 `json:"message,omitempty"`
	Chars           int                    `json:"chars,omitempty"`
	InputHash       string                 `json:"input_hash,omitempty"`
	InputPreview    string                 `json:"input_preview,omitempty"`
	OutputPreview   string                 `json:"output_preview,omitempty"`
	InputTokens     int                    `json:"input_tokens,omitempty"`
	OutputTokens    int                    `json:"output_tokens,omitempty"`
	ToolCalls       int                    `json:"tool_calls,omitempty"`
	ActionToolCalls int                    `json:"action_tool_calls,omitempty"`
	ToolErrors      int                    `json:"tool_errors,omitempty"`
	CheckResults    []CheckResult          `json:"check_results,omitempty"`
	Metadata        map[string]interface{} `json:"metadata,omitempty"`
}

type Recorder struct {
	mu              sync.Mutex
	file            *os.File
	caseID          string
	start           time.Time
	turnStart       time.Time
	firstToken      bool
	output          strings.Builder
	toolCalls       int
	actionToolCalls int
	toolErrors      int
	errors          []string
	errorCategory   map[string]int
	recordContent   bool
}

func NewRecorder(path string, recordContent bool) (*Recorder, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("output path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &Recorder{file: f, recordContent: recordContent, errorCategory: map[string]int{}}, nil
}

func (r *Recorder) Close() error {
	if r == nil || r.file == nil {
		return nil
	}
	return r.file.Close()
}

func (r *Recorder) StartCase(c *Case, provider, model, workspace string) {
	r.mu.Lock()
	r.caseID = c.ID
	r.start = time.Now()
	r.output.Reset()
	r.toolCalls = 0
	r.actionToolCalls = 0
	r.toolErrors = 0
	r.errors = nil
	r.errorCategory = map[string]int{}
	r.mu.Unlock()
	r.Write(JSONLEvent{
		Type:      "case_started",
		CaseID:    c.ID,
		Provider:  provider,
		Model:     model,
		Workspace: workspace,
		Message:   c.Title,
	})
}

func (r *Recorder) StartTurn(index int, input, channel string) {
	r.mu.Lock()
	r.turnStart = time.Now()
	r.firstToken = false
	r.mu.Unlock()
	r.Write(JSONLEvent{
		Type:         "turn_started",
		CaseID:       r.caseID,
		TurnIndex:    index,
		Channel:      channel,
		InputHash:    hashText(input),
		InputPreview: preview(input, 180),
	})
}

func (r *Recorder) ObserveStreamEvent(event llm.StreamEvent) {
	if r == nil {
		return
	}
	switch event.EventType {
	case "stream":
		if event.Content != "" {
			r.mu.Lock()
			if !r.firstToken {
				r.firstToken = true
				elapsed := time.Since(r.turnStart).Milliseconds()
				r.mu.Unlock()
				r.Write(JSONLEvent{Type: "model_stream_first_token", CaseID: r.caseID, ElapsedMS: elapsed})
				r.mu.Lock()
			}
			r.output.WriteString(event.Content)
			r.mu.Unlock()
		}
	case "tool.started":
		r.mu.Lock()
		r.toolCalls++
		if !isLifecycleTool(event.ToolName) {
			r.actionToolCalls++
		}
		r.mu.Unlock()
		r.Write(JSONLEvent{
			Type:       "tool_started",
			CaseID:     r.caseID,
			Tool:       event.ToolName,
			ToolCallID: event.ToolCallID,
			Message:    tools.RedactSensitive(preview(event.ToolArgs, 300)),
		})
	case "tool.completed":
		ok := event.Err == nil
		e := JSONLEvent{
			Type:       "tool_finished",
			CaseID:     r.caseID,
			Tool:       event.ToolName,
			ToolCallID: event.ToolCallID,
			OK:         &ok,
			DurationMS: int64(event.DurationSeconds * 1000),
			Message:    tools.RedactSensitive(preview(event.ToolResult, 500)),
		}
		if event.Err != nil {
			e.Type = "tool_failed"
			e.Error = tools.RedactSensitive(event.Err.Error())
			e.ErrorCategory = classifyError(event.Err.Error())
			if cat, ok := event.Payload["error_category"].(string); ok && strings.TrimSpace(cat) != "" {
				e.ErrorCategory = strings.TrimSpace(cat)
			}
			if event.Payload != nil {
				e.Metadata = event.Payload
			}
			r.recordError(e.Error, e.ErrorCategory)
		}
		r.Write(e)
	case "token.updated":
		if event.Payload != nil {
			r.Write(JSONLEvent{Type: "token_updated", CaseID: r.caseID, Metadata: event.Payload})
		}
	default:
		if event.Err != nil {
			msg := event.Err.Error()
			cat := classifyError(msg)
			r.recordError(msg, cat)
			r.Write(JSONLEvent{Type: "stream_error", CaseID: r.caseID, Error: tools.RedactSensitive(msg), ErrorCategory: cat})
			return
		}
		if event.EventType != "" && event.EventType != "tool.heartbeat" {
			r.Write(JSONLEvent{
				Type:       event.EventType,
				CaseID:     r.caseID,
				Tool:       event.ToolName,
				ToolCallID: event.ToolCallID,
				Message:    tools.RedactSensitive(preview(event.Content, 500)),
				Metadata:   event.Payload,
			})
		}
	}
}

func (r *Recorder) FinishTurn(index, httpStatus int, content, errText string, inputTokens, outputTokens int, started time.Time) {
	if content != "" {
		r.mu.Lock()
		current := r.output.String()
		if !strings.Contains(current, content) {
			if r.output.Len() > 0 {
				r.output.WriteString("\n")
			}
			r.output.WriteString(content)
		}
		r.mu.Unlock()
	}
	e := JSONLEvent{
		Type:          "turn_finished",
		CaseID:        r.caseID,
		TurnIndex:     index,
		HTTPStatus:    httpStatus,
		DurationMS:    time.Since(started).Milliseconds(),
		Chars:         len([]rune(content)),
		OutputPreview: preview(content, 400),
		InputTokens:   inputTokens,
		OutputTokens:  outputTokens,
	}
	if errText != "" {
		e.Status = "failed"
		e.Error = tools.RedactSensitive(errText)
		e.ErrorCategory = classifyError(errText)
		r.recordError(e.Error, e.ErrorCategory)
	} else {
		e.Status = "completed"
	}
	if r.recordContent {
		e.Metadata = map[string]interface{}{"content": content}
	}
	r.Write(e)
}

func (r *Recorder) FinishCase(status string, checks []CheckResult, inputTokens, outputTokens int) {
	r.mu.Lock()
	toolCalls := r.toolCalls
	actionToolCalls := r.actionToolCalls
	toolErrors := r.toolErrors
	duration := time.Since(r.start).Milliseconds()
	r.mu.Unlock()
	r.Write(JSONLEvent{
		Type:            "case_finished",
		CaseID:          r.caseID,
		Status:          status,
		DurationMS:      duration,
		InputTokens:     inputTokens,
		OutputTokens:    outputTokens,
		ToolCalls:       toolCalls,
		ActionToolCalls: actionToolCalls,
		ToolErrors:      toolErrors,
		CheckResults:    checks,
	})
}

func (r *Recorder) Snapshot() RunSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	cats := make(map[string]int, len(r.errorCategory))
	for k, v := range r.errorCategory {
		cats[k] = v
	}
	return RunSnapshot{
		Output:          r.output.String(),
		ToolCalls:       r.toolCalls,
		ActionToolCalls: r.actionToolCalls,
		ToolErrors:      r.toolErrors,
		Errors:          append([]string(nil), r.errors...),
		ErrorCategories: cats,
	}
}

func isLifecycleTool(name string) bool {
	switch strings.TrimSpace(name) {
	case "finish_run", "update_plan":
		return true
	default:
		return false
	}
}

func (r *Recorder) Write(event JSONLEvent) {
	if r == nil || r.file == nil {
		return
	}
	event.Time = time.Now().Format(time.RFC3339Nano)
	if event.CaseID == "" {
		event.CaseID = r.caseID
	}
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, _ = r.file.Write(append(data, '\n'))
}

func (r *Recorder) recordError(message, category string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.toolErrors++
	r.errors = append(r.errors, message)
	if category == "" {
		category = "unknown"
	}
	r.errorCategory[category]++
}
