package cliapp

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
)

func TestMaintenancePruneSkillCandidateRefsPreviewsAppliesAndVerifies(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := control.OpenStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := store.ResolveOrCreateAccount(ctx, control.DefaultTenantID, "cli", "candidate-prune", "Candidate Prune")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.IssueSkillCandidateRef(ctx, control.IssueSkillCandidateRefInput{
		IdentityTenantID: identity.TenantID, ControlTenantID: identity.TenantID,
		PersonID: identity.PersonID, RunID: "missing-run", WorkUnitID: "missing-unit",
		SkillKey: "skill-key", SkillName: "flow", VersionHash: "version",
		PackageHash: "package", DescriptionHash: "description",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	run := func(extra ...string) string {
		t.Helper()
		out := &bytes.Buffer{}
		args := []string{"selfmind", "maintenance", "prune-skill-candidate-refs", "--data-dir", dataDir}
		args = append(args, extra...)
		app := &App{ctx: ctx, args: args, stdout: out, stderr: out}
		if handled, code := app.runMaintenanceCommandIfRequested(); !handled || code != 0 {
			t.Fatalf("handled=%v code=%d output=%s", handled, code, out.String())
		}
		return out.String()
	}
	if out := run(); !strings.Contains(out, "DRY-RUN terminal=0 orphan=1 deleted=0") || !strings.Contains(out, "Re-run with --apply") {
		t.Fatalf("dry-run output:\n%s", out)
	}
	if out := run("--apply"); !strings.Contains(out, "APPLIED terminal=0 orphan=1 deleted=1") ||
		!strings.Contains(out, "VERIFY terminal=0 orphan=0") || !strings.Contains(out, "doctor --verbose") {
		t.Fatalf("apply output:\n%s", out)
	}
	if out := run(); !strings.Contains(out, "DRY-RUN terminal=0 orphan=0 deleted=0") {
		t.Fatalf("idempotent preview output:\n%s", out)
	}
}
