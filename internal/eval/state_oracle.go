package eval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/kernel/memory"
)

// WorldState is a snapshot of the durable state after a scenario runs. State
// predicates evaluate against this snapshot — an oracle the agent cannot game
// the way it can game its own narration (control.db rows / real files / memory).
type WorldState struct {
	Task          *control.Task
	Run           *control.Run
	Handoff       *control.Handoff
	Events        []control.Event
	Artifacts     []control.Artifact
	Approvals     []control.ApprovalRequest
	Facts         map[string][]memory.Fact // keyed by target
	WorkspaceRoot string
	subjectTaskID string
	subjectRunID  string
}

// CollectWorldState pulls the post-run state once so predicates evaluate
// against an in-memory snapshot and can report the actual value on failure.
func CollectWorldState(ctx context.Context, store *control.Store, mem *memory.MemoryManager, identity *control.IdentityContext, subjectTaskID, subjectRunID, workspaceRoot string) WorldState {
	w := WorldState{WorkspaceRoot: workspaceRoot, subjectTaskID: subjectTaskID, subjectRunID: subjectRunID, Facts: map[string][]memory.Fact{}}
	if store == nil || identity == nil {
		return w
	}
	if subjectTaskID != "" {
		w.Task, _ = store.GetTask(ctx, identity.TenantID, subjectTaskID)
		w.Handoff, _ = store.LatestHandoff(ctx, subjectTaskID)
		w.Events, _ = store.ListTaskEvents(ctx, subjectTaskID, 200)
		w.Artifacts, _ = store.ListTaskArtifacts(ctx, subjectTaskID, 50)
	}
	if subjectRunID != "" {
		w.Run, _ = store.GetRun(ctx, identity.TenantID, subjectRunID)
	} else if subjectTaskID != "" {
		if runs, err := store.ListTaskRuns(ctx, identity.TenantID, subjectTaskID, 1); err == nil && len(runs) > 0 {
			w.Run = &runs[0]
			w.subjectRunID = runs[0].ID
		}
	}
	w.Approvals, _ = store.ListApprovalRequests(ctx, identity.TenantID, identity.PersonID, "", 50)
	if mem != nil {
		// Person memory is person-partitioned; the tenant partition is only a
		// fallback for degenerate identities (mirrors production reads).
		partition := strings.TrimSpace(identity.PersonID)
		if partition == "" {
			partition = identity.TenantID
		}
		for _, target := range []string{"user", "memory"} {
			if facts, err := mem.GetFacts(ctx, partition, target); err == nil {
				w.Facts[target] = facts
			}
		}
	}
	return w
}

// EvaluateStatePredicates runs each predicate against the world snapshot and
// returns CheckResults (same type the deterministic checks use).
func EvaluateStatePredicates(preds []StatePredicate, w WorldState) []CheckResult {
	var out []CheckResult
	for _, p := range preds {
		out = append(out, evalPredicate(p, w))
	}
	return out
}

func evalPredicate(p StatePredicate, w WorldState) CheckResult {
	name := "state:" + strings.TrimSpace(p.On)
	if p.Field != "" {
		name += "." + p.Field
	} else if p.Path != "" {
		name += ":" + p.Path
	} else if p.Type != "" {
		name += ":" + p.Type
	}
	ok, msg := dispatchPredicate(p, w)
	score := 0.0
	if ok {
		score = 1.0
	}
	return CheckResult{Name: name, OK: ok, Message: msg, Score: score}
}

func dispatchPredicate(p StatePredicate, w WorldState) (bool, string) {
	switch strings.ToLower(strings.TrimSpace(p.On)) {
	case "task":
		if w.Task == nil {
			return false, "no subject task found"
		}
		return evalFieldedObject(p, taskStringFields(w.Task), taskListFields(w.Task))
	case "handoff":
		if w.Handoff == nil {
			return false, "no handoff found for task"
		}
		return evalFieldedObject(p, handoffStringFields(w.Handoff), handoffListFields(w.Handoff))
	case "events":
		return evalCount(p, countEvents(w.Events, p.Type, p.PayloadContains))
	case "artifact", "artifacts":
		return evalArtifacts(p, w.Artifacts)
	case "approval", "approvals":
		return evalCount(p, countApprovals(w.Approvals, p.Status))
	case "run":
		if w.Run == nil {
			return false, "no subject run found"
		}
		return evalFieldedObject(p, runStringFields(w.Run), nil)
	case "file":
		return evalFile(p, w.WorkspaceRoot)
	case "memory":
		return evalMemory(p, w.Facts[firstNonEmpty(p.Target, "user")])
	default:
		return false, "unknown predicate target: " + p.On
	}
}

