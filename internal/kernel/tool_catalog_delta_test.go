package kernel

import (
	"reflect"
	"testing"

	"selfmind/internal/kernel/llm"
)

// The exposed tool set is part of the provider's cached prefix, so changing it
// re-uploads the whole prompt: measured at about 43,000 extra uncached tokens
// against a 3,000-token baseline for an ordinary incremental call. Four days of
// traffic recorded 52 such changes and not one could be attributed, because the
// breakdown carried only a count and a hash.
//
// The delta is the evidence the deferral cohort and the exposure rules are
// tuned against, so it has to name the tools, not count them.
func TestToolCatalogDeltaNamesWhatChanged(t *testing.T) {
	defs := func(names ...string) []llm.ToolDefinition {
		out := make([]llm.ToolDefinition, 0, len(names))
		for _, name := range names {
			out = append(out, llm.ToolDefinition{Name: name})
		}
		return out
	}
	first := exposedToolNameSet(defs("terminal", "patch", "read_file"))

	// The first call of a turn has nothing to compare against; it is a baseline,
	// not a change, or every turn would report its whole catalog as new.
	if added, removed := toolCatalogDelta(nil, first); added != nil || removed != nil {
		t.Fatalf("first call must report no delta, got added=%v removed=%v", added, removed)
	}

	// An unchanged set is the common case and must stay silent: a per-call
	// record of 26 unchanged names would bury the 52 that matter.
	if added, removed := toolCatalogDelta(first, exposedToolNameSet(defs("read_file", "terminal", "patch"))); len(added) > 0 || len(removed) > 0 {
		t.Fatalf("reordering is not a change, got added=%v removed=%v", added, removed)
	}

	// Both directions are recorded. A tool LEAVING the catalog invalidates the
	// prefix exactly as a tool joining does, and the observed changes were
	// removals (27 tools became 26), which a join-only record would have missed
	// entirely.
	added, removed := toolCatalogDelta(first, exposedToolNameSet(defs("terminal", "read_file", "web_search")))
	if want := []string{"web_search"}; !reflect.DeepEqual(added, want) {
		t.Errorf("added = %v, want %v", added, want)
	}
	if want := []string{"patch"}; !reflect.DeepEqual(removed, want) {
		t.Errorf("removed = %v, want %v", removed, want)
	}
}

func TestSortedNamesIsDeterministic(t *testing.T) {
	set := map[string]bool{"zeta": true, "alpha": true, "mid": true}
	want := []string{"alpha", "mid", "zeta"}
	if got := sortedNames(set); !reflect.DeepEqual(got, want) {
		t.Fatalf("sortedNames = %v, want %v", got, want)
	}
	if got := sortedNames(nil); len(got) != 0 {
		t.Fatalf("empty set must render empty, got %v", got)
	}
}
