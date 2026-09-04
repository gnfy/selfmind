package cliapp

import (
	"strings"
	"testing"

	"selfmind/internal/gateway/command"
)

// TestWorkspaceCLIForwardsOnlyLiveCommands is a regression guard for a class of
// defect, not one instance: retiring a slash spelling left CLI subcommands
// still forwarding it, so `selfmind ws 2` answered "Unknown command" while
// looking perfectly healthy. Every forwarded command must exist in the shared
// registry.
func TestWorkspaceCLIForwardsOnlyLiveCommands(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "/ws"},
		{[]string{"2"}, "/ws 2"},
		{[]string{"ws_abc12345"}, "/ws ws_abc12345"},
		{[]string{"default", "3"}, "/ws default 3"},
	} {
		app, recorded, _, _ := newSendTestApp(t, []string{"selfmind", "ws"})
		if code := app.handleWorkspaceCommand(tc.args); code != 0 {
			t.Fatalf("ws %v exited %d", tc.args, code)
		}
		if recorded.Content != tc.want {
			t.Fatalf("ws %v forwarded %q, want %q", tc.args, recorded.Content, tc.want)
		}
		name := strings.Fields(tc.want)[0]
		if _, ok := command.Lookup(name); !ok {
			t.Fatalf("ws %v forwards %q, which is not in the shared registry", tc.args, name)
		}
	}
}

// TestRetiredWorkspaceSubcommandsAreGone: `use` and `list` did exactly what the
// bare form and a bare ordinal already do. Two extra names for two existing
// actions is something to read, remember, and keep in sync — and both had
// already drifted out of sync.
func TestRetiredWorkspaceSubcommandsAreGone(t *testing.T) {
	for _, retired := range [][]string{{"use", "ws_abc12345"}, {"list"}} {
		app, recorded, _, _ := newSendTestApp(t, []string{"selfmind", "ws"})
		app.handleWorkspaceCommand(retired)
		// A retired subcommand must fall through to selection, which forwards
		// the word verbatim and lets the gateway reject it — never silently
		// behave like something else.
		if strings.HasPrefix(recorded.Content, "/workspace") {
			t.Fatalf("ws %v still forwards a retired spelling: %q", retired, recorded.Content)
		}
	}
}