// ---- field maps ----

func taskStringFields(t *control.Task) map[string]string {
	return map[string]string{
		"status":          t.Status,
		"current_summary": t.CurrentSummary,
		"blocked_reason":  t.BlockedReason,
		"workspace_id":    t.WorkspaceID,
		"last_channel":    t.LastChannel,
		"title":           t.Title,
		"kind":            t.Kind,
		"visibility":      t.Visibility,
	}
}
func taskListFields(t *control.Task) map[string][]string {
	return map[string][]string{"next_steps": t.NextSteps}
}
func handoffStringFields(h *control.Handoff) map[string]string {
	return map[string]string{"summary": h.Summary, "test_status": h.TestStatus}
}
func handoffListFields(h *control.Handoff) map[string][]string {
	return map[string][]string{
		"done_items":    h.DoneItems,
		"next_steps":    h.NextSteps,
		"changed_files": h.ChangedFiles,
		"risks":         h.Risks,
	}
}
func runStringFields(r *control.Run) map[string]string {
	return map[string]string{
		"status":        r.Status,
		"thread_id":     r.TaskID,
		"parent_run_id": r.ParentRunID,
		"workspace_id":  r.WorkspaceID,
		"input_summary": r.InputSummary,
	}
}

// ---- evaluators ----

// evalFieldedObject handles task/handoff predicates: a string field uses string
// ops; a list field uses len ops and "any element contains" for contains.
func evalFieldedObject(p StatePredicate, strFields map[string]string, listFields map[string][]string) (bool, string) {
	field := strings.TrimSpace(p.Field)
	if list, ok := listFields[field]; ok {
		return evalListValue(p, list)
	}
	if val, ok := strFields[field]; ok {
		return evalStringValue(p, val)
	}
	return false, "unknown field: " + field
}

func evalStringValue(p StatePredicate, actual string) (bool, string) {
	if p.Eq != nil {
		if !strings.EqualFold(strings.TrimSpace(actual), strings.TrimSpace(*p.Eq)) {
			return false, fmt.Sprintf("eq %q, got %q", *p.Eq, actual)
		}
	}
	if p.Contains != nil && !strings.Contains(actual, *p.Contains) {
		return false, fmt.Sprintf("contains %q, got %q", *p.Contains, preview(actual, 80))
	}
	if p.NotContains != nil && strings.Contains(actual, *p.NotContains) {
		return false, fmt.Sprintf("not_contains %q, but present", *p.NotContains)
	}
	if p.Matches != nil {
		re, err := regexp.Compile(*p.Matches)
		if err != nil {
			return false, "invalid matches regex: " + err.Error()
		}
		if !re.MatchString(actual) {
			return false, fmt.Sprintf("matches %q failed", *p.Matches)
		}
	}
	if p.Empty != nil {
		isEmpty := strings.TrimSpace(actual) == ""
		if *p.Empty != isEmpty {
			return false, fmt.Sprintf("empty=%v, got value %q", *p.Empty, preview(actual, 80))
		}
	}
	return true, ""
}

func evalListValue(p StatePredicate, list []string) (bool, string) {
	if p.LenGte != nil && len(list) < *p.LenGte {
		return false, fmt.Sprintf("len_gte %d, got %d", *p.LenGte, len(list))
	}
	if p.LenLte != nil && len(list) > *p.LenLte {
		return false, fmt.Sprintf("len_lte %d, got %d", *p.LenLte, len(list))
	}
	joined := strings.Join(list, "\n")
	if p.Contains != nil && !strings.Contains(joined, *p.Contains) {
		return false, fmt.Sprintf("contains %q, got %v", *p.Contains, list)
	}
	if p.NotContains != nil && strings.Contains(joined, *p.NotContains) {
		return false, fmt.Sprintf("not_contains %q, but present", *p.NotContains)
	}
	return true, ""
}

