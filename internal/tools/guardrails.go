package tools

import (
	"context"
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

// planGuardrailRevision is optional because unit and legacy plan projections
// have no durable evidence ledger. The production projection supplies a
// revision that changes when a plan or successful verification changes.
type planGuardrailRevision interface {
	GuardrailRevision(context.Context, []string) (string, error)
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
		if toolName == "terminal" {
			command := stringArg(args, "command")
			if _, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), ""); err != nil {
				return "", newStableToolRecoveryError(err, "command_syntax", "syntax", "Command has invalid shell syntax; nothing was executed.", "Correct the syntax before requesting execution; keep the target and intended effect unchanged.", "preparation", "corrected_input", "not_dispatched", false)
			}
		}

		if reason := activeTurnPollingReason(toolName, args); reason != "" {
			return "", guardrailRefusal("active_turn_polling",
				fmt.Sprintf("%s; choose a supported durable watch_external check, provider-native wait, or one bounded status observation; if none is available, park with an actionable blocker", reason),
				"watch_external", "provider_native_wait", "bounded_status_observation", "report_actionable_blocker")
		}
		runID := guardrailRunID(args)
		key := guardrailKey(runID, toolName, args)
		if toolName == "update_plan" {
			if projection, ok := runPlanProjectionFromArgs(args).(planGuardrailRevision); ok {
				var completedStepIDs []string
				if steps, parseErr := planStepsFromArgs(args["plan"]); parseErr == nil {
					for _, step := range steps {
						if step.Status == "completed" && step.StepID != "" {
							completedStepIDs = append(completedStepIDs, step.StepID)
						}
					}
				}
				revision, err := projection.GuardrailRevision(ContextFromArgs(args), completedStepIDs)
				if err != nil {
					return "", err
				}
				key += "|" + revision
			}
		}

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
	// These calls are deliberately redirected by runtime policy before dispatch.
	// They are not malformed provider tool calls; counting them as protocol
	// failures makes model comparisons and the daily failure rate misleading.
	return newStableToolRecoveryError(errors.New(message), code, "policy_redirect", message,
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
	return isRemoteObservationCommand(command)
}

func activeTurnPollingReason(toolName string, args map[string]interface{}) string {
	if toolName != "terminal" && toolName != "run_command" && toolName != "execute_command" {
		return ""
	}
	command, _ := args["command"].(string)
	if !isRemoteObservationCommand(command) || !containsPollingLoop(command) {
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
		// A finite list can still repeat the SAME observation until its state
		// changes. A read of each distinct item is a batch, even with pacing.
		return loopHasRemoteObservation(clause.Do) && !loopObservesIterator(clause)
	}
	return stmtsWait(clause.Do)
}

func loopHasRemoteObservation(stmts []*syntax.Stmt) bool {
	found := false
	for _, stmt := range stmts {
		syntax.Walk(stmt, func(node syntax.Node) bool {
			if call, ok := node.(*syntax.CallExpr); ok && remoteObservationCall(call) {
				found = true
				return false
			}
			return !found
		})
		if found {
			return true
		}
	}
	return false
}

func loopObservesIterator(clause *syntax.ForClause) bool {
	iter, ok := clause.Loop.(*syntax.WordIter)
	if !ok || iter.Name == nil || iter.Name.Value == "" {
		return false
	}
	found := false
	allDistinct := true
	for _, stmt := range clause.Do {
		syntax.Walk(stmt, func(node syntax.Node) bool {
			call, ok := node.(*syntax.CallExpr)
			if !ok || !remoteObservationCall(call) {
				return true
			}
			found = true
			usesIterator := false
			syntax.Walk(call, func(part syntax.Node) bool {
				if param, ok := part.(*syntax.ParamExp); ok && param.Param != nil && param.Param.Value == iter.Name.Value {
					usesIterator = true
				}
				return true
			})
			if !usesIterator {
				allDistinct = false
			}
			return false
		})
	}
	return found && allDistinct
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

func isRemoteObservationCommand(command string) bool {
	return remoteObservationCommandDepth(command, 0)
}

func remoteObservationCommandDepth(command string, depth int) bool {
	if depth > 4 {
		return false
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil || file == nil {
		return false
	}
	found := false
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || found {
			return !found
		}
		if nested, ok := nestedShellCommand(call); ok && remoteObservationCommandDepth(nested, depth+1) {
			found = true
			return false
		}
		if remoteObservationCall(call) {
			found = true
			return false
		}
		return true
	})
	return found
}

