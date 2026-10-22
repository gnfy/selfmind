package eval

import (
	"encoding/json"
	"testing"

	"selfmind/internal/control"
)

func TestEventFieldsMatchSemanticToolArgumentsOnTheSameEvent(t *testing.T) {
	var events []control.Event
	for _, args := range []interface{}{`{"mode":"patch","plan":[]}`, "{\n \"plan\": [], \"mode\": \"patch\"\n}", map[string]interface{}{"mode": "patch"}, `{"mode":"snapshot"}`, `{bad`, map[string]interface{}{"mode": true}} {
		raw, _ := json.Marshal(map[string]interface{}{"tool": "update_plan", "args": args})
		events = append(events, control.Event{Type: "tool.started", Payload: raw})
	}
	if n := countEvents(events, "tool.started", nil, map[string]string{"tool": "update_plan", "args.mode": "patch"}); n != 3 {
		t.Fatalf("equivalent JSON arguments counted as %d", n)
	}
	if n := countEvents(events, "tool.started", nil, map[string]string{"tool": "finish_run", "args.mode": "patch"}); n != 0 {
		t.Fatal("fields were joined across events")
	}
	if n := countEvents(events, "tool.started", nil, map[string]string{"args.missing": "patch"}); n != 0 {
		t.Fatal("missing data matched")
	}
}
