package tools

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

type ToolGuardrails struct {
	mu      sync.Mutex
	records map[string]toolGuardrailRecord
}

type toolGuardrailRecord struct {
	Failures       int
	SameResults    int
	LastErrorHash  string
	LastResultHash string
	UpdatedAt      time.Time
}

func NewToolGuardrails() *ToolGuardrails {
	return &ToolGuardrails{records: map[string]toolGuardrailRecord{}}
}

func (g *ToolGuardrails) Middleware(next ToolExecutor) ToolExecutor {
	return func(args map[string]interface{}) (string, error) {
		if g == nil {
			return next(args)
		}
		toolName, _ := args["_tool_name"].(string)
		runID := guardrailRunID(args)
		key := guardrailKey(runID, toolName, args)

		g.mu.Lock()
		rec := g.records[key]
		if rec.Failures >= 2 {
			g.mu.Unlock()
			return "", fmt.Errorf("tool guardrail blocked repeated failure for %s; change arguments or explain why retrying is necessary", toolName)
		}
		if idempotentTool(toolName) && rec.SameResults >= 3 {
			g.mu.Unlock()
			return "", fmt.Errorf("tool guardrail blocked no-progress loop for %s; use the existing result or try a different approach", toolName)
		}
		g.mu.Unlock()

		result, err := next(args)
		g.record(key, toolName, result, err)
		return result, err
	}
}

func (g *ToolGuardrails) record(key, toolName, result string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	rec := g.records[key]
	rec.UpdatedAt = time.Now()
	if err != nil {
		hash := hashString(RedactSensitive(err.Error()))
		if rec.LastErrorHash == hash {
			rec.Failures++
		} else {
			rec.Failures = 1
			rec.LastErrorHash = hash
		}
		g.records[key] = rec
		g.sweepLocked()
		return
	}
	rec.Failures = 0
	hash := hashString(strings.TrimSpace(result))
	if idempotentTool(toolName) && rec.LastResultHash == hash {
		rec.SameResults++
	} else {
		rec.SameResults = 1
		rec.LastResultHash = hash
	}
	g.records[key] = rec
	g.sweepLocked()
}

func (g *ToolGuardrails) sweepLocked() {
	if len(g.records) < 1000 {
		return
	}
	cutoff := time.Now().Add(-30 * time.Minute)
	for key, rec := range g.records {
		if rec.UpdatedAt.Before(cutoff) {
			delete(g.records, key)
		}
	}
}

func guardrailRunID(args map[string]interface{}) string {
	if scope, ok := currentExecutionScope(args); ok && scope.RunID != "" {
		return scope.RunID
	}
	tenantID, _ := args["_tenant_id"].(string)
	if tenantID == "" {
		tenantID = "default"
	}
	return tenantID
}

func guardrailKey(runID, toolName string, args map[string]interface{}) string {
	clean := map[string]interface{}{}
	for k, v := range args {
		if strings.HasPrefix(k, "_") {
			continue
		}
		clean[k] = v
	}
	data, _ := json.Marshal(clean)
	return runID + "|" + toolName + "|" + hashString(string(data))
}

func hashString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum[:])
}

func idempotentTool(name string) bool {
	switch name {
	case "read_file", "list_files", "ls_r", "search_files", "grep", "session_search",
		"web_search", "web_extract", "get_current_time", "process_list", "process_poll":
		return true
	default:
		return false
	}
}