func remoteObservationCall(call *syntax.CallExpr) bool {
	if call == nil || len(call.Args) == 0 {
		return false
	}
	words := make([]string, 0, len(call.Args))
	for _, arg := range call.Args {
		value, static := staticObservationWord(arg)
		if !static {
			value = "__dynamic__"
		}
		words = append(words, value)
	}
	// These wrappers alter invocation mechanics, not the observation's target.
	// Only known forms are unwrapped; unknown options remain unclassified.
unwrap:
	for depth := 0; depth < 3 && len(words) > 0; depth++ {
		wrapper := strings.ToLower(filepath.Base(words[0]))
		switch wrapper {
		case "command", "timeout":
			inner, ok := observationWrappedCommand(wrapper, words[1:])
			if !ok {
				return false
			}
			words = inner
		case "env":
			inner, ok := externalObservationEnvCommand(words[1:])
			if !ok {
				return false
			}
			words = inner
		default:
			break unwrap
		}
	}
	if len(words) == 0 {
		return false
	}
	program := strings.ToLower(filepath.Base(words[0]))
	args, ok := observationCommandArgs(program, words[1:])
	if !ok {
		return false
	}
	if rule, ok := observationRuleByProgram[program]; ok && rule.external && rule.matches(args) && (rule.verify == nil || rule.verify(args)) {
		return true
	}
	rule, ok := pollingObservationRules[program]
	return ok && rule.matches(args) && (rule.verify == nil || rule.verify(args))
}

// pollingObservationRules recognizes external reads for this guard only. The
// guard restricts, so a form listed here never widens what runs without
// approval. Provider-native waits block until a condition holds and stay out of
// the approval catalog: one wait is the recommended alternative, a loop around
// it is polling. AWS and Azure reads follow their CLIs' naming conventions,
// while the catalog approves their operations one reviewed form at a time.
var pollingObservationRules = map[string]observationRule{
	"kubectl": {prefixes: [][]string{{"rollout", "status"}, {"wait"}}},
	"argocd":  {prefixes: [][]string{{"app", "wait"}}},
	"gh":      {prefixes: [][]string{{"run", "watch"}, {"pr", "checks"}}},
	"aws":     {anyArgs: true, verify: awsReadOperation},
	"az":      {anyArgs: true, verify: azReadOperation},
}

// awsReadOperation accepts an operation named after a Describe, Get, List or
// BatchGet API action, or a built-in waiter.
func awsReadOperation(args []string) bool {
	words := leadingCommandWords(args)
	if len(words) < 2 {
		return false
	}
	operation := strings.ToLower(words[1])
	return operation == "wait" || strings.HasPrefix(operation, "describe-") || strings.HasPrefix(operation, "get-") ||
		strings.HasPrefix(operation, "list-") || strings.HasPrefix(operation, "batch-get-")
}

// azReadOperation accepts a command whose final word before its arguments is a
// read verb, as in `az deployment group show`.
func azReadOperation(args []string) bool {
	words := leadingCommandWords(args)
	if len(words) < 2 {
		return false
	}
	switch strings.ToLower(words[len(words)-1]) {
	case "show", "list", "wait":
		return true
	}
	return false
}

func leadingCommandWords(args []string) []string {
	for i, arg := range args {
		if strings.HasPrefix(arg, "-") {
			return args[:i]
		}
	}
	return args
}

func externalObservationEnvCommand(args []string) ([]string, bool) {
	for len(args) > 0 {
		switch args[0] {
		case "-u", "--unset":
			if len(args) < 3 || args[1] == "" {
				return nil, false
			}
			args = args[2:]
		case "-i", "--ignore-environment", "--":
			args = args[1:]
		default:
			if strings.HasPrefix(args[0], "--unset=") || (strings.Contains(args[0], "=") && !strings.HasPrefix(args[0], "-")) {
				args = args[1:]
				continue
			}
			if strings.HasPrefix(args[0], "-") {
				return nil, false
			}
			return args, true
		}
	}
	return nil, false
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
