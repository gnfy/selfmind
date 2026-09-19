package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/kernel"
)

// repositorySkillFixture returns a workspace whose repository owns one Skill on
// a read-only root, plus the invocation args for a given mutation authority.
func repositorySkillFixture(t *testing.T, mode string) (string, map[string]interface{}) {
	t.Helper()
	ws := t.TempDir()
	writeSkillPackage(t, filepath.Join(ws, "repo", ".agents", "skills", "release"), "release", "Release procedure.")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SELFMIND_SKILLS_DIR", "")
	t.Setenv("SELFMIND_SKILLS_ROOTS", "")
	t.Chdir(t.TempDir())
	cleanup := SetExecutionScope("p", ExecutionScope{
		TenantID: "default", PersonID: "p", WorkspaceRoot: ws, WorkspaceID: "ws",
	})
	t.Cleanup(cleanup)
	args := map[string]interface{}{"_tenant_id": "p"}
	if mode != "" {
		args["_invocation_scope"] = kernel.ToolInvocationScope{
			ControlTenantID: "default", PersonID: "p", SkillMutationMode: mode,
		}
	}
	return ws, args
}

func repositorySkillBody(t *testing.T, ws string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(ws, "repo", ".agents", "skills", "release", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A repository's own Skill is the one the person actually uses, and it is the
// one nothing could ever change: the curator refused it at the source, so its
// incidents and verified recoveries were collected and never acted on.
//
// It stays closed to the background curator and to the model, and opens only
// for the explicit management surface, which the model cannot reach and only
// /v1/dispatch can set.
func TestRepositorySkillEditsOnlyUnderExplicitAuthority(t *testing.T) {
	ws, args := repositorySkillFixture(t, "")
	before := repositorySkillBody(t, ws)

	if _, err := editSkill("default", "release", "Corrected release procedure.", "Release procedure.", args); err == nil {
		t.Fatal("an unauthorized invocation must not rewrite a repository Skill")
	} else if !strings.Contains(err.Error(), "/skills promote") {
		t.Fatalf("the refusal must name the surface that can apply it, got %v", err)
	}
	if repositorySkillBody(t, ws) != before {
		t.Fatal("the refused edit still changed the file")
	}

	// The curator's own authority is not enough either: it governs assets this
	// runtime authored, not the person's repository.
	_, candidateArgs := repositorySkillFixture(t, kernel.SkillMutationCandidateOnly)
	if _, err := editSkill("default", "release", "Corrected release procedure.", "Release procedure.", candidateArgs); err == nil {
		t.Fatal("candidate-only authority must not rewrite a repository Skill")
	}
}

func TestRepositorySkillAcceptsAnExplicitlyAuthorizedEdit(t *testing.T) {
	ws, args := repositorySkillFixture(t, kernel.SkillMutationDirect)
	if _, err := editSkill("default", "release", "Corrected release procedure.", "Release procedure.", args); err != nil {
		t.Fatalf("an explicitly authorized apply must reach the repository Skill: %v", err)
	}
	body := repositorySkillBody(t, ws)
	if !strings.Contains(body, "Corrected release procedure.") {
		t.Fatalf("the repository Skill was not updated in place: %q", body)
	}
	// In place, under its own name: the old escape hatch was to copy the Skill
	// into a writable root, which makes the bare name ambiguous and breaks
	// both copies.
	skills := workspaceSkills(t, ws)
	if matches := matchSkillsByName(skills, "release"); len(matches) != 1 {
		t.Fatalf("the applied version must not fork the Skill, got %d matches", len(matches))
	}
}

// Applying a reviewed version is not a licence to remove the person's asset.
// Only the edit path opens; deletion and archiving stay closed.
func TestExplicitAuthorityDoesNotOpenDestructiveSkillOperations(t *testing.T) {
	ws, args := repositorySkillFixture(t, kernel.SkillMutationDirect)
	if _, err := deleteSkill("default", "release", args); err == nil {
		t.Fatal("explicit authority must not delete a repository Skill")
	}
	if _, err := ArchiveSkillForTenant("default", "release", args); err == nil {
		t.Fatal("explicit authority must not archive a repository Skill")
	}
	if body := repositorySkillBody(t, ws); !strings.Contains(body, "Body of release") {
		t.Fatalf("the repository Skill was altered: %q", body)
	}
}

// A pin is the person saying this content is not to be rewritten. It outranks
// the explicit apply, and it does so before any evidence is consulted.
func TestPinnedRepositorySkillRefusesEvenExplicitAuthority(t *testing.T) {
	ws, args := repositorySkillFixture(t, kernel.SkillMutationDirect)
	pinned := SkillInfo{Name: "release", Path: filepath.Join(ws, "repo", ".agents", "skills", "release"), Pinned: true}
	if err := ensureSkillEditAuthorized(pinned, "editing it", args); err == nil {
		t.Fatal("a pinned Skill must refuse an explicitly authorized edit")
	}
}

// No sidecar may appear in the person's working tree as a side effect.
func TestRepositorySkillEditLeavesNoUsageSidecar(t *testing.T) {
	ws, args := repositorySkillFixture(t, kernel.SkillMutationDirect)
	if _, err := editSkill("default", "release", "Corrected release procedure.", "Release procedure.", args); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(ws, "repo", ".agents", "skills")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") && strings.Contains(entry.Name(), "usage") {
			t.Fatalf("an untracked sidecar appeared in the repository: %s", entry.Name())
		}
	}
}
