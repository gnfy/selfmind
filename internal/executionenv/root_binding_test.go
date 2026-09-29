package executionenv

import "testing"

func TestRootBindingSnapshotCopiesAndComparesBaselineValues(t *testing.T) {
	original := []RootBinding{{Path: "/repo", Role: RootRolePrimary, AccessCap: RootAccessWrite, Source: RootSourceWorkspace,
		GitBaseline: &GitBaseline{Root: "/repo", CommonDir: "/repo/.git", Commit: "abc"}}}
	clone := CloneRootBindings(original)
	if !EqualRootBindings(original, clone) || original[0].GitBaseline == clone[0].GitBaseline {
		t.Fatalf("baseline was not independently copied: original=%+v clone=%+v", original, clone)
	}
	clone[0].GitBaseline.Commit = "def"
	if EqualRootBindings(original, clone) || original[0].GitBaseline.Commit != "abc" {
		t.Fatal("baseline mutation changed an admitted root or compared equal")
	}
}
