package tools

import (
	"os"
	"path/filepath"
	"testing"

	"selfmind/internal/executionenv"
)

func TestUntrustedWorkspaceSkillsAreNotVisible(t *testing.T) {
	workspace := t.TempDir()
	workspaceSkills := filepath.Join(workspace, ".selfmind", "skills")
	if err := os.MkdirAll(workspaceSkills, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	t.Setenv("SELFMIND_SKILLS_ROOTS", "")
	t.Setenv("SELFMIND_SKILLS_DIR", "")

	cleanup := SetExecutionScope("person-skill", ExecutionScope{
		WorkspaceRoot: workspace,
		TrustLevel:    executionenv.TrustUntrusted,
	})
	roots, err := SkillRootsForTenant("person-skill")
	cleanup()
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range roots {
		if root.Path == workspaceSkills {
			t.Fatalf("untrusted workspace skill root leaked into catalog: %#v", root)
		}
	}

	cleanup = SetExecutionScope("person-skill", ExecutionScope{
		WorkspaceRoot: workspace,
		TrustLevel:    executionenv.TrustTrusted,
	})
	defer cleanup()
	roots, err = SkillRootsForTenant("person-skill")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, root := range roots {
		if root.Path == workspaceSkills {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("trusted workspace skill root missing: %#v", roots)
	}
}

func TestSkillCredentialShapedEnvironmentDeclarationFailsClosed(t *testing.T) {
	for _, content := range []string{
		"---\nname: unsafe\nrequired_env:\n  - ANTHROPIC_API_KEY\n---\nbody\n",
		"---\nname: unsafe\nenv_passthrough: [GH_TOKEN]\n---\nbody\n",
	} {
		if err := validateSkillEnvironmentDeclarations(content); err == nil {
			t.Fatalf("credential-shaped declaration was accepted:\n%s", content)
		}
	}
	if err := validateSkillEnvironmentDeclarations(
		"---\nname: safe\nrequired_env:\n  - LANG\n  - CI\n---\nbody\n",
	); err != nil {
		t.Fatalf("ordinary environment declaration rejected: %v", err)
	}
}
