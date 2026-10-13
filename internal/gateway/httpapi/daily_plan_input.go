package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"selfmind/internal/control"
)

// dailyPlanInput measures how much of what the model sends in update_plan
// repeats what the runtime already stores for the step. The runtime keeps the
// text, criterion and work-unit attribution of an existing step_id, so a step
// that has not changed needs only its id and status; restated text is output
// the model generated for nothing.
//
// The stored plan is the one the last plan.updated event of the Run recorded,
// including a plan a continuation inherited. A first snapshot has nothing to
// repeat and is excluded rather than diluting the share.
type dailyPlanInput struct {
	Updates                          int
	FirstSnapshots, UnreadableInputs int
	Characters, Restated             int
	Entries, RestatedEntries         int
}

type dailyStoredPlanStep struct{ step, criteria, workUnit string }

type dailyPlanInputTracker struct {
	dailyPlanInput
	stored map[string]map[string]dailyStoredPlanStep
}

func newDailyPlanInputTracker() *dailyPlanInputTracker {
	return &dailyPlanInputTracker{stored: map[string]map[string]dailyStoredPlanStep{}}
}

// observe reads the two durable facts the measure needs: the Run's accepted
// plan, and the arguments of each of its own update_plan calls. A sub-agent's
// call and a batch child are not the Run's.
func (t *dailyPlanInputTracker) observe(event control.Event) {
	switch event.Type {
	case "plan.updated":
		t.planUpdated(event.RunID, event.Payload)
	case "tool.started":
		var p struct {
			Tool      string          `json:"tool"`
			Args      json.RawMessage `json:"args"`
			Delegated bool            `json:"delegated"`
			Nested    bool            `json:"nested"`
		}
		if json.Unmarshal(event.Payload, &p) == nil && p.Tool == "update_plan" && !p.Delegated && !p.Nested {
			t.planStarted(event.RunID, p.Args)
		}
	}
}

// planUpdated records the accepted snapshot a Run now stores.
func (t *dailyPlanInputTracker) planUpdated(runID string, payload []byte) {
	var p struct {
		Plan []struct {
			StepID          string `json:"step_id"`
			Step            string `json:"step"`
			SuccessCriteria string `json:"success_criteria"`
			WorkUnitID      string `json:"work_unit_id"`
		} `json:"plan"`
	}
	if runID == "" || json.Unmarshal(payload, &p) != nil {
		return
	}
	steps := make(map[string]dailyStoredPlanStep, len(p.Plan))
	for _, item := range p.Plan {
		if id := strings.TrimSpace(item.StepID); id != "" {
			steps[id] = dailyStoredPlanStep{
				step: strings.TrimSpace(item.Step), criteria: strings.TrimSpace(item.SuccessCriteria), workUnit: strings.TrimSpace(item.WorkUnitID),
			}
		}
	}
	t.stored[runID] = steps
}

// planStarted measures one update_plan call's arguments against the plan the
// Run stored before it. The recorded arguments are the raw call text.
func (t *dailyPlanInputTracker) planStarted(runID string, rawArgs json.RawMessage) {
	if runID == "" {
		return
	}
	stored := t.stored[runID]
	if len(stored) == 0 {
		t.FirstSnapshots++
		return
	}
	entries, ok := dailyPlanEntries(rawArgs)
	if !ok {
		t.UnreadableInputs++
		return
	}
	t.Updates++
	for _, entry := range entries {
		for _, value := range entry {
			if text, isText := value.(string); isText {
				t.Characters += utf8.RuneCountInString(text)
			}
		}
		t.Entries++
		before, known := stored[strings.TrimSpace(dailyEntryText(entry, "step_id"))]
		if !known {
			continue
		}
		restated := 0
		for _, field := range [...]struct{ name, kept string }{
			{"step", before.step}, {"success_criteria", before.criteria}, {"work_unit_id", before.workUnit},
		} {
			if text := strings.TrimSpace(dailyEntryText(entry, field.name)); text != "" && text == field.kept {
				restated += utf8.RuneCountInString(dailyEntryText(entry, field.name))
			}
		}
		if restated > 0 {
			t.Restated += restated
			t.RestatedEntries++
		}
	}
}

func dailyEntryText(entry map[string]interface{}, field string) string {
	text, _ := entry[field].(string)
	return text
}

// dailyPlanEntries decodes the plan array of a recorded update_plan call. The
// call text is stored as a JSON string holding the arguments object; an
// object stored directly is read the same way.
func dailyPlanEntries(raw json.RawMessage) ([]map[string]interface{}, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	if raw[0] == '"' {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return nil, false
		}
		raw = json.RawMessage(text)
	}
	var arguments struct {
		Plan []map[string]interface{} `json:"plan"`
	}
	if json.Unmarshal(raw, &arguments) != nil || len(arguments.Plan) == 0 {
		return nil, false
	}
	return arguments.Plan, true
}

func (p dailyPlanInput) render() string {
	excluded := fmt.Sprintf("%d first snapshots and %d unreadable inputs excluded", p.FirstSnapshots, p.UnreadableInputs)
	if p.Updates == 0 || p.Characters == 0 {
		return fmt.Sprintf("Plan input restating stored fields: no update measured (%s)\n", excluded)
	}
	return fmt.Sprintf("Plan input restating stored fields: %d%% of plan argument characters (%d of %d; %d of %d entries) across %d updates; %s\n",
		p.Restated*100/p.Characters, p.Restated, p.Characters, p.RestatedEntries, p.Entries, p.Updates, excluded)
}
