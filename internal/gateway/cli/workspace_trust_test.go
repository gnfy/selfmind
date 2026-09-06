package cli

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"selfmind/internal/gateway/api"
)

// TestTrustIsAskedOnceAtStartup pins the one-time trust question. Before this
// nothing ever asked: control commands run before the run pipeline would create
// the directory's workspace, so at startup there was no workspace to ask about,
// and two capabilities stayed silently off with no hint they existed.
func TestTrustIsAskedOnceAtStartup(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ws    *api.DigestWorkspace
		asked bool
	}{
		{"untrusted and unanswered asks", &api.DigestWorkspace{ID: "ws_1", Name: "proj", Path: "/p"}, true},
		{"trusted says nothing", &api.DigestWorkspace{ID: "ws_1", Name: "proj", Path: "/p", Trusted: true}, false},
		{"declined is never asked again", &api.DigestWorkspace{ID: "ws_1", Name: "proj", Path: "/p", TrustAsked: true}, false},
		{"no workspace says nothing", nil, false},
	} {
		model := NewController("", "", nil, "").model
		model.width, model.height = 100, 30
		model.sessionWorkspace = tc.ws
		if got := model.armWorkspaceTrustPrompt(); got != tc.asked {
			t.Errorf("%s: asked=%v want %v", tc.name, got, tc.asked)
		}
		if !tc.asked {
			continue
		}
		// The question must be a question, not a line of text among the startup
		// output: it renders in the pinned active region and names both what an
		// answer buys and how to defer.
		view := stripANSI(model.viewActiveRegion())
		for _, want := range []string{"Trust this workspace?", "proj", "(t)", "(n)", "(d)", "esc to decide later"} {
			if !strings.Contains(view, want) {
				t.Errorf("%s: the question must show %q:\n%s", tc.name, want, view)
			}
		}
		// Asking again for the same workspace would turn a deferral into a nag.
		model.workspaceTrustPrompt = nil
		if model.armWorkspaceTrustPrompt() {
			t.Errorf("%s: the same workspace was asked about twice in one session", tc.name)
		}
	}
}

// The question owns the keyboard while armed. A question stray keystrokes slip
// past is the notice this replaced: it was skipped, and two capabilities stayed
// off for the life of the workspace with nobody having decided that.
func TestTrustQuestionCapturesKeysAndSendsOnlyGatewayControls(t *testing.T) {
	for _, tc := range []struct {
		name    string
		key     string
		command string
	}{
		{"t trusts", "t", "/ws trust"},
		{"d stops asking", "d", "/ws decline"},
		{"n defers without recording anything", "n", ""},
		{"esc defers", "esc", ""},
	} {
		model := NewController("", "", nil, "").model
		model.width, model.height = 100, 30
		sent := []string{}
		model.messageProcessor = func(_ context.Context, req api.MessageRequest) (api.MessageResponse, int) {
			sent = append(sent, req.Content)
			return api.MessageResponse{Content: "ok"}, 200
		}
		model.sessionWorkspace = &api.DigestWorkspace{ID: "ws_1", Name: "proj", Path: "/p"}
		model.armWorkspaceTrustPrompt()

		// A key that is not an answer must not reach the composer.
		model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("z")})
		if model.workspaceTrustPrompt == nil {
			t.Fatalf("%s: an unrelated key dismissed the question", tc.name)
		}
		if draft := model.editor.Value(); draft != "" {
			t.Fatalf("%s: key leaked into the composer: %q", tc.name, draft)
		}

		var key tea.KeyMsg
		if tc.key == "esc" {
			key = tea.KeyMsg{Type: tea.KeyEsc}
		} else {
			key = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tc.key)}
		}
		_, cmd := model.Update(key)
		if cmd != nil {
			cmd()
		}
		if model.workspaceTrustPrompt != nil {
			t.Fatalf("%s: the question stayed armed after an answer", tc.name)
		}
		if tc.command == "" {
			if len(sent) != 0 {
				t.Fatalf("%s: deferring must record nothing, sent %v", tc.name, sent)
			}
			continue
		}
		if len(sent) != 1 || sent[0] != tc.command {
			t.Fatalf("%s: sent %v, want [%s]", tc.name, sent, tc.command)
		}
	}
}

