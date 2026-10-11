package tools

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// Every agent worker owns a dispatcher, and a Skill change can arrive through
// any of them. The shared manager reloads the hidden per-Skill tools in all of
// them from whichever dispatcher made the change.
func TestSkillChangeThroughAnyDispatcherReachesEveryDispatcher(t *testing.T) {
	workers := newSkillWorkers(t, 3)
	NewSkillToolManager(workers...)

	manageSkill(t, workers[2], map[string]interface{}{"action": "create", "name": "worker-made", "content": "Follow the declared procedure."})
	if _, err := workers[0].Dispatch("skill_catalog", map[string]interface{}{"action": "install", "source": "official/release-checklist"}); err != nil {
		t.Fatalf("install through the first worker: %v", err)
	}
	requireSkillTools(t, workers, "skill:release-checklist", "skill:worker-made")
	first, _ := workers[0].GetTool("skill:worker-made")
	for index, worker := range workers[1:] {
		if tool, _ := worker.GetTool("skill:worker-made"); tool != first {
			t.Fatalf("worker %d holds its own copy of skill:worker-made; one load must serve every worker", index+2)
		}
	}

	manageSkill(t, workers[1], map[string]interface{}{"action": "disable", "name": "worker-made"})
	requireSkillTools(t, workers, "skill:release-checklist")
	manageSkill(t, workers[0], map[string]interface{}{"action": "enable", "name": "worker-made"})
	requireSkillTools(t, workers, "skill:release-checklist", "skill:worker-made")
	manageSkill(t, workers[2], map[string]interface{}{"action": "delete", "name": "release-checklist"})
	requireSkillTools(t, workers, "skill:worker-made")
}

// Each worker starts from the Skill tools its own construction loaded. The
// manager makes the first worker's set everyone's, so a Skill written while
// the workers were being built cannot leave them different.
func TestSkillToolManagerStartsEveryDispatcherFromTheFirst(t *testing.T) {
	workers := newSkillWorkers(t, 2)
	manageSkill(t, workers[0], map[string]interface{}{"action": "create", "name": "early", "content": "Follow the declared procedure."})
	requireSkillTools(t, workers[:1], "skill:early")
	requireSkillTools(t, workers[1:])

	manager := NewSkillToolManager(workers...)
	requireSkillTools(t, workers, "skill:early")
	same := true
	manager.HoldCatalog(func() {
		first, _ := workers[0].GetTool("skill:early")
		second, _ := workers[1].GetTool("skill:early")
		same = first == second
	})
	if !same {
		t.Fatal("the workers do not share the first worker's Skill tool")
	}
}

// Disabling a Skill takes its compatibility tool out even from a dispatcher
// without a manager, and enabling it puts the tool back.
func TestSkillReloadLeavesDisabledSkillsOut(t *testing.T) {
	workers := newSkillWorkers(t, 1)
	for _, name := range []string{"kept", "paused"} {
		manageSkill(t, workers[0], map[string]interface{}{"action": "create", "name": name, "content": "Follow the declared procedure."})
	}
	requireSkillTools(t, workers, "skill:kept", "skill:paused")
	manageSkill(t, workers[0], map[string]interface{}{"action": "disable", "name": "paused"})
	requireSkillTools(t, workers, "skill:kept")
	manageSkill(t, workers[0], map[string]interface{}{"action": "reload"})
	requireSkillTools(t, workers, "skill:kept")
	manageSkill(t, workers[0], map[string]interface{}{"action": "enable", "name": "paused"})
	requireSkillTools(t, workers, "skill:kept", "skill:paused")
}

// A Skill's compatibility address stays inside the skill: namespace, whatever
// its front matter declares, so no Skill can replace another tool and a
// reload replaces exactly the tools Skills registered.
func TestSkillCompatibilityToolStaysInTheSkillNamespace(t *testing.T) {
	workers := newSkillWorkers(t, 1)
	builtin := NewMockTool()
	builtin.name = "terminal"
	workers[0].RegisterTool(builtin)
	for _, skill := range []struct{ name, toolName string }{
		{"shadow-terminal", "terminal"},
		{"custom-address", "skill:custom-address-v2"},
	} {
		manageSkill(t, workers[0], map[string]interface{}{
			"action": "create", "name": skill.name,
			"content": "---\nname: " + skill.name + "\ndescription: namespace probe\ntool_name: " + skill.toolName + "\n---\n\nFollow the declared procedure.\n",
		})
	}
	if got, _ := workers[0].GetTool("terminal"); got != builtin {
		t.Fatalf("a Skill replaced the terminal tool: %#v", got)
	}
	requireSkillTools(t, workers, "skill:custom-address-v2", "skill:shadow-terminal")
	manageSkill(t, workers[0], map[string]interface{}{"action": "reload"})
	if got, _ := workers[0].GetTool("terminal"); got != builtin {
		t.Fatalf("a reload replaced the terminal tool: %#v", got)
	}
	requireSkillTools(t, workers, "skill:custom-address-v2", "skill:shadow-terminal")
}

