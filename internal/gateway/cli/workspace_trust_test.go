package cli

import (
	"strings"
	"testing"

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
		{"untrusted and unanswered asks", &api.DigestWorkspace{Name: "proj", Path: "/p"}, true},
		{"trusted says nothing", &api.DigestWorkspace{Name: "proj", Path: "/p", Trusted: true}, false},
		{"declined is never asked again", &api.DigestWorkspace{Name: "proj", Path: "/p", TrustAsked: true}, false},
		{"no workspace says nothing", nil, false},
	} {
		model := NewController("", "", nil, "").model
		model.sessionWorkspace = tc.ws
		notice := model.untrustedWorkspaceNotice()
		if got := notice != ""; got != tc.asked {
			t.Errorf("%s: asked=%v want %v (notice=%q)", tc.name, got, tc.asked, notice)
		}
		if tc.asked {
			for _, want := range []string{"untrusted", "/ws trust", "/ws decline", "proj"} {
				if !strings.Contains(notice, want) {
					t.Errorf("%s: the question must name %q: %q", tc.name, want, notice)
				}
			}
		}
	}
}

// TestStartupCardNamesTheResolvedWorkspace: the card used to print a bare
// directory that could not say whether it was even a workspace, or whether it
// was trusted.
func TestStartupCardNamesTheResolvedWorkspace(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.width = 200
	model.sessionWorkspace = &api.DigestWorkspace{
		ID: "ws_1", Name: "proj", Path: "/work/proj", Trusted: false,
	}
	card := stripANSI(strings.Join(model.renderStartupCard(200), "\n"))
	for _, want := range []string{"proj", "/work/proj", "[untrusted]"} {
		if !strings.Contains(card, want) {
			t.Fatalf("startup card missing %q:\n%s", want, card)
		}
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