// Entering a workspace is when the question is worth asking, and a session
// switch is an entry: /ws <n> into an untrusted workspace must ask too, and
// must learn trust from the daemon's typed reply rather than its prose.
func TestSwitchingIntoAnUntrustedWorkspaceAsks(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.width, model.height = 100, 30
	model.sessionWorkspace = &api.DigestWorkspace{ID: "ws_1", Name: "here", Path: "/here", Trusted: true}

	model.Update(MsgWorkspaceSwitched{
		ID: "ws_2", Name: "other", Path: "/other", Reply: "Current workspace: other (ws_2)",
		Workspace: &api.DigestWorkspace{ID: "ws_2", Name: "other", Path: "/other"},
	})
	if model.workspaceTrustPrompt == nil {
		t.Fatal("switching into an untrusted workspace did not ask")
	}
	if !strings.Contains(stripANSI(model.viewActiveRegion()), "other") {
		t.Fatalf("the question does not name the workspace entered:\n%s", stripANSI(model.viewActiveRegion()))
	}

	// A switch into a workspace whose answer is already recorded stays silent.
	model.workspaceTrustPrompt = nil
	model.Update(MsgWorkspaceSwitched{
		ID: "ws_3", Name: "trusted", Path: "/t", Reply: "Current workspace: trusted (ws_3)",
		Workspace: &api.DigestWorkspace{ID: "ws_3", Name: "trusted", Path: "/t", Trusted: true},
	})
	if model.workspaceTrustPrompt != nil {
		t.Fatal("a trusted workspace was asked about")
	}
}

// TestStartupCardNamesTheResolvedWorkspace: the card used to print a bare
// directory that could not say whether it was even a workspace, or whether it
// was trusted.
func TestStartupCardNamesTheResolvedWorkspace(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.width = 200
	// The tag reports a settled answer. A workspace whose trust question is
	// still open carries none: the card is committed scrollback, so tagging one
	// the person is about to be asked about leaves the card contradicting their
	// own answer one line below, with no way to correct it.
	model.sessionWorkspace = &api.DigestWorkspace{
		ID: "ws_1", Name: "proj", Path: "/work/proj", Trusted: false,
	}
	card := stripANSI(strings.Join(model.renderStartupCard(200), "\n"))
	for _, want := range []string{"proj", "/work/proj"} {
		if !strings.Contains(card, want) {
			t.Fatalf("startup card missing %q:\n%s", want, card)
		}
	}
	if strings.Contains(card, "[untrusted]") {
		t.Fatalf("an unanswered trust question must not be tagged as settled:\n%s", card)
	}

	// Declined is an answer, so the tag explains the missing capability.
	model.sessionWorkspace.TrustAsked = true
	card = stripANSI(strings.Join(model.renderStartupCard(200), "\n"))
	if !strings.Contains(card, "[untrusted]") {
		t.Fatalf("a declined workspace must be tagged:\n%s", card)
	}

	// A trusted workspace carries no tag: the tag exists to explain a missing
	// capability, not to decorate a healthy one.
	model.sessionWorkspace.Trusted = true
	card = stripANSI(strings.Join(model.renderStartupCard(200), "\n"))
	if strings.Contains(card, "[untrusted]") {
		t.Fatalf("a trusted workspace must not be tagged:\n%s", card)
	}

	// An explicit /ws switch still wins over the resolved directory.
	model.Update(MsgWorkspaceSwitched{ID: "ws_2", Name: "other", Path: "/work/other"})
	card = stripANSI(strings.Join(model.renderStartupCard(200), "\n"))
	if !strings.Contains(card, "/work/other") {
		t.Fatalf("an explicit switch must win:\n%s", card)
	}
}

// Answering the question must confirm the resulting capability, not echo the
// daemon's CLI reply: that relayed a workspace UUID into the TUI as the answer
// to a plain question.
func TestTrustAnswerConfirmsCapabilityNotIdentifier(t *testing.T) {
	for _, tc := range []struct {
		name     string
		key      string
		result   *api.DigestWorkspace
		want     string
		unwanted string
	}{
		{
			name: "trusting states what turned on", key: "t",
			result: &api.DigestWorkspace{ID: "ws_1", Name: "proj", Path: "/p", Trusted: true, TrustAsked: true},
			want:   "Trusted proj", unwanted: "ws_1",
		},
		{
			name: "declining states it will not ask again", key: "d",
			result: &api.DigestWorkspace{ID: "ws_1", Name: "proj", Path: "/p", TrustAsked: true},
			want:   "will not ask", unwanted: "ws_1",
		},
		{
			name: "deferring says the workspace stays untrusted", key: "n",
			want: "stays untrusted for now", unwanted: "ws_1",
		},
	} {
		model := NewController("", "", nil, "").model
		model.width, model.height = 100, 30
		model.messageProcessor = func(_ context.Context, _ api.MessageRequest) (api.MessageResponse, int) {
			return api.MessageResponse{
				Content:   "Trusted proj (ws_1)\n/p",
				Workspace: tc.result,
			}, 200
		}
		model.sessionWorkspace = &api.DigestWorkspace{ID: "ws_1", Name: "proj", Path: "/p"}
		model.armWorkspaceTrustPrompt()

		_, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tc.key)})
		if cmd != nil {
			if msg := cmd(); msg != nil {
				model.Update(msg)
			}
		}
		said := ""
		for _, message := range model.messages {
			if message.Role == "notice" {
				said = message.Content
			}
		}
		if !strings.Contains(said, tc.want) {
			t.Errorf("%s: confirmation %q must contain %q", tc.name, said, tc.want)
		}
		if strings.Contains(said, tc.unwanted) {
			t.Errorf("%s: confirmation %q must not expose %q", tc.name, said, tc.unwanted)
		}
	}
}