// A reload swaps a dispatcher's Skill tools in one step: a catalogue read
// running beside it sees the old set or the new one, never a dispatcher that
// has dropped its Skill tools and not yet registered them again.
func TestSkillReloadSwapsTheSkillToolsInOneStep(t *testing.T) {
	for _, shared := range []bool{false, true} {
		name := "lone dispatcher"
		if shared {
			name = "shared by two workers"
		}
		t.Run(name, func(t *testing.T) {
			workers := newSkillWorkers(t, 2)
			if shared {
				NewSkillToolManager(workers...)
			}
			for _, skill := range []string{"alpha", "beta"} {
				manageSkill(t, workers[0], map[string]interface{}{"action": "create", "name": skill, "content": "Follow the declared procedure."})
			}
			manageSkill(t, workers[1], map[string]interface{}{"action": "reload"})
			requireSkillTools(t, workers, "skill:alpha", "skill:beta")

			done := make(chan struct{})
			var wg sync.WaitGroup
			for _, worker := range workers {
				wg.Add(1)
				go func(worker *Dispatcher) {
					defer wg.Done()
					for i := 0; i < 50; i++ {
						if _, err := worker.Dispatch("skill_manage", map[string]interface{}{"action": "reload"}); err != nil {
							t.Errorf("reload: %v", err)
							return
						}
					}
				}(worker)
			}
			partial := make(chan []string, 1)
			go func() {
				defer close(partial)
				for {
					select {
					case <-done:
						return
					default:
					}
					for _, worker := range workers {
						if got := catalogSkillTools(worker); strings.Join(got, ",") != "skill:alpha,skill:beta" {
							partial <- got
							return
						}
					}
				}
			}()
			wg.Wait()
			close(done)
			if got, ok := <-partial; ok {
				t.Fatalf("a catalogue read during a reload saw Skill tools %v, want both or the old set", got)
			}
		})
	}
}

// newSkillWorkers builds dispatchers that each carry the Skill management
// tools over one shared, isolated Skill store.
func newSkillWorkers(t *testing.T, count int) []*Dispatcher {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	// Outside a Run the working directory is a Skill root; keep the
	// checkout's own Skills out of these fixtures.
	t.Chdir(t.TempDir())
	storage, err := NewSkillStorage(filepath.Join(t.TempDir(), "skill-assets"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(storage.BaseDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	workers := make([]*Dispatcher, 0, count)
	for i := 0; i < count; i++ {
		worker := NewDispatcherWithRegistry(NewRegistry())
		worker.InjectMiddleware(SkillStorageMiddleware(storage))
		worker.RegisterTool(NewSkillManageTool())
		worker.RegisterTool(NewSkillCatalogTool())
		workers = append(workers, worker)
	}
	return workers
}

func manageSkill(t *testing.T, worker *Dispatcher, args map[string]interface{}) {
	t.Helper()
	if _, err := worker.Dispatch("skill_manage", directSkillMutationArgs(args)); err != nil {
		t.Fatalf("skill_manage %v: %v", args["action"], err)
	}
}

func requireSkillTools(t *testing.T, workers []*Dispatcher, want ...string) {
	t.Helper()
	sort.Strings(want)
	for index, worker := range workers {
		var got []string
		for _, name := range worker.ListTools() {
			if strings.HasPrefix(name, "skill:") {
				got = append(got, name)
			}
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("dispatcher %d Skill tools = %v, want %v", index+1, got, want)
		}
	}
}

// catalogSkillTools reads the Skill tools from the catalogue a worker would
// offer a Run, the same read a Run's model request makes.
func catalogSkillTools(worker *Dispatcher) []string {
	var names []string
	for _, definition := range worker.GetToolDefinitions() {
		function, _ := definition["function"].(map[string]interface{})
		if name, _ := function["name"].(string); strings.HasPrefix(name, "skill:") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
