package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
)

type attributionFixture struct {
	store      *control.Store
	observer   SkillAttributionObserver
	args       map[string]interface{}
	tenantID   string
	personID   string
	runID      string
	workUnitID string
	root       string
}

func newAttributionFixture(t *testing.T, packages ...string) attributionFixture {
	t.Helper()
	ResetSkillAttributionIndexCache()
	t.Cleanup(ResetSkillAttributionIndexCache)

	root := t.TempDir()
	for _, name := range packages {
		writeSkillPackage(t, filepath.Join(root, name), name, "package "+name)
	}
	isolatedSkillRoots(t, root)

	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "attribution", "Attribution")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "implicit use", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "implicit use")
	if err != nil {
		t.Fatal(err)
	}
	scope := kernel.ToolInvocationScope{
		ControlTenantID: identity.TenantID, PersonID: identity.PersonID,
		RunID: run.ID, WorkUnitID: run.WorkUnitID, ExecutionLane: "main",
	}
	return attributionFixture{
		store:    store,
		observer: NewSkillAttributionObserver(store),
		args: map[string]interface{}{
			"_context": ctx, "_invocation_scope": scope, "_tenant_id": identity.TenantID,
		},
		tenantID: identity.TenantID, personID: identity.PersonID,
		runID: run.ID, workUnitID: run.WorkUnitID, root: root,
	}
}

func (f attributionFixture) callArgs(extra map[string]interface{}) map[string]interface{} {
	args := map[string]interface{}{}
	for key, value := range f.args {
		args[key] = value
	}
	for key, value := range extra {
		args[key] = value
	}
	return args
}

func (f attributionFixture) summaries(t *testing.T) map[string]int {
	t.Helper()
	summaries, err := f.store.SkillAttributionSummaries(context.Background(), f.tenantID)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, summary := range summaries {
		counts[summary.SkillName] = summary.Attributions
	}
	return counts
}

// Two distinct Skills read in one work unit yield two records; the same Skill
// read twice yields one.
func TestAttributionCountsPerSkillWithinOneWorkUnit(t *testing.T) {
	f := newAttributionFixture(t, "alpha-flow", "beta-flow")

	f.observer("skill_view", f.callArgs(map[string]interface{}{"name": "alpha-flow"}))
	f.observer("skill_view", f.callArgs(map[string]interface{}{"name": "beta-flow"}))
	f.observer("skill_view", f.callArgs(map[string]interface{}{"name": "alpha-flow"}))

	counts := f.summaries(t)
	if counts["alpha-flow"] != 1 || counts["beta-flow"] != 1 {
		t.Fatalf("unexpected counts: %v", counts)
	}
}

// Reading an activated Skill's own content is that activation's progressive
// disclosure, so it is suppressed; another Skill in the same work unit is real
// implicit use and still counts.
func TestAttributionSuppressedForTheActivatedSkill(t *testing.T) {
	f := newAttributionFixture(t, "alpha-flow", "beta-flow")

	activated, err := findSkill(f.tenantID, "alpha-flow", f.args)
	if err != nil {
		t.Fatal(err)
	}
	skillKey, err := resolvedSkillKey(f.tenantID, activated)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertTestActivation(f, skillKey, "alpha-flow"); err != nil {
		t.Fatal(err)
	}

	f.observer("skill_view", f.callArgs(map[string]interface{}{"name": "alpha-flow"}))
	f.observer("skill_view", f.callArgs(map[string]interface{}{"name": "beta-flow"}))

	counts := f.summaries(t)
	if _, present := counts["alpha-flow"]; present {
		t.Fatalf("activated Skill was attributed: %v", counts)
	}
	if counts["beta-flow"] != 1 {
		t.Fatalf("unrelated Skill lost its attribution: %v", counts)
	}
}

