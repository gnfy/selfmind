package cli

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/gateway/api"
	commandcatalog "selfmind/internal/gateway/command"
)

// TestBareResumeArmsPickerFromDaemonList pins that a bare /resume presents the
// daemon's own task list. The numbering must come from the daemon because
// /resume <n> is resolved there; a locally numbered menu would drift from the
// resolver and resume the wrong task.
func TestBareResumeArmsPickerFromDaemonList(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.width, model.height = 100, 30

	var relayed []string
	model.messageProcessor = fakeControlProcessor(&relayed, "Open work:\n1. Fix parser\n2. Ship beta")

	cmd := model.handleResumeSelect(nil)
	if cmd == nil {
		t.Fatal("bare /resume must return a command")
	}
	msg, ok := cmd().(MsgAgentDone)
	if !ok {
		t.Fatalf("expected MsgAgentDone, got %T", cmd())
	}
	// The daemon owns the ordering AND the ordinal snapshot, so bare /resume
	// asks it for its own list rather than borrowing another command's.
	if len(relayed) != 1 || relayed[0] != "/resume" {
		t.Fatalf("bare /resume must relay /resume for the ordering: %v", relayed)
	}
	if !strings.Contains(msg.Response, "1. Fix parser") {
		t.Fatalf("picker must show the daemon list: %q", msg.Response)
	}
	if !strings.Contains(msg.Response, "Type a number") {
		t.Fatalf("picker must tell the user how to choose: %q", msg.Response)
	}
	if !model.resumePickerArmed {
		t.Fatal("bare /resume must arm the numeric picker")
	}
}

// TestResumeWithReferenceStaysAPlainRelay keeps the explicit form unchanged: it
// must reach the daemon as /resume <ref>, not as a task listing.
func TestResumeWithReferenceStaysAPlainRelay(t *testing.T) {
	model := NewController("", "", nil, "").model
	var relayed []string
	model.messageProcessor = fakeControlProcessor(&relayed, "Resumed.")
	model.resumePickerArmed = true

	cmd := model.handleResumeSelect([]string{"tsk_1234"})
	if cmd == nil {
		t.Fatal("/resume <ref> must return a command")
	}
	cmd()
	if len(relayed) != 1 || relayed[0] != "/resume tsk_1234" {
		t.Fatalf("explicit /resume must relay verbatim: %v", relayed)
	}
	if model.resumePickerArmed {
		t.Fatal("an explicit reference must disarm the picker")
	}
}

// TestArmedPickerExpandsBareNumber covers the selection keystroke: while the
// picker is armed a bare number means "resume that entry", and anything else
// disarms it so a digit-leading sentence still reaches the agent as text.
func TestArmedPickerExpandsBareNumber(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   string
		want    string
		expands bool
	}{
		{name: "bare number picks", input: "2", want: "/resume 2", expands: true},
		{name: "zero is not a list index", input: "0", expands: false},
		{name: "digit-leading text stays text", input: "2 more things please", expands: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := bareListNumber(tc.input)
			if tc.expands {
				if got == "" {
					t.Fatalf("%q must be read as a list index", tc.input)
				}
				if "/resume "+got != tc.want {
					t.Fatalf("expansion = %q, want %q", "/resume "+got, tc.want)
				}
				return
			}
			if got != "" {
				t.Fatalf("%q must not be read as a list index (got %q)", tc.input, got)
			}
		})
	}
}

// TestSearchCurrentOpensFullDiffPager pins the /history fold: deleting the
// command must not delete the only way to read a truncated diff in full.
func TestSearchCurrentOpensFullDiffPager(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.width, model.height = 100, 30

	if cmd := model.handleSessionSearch([]string{"current"}); cmd != nil {
		t.Fatal("/search current is a local overlay, not a dispatched command")
	}
	if model.pager == nil {
		t.Fatal("/search current must open the transcript pager")
	}
}

// TestHistoryCommandIsGone guards the consolidation: one look-back entry point.
func TestHistoryCommandIsGone(t *testing.T) {
	if _, ok := slashCommandIndex["/history"]; ok {
		t.Fatal("/history must no longer be a slash command; use /search current")
	}
	if _, ok := slashCommandIndex["/search"]; !ok {
		t.Fatal("/search must be the surviving look-back command")
	}
}

// TestSlashCommandMetaIndicesMatchNames guards the fragile part of the table:
// handlers used to bind to slashCommandMetas by position, so removing a meta
// silently rewired commands.
func TestSlashCommandMetaIndicesMatchNames(t *testing.T) {
	for _, cmd := range allSlashCommands {
		if strings.TrimSpace(cmd.Name) == "" {
			t.Fatal("every slash command must carry its metadata")
		}
		if cmd.Run == nil {
			t.Fatalf("%s has no handler", cmd.Name)
		}
	}
	for _, name := range []string{"/copy", "/queue", "/watchers", "/diag", "/report", "/search", "/capture"} {
		cmd, ok := slashCommandIndex[name]
		if !ok {
			t.Fatalf("%s missing from the command table", name)
		}
		if cmd.Name != name {
			t.Fatalf("%s bound to metadata for %s", name, cmd.Name)
		}
	}
}

