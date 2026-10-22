package httpapi

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"selfmind/internal/control"
)

// The runtime records the accepted plan as plan.updated and the call text as a
// JSON string inside tool.started's args.
func planUpdatedEvent(run string, steps ...map[string]interface{}) control.Event {
	payload, _ := json.Marshal(map[string]interface{}{"plan": steps})
	return control.Event{RunID: run, Type: "plan.updated", Payload: payload}
}

func planStartedEvent(run string, entries ...map[string]interface{}) control.Event {
	arguments, _ := json.Marshal(map[string]interface{}{"plan": entries})
	payload, _ := json.Marshal(map[string]interface{}{"tool": "update_plan", "args": string(arguments)})
	return control.Event{RunID: run, Type: "tool.started", Payload: payload}
}

func stored(id, step, criteria, unit string) map[string]interface{} {
	return map[string]interface{}{"step_id": id, "step": step, "status": "pending", "success_criteria": criteria, "work_unit_id": unit}
}

func TestPlanInputCountsOnlyTextTheRuntimeAlreadyStores(t *testing.T) {
	events := []control.Event{
		planStartedEvent("a", map[string]interface{}{"step": "Inspect the sources", "status": "in_progress", "success_criteria": "Sources are read"}),
		planUpdatedEvent("a", stored("s1", "Inspect the sources", "Sources are read", "wu-1"), stored("s2", "Edit", "Edited", "wu-1")),
		// s1 repeats its text, criterion and work unit; s2 is carried by id and status alone.
		planStartedEvent("a",
			map[string]interface{}{"step_id": "s1", "status": "completed", "step": "Inspect the sources", "success_criteria": "Sources are read", "work_unit_id": "wu-1"},
			map[string]interface{}{"step_id": "s2", "status": "in_progress"}),
		// s1's wording changed, so the new step text is an edit; its criterion is still repeated.
		planStartedEvent("a",
			map[string]interface{}{"step_id": "s1", "status": "completed", "step": "Inspect every source", "success_criteria": "Sources are read"},
			map[string]interface{}{"step_id": "s2", "status": "completed", "verification_required": true}),
	}
	got := collectDailyPerformance(events).PlanInput
	if got.FirstSnapshots != 1 || got.UnreadableInputs != 0 || got.Updates != 2 || got.Entries != 4 || got.RestatedEntries != 2 {
		t.Fatalf("shape: %+v", got)
	}
	// Characters are the text values sent: ids, status, step, criterion, work unit.
	wantRestated := len("Inspect the sources") + len("Sources are read") + len("wu-1") + len("Sources are read")
	wantTotal := (len("s1") + len("completed") + len("Inspect the sources") + len("Sources are read") + len("wu-1")) + (len("s2") + len("in_progress")) +
		(len("s1") + len("completed") + len("Inspect every source") + len("Sources are read")) + (len("s2") + len("completed"))
	if got.Restated != wantRestated || got.Characters != wantTotal {
		t.Fatalf("restated %d of %d characters, want %d of %d", got.Restated, got.Characters, wantRestated, wantTotal)
	}
	line := collectDailyPerformance(events).render()
	want := "Plan input restating stored fields: " + strconv.Itoa(wantRestated*100/wantTotal) + "% of plan argument characters (" + strconv.Itoa(wantRestated) + " of " + strconv.Itoa(wantTotal) + "; 2 of 4 entries) across 2 updates; 1 first snapshots and 0 unreadable inputs excluded\n"
	if !strings.Contains(line, want) {
		t.Fatalf("report line changed:\n%s\nwant %q", line, want)
	}
}

