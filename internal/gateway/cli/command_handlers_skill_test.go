package cli

import "testing"

func TestHandleSkillsArchiveUsesManagedDispatch(t *testing.T) {
	var gotTool string
	var gotArgs map[string]interface{}
	m := &uiModel{
		tenantID: "default",
		toolDispatchFn: func(tool string, args map[string]interface{}) (string, error) {
			gotTool, gotArgs = tool, args
			return "archived through daemon", nil
		},
	}
	msg, ok := m.handleSkills([]string{"archive", "release-flow"})().(MsgAgentDone)
	if !ok {
		t.Fatalf("archive result type = %T, want MsgAgentDone", msg)
	}
	if msg.Response != "archived through daemon" {
		t.Fatalf("archive response = %q", msg.Response)
	}
	if gotTool != "skill_manage" || gotArgs["action"] != "archive" || gotArgs["name"] != "release-flow" {
		t.Fatalf("archive dispatch = tool %q args %+v", gotTool, gotArgs)
	}
}
