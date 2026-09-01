package tools

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mvdan.cc/sh/v3/syntax"
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
		if reason := activeTurnPollingReason(toolName, args); reason != "" {
			return "", fmt.Errorf("%s; choose a supported durable watch_external check, provider-native wait, or one bounded status observation; if none is available, park with an actionable blocker", reason)
		}
		runID := guardrailRunID(args)
		key := guardrailKey(runID, toolName, args)

		g.mu.Lock()
		rec := g.records[key]
		if rec.Failures >= 2 {
			g.mu.Unlock()
			return "", fmt.Errorf("tool guardrail blocked repeated failure for %s; change arguments or explain why retrying is necessary", toolName)
		}
		if noProgressToolCall(toolName, args) && rec.SameResults >= 3 {
			g.mu.Unlock()
			return "", fmt.Errorf("tool guardrail blocked a repeated no-progress check for %s; use the existing result, choose a supported watch_external or provider-native wait, or park with an actionable blocker", toolName)
		}
		g.mu.Unlock()

		result, err := next(args)
		g.record(key, toolName, args, result, err)
		return result, err
	}
}

func (g *ToolGuardrails) record(key, toolName string, args map[string]interface{}, result string, err error) {
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
	if noProgressToolCall(toolName, args) && rec.LastResultHash == hash {
		rec.SameResults++
	} else {
		rec.SameResults = 1
		rec.LastResultHash = hash
	}
	g.records[key] = rec
	g.sweepLocked()
}

func noProgressToolCall(toolName string, args map[string]interface{}) bool {
	if idempotentTool(toolName) {
		return true
	}
	if toolName != "terminal" && toolName != "run_command" && toolName != "execute_command" {
		return false
	}
	if args == nil {
		return true
	}
	command, _ := args["command"].(string)
	return isRemoteStatusCommand(command)
}

func activeTurnPollingReason(toolName string, args map[string]interface{}) string {
	if toolName != "terminal" && toolName != "run_command" && toolName != "execute_command" {
		return ""
	}
	command, _ := args["command"].(string)
	if !isRemoteStatusCommand(command) || !containsPollingLoop(command) {
		return ""
	}
	return "tool guardrail blocked active-turn polling of external state"
}

func containsPollingLoop(command string) bool {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil || file == nil {
		// This guard prevents token-burning wait loops, not unsafe execution.
		// On parse failure only reject constructs that are independently strong
		// evidence of an unbounded wait; the normal safety middleware still owns
		// whether the command may execute.
		normalized := " " + strings.ToLower(strings.Join(strings.Fields(command), " ")) + " "
		return strings.Contains(normalized, " while ") || strings.Contains(normalized, " until ") ||
			strings.Contains(normalized, " watch ") || strings.Contains(normalized, " for ((")
	}
	activeWait := false
	syntax.Walk(file, func(node syntax.Node) bool {
		if node == nil || activeWait {
			return false
		}
		switch value := node.(type) {
		case *syntax.WhileClause:
			activeWait = true
		case *syntax.ForClause:
			activeWait = !finiteLiteralForLoop(value)
		case *syntax.CallExpr:
			if len(value.Args) == 0 {
				return true
			}
			name, ok := staticObservationWord(value.Args[0])
			activeWait = ok && strings.EqualFold(filepath.Base(name), "watch")
		}
		return !activeWait
	})
	return activeWait
}

// finiteLiteralForLoop distinguishes a bounded batch such as
// `for id in a b; do aws ...; done` from C-style, positional, dynamic, or
// interactive loops. Array length is capped so a generated giant literal list
// cannot occupy an agent turn indefinitely.
func finiteLiteralForLoop(clause *syntax.ForClause) bool {
	if clause == nil || clause.Select {
		return false
	}
	iter, ok := clause.Loop.(*syntax.WordIter)
	if !ok || !iter.InPos.IsValid() || len(iter.Items) > 100 {
		return false
	}
	for _, item := range iter.Items {
		if _, ok := staticObservationWord(item); !ok {
			return false
		}
	}
	return true
}

func isRemoteStatusCommand(command string) bool {
	normalized := strings.ToLower(strings.Join(strings.Fields(command), " "))
	for _, pattern := range []string{
		"gcloud builds describe", "gcloud builds list",
		"argocd app get", "argocd app wait",
		"kubectl get", "kubectl wait", "kubectl rollout status",
		"gh run view", "gh run watch", "gh run list",
		"aws codebuild batch-get-builds", "aws cloudformation describe-stacks",
		"az deployment show",
	} {
		if strings.Contains(normalized, pattern) {
			return true
		}
	}
	return false
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
