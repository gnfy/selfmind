package envprofiles

import (
	"fmt"
	"sort"
	"strings"
)

// Match resolves the profiles needed by a command's program set, including
// transitive RequiresProfiles. Programs the catalog does not know are ignored.
//
// The caller supplies the program set because shell parsing belongs to the tool
// layer. Deriving it from the FIRST token instead is what produced approval
// grants keyed on `set` and `for`, so profile matching deliberately looks at
// EVERY program a command will run.
func Match(programs []string) []*EnvProfile {
	selected := make(map[string]*EnvProfile)
	for _, program := range programs {
		name := strings.ToLower(strings.TrimSpace(program))
		if name == "" {
			continue
		}
		profile, ok := byExecutable[name]
		if !ok {
			continue
		}
		addWithRequires(selected, profile, 0)
	}
	if len(selected) == 0 {
		return nil
	}
	ids := make([]string, 0, len(selected))
	for id := range selected {
		ids = append(ids, id)
	}
	// Stable order: a dependency before its dependent, then alphabetical. The
	// order matters because a dependent profile's redirects may point into the
	// directories its dependency creates.
	sort.Slice(ids, func(i, j int) bool {
		left, right := selected[ids[i]], selected[ids[j]]
		leftDepends := dependsOn(left, right.ID)
		rightDepends := dependsOn(right, left.ID)
		if leftDepends != rightDepends {
			return rightDepends
		}
		return ids[i] < ids[j]
	})
	out := make([]*EnvProfile, 0, len(ids))
	for _, id := range ids {
		out = append(out, selected[id])
	}
	return out
}

func addWithRequires(selected map[string]*EnvProfile, profile *EnvProfile, depth int) {
	if profile == nil || depth > 8 {
		return
	}
	if _, exists := selected[profile.ID]; exists {
		return
	}
	selected[profile.ID] = profile
	for _, required := range profile.RequiresProfiles {
		addWithRequires(selected, byID[required], depth+1)
	}
}

func dependsOn(profile *EnvProfile, id string) bool {
	if profile == nil {
		return false
	}
	for _, required := range profile.RequiresProfiles {
		if required == id {
			return true
		}
		if dependsOn(byID[required], id) {
			return true
		}
	}
	return false
}

// ByID exposes one profile for diagnostics.
func ByID(id string) (*EnvProfile, bool) {
	profile, ok := byID[strings.TrimSpace(id)]
	return profile, ok
}

// IDs lists every profile in the catalog, for diagnostics.
func IDs() []string {
	ids := make([]string, 0, len(Catalog))
	for i := range Catalog {
		ids = append(ids, Catalog[i].ID)
	}
	sort.Strings(ids)
	return ids
}

// conflictError reports two profiles redirecting the same variable to different
// targets. It is a hard failure: silently picking one would give the command an
// environment neither profile describes.
type conflictError struct {
	Variable string
	First    string
	Second   string
}

func (e *conflictError) Error() string {
	return fmt.Sprintf("environment profile conflict: %s is redirected to two different targets (%s and %s)",
		e.Variable, e.First, e.Second)
}
