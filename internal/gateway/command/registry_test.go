package command

import (
	"sort"
	"strings"
	"testing"
)

func TestIsGatewayControlCoversPreviouslyOmittedCommands(t *testing.T) {
	// The old per-adapter async-hint copies omitted these six; the registry
	// must now detect them so IM adapters treat them as synchronous control.
	control := []string{
		"/queue", "/diag", "/mode", "/notify", "/help", "/model",
		"/status", "/tasks", "/events", "/approvals", "/approve 1",
		"/reject 2", "/stop", "/cancel", "/id", "/new title", "/resume tsk_1",
		"/workspace ws_1", "/workspaces",
		"/QUEUE", "/Mode smart", // case-insensitive
		"/task status", "/task tsk_1", // aliases
	}
	for _, c := range control {
		if !IsGatewayControl(c) {
			t.Errorf("IsGatewayControl(%q) = false, want true", c)
		}
	}

	notControl := []string{
		"", "hello world", "run a long task",
		"/copy", "/paste-image", "/skills list", "/memory", // Local-scope only
		"/qwer", "/unknown", "status", "approve", // bare words are not slash control
	}
	for _, c := range notControl {
		if IsGatewayControl(c) {
			t.Errorf("IsGatewayControl(%q) = true, want false", c)
		}
	}
}

func TestSuggest(t *testing.T) {
	cases := map[string]string{
		"/qwer":       "",           // too far from any command
		"/approves":   "/approve",   // one deletion
		"/aproval":    "/approvals", // near /approvals
		"/status":     "",           // exact command is not a typo
		"hello":       "",           // not a slash command
		"":            "",           // empty
		"/statuss":    "/status",    // one insertion
		"/queu clear": "/queue",     // first token only, one deletion
	}
	for input, want := range cases {
		if got := Suggest(input); got != want {
			t.Errorf("Suggest(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestKnownAreAllGatewayScope(t *testing.T) {
	for _, name := range Known() {
		e, ok := Lookup(name)
		if !ok {
			t.Errorf("Known() name %q not found via Lookup", name)
			continue
		}
		if e.Scope != Gateway {
			t.Errorf("Known() name %q has scope %v, want Gateway", name, e.Scope)
		}
		if !e.SyncControl {
			t.Errorf("Gateway command %q must have SyncControl=true", name)
		}
	}
}

func TestHelpTextListsEveryGatewayCommand(t *testing.T) {
	help := HelpText()
	if !strings.HasPrefix(help, "SelfMind commands:") {
		t.Fatalf("help text missing header: %q", help)
	}
	for _, name := range Known() {
		e, _ := Lookup(name)
		if !strings.Contains(help, e.Usage) {
			t.Errorf("help text missing usage for %q (%q)", name, e.Usage)
		}
		if !strings.Contains(help, e.Summary) {
			t.Errorf("help text missing summary for %q", name)
		}
	}
}

func TestLocalCommandsNotGatewayRoutable(t *testing.T) {
	local := []string{"/copy", "/paste-image", "/history", "/compact", "/clear",
		"/exit", "/skills", "/bundles", "/memory", "/curator", "/checkpoint",
		"/reload-skills", "/migrate", "/capture"}
	for _, name := range local {
		e, ok := Lookup(name)
		if !ok {
			t.Errorf("Local command %q missing from registry", name)
			continue
		}
		if e.Scope != Local {
			t.Errorf("%q scope = %v, want Local", name, e.Scope)
		}
		if IsGatewayControl(name) {
			t.Errorf("Local command %q must not be gateway-routable", name)
		}
	}
}

// TestKnownMatchesGatewayContract mirrors the tryHandleControlCommand switch's
// gateway command set. If a case is added to the switch, add it here AND to the
// registry so help/suggest/async-hint cannot drift again.
func TestKnownMatchesGatewayContract(t *testing.T) {
	want := []string{
		"/help", "/model", "/id", "/status", "/tasks", "/queue", "/diag",
		"/events", "/approvals", "/approve", "/reject", "/mode", "/stop",
		"/cancel", "/notify", "/new", "/resume", "/workspace", "/workspaces",
	}
	got := Known()
	sort.Strings(want)
	sortedGot := append([]string(nil), got...)
	sort.Strings(sortedGot)
	if strings.Join(want, ",") != strings.Join(sortedGot, ",") {
		t.Fatalf("Known() = %v, want gateway contract %v", got, want)
	}
}
