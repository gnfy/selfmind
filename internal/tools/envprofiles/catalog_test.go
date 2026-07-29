package envprofiles

import (
	"path/filepath"
	"strings"
	"testing"
)

// The catalog is behaviour data compiled into the binary, so its invariants are
// enforced at build time rather than discovered at runtime. A malformed entry
// must fail CI, never a user's command.
func TestCatalogInvariants(t *testing.T) {
	seenIDs := map[string]bool{}
	seenExecutables := map[string]string{}

	for i := range Catalog {
		profile := &Catalog[i]
		t.Run(profile.ID, func(t *testing.T) {
			if strings.TrimSpace(profile.ID) == "" {
				t.Fatal("profile id is required")
			}
			if seenIDs[profile.ID] {
				t.Fatalf("duplicate profile id %q", profile.ID)
			}
			seenIDs[profile.ID] = true

			if len(profile.MatchExecutables) == 0 {
				t.Fatal("a profile must match at least one executable")
			}
			for _, executable := range profile.MatchExecutables {
				if executable != strings.ToLower(strings.TrimSpace(executable)) {
					t.Fatalf("executable %q must be lowercase and trimmed", executable)
				}
				// Globally unique: one executable mapping to two profiles would
				// make matching ambiguous, which is exactly the class of defect
				// that produced grant keys named after shell prologues.
				if owner, exists := seenExecutables[executable]; exists {
					t.Fatalf("executable %q is claimed by both %q and %q", executable, owner, profile.ID)
				}
				seenExecutables[executable] = profile.ID
			}

			switch profile.CredentialAccess {
			case CredentialAccessOperator, CredentialAccessToolchain, CredentialAccessNone:
			default:
				t.Fatalf("unknown credential access %q", profile.CredentialAccess)
			}

			for _, required := range profile.RequiresProfiles {
				if _, ok := byID[required]; !ok {
					t.Fatalf("requires unknown profile %q", required)
				}
				if required == profile.ID {
					t.Fatal("a profile must not require itself")
				}
			}
			if hasRequireCycle(profile, map[string]bool{}) {
				t.Fatal("requires graph has a cycle")
			}

			for _, spec := range profile.CopyIn {
				if len(spec.Include) == 0 {
					t.Fatal("copy_in needs an explicit include list; an empty list means 'everything'")
				}
				if spec.MaxBytes <= 0 || spec.MaxFiles <= 0 || spec.MaxDepth <= 0 {
					t.Fatalf("copy_in needs positive bounds, got bytes=%d files=%d depth=%d",
						spec.MaxBytes, spec.MaxFiles, spec.MaxDepth)
				}
				if spec.From.EnvVar == "" && spec.From.HomeRelPath == "" {
					t.Fatal("copy_in source needs an env var or a home-relative path")
				}
				assertSafeRel(t, spec.From.HomeRelPath)
				for _, pattern := range append(append([]string{}, spec.Include...), spec.Exclude...) {
					if strings.Contains(pattern, "..") || strings.HasPrefix(pattern, "/") {
						t.Fatalf("pattern %q must be relative and must not traverse", pattern)
					}
				}
			}
			for _, mapping := range profile.MapRO {
				if mapping.From.EnvVar == "" && mapping.From.HomeRelPath == "" {
					t.Fatal("map_ro source needs an env var or a home-relative path")
				}
				assertSafeRel(t, mapping.From.HomeRelPath)
			}
			for _, mapping := range profile.MapRW {
				assertSafeRel(t, mapping.Key)
				if strings.TrimSpace(mapping.Key) == "" {
					t.Fatal("map_rw needs a key")
				}
			}
			for _, redirect := range profile.EnvRedirect {
				if strings.TrimSpace(redirect.Name) == "" {
					t.Fatal("env_redirect needs a variable name")
				}
				assertSafeRel(t, redirect.RelPath)
				switch redirect.Kind {
				case TargetLeaseState, TargetToolchain, TargetScratch, TargetHostPath:
				default:
					t.Fatalf("unknown redirect target kind %d", redirect.Kind)
				}
				// A toolchain redirect only makes sense for a persistent mapping,
				// and vice versa: mismatching them would put a build cache in a
				// directory deleted after every run.
				if redirect.Kind == TargetToolchain && !hasPersistentKey(profile, redirect.RelPath) {
					t.Fatalf("redirect %s targets the toolchain root but no persistent map_rw declares %q",
						redirect.Name, redirect.RelPath)
				}
			}
			// write_back is a reserved protocol slot in P0: committing a modified
			// SQLite credential store back to the operator's files needs locking,
			// WAL handling and conflict detection that a copy cannot provide.
			if profile.WriteBack != nil {
				t.Fatal("write_back is not implemented; it must stay nil")
			}
		})
	}
}

func TestCatalogCoversTheToolsThatFailed(t *testing.T) {
	// These are the programs that actually failed inside the sandbox on
	// 2026-07-28. A regression that drops one of them would bring the failure
	// class straight back.
	for _, executable := range []string{"gcloud", "kubectl", "helm", "aws", "go"} {
		if _, ok := byExecutable[executable]; !ok {
			t.Fatalf("no profile matches %q", executable)
		}
	}
}

func assertSafeRel(t *testing.T, value string) {
	t.Helper()
	if strings.TrimSpace(value) == "" {
		return
	}
	if filepath.IsAbs(value) || strings.Contains(value, "..") {
		t.Fatalf("relative path %q must not be absolute or traverse", value)
	}
}

func hasPersistentKey(profile *EnvProfile, rel string) bool {
	for _, mapping := range profile.MapRW {
		if mapping.Persistent && mapping.Key == rel {
			return true
		}
	}
	return false
}

func hasRequireCycle(profile *EnvProfile, seen map[string]bool) bool {
	if profile == nil {
		return false
	}
	if seen[profile.ID] {
		return true
	}
	seen[profile.ID] = true
	defer delete(seen, profile.ID)
	for _, required := range profile.RequiresProfiles {
		if hasRequireCycle(byID[required], seen) {
			return true
		}
	}
	return false
}