// An extra directory is only meaningful against the workspace it extends. Two
// surfaces need two levels of that: the card is a glance, so it carries the
// shortest form that still says where the directory sits; `/add-dir` is a
// deliberate question, so it answers with the path plus the relationship.
func TestAdditionalRootFormsAreRelativeToTheWorkspace(t *testing.T) {
	const ws = "/Users/cwill/Workspace/ai/selfmind"
	for _, tc := range []struct {
		name   string
		root   string
		glance string
		detail string
	}{
		{"a neighbour reads as the hop", "/Users/cwill/Workspace/ai/codex",
			"../codex", "/Users/cwill/Workspace/ai/codex  (../codex)"},
		{"a parent is the parent", "/Users/cwill/Workspace/ai",
			"..", "/Users/cwill/Workspace/ai  (contains selfmind)"},
		{"a child is a plain subpath", ws + "/docs",
			"docs", ws + "/docs  (inside selfmind)"},
		{"the workspace itself says so", ws,
			".", ws + "  (the workspace itself)"},
		// A relative form is only an improvement while it is shorter.
		{"a distant tree stays absolute", "/etc/ssl", "/etc/ssl", "/etc/ssl"},
		// /repo must not look like it contains /repo-fork.
		{"a sibling sharing a prefix is not containment", "/Users/cwill/Workspace/ai/selfmind-fork",
			"../selfmind-fork", "/Users/cwill/Workspace/ai/selfmind-fork  (../selfmind-fork)"},
	} {
		if got := addDirFlagForm(tc.root, ws); got != tc.glance {
			t.Errorf("%s: glance form %q, want %q", tc.name, got, tc.glance)
		}
		if got := describeAdditionalRoot(tc.root, ws, "selfmind"); got != tc.detail {
			t.Errorf("%s: detail form %q, want %q", tc.name, got, tc.detail)
		}
	}

	// With no workspace resolved yet there is nothing to relate to.
	if got := addDirFlagForm("/work/shared", ""); got != "/work/shared" {
		t.Errorf("without a workspace the path stands alone: %q", got)
	}
	if got := describeAdditionalRoot("/work/shared", "", ""); got != "/work/shared" {
		t.Errorf("without a workspace the detail form stands alone: %q", got)
	}
}

// The overlay rides the row it widens: the right-hand slot carries the live
// command the way MAIN carries /model, and the paths go in the description line
// this row previously left empty. A session carrying none must look exactly as
// it did before, so the affordance advertises a state rather than a feature.
func TestStartupCardShowsAdditionalReachOnTheWorkspaceRow(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.width = 200
	model.sessionWorkspace = &api.DigestWorkspace{ID: "ws_1", Name: "proj", Path: "/work/proj", Trusted: true}

	card := stripANSI(strings.Join(model.renderStartupCard(200), "\n"))
	for _, unwanted := range []string{"/add-dir", "Also reachable"} {
		if strings.Contains(card, unwanted) {
			t.Fatalf("a session with no extra directories must not mention %q:\n%s", unwanted, card)
		}
	}

	model.additionalRoots = []string{"/work/shared", "/work"}
	card = stripANSI(strings.Join(model.renderStartupCard(200), "\n"))
	workspaceRow := ""
	for _, line := range strings.Split(card, "\n") {
		if strings.HasPrefix(line, "WORKSPACE") {
			workspaceRow = line
		}
	}
	if !strings.HasSuffix(strings.TrimRight(workspaceRow, " "), "/add-dir") {
		t.Fatalf("the workspace row must end in the live command:\n%s", workspaceRow)
	}
	if !strings.Contains(card, "Also reachable: ../shared  ..") {
		t.Fatalf("the description line must name the reach in short form:\n%s", card)
	}
	// The card is a glance; relationship words belong to the deliberate query.
	if strings.Contains(card, "contains proj") {
		t.Fatalf("the card must not carry the detail form:\n%s", card)
	}
}

// Bare `/add-dir` is the deliberate question, so it answers with the whole
// truth: the absolute path, and how it sits against the workspace.
func TestAddDirListingAnswersWithPathAndRelationship(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.width = 200
	model.sessionWorkspace = &api.DigestWorkspace{ID: "ws_1", Name: "proj", Path: "/work/proj", Trusted: true}
	model.additionalRoots = []string{"/work/shared", "/work"}

	model.reportAddDirState("")
	listing := ""
	for _, message := range model.messages {
		if strings.Contains(message.Content, "Extra directories this session:") {
			listing = message.Content
		}
	}
	for _, want := range []string{"/work/shared  (../shared)", "/work  (contains proj)"} {
		if !strings.Contains(listing, want) {
			t.Errorf("/add-dir listing missing %q:\n%s", want, listing)
		}
	}
}
