package httpapi

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/kernel"
	"selfmind/internal/tools"
)

// directWriterRoots is the binding a Run working in the original checkout
// holds: its sandbox may write the checkout's .git, which host-side delivery
// and prune then read.
func directWriterRoots(t *testing.T, path string) []executionenv.RootBinding {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return []executionenv.RootBinding{{Path: canonical, Role: executionenv.RootRolePrimary,
		AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceWorkspace, ContextRoot: true}}
}

func TestApplyAndPruneWaitForWritersOfTheSourceCheckout(t *testing.T) {
	type writer struct {
		name  string
		start func(t *testing.T, f gitViewCommandFixture) (blocker string, release func())
	}
	durable := func(status string, pathOf func(f gitViewCommandFixture) string) func(t *testing.T, f gitViewCommandFixture) (string, func()) {
		return func(t *testing.T, f gitViewCommandFixture) (string, func()) {
			ctx := context.Background()
			run, err := f.store.StartRunWithOptions(ctx, f.task, "direct-session", "edit the checkout directly",
				control.StartRunOptions{ExecutionRoots: directWriterRoots(t, pathOf(f))})
			if err != nil {
				t.Fatal(err)
			}
			if status != "running" {
				if err := f.store.FinishRun(ctx, f.owner.TenantID, run.ID, status); err != nil {
					t.Fatal(err)
				}
			}
			return run.ID, func() {
				if err := f.store.FinishRun(ctx, f.owner.TenantID, run.ID, "done"); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	checkout := func(f gitViewCommandFixture) string { return f.root }
	writers := []writer{
		{"running-direct-writer", durable("running", checkout)},
		{"parked-direct-writer", durable("waiting_user", checkout)},
		{"writer-of-a-parent-directory", durable("running", func(f gitViewCommandFixture) string { return filepath.Dir(f.root) })},
		{"admission-in-flight", func(t *testing.T, f gitViewCommandFixture) (string, func()) {
			active := &activeRun{TenantID: f.owner.TenantID, PersonID: f.owner.PersonID, Platform: "cli", Channel: "direct-session",
				ExecutionRoots: directWriterRoots(t, f.root)}
			if !f.daemon.coordinator().beginActive(f.owner.PersonID, active) {
				t.Fatal("could not register the in-flight admission")
			}
			return "starting", func() { f.daemon.coordinator().endActiveRun(f.owner.PersonID, active) }
		}},
	}
	for _, w := range writers {
		t.Run(w.name, func(t *testing.T) {
			f := newGitViewCommandFixture(t, "done")
			blocker, release := w.start(t, f)
			reply, err := f.command(t, "/apply "+f.run.ID)
			if err == nil || !strings.Contains(err.Error(), blocker) {
				t.Fatalf("/apply ran host-side Git on a checkout another Run may still write: %q %v", reply, err)
			}
			if out := gitAdmissionOutput(t, f.root, "for-each-ref", "--format=%(refname)", "refs/heads/selfmind"); out != "" {
				t.Fatalf("refused delivery still created %s", out)
			}
			release()
			if _, err := f.command(t, "/apply "+f.run.ID); err != nil {
				t.Fatalf("delivery stayed blocked after the writer finished: %v", err)
			}
			if _, err := f.command(t, "/views archive "+f.run.ID); err != nil {
				t.Fatal(err)
			}
			blocker, release = w.start(t, f)
			defer release()
			if reply, err := f.command(t, "/views prune "+f.run.ID); err == nil || !strings.Contains(err.Error(), blocker) {
				t.Fatalf("prune wrote a protected ref while another Run may still write the checkout: %q %v", reply, err)
			}
			if out := gitAdmissionOutput(t, f.root, "for-each-ref", "--format=%(refname)", "refs/selfmind/retained"); out != "" {
				t.Fatalf("refused prune still created %s", out)
			}
		})
	}
}

func TestApplyWaitsForABackgroundProcessOfAFinishedSourceWriter(t *testing.T) {
	ctx := context.Background()
	f := newGitViewCommandFixture(t, "done")
	run, err := f.store.StartRunWithOptions(ctx, f.task, "direct-session", "start a watcher in the checkout",
		control.StartRunOptions{ExecutionRoots: directWriterRoots(t, f.root)})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.FinishRun(ctx, f.owner.TenantID, run.ID, "done"); err != nil {
		t.Fatal(err)
	}
	registry := tools.ProcessRegistryForArgs(map[string]interface{}{
		"_invocation_scope": kernel.ToolInvocationScope{ControlTenantID: f.owner.TenantID, PersonID: f.owner.PersonID, RunID: run.ID},
	})
	id, err := registry.StartProcess("sleep 60", f.root, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Kill(id) })
	reply, err := f.command(t, "/apply "+f.run.ID)
	if err == nil || !strings.Contains(err.Error(), id) || !strings.Contains(err.Error(), run.ID) {
		t.Fatalf("/apply ignored a live background process of the checkout's writer: %q %v", reply, err)
	}
	if err := registry.Kill(id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.command(t, "/apply "+f.run.ID); err != nil {
		t.Fatalf("delivery stayed blocked after the background process ended: %v", err)
	}
}

func gitAdmissionOutput(t *testing.T, root string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
