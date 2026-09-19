package verification

import "testing"

func scopedCheck(id, status string, paths ...string) Check {
	return Check{ToolCallID: id, Status: status, StartedAt: 10, FinishedAt: 20, Binding: &Binding{Version: 2, Criterion: "observable condition", Target: id, LocalDependencies: &paths}}
}

func TestDeclaredInputsBoundInvalidation(t *testing.T) {
	for _, test := range []struct {
		name     string
		checks   []Check
		mutation Mutation
		want     string
	}{
		{"independent report", []Check{scopedCheck("service", "succeeded", "/project/config")}, Mutation{"/project/report.md", 30}, "passed"},
		{"changed config", []Check{scopedCheck("service", "succeeded", "/project/config")}, Mutation{"/project/config", 30}, "stale"},
		{"changed descendant", []Check{scopedCheck("build", "succeeded", "/project/src")}, Mutation{"/project/src/lib/item.rs", 30}, "stale"},
		{"similar path is different", []Check{scopedCheck("build", "succeeded", "/project/src")}, Mutation{"/project/src-other/item", 30}, "passed"},
		{"external observation", []Check{scopedCheck("external", "succeeded", []string{}...)}, Mutation{"/project/receipt", 30}, "passed"},
		{"unknown effect", []Check{scopedCheck("external", "succeeded", []string{}...)}, Mutation{"", 30}, "stale"},
		{"unresolved effect", []Check{scopedCheck("external", "succeeded", []string{}...)}, Mutation{"relative/path", 30}, "stale"},
		{"unrelated pass cannot hide stale", []Check{scopedCheck("a", "succeeded", "/project/a"), scopedCheck("b", "succeeded", "/project/b")}, Mutation{"/project/a", 30}, "stale"},
		{"failed check is not erased", []Check{scopedCheck("a", "failed", "/project/a"), scopedCheck("b", "succeeded", "/project/b")}, Mutation{"/project/report", 30}, "failed"},
		{"historical conservative", []Check{{Status: "succeeded", StartedAt: 10, FinishedAt: 20, Binding: &Binding{Version: 1, Criterion: "same", Target: "external"}}}, Mutation{"/project/report", 30}, "stale"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got, _ := StateWithMutations([]Mutation{test.mutation}, test.checks); got != test.want {
				t.Fatalf("got %s want %s", got, test.want)
			}
		})
	}
}

func TestReplacementCannotNarrowDeclaredInputs(t *testing.T) {
	old := scopedCheck("old", "failed", "/project/src")
	next := old
	next.ToolCallID = "next"
	next.StartedAt = 30
	next.FinishedAt = 40
	b := *old.Binding
	next.Binding = &b
	b.Replaces = "old"
	b.Reason = "correct probe method"
	if !CanReplace(old, next) {
		t.Fatal("same obligation could not be retried")
	}
	empty := []string{}
	b.LocalDependencies = &empty
	if CanReplace(old, next) {
		t.Fatal("replacement silently dropped dependency")
	}
	b.Version = 1
	b.LocalDependencies = nil
	if CanReplace(old, next) {
		t.Fatal("replacement changed contract version")
	}
}
