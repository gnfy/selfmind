package tools

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"selfmind/internal/kernel"
	"selfmind/internal/verification"
)

func TestVerificationDependenciesNormalizeAndPreserveAliases(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	alias := filepath.Join(root, "alias")
	if err := os.WriteFile(source, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, alias); err != nil {
		t.Fatal(err)
	}
	b, err := prepareVerificationBinding(map[string]interface{}{
		"cwd": root, "criterion": "value", "target": "source", "local_dependencies": []string{"alias", "./alias"},
	})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	if b.Version != 2 || b.LocalDependencies == nil || !slices.Contains(*b.LocalDependencies, alias) || !slices.Contains(b.ResolvedLocalDependencies, canonical) || len(*b.LocalDependencies) != 1 {
		t.Fatalf("binding=%+v", b)
	}
	if got := verification.RelevantMutationAt(verification.Check{Binding: b}, []verification.Mutation{{Path: canonical, FinishedAt: 30}}); got != 30 {
		t.Fatal("direct referent mutation did not invalidate alias check")
	}
	// An atomic replacement writes the alias itself, leaving the old referent
	// unchanged. Observed hashes must follow the original invocation path.
	exec := EvidenceMiddleware()(func(map[string]interface{}) (kernel.ToolDispatchResult, error) {
		if err := os.Remove(alias); err != nil {
			return kernel.ToolDispatchResult{}, err
		}
		return kernel.ToolDispatchResult{}, os.WriteFile(alias, []byte("new"), 0600)
	})
	r, err := exec(map[string]interface{}{"_tool_name": "write_file", "path": alias})
	if err != nil {
		t.Fatal(err)
	}
	e := r.Evidence[0].Files[0]
	if e.Path != alias || e.ResolvedPath != canonical || e.BeforeSHA256 == e.AfterSHA256 {
		t.Fatalf("lost alias mutation: %+v", e)
	}
	// The same declared path must remain recheckable after its referent changes.
	next, err := prepareVerificationBinding(map[string]interface{}{"cwd": root, "criterion": "value", "target": "source", "local_dependencies": []string{"alias"}})
	if err != nil {
		t.Fatal(err)
	}
	next.Replaces, next.Reason = "old", "recheck input after replacement"
	if !verification.CanReplace(verification.Check{ToolCallID: "old", Binding: b, FinishedAt: 10}, verification.Check{ToolCallID: "next", Binding: next, StartedAt: 20}) {
		t.Fatal("changed referent stranded the same declared input")
	}
}

func TestVerificationDependencyContractVersions(t *testing.T) {
	for _, test := range []struct {
		name    string
		deps    interface{}
		present bool
		version int
		invalid bool
	}{
		{"omitted", nil, false, 1, false},
		{"explicit external", []string{}, true, 2, false},
		{"empty path", []string{""}, true, 0, true},
		{"too many", make([]string, 33), true, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := map[string]interface{}{"criterion": "value", "target": "source"}
			if test.present {
				args["local_dependencies"] = test.deps
			}
			args["cwd"] = t.TempDir()
			b, err := prepareVerificationBinding(args)
			if (err != nil) != test.invalid {
				t.Fatalf("err=%v", err)
			}
			if err == nil && b.Version != test.version {
				t.Fatalf("version=%d", b.Version)
			}
			if err == nil && len(b.ResolvedLocalDependencies) > 0 {
				t.Fatal("caller supplied runtime referents")
			}
		})
	}
}

func TestVerificationDependenciesRespectExecutionScope(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	cleanup := SetExecutionScope("dependency-scope", ExecutionScope{WorkspaceRoot: root, AllowedRoots: []string{root}})
	defer cleanup()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{outside, "escape/file"} {
		_, err := prepareVerificationBinding(map[string]interface{}{"_tenant_id": "dependency-scope", "cwd": root, "criterion": "value", "target": "source", "local_dependencies": []string{path}})
		if err == nil {
			t.Fatalf("accepted out-of-scope dependency %q", path)
		}
	}
}

func TestMissingDependencyCannotClaimUnrelatedMutation(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"wrong-root/input.txt", "nested/nested/config.json"} {
		b, err := prepareVerificationBinding(map[string]interface{}{"cwd": root, "criterion": "input is valid", "target": "input", "local_dependencies": []string{path}})
		if err != nil {
			t.Fatal(err)
		}
		got := verification.RelevantMutationAt(verification.Check{Binding: b}, []verification.Mutation{{Path: filepath.Join(root, "actual-input"), FinishedAt: 30}})
		if got != 30 {
			t.Fatalf("unresolved dependency %q claimed independence from actual input mutation", path)
		}
	}
}