// A path argument that lands inside a package directory is implicit use; one
// outside every package is not.
func TestAttributionMatchesPathArgumentsInsideAPackage(t *testing.T) {
	f := newAttributionFixture(t, "alpha-flow")

	inside := filepath.Join(f.root, "alpha-flow", "references", "detail.md")
	if err := os.MkdirAll(filepath.Dir(inside), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(inside, "# Detail\n"); err != nil {
		t.Fatal(err)
	}
	f.observer("read_file", f.callArgs(map[string]interface{}{"path": inside}))
	if counts := f.summaries(t); counts["alpha-flow"] != 1 {
		t.Fatalf("path inside the package was not attributed: %v", counts)
	}

	outside := filepath.Join(t.TempDir(), "unrelated.md")
	if err := atomicWriteFile(outside, "unrelated\n"); err != nil {
		t.Fatal(err)
	}
	f.observer("read_file", f.callArgs(map[string]interface{}{"path": outside}))
	if counts := f.summaries(t); len(counts) != 1 {
		t.Fatalf("an unrelated path was attributed: %v", counts)
	}
}

// The reason attribution lives in the control store: its usual subject is a
// package on a read-only root, where a sidecar usage file cannot be written.
func TestAttributionSucceedsForAPackageOnAReadOnlyRoot(t *testing.T) {
	f := newAttributionFixture(t, "alpha-flow")

	info, err := findSkill(f.tenantID, "alpha-flow", f.args)
	if err != nil {
		t.Fatal(err)
	}
	if info.Writable {
		t.Fatalf("fixture root should be read-only: %+v", info)
	}
	f.observer("skill_view", f.callArgs(map[string]interface{}{"name": "alpha-flow"}))

	summaries, err := f.store.SkillAttributionSummaries(context.Background(), f.tenantID)
	if err != nil || len(summaries) != 1 {
		t.Fatalf("attribution missing on a read-only root: %d %v", len(summaries), err)
	}
	stats, err := f.store.SkillUsageStats(context.Background(), f.tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].Attributions != 1 || stats[0].Calls != 0 {
		t.Fatalf("unexpected stats projection: %+v", stats)
	}
}

// Without work-unit identity there is nothing to attribute to.
func TestAttributionRequiresWorkUnitIdentityAtTheObserver(t *testing.T) {
	f := newAttributionFixture(t, "alpha-flow")
	scope := kernel.ToolInvocationScope{ControlTenantID: f.tenantID, PersonID: "person-1"}
	f.observer("skill_view", map[string]interface{}{
		"_context": context.Background(), "_invocation_scope": scope,
		"_tenant_id": f.tenantID, "name": "alpha-flow",
	})
	if counts := f.summaries(t); len(counts) != 0 {
		t.Fatalf("attributed without work-unit identity: %v", counts)
	}
}

type attributionProbeTool struct {
	BaseTool
	fail bool
}

func (t *attributionProbeTool) Execute(map[string]interface{}) (string, error) {
	if t.fail {
		return "", errors.New("probe failed")
	}
	return "ok", nil
}

// Attribution means content actually reached the model, so it is observed at the
// single registry execution path and only after a call completes.
func TestAttributionObserverFiresFromRegistryOnlyOnSuccess(t *testing.T) {
	f := newAttributionFixture(t, "alpha-flow")

	probe := &attributionProbeTool{BaseTool: BaseTool{
		name: "skill_view",
		schema: ToolSchema{Type: "object", Properties: map[string]PropertyDef{
			"name": {Type: "string"},
		}},
	}}
	registry := NewRegistry()
	registry.Register(probe)
	registry.InjectSkillAttributionObserver(f.observer)

	probe.fail = true
	if _, err := registry.Dispatch("skill_view", f.callArgs(map[string]interface{}{"name": "alpha-flow"})); err == nil {
		t.Fatal("probe should have failed")
	}
	if counts := f.summaries(t); len(counts) != 0 {
		t.Fatalf("a failed call was attributed: %v", counts)
	}

	probe.fail = false
	if _, err := registry.Dispatch("skill_view", f.callArgs(map[string]interface{}{"name": "alpha-flow"})); err != nil {
		t.Fatal(err)
	}
	if counts := f.summaries(t); counts["alpha-flow"] != 1 {
		t.Fatalf("a completed call was not attributed: %v", counts)
	}
}

func insertTestActivation(f attributionFixture, skillKey, skillName string) error {
	_, err := f.store.ActivateSkill(context.Background(), control.ActivateSkillInput{
		IdentityTenantID: f.tenantID, ControlTenantID: f.tenantID, PersonID: f.personID,
		RunID: f.runID, WorkUnitID: f.workUnitID, ExecutionLane: "main",
		SkillKey: skillKey, SkillName: skillName, VersionHash: "v1",
		ActivationSource: "model", CreatedBy: "test",
	})
	return err
}