// A continuation starts from the plan it inherited: its first update_plan can
// already repeat stored text, and a plan one Run stored never serves another.
func TestPlanInputUsesTheInheritedPlanOfAContinuationAndKeepsRunsApart(t *testing.T) {
	inherited := planUpdatedEvent("child", stored("s1", "检查源码", "源码已读", ""))
	inherited.Payload, _ = json.Marshal(map[string]interface{}{
		"plan": []map[string]interface{}{stored("s1", "检查源码", "源码已读", "")}, "source": "continuation", "resumes_run_id": "parent",
	})
	events := []control.Event{
		planUpdatedEvent("parent", stored("s1", "Parent step", "Parent criterion", "")),
		inherited,
		planStartedEvent("child", map[string]interface{}{"step_id": "s1", "status": "completed", "step": "检查源码"}),
		// Another Run with no plan of its own: a first snapshot, whatever ids it sends.
		planStartedEvent("other", map[string]interface{}{"step_id": "s1", "status": "completed", "step": "Parent step"}),
	}
	got := collectDailyPerformance(events).PlanInput
	if got.Updates != 1 || got.FirstSnapshots != 1 || got.Restated != 4 {
		t.Fatalf("continuation / isolation: %+v", got)
	}
	// Text is measured in characters, not bytes: CJK is 4 here, not 12.
	if got.Characters != len("s1")+len("completed")+4 {
		t.Fatalf("characters = %d", got.Characters)
	}
}

func TestPlanInputIgnoresWhatIsNotThisRunsUpdatePlanAndReportsUnreadableCalls(t *testing.T) {
	plan := planUpdatedEvent("a", stored("s1", "Inspect", "Inspected", ""))
	asObject := control.Event{RunID: "a", Type: "tool.started", Payload: []byte(`{"tool":"update_plan","args":{"plan":[{"step_id":"s1","status":"completed","step":"Inspect"}]}}`)}
	events := []control.Event{
		plan,
		// A sub-agent's call, a batch child, another tool and a Run-less call are not measured.
		{RunID: "a", Type: "tool.started", Payload: []byte(`{"tool":"update_plan","delegated":true,"args":"{\"plan\":[{\"step_id\":\"s1\",\"status\":\"x\",\"step\":\"Inspect\"}]}"}`)},
		{RunID: "a", Type: "tool.started", Payload: []byte(`{"tool":"update_plan","nested":true,"args":"{\"plan\":[{\"step_id\":\"s1\",\"status\":\"x\",\"step\":\"Inspect\"}]}"}`)},
		{RunID: "a", Type: "tool.started", Payload: []byte(`{"tool":"read_file","args":"{\"plan\":[{\"step_id\":\"s1\",\"step\":\"Inspect\"}]}"}`)},
		planStartedEvent("", map[string]interface{}{"step_id": "s1", "status": "completed", "step": "Inspect"}),
		// Arguments that are not a plan are reported, not guessed at.
		{RunID: "a", Type: "tool.started", Payload: []byte(`{"tool":"update_plan","args":"not json"}`)},
		{RunID: "a", Type: "tool.started", Payload: []byte(`{"tool":"update_plan","args":"{\"plan\":[]}"}`)},
		{RunID: "a", Type: "tool.started", Payload: []byte(`{"tool":"update_plan"}`)},
		// The recorder stores the call text as a string; an object is read the same way.
		asObject,
	}
	got := collectDailyPerformance(events).PlanInput
	if got.Updates != 1 || got.UnreadableInputs != 3 || got.FirstSnapshots != 0 || got.Restated != len("Inspect") {
		t.Fatalf("scope of the measure: %+v", got)
	}
	if none := collectDailyPerformance([]control.Event{plan}).render(); !strings.Contains(none, "Plan input restating stored fields: no update measured (0 first snapshots and 0 unreadable inputs excluded)\n") {
		t.Fatalf("an empty window says so:\n%s", none)
	}
}

// A plan that was replaced stops serving as the stored one: the snapshot the
// last plan.updated recorded is what the next call is measured against.
func TestPlanInputFollowsTheLatestAcceptedSnapshot(t *testing.T) {
	events := []control.Event{
		planUpdatedEvent("a", stored("s1", "Old text", "", "")),
		planUpdatedEvent("a", stored("s1", "New wording", "", "")),
		planStartedEvent("a", map[string]interface{}{"step_id": "s1", "status": "pending", "step": "Old text"}),
		planStartedEvent("a", map[string]interface{}{"step_id": "s1", "status": "pending", "step": "New wording"}),
	}
	if got := collectDailyPerformance(events).PlanInput; got.Updates != 2 || got.Restated != len("New wording") {
		t.Fatalf("only the current wording is a restatement: %+v", got)
	}
}
