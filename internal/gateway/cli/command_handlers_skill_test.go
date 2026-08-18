package cli

import (
	"strings"
	"testing"
)

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

func TestSkillSlashResolutionRunsOnDaemonWithClientScope(t *testing.T) {
	var gotTool string
	var gotArgs map[string]interface{}
	m := &uiModel{
		tenantID: "default", workspaceOverrideID: "ws_selected",
		toolDispatchFn: func(tool string, args map[string]interface{}) (string, error) {
			gotTool, gotArgs = tool, args
			return `{"found":true,"display_name":"release-flow","prompt":"loaded prompt"}`, nil
		},
	}
	msg, ok := m.handleSkillSlash("/release-flow", "ship it")().(MsgSkillInvocationResolved)
	if !ok || !msg.Found || msg.Prompt != "loaded prompt" {
		t.Fatalf("resolution message = %#v", msg)
	}
	if gotTool != "skill_invocation_resolve" || gotArgs["command"] != "/release-flow" || gotArgs["instruction"] != "ship it" {
		t.Fatalf("daemon resolution = tool %q args %+v", gotTool, gotArgs)
	}
	if gotArgs["_workspace_id"] != "ws_selected" || strings.TrimSpace(gotArgs["_client_cwd"].(string)) == "" {
		t.Fatalf("client scope missing: %+v", gotArgs)
	}
}

func TestCuratorUsesManagedDaemonDispatch(t *testing.T) {
	var gotArgs map[string]interface{}
	m := &uiModel{
		tenantID: "default",
		toolDispatchFn: func(tool string, args map[string]interface{}) (string, error) {
			if tool != "skill_manage" {
				t.Fatalf("tool = %q", tool)
			}
			gotArgs = args
			return "curator result", nil
		},
	}
	msg, ok := m.handleCurator([]string{"run", "--dry-run", "--report"})().(MsgAgentDone)
	if !ok || msg.Response != "curator result" {
		t.Fatalf("curator response = %#v", msg)
	}
	if gotArgs["action"] != "curator_run" || gotArgs["dry_run"] != true || gotArgs["write_report"] != true {
		t.Fatalf("curator dispatch args = %+v", gotArgs)
	}
}
