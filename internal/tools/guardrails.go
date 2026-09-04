package tools

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
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
			return "", guardrailRefusal("active_turn_polling",
				fmt.Sprintf("%s; choose a supported durable watch_external check, provider-native wait, or one bounded status observation; if none is available, park with an actionable blocker", reason),
				"watch_external", "provider_native_wait", "bounded_status_observation", "report_actionable_blocker")
		}
		runID := guardrailRunID(args)
		key := guardrailKey(runID, toolName, args)

		g.mu.Lock()
		rec := g.records[key]
		if rec.Failures >= 2 {
			g.mu.Unlock()
			return "", guardrailRefusal("repeated_failure",
				fmt.Sprintf("tool guardrail blocked repeated failure for %s; change arguments or explain why retrying is necessary", toolName),
				"inspect_current_state", "change_strategy", "report_actionable_blocker")
		}
		if noProgressToolCall(toolName, args) && rec.SameResults >= 3 {
			g.mu.Unlock()
			return "", guardrailRefusal("no_progress_check",
				fmt.Sprintf("tool guardrail blocked a repeated no-progress check for %s; use the existing result, choose a supported watch_external or provider-native wait, or park with an actionable blocker", toolName),
				"use_existing_result", "watch_external", "report_actionable_blocker")
		}
		g.mu.Unlock()

		result, err := next(args)
		g.record(key, toolName, args, result, err)
		return result, err
	}
}

// guardrailRefusal is a typed, not-dispatched refusal: the tool never ran, so
// the kernel recovery policy must not count it as a failed strategy attempt,
// and the model receives the same structured alternatives as a policy refusal.
func guardrailRefusal(code, message string, alternatives ...string) error {
	return newStableToolRecoveryError(errors.New(message), code, "blocked_model_protocol", message,
		"Use the typed alternatives or finish with an actionable blocker; do not retry a cosmetic variant.",
		"planning", "different_strategy", "not_dispatched", false, alternatives...)
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
	return containsPollingLoopDepth(command, 0)
}

func containsPollingLoopDepth(command string, depth int) bool {
	if depth > 4 {
		return false
	}
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
			activeWait = forClauseWaits(value)
		case *syntax.CallExpr:
			if len(value.Args) == 0 {
				return true
			}
			name, ok := staticObservationWord(value.Args[0])
			activeWait = ok && strings.EqualFold(filepath.Base(name), "watch")
			if !activeWait {
				if nested, ok := nestedShellCommand(value); ok {
					activeWait = containsPollingLoopDepth(nested, depth+1)
				}
			}
		}
		return !activeWait
	})
	return activeWait
}

// nestedShellCommand extracts a static `sh -c ...` body through common
// process wrappers. The shell parser sees a quoted `bash -c` body as one word,
// not as a nested AST; without this second bounded parse, `nohup bash -c
// 'while ...' &` bypasses the active-turn polling guard while doing exactly
// the same work as a top-level loop.
func nestedShellCommand(call *syntax.CallExpr) (string, bool) {
	if call == nil || len(call.Args) < 3 {
		return "", false
	}
	words := make([]string, len(call.Args))
	for i, arg := range call.Args {
		word, ok := staticObservationWord(arg)
		if !ok {
			return "", false
		}
		words[i] = word
	}
	wrappers := map[string]bool{
		"command": true, "env": true, "nohup": true, "setsid": true,
		"timeout": true, "nice": true, "ionice": true, "stdbuf": true,
	}
	for i := 0; i+2 < len(words); i++ {
		name := strings.ToLower(filepath.Base(words[i]))
		if name != "sh" && name != "bash" && name != "dash" && name != "ksh" && name != "zsh" {
			continue
		}
		if i > 0 && !wrappers[strings.ToLower(filepath.Base(words[0]))] {
			continue
		}
		flags := strings.TrimPrefix(words[i+1], "-")
		if !strings.Contains(flags, "c") || strings.TrimSpace(words[i+2]) == "" {
			continue
		}
		return words[i+2], true
	}
	return "", false
}

// forClauseWaits separates an active wait loop from a bounded fan-out. A
// literal item list is finite by construction. A dynamic list (`for id in
// $ids`) is still one read per item unless its body sleeps, watches, or nests
// another wait loop.
func forClauseWaits(clause *syntax.ForClause) bool {
	if clause == nil {
		return false
	}
	if clause.Select {
		return true
	}
	if _, cStyle := clause.Loop.(*syntax.CStyleLoop); cStyle {
		return true
	}
	if finiteLiteralForLoop(clause) {
		return false
	}
	return stmtsWait(clause.Do)
}

func stmtsWait(stmts []*syntax.Stmt) bool {
	waits := false
	for _, stmt := range stmts {
		syntax.Walk(stmt, func(node syntax.Node) bool {
			if node == nil || waits {
				return false
			}
			switch value := node.(type) {
			case *syntax.WhileClause:
				waits = true
			case *syntax.ForClause:
				if value.Select {
					waits = true
				} else if _, cStyle := value.Loop.(*syntax.CStyleLoop); cStyle {
					waits = true
				}
			case *syntax.CallExpr:
				if len(value.Args) > 0 {
					if name, ok := staticObservationWord(value.Args[0]); ok {
						switch strings.ToLower(filepath.Base(name)) {
						case "sleep", "watch":
							waits = true
						}
					}
				}
			}
			return !waits
		})
		if waits {
			return true
		}
	}
	return false
}

// finiteLiteralForLoop distinguishes a bounded batch such as
// `for id in a b; do aws ...; done` from dynamic loops. Array length is capped
// so a generated giant literal list cannot occupy an agent turn indefinitely.
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
	case "read_file", "list_files", "ls_r", "search_files", "grep", "session_search", "work_search", "work_inspect",
		"web_search", "web_extract", "get_current_time", "process_list", "process_poll":
		return true
	default:
		return false
	}
}