// Every daemon control command must be discoverable and executable in the TUI.
// The gateway catalog is authoritative; this guard prevents a newly added
// command from working over IM/HTTP while silently appearing unknown locally.
func TestTUIExposesEveryGatewayControlCommand(t *testing.T) {
	for _, entry := range commandcatalog.All() {
		if entry.Scope != commandcatalog.Gateway {
			continue
		}
		if _, ok := slashCommandIndex[entry.Name]; !ok {
			t.Errorf("gateway command %s is missing from the TUI command table", entry.Name)
		}
		for _, alias := range entry.Aliases {
			if strings.ContainsAny(alias, " \t\r\n") {
				continue
			}
			if _, ok := slashCommandIndex[alias]; !ok {
				t.Errorf("gateway command alias %s is missing from the TUI command table", alias)
			}
		}
	}
}

// fakeControlProcessor records every control message the TUI relays and answers
// with a fixed reply, so tests can assert what reached the daemon.
func fakeControlProcessor(seen *[]string, reply string) MessageProcessor {
	return func(ctx context.Context, req api.MessageRequest) (api.MessageResponse, int) {
		*seen = append(*seen, req.Content)
		return api.MessageResponse{Content: reply}, 0
	}
}

// TestWorkHistorySlashMetasComeFromSharedCatalog guards the single command
// catalog rule: the TUI's /new, /resume, and /search presentation is the
// gateway registry's usage and summary, never a local copy that can drift from
// what the daemon accepts.
func TestWorkHistorySlashMetasComeFromSharedCatalog(t *testing.T) {
	for _, name := range []string{"/new", "/resume", "/search"} {
		entry, ok := commandcatalog.Lookup(name)
		if !ok {
			t.Fatalf("%s is missing from the shared catalog", name)
		}
		cmd, ok := slashCommandIndex[name]
		if !ok {
			t.Fatalf("%s is missing from the TUI command table", name)
		}
		if cmd.Usage != entry.Usage || cmd.Description != entry.Summary {
			t.Errorf("%s TUI meta usage=%q description=%q drifted from catalog usage=%q summary=%q",
				name, cmd.Usage, cmd.Description, entry.Usage, entry.Summary)
		}
		found := false
		for _, meta := range slashHelpMetas() {
			if meta.Name == name {
				found = true
				if meta.Usage != entry.Usage {
					t.Errorf("%s help usage=%q, want catalog usage %q", name, meta.Usage, entry.Usage)
				}
			}
		}
		if !found {
			t.Errorf("%s is missing from the help page", name)
		}
	}
}

// TestSlashMetasMatchSharedCatalog is the drift guard for the one-catalog rule:
// every TUI command the shared registry knows must render the registry's usage
// and summary verbatim, on the help page and in the command table alike. Local
// copies drifted before — the TUI advertised "/queue [clear]" while the daemon
// accepted "/queue [drop <n>|clear]", and "/notify <platform|auto>" while the
// daemon also accepted desk-first|phone-first — so a user reading TUI help was
// told a command was narrower than it is. Aliases are exempt: the catalog
// resolves an alias to its canonical entry and holds no alias-specific text.
func TestSlashMetasMatchSharedCatalog(t *testing.T) {
	check := func(surface string, metas []slashCommandMeta) {
		for _, meta := range metas {
			entry, ok := commandcatalog.Lookup(meta.Name)
			if !ok {
				continue // TUI-only command the shared catalog does not define
			}
			if entry.Name != meta.Name {
				continue // alias row; the catalog defines only the canonical name
			}
			if meta.Usage != entry.Usage {
				t.Errorf("%s: %s usage %q drifted from shared catalog usage %q",
					surface, meta.Name, meta.Usage, entry.Usage)
			}
			if meta.Description != entry.Summary {
				t.Errorf("%s: %s description %q drifted from shared catalog summary %q",
					surface, meta.Name, meta.Description, entry.Summary)
			}
		}
	}
	help := slashHelpMetas()
	check("help page", help)

	table := make([]slashCommandMeta, 0, len(allSlashCommands))
	for _, cmd := range allSlashCommands {
		table = append(table, cmd.slashCommandMeta)
	}
	check("command table", table)

	// A name that no longer resolves would render a blank help row rather than
	// fail the comparison above.
	for _, meta := range help {
		if strings.TrimSpace(meta.Usage) == "" || strings.TrimSpace(meta.Description) == "" {
			t.Errorf("help page: %s has no usage or description", meta.Name)
		}
	}
}
