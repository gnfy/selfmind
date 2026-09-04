package cli

import (
	"strings"
	"testing"
)

// TestOnboardingDoesNotPinTheSessionWorkspace pins the fix for a session that
// silently worked in the wrong directory.
//
// The onboarding file records the workspace chosen at first use. Seeding the
// SESSION override from it made every launch send that WorkspaceID on every
// turn, and the gateway prefers an explicit WorkspaceID over the launch
// directory — so a session started in a project ran somewhere else, entering a
// new directory never created a workspace for it, and the startup card
// disagreed with /ws about which workspace was current.
func TestOnboardingDoesNotPinTheSessionWorkspace(t *testing.T) {
	c := NewController("", "", nil, "")
	c.SetOnboardingContext(OnboardingContext{
		BackgroundModel:  "some-model",
		WorkspaceID:      "ws_from_onboarding",
		WorkspaceName:    "workspace",
		WorkspacePath:    "/Users/someone/Workspace/workspace",
		FirstTaskPending: false,
	})
	m := c.model
	if m.workspaceOverrideID != "" {
		t.Fatalf("onboarding pinned the session workspace: %q", m.workspaceOverrideID)
	}
	if m.workspaceOverridePath != "" || m.workspaceOverrideName != "" {
		t.Fatalf("onboarding pinned the displayed workspace: %q %q",
			m.workspaceOverrideName, m.workspaceOverridePath)
	}
	if m.backgroundModelName != "some-model" {
		t.Fatalf("onboarding must still supply what it is about: %q", m.backgroundModelName)
	}
}

// TestWorkspaceSelectStillSetsTheSessionOverride: the override fields exist for
// an explicit /ws switch, and that must keep working — otherwise removing the
// onboarding pin would take the real feature with it.
func TestWorkspaceSelectStillSetsTheSessionOverride(t *testing.T) {
	model := NewController("", "", nil, "").model
	updated, _ := model.Update(MsgWorkspaceSwitched{
		ID: "ws_chosen", Name: "chosen", Path: "/tmp/chosen", Reply: "Current workspace: chosen",
	})
	m := updated.(*uiModel)
	if m.workspaceOverrideID != "ws_chosen" || m.workspaceOverridePath != "/tmp/chosen" {
		t.Fatalf("an explicit switch must pin the session: %q %q",
			m.workspaceOverrideID, m.workspaceOverridePath)
	}
}

// TestStartupCardShowsTheLaunchDirectory: with no explicit switch, the card must
// name the directory the session actually runs in. It used to show whatever
// directory onboarding ran in, which is how the mismatch stayed invisible.
func TestStartupCardShowsTheLaunchDirectory(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.width = 200
	card := stripANSI(strings.Join(model.renderStartupCard(200), "\n"))
	cwd := currentWorkingDir()
	if !strings.Contains(card, cwd) {
		t.Fatalf("startup card must name the launch directory %s:\n%s", cwd, card)
	}
}
