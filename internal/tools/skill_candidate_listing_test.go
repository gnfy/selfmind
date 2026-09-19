package tools

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
)

// `/skills candidates` is how a person finds a candidate the curator withheld.
// It listed nothing, ever: SanitizeSkillName is a NAME GENERATOR — it must
// always yield a usable directory name and substitutes "unnamed-skill" for
// empty input — so sanitizing the optional filter before checking it made the
// filter unconditional and excluded every candidate.
//
// The same misreading disabled the required-argument check on candidate_create,
// where a missing name could never be detected and became "unnamed-skill".
func TestCandidateListingShowsCandidatesWithoutAFilter(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for _, name := range []string{"release-ledger", "index-audit"} {
		if _, err := store.CreateSkillCandidateVersion(ctx, "default", "skill_"+name, name, "", "body", "ev-"+name, []string{"o1"}, map[string]string{}); err != nil {
			t.Fatal(err)
		}
	}
	tool := NewSkillLifecycleManageTool(store)
	call := func(args map[string]interface{}) string {
		args["_tenant_id"] = "default"
		args["_context"] = ctx
		out, err := tool.Execute(args)
		if err != nil {
			t.Fatalf("execute %v: %v", args["action"], err)
		}
		return out
	}

	all := call(map[string]interface{}{"action": "candidate_list"})
	for _, name := range []string{"release-ledger", "index-audit"} {
		if !strings.Contains(all, name) {
			t.Fatalf("candidate %q must be listed without a filter: %q", name, all)
		}
	}

	// The constraint that must change the result: a filter still filters.
	one := call(map[string]interface{}{"action": "candidate_list", "name": "release-ledger"})
	if !strings.Contains(one, "release-ledger") || strings.Contains(one, "index-audit") {
		t.Fatalf("an explicit filter must select one candidate: %q", one)
	}
	none := call(map[string]interface{}{"action": "candidate_list", "name": "nothing-here"})
	if !strings.Contains(none, "No pending") {
		t.Fatalf("a filter matching nothing must say so: %q", none)
	}
}

func TestCandidateCreateStillRequiresAName(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tool := NewSkillLifecycleManageTool(store)
	_, err = tool.Execute(map[string]interface{}{
		"action": "candidate_create", "skill_key": "skill_x", "content": "body",
		"evidence_set_hash": "ev-1", "_tenant_id": "default", "_context": context.Background(),
		"_invocation_scope": kernel.ToolInvocationScope{
			ControlTenantID: "default", SkillMutationMode: kernel.SkillMutationCandidateOnly,
		},
	})
	if err == nil {
		t.Fatal("a missing name must be refused, not renamed to unnamed-skill")
	}
	if !strings.Contains(err.Error(), "requires skill_key, name, content") {
		t.Fatalf("unexpected refusal: %v", err)
	}
}