func evalCount(p StatePredicate, count int) (bool, string) {
	if p.Exists != nil {
		got := count > 0
		if *p.Exists != got {
			return false, fmt.Sprintf("exists=%v, count=%d", *p.Exists, count)
		}
	}
	if p.CountGte != nil && count < *p.CountGte {
		return false, fmt.Sprintf("count_gte %d, got %d", *p.CountGte, count)
	}
	if p.CountLte != nil && count > *p.CountLte {
		return false, fmt.Sprintf("count_lte %d, got %d", *p.CountLte, count)
	}
	return true, ""
}

func evalArtifacts(p StatePredicate, artifacts []control.Artifact) (bool, string) {
	matched := 0
	for _, a := range artifacts {
		hay := a.URI + "\n" + a.Name + "\n" + a.Kind
		if p.Contains == nil || strings.Contains(hay, *p.Contains) {
			matched++
		}
	}
	return evalCount(p, matched)
}

func countEvents(events []control.Event, eventType string, payloadContains *string) int {
	n := 0
	for _, e := range events {
		if eventType != "" && !strings.EqualFold(e.Type, eventType) {
			continue
		}
		if payloadContains != nil && !strings.Contains(string(e.Payload), *payloadContains) {
			continue
		}
		n++
	}
	return n
}

func countApprovals(approvals []control.ApprovalRequest, status string) int {
	n := 0
	for _, a := range approvals {
		if status != "" && !strings.EqualFold(a.Status, status) {
			continue
		}
		n++
	}
	return n
}

func evalFile(p StatePredicate, workspaceRoot string) (bool, string) {
	rel := strings.TrimSpace(p.Path)
	if rel == "" {
		return false, "file predicate requires path"
	}
	full := filepath.Join(workspaceRoot, filepath.Clean(rel))
	info, err := os.Stat(full)
	exists := err == nil && !info.IsDir()
	if p.Exists != nil {
		if *p.Exists != exists {
			return false, fmt.Sprintf("exists=%v, got %v (%s)", *p.Exists, exists, rel)
		}
		if !*p.Exists {
			return true, ""
		}
	}
	if !exists {
		// If any content op is set, the file must exist.
		if p.Contains != nil || p.NotContains != nil || p.Matches != nil || p.MinBytes != nil {
			return false, "file does not exist: " + rel
		}
		return true, ""
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return false, "read file failed: " + err.Error()
	}
	if p.MinBytes != nil && len(data) < *p.MinBytes {
		return false, fmt.Sprintf("min_bytes %d, got %d", *p.MinBytes, len(data))
	}
	content := string(data)
	if p.Contains != nil && !strings.Contains(content, *p.Contains) {
		return false, fmt.Sprintf("contains %q not found in %s", *p.Contains, rel)
	}
	if p.NotContains != nil && strings.Contains(content, *p.NotContains) {
		return false, fmt.Sprintf("not_contains %q present in %s", *p.NotContains, rel)
	}
	if p.Matches != nil {
		re, err := regexp.Compile(*p.Matches)
		if err != nil {
			return false, "invalid matches regex: " + err.Error()
		}
		if !re.MatchString(content) {
			return false, fmt.Sprintf("matches %q failed in %s", *p.Matches, rel)
		}
	}
	return true, ""
}

func evalMemory(p StatePredicate, facts []memory.Fact) (bool, string) {
	var contents []string
	for _, f := range facts {
		contents = append(contents, f.Content)
	}
	if p.CountGte != nil && len(facts) < *p.CountGte {
		return false, fmt.Sprintf("count_gte %d, got %d", *p.CountGte, len(facts))
	}
	if p.CountLte != nil && len(facts) > *p.CountLte {
		return false, fmt.Sprintf("count_lte %d, got %d", *p.CountLte, len(facts))
	}
	joined := strings.Join(contents, "\n")
	if p.Contains != nil && !strings.Contains(joined, *p.Contains) {
		return false, fmt.Sprintf("contains %q, got %d facts", *p.Contains, len(facts))
	}
	if p.NotContains != nil && strings.Contains(joined, *p.NotContains) {
		return false, fmt.Sprintf("not_contains %q present", *p.NotContains)
	}
	return true, ""
}
