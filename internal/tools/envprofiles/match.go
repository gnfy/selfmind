package envprofiles

import (
	"fmt"
	"os"
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
	return orderProfiles(selected)
}

// AvailableOnHost returns the operator-credential profiles whose declared host
// state actually exists on this machine.
//
// It is the answer to a question static analysis cannot decide: WHICH tools a
// command will really invoke. A program reached through an interpreter, a script
// file, `make`, a package-manager script, `find -exec`, or a background process
// is invisible to shell parsing — `python3 - <<'PY' … subprocess.run(['gcloud',
// …])` yields the program set {python3}, so gcloud received no credential state
// and the check could not authenticate. Preparing what the HOST has, once per
// lease, removes the entire class instead of adding one more parser.
//
// It deliberately covers only operator-credential profiles with real state:
// toolchain caches are meaningless without the tool and stay match-driven, and a
// profile whose state root does not exist has nothing to prepare. The trust
// decision is unchanged and still made in Apply, so this widens WHEN credential
// state is prepared, never WHO may have it.
func AvailableOnHost(ctx ApplyContext) []*EnvProfile {
	if ctx.Lookup == nil {
		ctx.Lookup = func(string) (string, bool) { return "", false }
	}
	selected := make(map[string]*EnvProfile)
	for i := range Catalog {
		profile := &Catalog[i]
		if profile.CredentialAccess != CredentialAccessOperator {
			continue
		}
		if !ctx.profileHasHostState(profile) {
			continue
		}
		// A profile the context cannot prepare must not be pulled in
		// speculatively: Apply treats a missing persistent-cache root as a hard
		// failure, which is right when the command NAMED the tool and wrong when
		// the inventory volunteered it — that would fail commands that never
		// mentioned it.
		if ctx.ToolchainRoot == "" && profileNeedsToolchainRoot(profile) {
			continue
		}
		addWithRequires(selected, profile, 0)
	}
	return orderProfiles(selected)
}

// profileNeedsToolchainRoot reports whether preparing the profile requires the
// person-level persistent cache root. It also inspects the profiles it requires,
// because a dependency is applied together with it.
func profileNeedsToolchainRoot(profile *EnvProfile) bool {
	if profile == nil {
		return false
	}
	for _, mapping := range profile.MapRW {
		if mapping.Persistent {
			return true
		}
	}
	for _, redirect := range profile.EnvRedirect {
		if redirect.Kind == TargetToolchain {
			return true
		}
	}
	for _, required := range profile.RequiresProfiles {
		if profileNeedsToolchainRoot(byID[required]) {
			return true
		}
	}
	return false
}

// profileHasHostState reports whether any state location the profile declares
// exists on this host.
func (c ApplyContext) profileHasHostState(profile *EnvProfile) bool {
	sources := make([]StateSource, 0, 4)
	for _, spec := range profile.CopyIn {
		sources = append(sources, spec.From)
	}
	for _, mapping := range profile.MapRO {
		sources = append(sources, mapping.From)
	}
	for _, mapping := range profile.MapRWAt {
		sources = append(sources, mapping.At)
	}
	for _, spec := range profile.SynthesizeDir {
		sources = append(sources, spec.At)
	}
	for _, source := range sources {
		for _, path := range c.resolveSources(source) {
			if path == "" {
				continue
			}
			if _, err := os.Stat(path); err == nil {
				return true
			}
		}
	}
	return false
}

// orderProfiles returns a stable order: a dependency before its dependent, then
// alphabetical. The order matters because a dependent profile's redirects may
// point into the directories its dependency creates.
func orderProfiles(selected map[string]*EnvProfile) []*EnvProfile {
	if len(selected) == 0 {
		return nil
	}
	ids := make([]string, 0, len(selected))
	for id := range selected {
		ids = append(ids, id)
	}
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

// Union merges two profile sets, keeping the stable dependency order. The AST
// match contributes what the command names; the host inventory contributes what
// it might reach indirectly.
func Union(sets ...[]*EnvProfile) []*EnvProfile {
	selected := make(map[string]*EnvProfile)
	for _, set := range sets {
		for _, profile := range set {
			if profile == nil {
				continue
			}
			selected[profile.ID] = profile
		}
	}
	return orderProfiles(selected)
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
