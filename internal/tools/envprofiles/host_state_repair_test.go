package envprofiles

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repairFixture is an SSO host whose two declared caches exist and hold one
// legitimate token, planned under profiles.
func repairFixture(t *testing.T, profiles []*EnvProfile) HostStatePlan {
	t.Helper()
	home := awsHost(t, "[profile sso-dev]\nsso_session=dev\n")
	for _, rel := range []string{"sso/cache", "cli/cache"} {
		if err := os.MkdirAll(filepath.Join(home, ".aws", rel), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, ".aws", "sso", "cache", "token.json"), []byte(`{"token":"kept"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanHostStateWrites(profiles, inPlaceContext(home, TrustTrusted, "sso-dev"))
	if err != nil || len(plan.Writes) != 2 {
		t.Fatalf("fixture plan: %+v %v", plan, err)
	}
	return plan
}

// requireCleanCache walks a cache the way a host tool would meet it.
func requireCleanCache(t *testing.T, write HostStateWrite) {
	t.Helper()
	_ = filepath.WalkDir(write.Path, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == write.Path {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return nil
		}
		switch mode := info.Mode(); {
		case mode.IsDir() && write.Flat:
			t.Errorf("%s is a directory in a flat cache", path)
		case mode.IsRegular() && linkCount(info) > 1:
			t.Errorf("%s is a hard-linked file", path)
		case !mode.IsDir() && !mode.IsRegular():
			t.Errorf("%s is neither a regular file nor a directory (%v)", path, mode.Type())
		}
		return nil
	})
}

// The post-command scan: whatever a sandboxed command left in a cache that a
// host tool must never meet — a link, a special file, an alias of a file
// elsewhere, a directory in a flat cache — is moved out of the cache into the
// quarantine and reported; everything else stays.
func TestRepairHostStateWritesQuarantinesWhatHostToolsMustNotMeet(t *testing.T) {
	victim := filepath.Join(t.TempDir(), "victim.json")
	if err := os.WriteFile(victim, []byte("victim"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		profiles func() []*EnvProfile
		plant    func(t *testing.T, sso, cli string) map[string]string
	}{
		{"flat", func() []*EnvProfile { return Match([]string{"aws"}) }, func(t *testing.T, sso, cli string) map[string]string {
			mustDo(t, os.Symlink(victim, filepath.Join(sso, "link.json")))
			mustDo(t, mkfifo(filepath.Join(sso, "pipe")))
			mustDo(t, os.Link(victim, filepath.Join(sso, "alias.json")))
			mustDo(t, os.Mkdir(filepath.Join(sso, "sub"), 0o700))
			mustDo(t, os.Symlink(victim, filepath.Join(sso, "sub", "link.json")))
			mustDo(t, mkfifo(filepath.Join(cli, "pipe")))
			return map[string]string{
				filepath.Join(sso, "link.json"):  "symbolic link",
				filepath.Join(sso, "pipe"):       "named pipe",
				filepath.Join(sso, "alias.json"): "hard-linked file",
				filepath.Join(sso, "sub"):        "directory",
				filepath.Join(cli, "pipe"):       "named pipe",
			}
		}},
		// The declared subpath itself replaced by a link.
		{"subpath", func() []*EnvProfile { return Match([]string{"aws"}) }, func(t *testing.T, sso, cli string) map[string]string {
			mustDo(t, os.Rename(sso, sso+".moved"))
			mustDo(t, os.Symlink(filepath.Dir(victim), sso))
			return map[string]string{sso: "symbolic link"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := repairFixture(t, tc.profiles())
			sso, cli := plan.Writes[0].Path, plan.Writes[1].Path
			want := tc.plant(t, sso, cli)
			quarantine := filepath.Join(t.TempDir(), "quarantine")
			repaired := RepairHostStateWrites(plan, quarantine)
			if len(repaired) != len(want) {
				t.Fatalf("repaired %d entries, want %d: %+v", len(repaired), len(want), repaired)
			}
			for _, entry := range repaired {
				kind, expected := want[entry.Path]
				if !expected || !strings.HasPrefix(entry.Kind, kind) || entry.Error != "" {
					t.Fatalf("unexpected repair %+v (want %q)", entry, kind)
				}
				if !strings.HasPrefix(entry.MovedTo, quarantine+string(filepath.Separator)) {
					t.Fatalf("%s was not moved into the quarantine: %q", entry.Path, entry.MovedTo)
				}
				if _, err := os.Lstat(entry.MovedTo); err != nil {
					t.Fatalf("the quarantine does not hold %s: %v", entry.Path, err)
				}
				if _, err := os.Lstat(entry.Path); !os.IsNotExist(err) {
					t.Fatalf("%s is still in the cache: %v", entry.Path, err)
				}
			}
			for _, write := range plan.Writes {
				requireCleanCache(t, write)
			}
			if tc.name != "subpath" {
				if data, err := os.ReadFile(filepath.Join(sso, "token.json")); err != nil || string(data) != `{"token":"kept"}` {
					t.Fatalf("a legitimate token was disturbed: %q %v", data, err)
				}
			}
			if data, _ := os.ReadFile(victim); string(data) != "victim" {
				t.Fatal("the file a planted link pointed at changed")
			}
		})
	}

	// The constraint that must change the result: a clean cache is left alone
	// and no quarantine is made.
	plan := repairFixture(t, Match([]string{"aws"}))
	quarantine := filepath.Join(t.TempDir(), "quarantine")
	if repaired := RepairHostStateWrites(plan, quarantine); len(repaired) != 0 {
		t.Fatalf("a clean cache was repaired: %+v", repaired)
	}
	if _, err := os.Stat(quarantine); !os.IsNotExist(err) {
		t.Fatalf("a quarantine was made for nothing: %v", err)
	}
}

// An honest writer refreshes a token by writing a temporary file and renaming
// it into place — the operator's own aws, or another run. An entry that
// vanishes while the cache is listed is gone, not irregular: the repair never
// moves the live cache, or any of its files, because of one.
func TestRepairHostStateWritesNeverMovesALiveCacheForAVanishedEntry(t *testing.T) {
	plan := repairFixture(t, Match([]string{"aws"}))
	sso := plan.Writes[0].Path
	quarantine := filepath.Join(t.TempDir(), "quarantine")
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			tmp := filepath.Join(sso, fmt.Sprintf(".tmp-%d.json", i))
			if os.WriteFile(tmp, []byte(`{"token":"fresh"}`), 0o600) == nil {
				_ = os.Rename(tmp, filepath.Join(sso, "token.json"))
			}
		}
	}()
	var repaired []QuarantinedStateEntry
	for scan := 0; scan < 2000 && len(repaired) == 0; scan++ {
		repaired = RepairHostStateWrites(plan, quarantine)
	}
	close(stop)
	<-done
	if len(repaired) > 0 {
		t.Fatalf("a live cache was repaired because a writer's temporary file vanished: %+v", repaired)
	}
	if _, err := os.Stat(filepath.Join(sso, "token.json")); err != nil {
		t.Fatalf("the live token is gone: %v", err)
	}
}

// The startup sweep covers every catalog cache that exists on this host,
// whatever the configuration selects now: the command that planted an entry
// before a crash may have run under another profile or configuration.
func TestCatalogHostStateWritesNamesEveryExistingCache(t *testing.T) {
	lookup := func(string) (string, bool) { return "", false }
	home := awsHost(t, "[default]\nregion=us-east-1\n")
	sso := filepath.Join(home, ".aws", "sso", "cache")
	mustDo(t, os.MkdirAll(sso, 0o700))
	plan := CatalogHostStateWrites(ApplyContext{Home: home, Lookup: lookup})
	if len(plan.Writes) != 1 || plan.Writes[0].Path != canonical(t, sso) || !plan.Writes[0].Flat {
		t.Fatalf("the sweep must cover the existing SSO cache, and only it: %+v", plan.Writes)
	}
	// The constraint that must change the result: a host with no state root.
	if plan := CatalogHostStateWrites(ApplyContext{Home: t.TempDir(), Lookup: lookup}); len(plan.Writes) != 0 {
		t.Fatalf("a host without the caches has nothing to sweep: %+v", plan.Writes)
	}
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
