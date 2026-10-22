package executionenv

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type gitViewSafetyFixture struct {
	root     string
	baseline GitBaseline
	views    string
	view     GitView
}

func newGitViewSafetyFixture(t *testing.T, id string) gitViewSafetyFixture {
	t.Helper()
	ctx := context.Background()
	root := cleanGitFixture(t)
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	view, err := EnsureGitView(ctx, views, id, baseline)
	if err != nil {
		t.Fatal(err)
	}
	return gitViewSafetyFixture{root: root, baseline: baseline, views: views, view: view}
}

func (f gitViewSafetyFixture) commit(t *testing.T, name, content, message string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.view.Path, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	baselineGit(t, f.view.Path, "add", name)
	baselineGit(t, f.view.Path, "commit", "-qm", message)
}

func requireNoMarker(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("view-writable Git state executed a host command (marker %s): %v", marker, err)
	}
}

// plantCommonDirRedirect reproduces the review finding: the validated
// .git/config stays passive while real Git resolves configuration through a
// redirected common directory that defines a command-bearing filter.
func plantCommonDirRedirect(t *testing.T, view GitView, commonDir, config string) {
	t.Helper()
	gitDir := filepath.Join(view.Path, ".git")
	target := commonDir
	if !filepath.IsAbs(target) {
		target = filepath.Join(gitDir, commonDir)
	}
	for _, dir := range []string{filepath.Join(target, "objects", "info"), filepath.Join(target, "refs")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(target, "objects", "info", "alternates"), []byte(filepath.Join(gitDir, "objects")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte(commonDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(view.Path, ".gitattributes"), []byte("* filter=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Rewriting identical bytes leaves the file stat-dirty, so a status
	// refresh must hash it through the configured clean filter.
	data, err := os.ReadFile(filepath.Join(view.Path, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(view.Path, "file.txt"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGitViewRejectsRedirectedMetadataWithoutRunningHostCommands(t *testing.T) {
	cases := []struct {
		name  string
		plant func(t *testing.T, view GitView, marker string)
	}{
		{"relative commondir with clean filter", func(t *testing.T, view GitView, marker string) {
			plantCommonDirRedirect(t, view, "evil",
				"[core]\n\trepositoryformatversion = 0\n\tbare = false\n[filter \"x\"]\n\tclean = touch '"+marker+"'; cat\n")
		}},
		{"absolute commondir with included process filter", func(t *testing.T, view GitView, marker string) {
			include := filepath.Join(t.TempDir(), "included")
			if err := os.WriteFile(include, []byte("[filter \"x\"]\n\tprocess = touch '"+marker+"'\n\tsmudge = touch '"+marker+"'\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			plantCommonDirRedirect(t, view, filepath.Join(t.TempDir(), "outside-common"),
				"[core]\n\trepositoryformatversion = 0\n\tbare = false\n[include]\n\tpath = "+include+"\n")
		}},
		{"gitdir indirection file", func(t *testing.T, view GitView, marker string) {
			if err := os.WriteFile(filepath.Join(view.Path, ".git", "gitdir"), []byte(filepath.Join(t.TempDir(), "elsewhere", ".git")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"worktree config file", func(t *testing.T, view GitView, marker string) {
			if err := os.WriteFile(filepath.Join(view.Path, ".git", "config.worktree"), []byte("[filter \"x\"]\n\tclean = touch '"+marker+"'; cat\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"gitfile replaces the metadata directory", func(t *testing.T, view GitView, marker string) {
			moved := filepath.Join(t.TempDir(), "moved.git")
			if err := os.Rename(filepath.Join(view.Path, ".git"), moved); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(view.Path, ".git"), []byte("gitdir: "+moved+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"extra alternate object store", func(t *testing.T, view GitView, marker string) {
			alternates := filepath.Join(view.Path, ".git", "objects", "info", "alternates")
			data, err := os.ReadFile(alternates)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(alternates, append(data, []byte(filepath.Join(t.TempDir(), "objects")+"\n")...), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newGitViewSafetyFixture(t, "redirected")
			f.commit(t, "file.txt", "delivered\n", "isolated work")
			marker := filepath.Join(t.TempDir(), "MARKER")
			tc.plant(t, f.view, marker)
			if _, err := InspectGitView(ctx, f.views, f.view.ID, f.baseline); !errors.Is(err, ErrGitViewUnavailable) {
				t.Fatalf("redirected view metadata was accepted: %v", err)
			}
			if _, err := InspectGitViewDelivery(ctx, f.views, f.view.ID, f.baseline); !errors.Is(err, ErrGitViewUnavailable) {
				t.Fatalf("/views inspection accepted redirected metadata: %v", err)
			}
			if _, err := DeliverGitViewBranch(ctx, f.views, f.view.ID, f.baseline); !errors.Is(err, ErrGitViewUnavailable) {
				t.Fatalf("/apply delivery accepted redirected metadata: %v", err)
			}
			requireNoMarker(t, marker)
			if _, err := os.Lstat(filepath.Join(f.view.Path, "file.txt")); err != nil {
				t.Fatalf("rejected view was not preserved: %v", err)
			}
			if out := baselineGit(t, f.root, "for-each-ref", "--format=%(refname)", "refs/heads/selfmind"); out != "" {
				t.Fatalf("rejected view was delivered: %s", out)
			}
		})
	}
}

// gitInvocation is one git process observed through a PATH wrapper.
type gitInvocation struct {
	cwd, gitDir, commonDir, workTree, indexFile string
	args                                        []string
}

func (inv gitInvocation) subcommand() string {
	for i := 0; i < len(inv.args); i++ {
		switch inv.args[i] {
		case "-c", "-C":
			i++
		default:
			return inv.args[i]
		}
	}
	return ""
}

// recordGitInvocations runs fn with a git wrapper first on PATH and returns
// every git process it started, including the exact Git location environment.
func recordGitInvocations(t *testing.T, fn func()) []gitInvocation {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	bin := t.TempDir()
	// One log file per git process: the daemon streams pack-objects into
	// index-pack, so two recorded processes run at once, and records written
	// piecewise to one shared file would interleave and tear.
	logDir := t.TempDir()
	script := "#!/bin/sh\n{\n" +
		"  printf '%s\\036%s\\036%s\\036%s\\036%s\\036' \"$(pwd -P)\" \"${GIT_DIR-<unset>}\" \"${GIT_COMMON_DIR-<unset>}\" \"${GIT_WORK_TREE-<unset>}\" \"${GIT_INDEX_FILE-<unset>}\"\n" +
		"  for a in \"$@\"; do printf '%s\\037' \"$a\"; done\n" +
		"  printf '\\n'\n" +
		"} >> '" + logDir + "'/$$.log\n" +
		"exec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	fn()
	entries, err := os.ReadDir(logDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var data []byte
	for _, entry := range entries {
		part, err := os.ReadFile(filepath.Join(logDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, part...)
	}
	var out []gitInvocation
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\x1e", 6)
		if len(fields) != 6 {
			t.Fatalf("malformed git log line %q", line)
		}
		inv := gitInvocation{cwd: fields[0], gitDir: fields[1], commonDir: fields[2], workTree: fields[3], indexFile: fields[4]}
		for _, arg := range strings.Split(strings.TrimSuffix(fields[5], "\x1f"), "\x1f") {
			inv.args = append(inv.args, arg)
		}
		out = append(out, inv)
	}
	return out
}

func requirePinnedViewInvocations(t *testing.T, viewPath string, calls []gitInvocation) {
	t.Helper()
	gitDir := filepath.Join(viewPath, ".git")
	touched := 0
	for _, inv := range calls {
		targetsView := inv.cwd == viewPath || strings.HasPrefix(inv.gitDir, viewPath) || inv.workTree == viewPath
		for i, arg := range inv.args {
			if arg == "-C" && i+1 < len(inv.args) && inv.args[i+1] == viewPath {
				targetsView = true
			}
		}
		if !targetsView {
			continue
		}
		touched++
		if inv.gitDir == "<unset>" {
			t.Fatalf("git %v discovered the view's Git directory instead of pinning it", inv.args)
		}
		if inv.gitDir == gitDir && inv.commonDir != gitDir {
			t.Fatalf("git %v pinned GIT_DIR but left the common directory to %q", inv.args, inv.commonDir)
		}
		if inv.workTree != "<unset>" && inv.workTree != viewPath {
			t.Fatalf("git %v used work tree %q", inv.args, inv.workTree)
		}
		switch inv.subcommand() {
		case "status", "ls-files", "diff-files", "diff-index", "checkout", "add":
			if inv.workTree != viewPath {
				t.Fatalf("worktree command git %v did not pin the work tree: %q", inv.args, inv.workTree)
			}
			if strings.HasPrefix(inv.gitDir, viewPath+string(filepath.Separator)) || strings.HasPrefix(inv.commonDir, viewPath+string(filepath.Separator)) {
				t.Fatalf("worktree command git %v read view-writable configuration from %q", inv.args, inv.commonDir)
			}
		case "diff":
			joined := strings.Join(inv.args, " ")
			if !strings.Contains(joined, "--no-ext-diff") || !strings.Contains(joined, "--no-textconv") {
				t.Fatalf("diff may run an external program: git %v", inv.args)
			}
		}
	}
	if touched == 0 {
		t.Fatal("no daemon-side git command reached the view; the recorder is not observing")
	}
}

func TestGitViewInspectionPinsGitLocationAndNeverReadsWritableConfigForWorktreeCommands(t *testing.T) {
	ctx := context.Background()
	f := newGitViewSafetyFixture(t, "pinned")
	f.commit(t, "file.txt", "delivered\n", "isolated work")
	calls := recordGitInvocations(t, func() {
		if _, err := InspectGitView(ctx, f.views, f.view.ID, f.baseline); err != nil {
			t.Fatal(err)
		}
		if _, err := InspectGitViewDelivery(ctx, f.views, f.view.ID, f.baseline); err != nil {
			t.Fatal(err)
		}
		if _, err := DeliverGitViewBranch(ctx, f.views, f.view.ID, f.baseline); err != nil {
			t.Fatal(err)
		}
	})
	requirePinnedViewInvocations(t, f.view.Path, calls)
}

func TestRetiredGitViewInspectionPinsItsArchivedLocation(t *testing.T) {
	ctx := context.Background()
	f := newGitViewSafetyFixture(t, "pinned-archive")
	f.commit(t, "file.txt", "delivered\n", "isolated work")
	if _, err := DeliverGitViewBranch(ctx, f.views, f.view.ID, f.baseline); err != nil {
		t.Fatal(err)
	}
	retired, err := RetireGitView(ctx, f.views, f.view.ID, f.baseline, nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := recordGitInvocations(t, func() {
		if _, err := InspectRetiredGitView(ctx, f.views, f.view.ID, f.baseline); err != nil {
			t.Fatal(err)
		}
		if _, err := InspectGitViewDelivery(ctx, filepath.Dir(retired.Path), f.view.ID, f.baseline); err != nil {
			t.Fatal(err)
		}
	})
	requirePinnedViewInvocations(t, retired.Path, calls)
}

// A Run may still be writing when validation finishes. Worktree reads must
// not consult configuration it can change afterwards, whether it edits
// .git/config directly or redirects the common directory.
func TestGitViewWorktreeReadsIgnoreConfigChangedAfterValidation(t *testing.T) {
	for _, change := range []string{"edited-config", "commondir-redirect"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			f := newGitViewSafetyFixture(t, "toctou")
			state, err := inspectGitViewState(ctx, f.view)
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "MARKER")
			filter := "[filter \"x\"]\n\tclean = touch '" + marker + "'; cat\n\tprocess = touch '" + marker + "'\n"
			switch change {
			case "edited-config":
				config := filepath.Join(f.view.Path, ".git", "config")
				data, err := os.ReadFile(config)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(config, append(data, []byte("[core]\n\tfsmonitor = touch '"+marker+"'\n"+filter)...), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(f.view.Path, ".gitattributes"), []byte("* filter=x\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(f.view.Path, "file.txt"), []byte("base\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "commondir-redirect":
				plantCommonDirRedirect(t, f.view, "late", "[core]\n\trepositoryformatversion = 0\n\tbare = false\n"+filter)
			}
			worktree, err := newGitViewWorktree(f.view, state)
			if err != nil {
				t.Fatal(err)
			}
			defer worktree.close()
			var failures []error
			for _, args := range [][]string{
				{"status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=all"},
				{"ls-files", "--others", "--ignored", "--exclude-standard", "-z"},
				{"ls-files", "--stage", "-v", "-z"},
			} {
				if _, err := worktree.command(ctx, args...); err != nil {
					failures = append(failures, err)
				}
			}
			if _, err := gitViewCommand(ctx, f.view.Path, "rev-parse", "--verify", "HEAD^{commit}"); err != nil {
				failures = append(failures, err)
			}
			requireNoMarker(t, marker)
			if len(failures) != 0 {
				t.Fatalf("worktree reads failed: %v", failures)
			}
		})
	}
}

func TestGitViewConfigAllowlistRejectsCommandBearingAndRedirectingKeys(t *testing.T) {
	keys := map[string]string{
		"include.path":                "/tmp/other-config",
		"includeIf.gitdir:/tmp/.path": "/tmp/other-config",
		"filter.lfs.clean":            "git-lfs clean -- %f",
		"filter.x.process":            "helper",
		"diff.x.textconv":             "helper",
		"diff.external":               "helper",
		"merge.x.driver":              "helper %A %B",
		"core.fsmonitor":              "helper",
		"core.sshCommand":             "helper",
		"core.pager":                  "helper",
		"core.editor":                 "helper",
		"core.worktree":               "/tmp",
		"core.gitProxy":               "helper",
		"core.alternateRefsCommand":   "helper",
		"uploadpack.packObjectsHook":  "helper",
		"receive.procReceiveRefs":     "refs/for",
		"credential.helper":           "helper",
		"protocol.ext.allow":          "always",
		"extensions.worktreeConfig":   "true",
		"gpg.program":                 "helper",
		"alias.st":                    "!helper",
		"url.ext::helper.insteadOf":   "https://",
		"submodule.recurse":           "true",
		"trailer.x.command":           "helper",
	}
	for key, value := range keys {
		t.Run(key, func(t *testing.T) {
			ctx := context.Background()
			f := newGitViewSafetyFixture(t, "config-key")
			baselineGit(t, f.view.Path, "config", "--local", key, value)
			if _, err := InspectGitView(ctx, f.views, f.view.ID, f.baseline); !errors.Is(err, ErrGitViewUnavailable) {
				t.Fatalf("view config key %s was accepted: %v", key, err)
			}
		})
	}
}

func TestPruneRetiredGitViewRefusesPackedUnreachableCommits(t *testing.T) {
	for _, hidden := range []string{"rewound-then-gc", "deleted-ref-after-repack"} {
		t.Run(hidden, func(t *testing.T) {
			ctx := context.Background()
			f := newGitViewSafetyFixture(t, "packed-hidden")
			switch hidden {
			case "rewound-then-gc":
				f.commit(t, "draft.txt", "saved only in Git\n", "unreviewed work")
				baselineGit(t, f.view.Path, "reset", "-q", "--hard", f.baseline.Commit)
				baselineGit(t, f.view.Path, "gc", "-q")
			case "deleted-ref-after-repack":
				tree := baselineGit(t, f.view.Path, "rev-parse", f.baseline.Commit+"^{tree}")
				commit := baselineGit(t, f.view.Path, "commit-tree", "-p", f.baseline.Commit, "-m", "parked work", tree)
				baselineGit(t, f.view.Path, "update-ref", "refs/heads/parked", commit)
				baselineGit(t, f.view.Path, "repack", "-q", "-a", "-d")
				baselineGit(t, f.view.Path, "update-ref", "-d", "refs/heads/parked")
			}
			if out := baselineGit(t, f.view.Path, "count-objects", "-v"); !strings.Contains(out, "count: 0") {
				t.Fatalf("fixture left loose objects; the case must prove packed detection:\n%s", out)
			}
			retired, err := RetireGitView(ctx, f.views, f.view.ID, f.baseline, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := PruneRetiredGitView(ctx, f.views, f.view.ID, f.baseline, nil); err == nil || !strings.Contains(err.Error(), "unreachable") {
				t.Fatalf("packed unreachable commit was not reported: %v", err)
			}
			if _, err := os.Stat(retired.Path); err != nil {
				t.Fatalf("archive with hidden work was not preserved: %v", err)
			}
		})
	}
}

func TestPruneRetiredGitViewIgnoresSourceObjectsOutsideTheView(t *testing.T) {
	ctx := context.Background()
	root := cleanGitFixture(t)
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	view, err := EnsureGitView(ctx, views, "source-has-history", baseline)
	if err != nil {
		t.Fatal(err)
	}
	// The source repository keeps unrelated packed and loose history that the
	// view can read through alternates but never owned.
	for i, name := range []string{"packed", "loose"} {
		baselineGit(t, root, "checkout", "-q", "-b", name)
		if err := os.WriteFile(filepath.Join(root, name+".txt"), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		baselineGit(t, root, "add", name+".txt")
		baselineGit(t, root, "commit", "-qm", name)
		baselineGit(t, root, "checkout", "-q", "-")
		if i == 0 {
			baselineGit(t, root, "repack", "-q", "-a", "-d")
		}
	}
	if err := os.WriteFile(filepath.Join(view.Path, "file.txt"), []byte("delivered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	baselineGit(t, view.Path, "add", "file.txt")
	baselineGit(t, view.Path, "commit", "-qm", "isolated work")
	if _, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err != nil {
		t.Fatal(err)
	}
	if _, err := RetireGitView(ctx, views, view.ID, baseline, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := PruneRetiredGitView(ctx, views, view.ID, baseline, nil); err != nil {
		t.Fatalf("source-only history blocked a proven-safe prune: %v", err)
	}
}

func TestGitViewRetentionRefusesEditsHiddenByIndexBits(t *testing.T) {
	for _, bit := range []string{"--skip-worktree", "--assume-unchanged"} {
		t.Run(bit, func(t *testing.T) {
			ctx := context.Background()
			f := newGitViewSafetyFixture(t, "hidden-index")
			baselineGit(t, f.view.Path, "update-index", bit, "file.txt")
			edited := filepath.Join(f.view.Path, "file.txt")
			if err := os.WriteFile(edited, []byte("edit that status does not show\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := RetireGitView(ctx, f.views, f.view.ID, f.baseline, nil); err == nil ||
				!strings.Contains(err.Error(), "skip-worktree") {
				t.Fatalf("archive accepted an edit hidden by %s: %v", bit, err)
			}
			if data, err := os.ReadFile(edited); err != nil || string(data) != "edit that status does not show\n" {
				t.Fatalf("hidden edit was lost: %q %v", data, err)
			}
		})
	}
}

func TestPruneRetiredGitViewRefusesArchiveWithHiddenIndexEdit(t *testing.T) {
	ctx := context.Background()
	f := newGitViewSafetyFixture(t, "hidden-archive")
	retired, err := RetireGitView(ctx, f.views, f.view.ID, f.baseline, nil)
	if err != nil {
		t.Fatal(err)
	}
	// An archive written before these checks existed may already contain it.
	baselineGit(t, retired.Path, "update-index", "--skip-worktree", "file.txt")
	if err := os.WriteFile(filepath.Join(retired.Path, "file.txt"), []byte("hidden\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PruneRetiredGitView(ctx, f.views, f.view.ID, f.baseline, nil); err == nil || !strings.Contains(err.Error(), "skip-worktree") {
		t.Fatalf("prune accepted an archive with a hidden edit: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(retired.Path, "file.txt")); err != nil || string(data) != "hidden\n" {
		t.Fatalf("hidden archived edit was lost: %q %v", data, err)
	}
}

func TestGitViewRetentionRefusesLinkedWorktrees(t *testing.T) {
	for _, shape := range []string{"inside-git-dir", "stale-admin-entry"} {
		t.Run(shape, func(t *testing.T) {
			ctx := context.Background()
			f := newGitViewSafetyFixture(t, "linked")
			linked := filepath.Join(f.view.Path, ".git", "wt")
			baselineGit(t, f.view.Path, "worktree", "add", "-q", "--detach", linked, "HEAD")
			if err := os.WriteFile(filepath.Join(linked, "file.txt"), []byte("linked work\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			baselineGit(t, linked, "commit", "-qam", "detached linked work")
			if shape == "stale-admin-entry" {
				if err := os.RemoveAll(linked); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := RetireGitView(ctx, f.views, f.view.ID, f.baseline, nil); err == nil || !strings.Contains(err.Error(), "worktree") {
				t.Fatalf("archive accepted a view with linked worktrees: %v", err)
			}
			if _, err := os.Stat(filepath.Join(f.view.Path, ".git", "worktrees")); err != nil {
				t.Fatalf("linked worktree metadata was lost: %v", err)
			}
		})
	}
}

// Host-side status never descends into submodules (their configuration is
// unscreened), so a view that gained one cannot prove its nested work safe.
func TestGitViewRetentionRefusesSubmoduleRepositories(t *testing.T) {
	for _, shape := range []string{"gitlink-to-nested-repository", "module-gitdir"} {
		t.Run(shape, func(t *testing.T) {
			ctx := context.Background()
			f := newGitViewSafetyFixture(t, "nested")
			kept := filepath.Join(f.view.Path, ".git", "modules", "lib", "kept.txt")
			switch shape {
			case "gitlink-to-nested-repository":
				nested := filepath.Join(f.view.Path, "sub")
				if err := os.Mkdir(nested, 0o700); err != nil {
					t.Fatal(err)
				}
				baselineGit(t, nested, "init", "-q")
				if err := os.WriteFile(filepath.Join(nested, "file.txt"), []byte("nested\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				baselineGit(t, nested, "add", "file.txt")
				baselineGit(t, nested, "-c", "user.name=T", "-c", "user.email=t@example.invalid", "commit", "-qm", "nested")
				head := baselineGit(t, nested, "rev-parse", "HEAD")
				baselineGit(t, f.view.Path, "update-index", "--add", "--cacheinfo", "160000,"+head+",sub")
				baselineGit(t, f.view.Path, "commit", "-qm", "register nested repository")
				kept = filepath.Join(nested, "file.txt")
				if err := os.WriteFile(kept, []byte("nested uncommitted work\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := DeliverGitViewBranch(ctx, f.views, f.view.ID, f.baseline); err != nil {
					t.Fatal(err)
				}
			case "module-gitdir":
				if err := os.MkdirAll(filepath.Dir(kept), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(kept, []byte("nested repository metadata\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			info, err := InspectGitViewDelivery(ctx, f.views, f.view.ID, f.baseline)
			if err != nil || info.Submodules == 0 || info.HiddenIndexEntries != 0 {
				t.Fatalf("nested repository was not reported separately: %+v %v", info, err)
			}
			if _, err := RetireGitView(ctx, f.views, f.view.ID, f.baseline, nil); err == nil || !strings.Contains(err.Error(), "submodule") {
				t.Fatalf("archive accepted a view with a nested repository: %v", err)
			}
			if _, err := os.Stat(kept); err != nil {
				t.Fatalf("nested work was lost: %v", err)
			}
		})
	}
}

func TestPruneRetiredGitViewRefusesArchiveWithLinkedWorktree(t *testing.T) {
	ctx := context.Background()
	f := newGitViewSafetyFixture(t, "linked-archive")
	retired, err := RetireGitView(ctx, f.views, f.view.ID, f.baseline, nil)
	if err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(retired.Path, ".git", "wt")
	baselineGit(t, retired.Path, "worktree", "add", "-q", "--detach", linked, "HEAD")
	if err := os.WriteFile(filepath.Join(linked, "file.txt"), []byte("linked work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	baselineGit(t, linked, "commit", "-qam", "detached linked work")
	if _, err := PruneRetiredGitView(ctx, f.views, f.view.ID, f.baseline, nil); err == nil || !strings.Contains(err.Error(), "worktree") {
		t.Fatalf("prune accepted an archive with a linked worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(retired.Path, ".git", "worktrees")); err != nil {
		t.Fatalf("archived linked worktree was lost: %v", err)
	}
}
