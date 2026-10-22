package executionenv

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sourceAttackFixture has one delivered, archived view (A) whose prune
// creates a protected source ref, and one committed, undelivered view (B).
// Attacks are planted afterwards, the way a sandboxed Run working directly in
// the original checkout could write that checkout's .git.
type sourceAttackFixture struct {
	root     string
	gitDir   string
	baseline GitBaseline
	views    string
	archived GitView
	pending  GitView
}

func newSourceAttackFixture(t *testing.T) sourceAttackFixture {
	t.Helper()
	ctx := context.Background()
	root := cleanGitFixture(t)
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	commitIn := func(view GitView, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(view.Path, "file.txt"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		baselineGit(t, view.Path, "add", "file.txt")
		baselineGit(t, view.Path, "commit", "-qm", "work: "+content)
	}
	archived, err := EnsureGitView(ctx, views, "archived", baseline)
	if err != nil {
		t.Fatal(err)
	}
	commitIn(archived, "archived work\n")
	if _, err := DeliverGitViewBranch(ctx, views, archived.ID, baseline); err != nil {
		t.Fatal(err)
	}
	if _, err := RetireGitView(ctx, views, archived.ID, baseline, nil); err != nil {
		t.Fatal(err)
	}
	pending, err := EnsureGitView(ctx, views, "pending", baseline)
	if err != nil {
		t.Fatal(err)
	}
	commitIn(pending, "pending work\n")
	return sourceAttackFixture{root: root, gitDir: filepath.Join(root, ".git"), baseline: baseline, views: views, archived: archived, pending: pending}
}

func (f sourceAttackFixture) config(t *testing.T, key, value string) {
	t.Helper()
	baselineGit(t, f.root, "config", "--local", key, value)
}

func (f sourceAttackFixture) write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// missingRefs points the delivery and protected refs at an object the source
// does not have, so resolving either would need a lazy promisor fetch.
func (f sourceAttackFixture) missingRefs(t *testing.T) {
	t.Helper()
	missing := strings.Repeat("ab", 20) + "\n"
	f.write(t, filepath.Join(f.gitDir, "refs", "heads", "selfmind", f.pending.ID), missing, 0o600)
	f.write(t, filepath.Join(f.gitDir, "refs", "selfmind", "retained", f.archived.ID), missing, 0o600)
}

func TestSourceRepositoryStateNeverRunsHostPrograms(t *testing.T) {
	// The view lives below this directory; an insteadOf prefix only has to
	// match it, so the attacker needs no exact view path.
	type attack struct {
		name  string
		plant func(t *testing.T, f sourceAttackFixture, marker string)
	}
	touch := func(marker string) string { return "touch '" + marker + "'" }
	// insteadOf matches the longest string prefix of the URL git is about to
	// use, which for a local fetch is the view's resolved path. The parent of
	// that path is a prefix, so the attacker needs no exact view path; it must
	// be the symlink-resolved parent, since that is what delivery would use.
	viewParent := func(f sourceAttackFixture) string { return filepath.Dir(f.pending.Path) }
	attacks := []attack{
		{"insteadof-to-ssh-with-ssh-command", func(t *testing.T, f sourceAttackFixture, marker string) {
			f.config(t, "url.ssh://attacker/.insteadOf", viewParent(f))
			f.config(t, "core.sshCommand", touch(marker)+"; false")
		}},
		{"insteadof-to-git-with-git-proxy", func(t *testing.T, f sourceAttackFixture, marker string) {
			f.config(t, "url.git://attacker/.insteadOf", viewParent(f))
			f.config(t, "core.gitProxy", touch(marker)+"; false")
		}},
		{"protocol-override-to-ext-transport", func(t *testing.T, f sourceAttackFixture, marker string) {
			f.config(t, "protocol.allow", "always")
			f.config(t, "url.ext::sh -c touch% "+marker+".insteadOf", viewParent(f))
		}},
		{"credential-helper", func(t *testing.T, f sourceAttackFixture, marker string) {
			f.config(t, "credential.helper", "!"+touch(marker))
			f.config(t, "url.https://attacker.invalid/.insteadOf", viewParent(f))
		}},
		{"alternate-refs-command-with-added-alternates", func(t *testing.T, f sourceAttackFixture, marker string) {
			other := cleanGitFixture(t)
			f.write(t, filepath.Join(f.gitDir, "objects", "info", "alternates"), filepath.Join(other, ".git", "objects")+"\n", 0o600)
			f.config(t, "core.alternateRefsCommand", touch(marker)+"; true")
			f.config(t, "core.alternateRefsPrefixes", "refs/heads/")
		}},
		{"pack-protocol-hooks", func(t *testing.T, f sourceAttackFixture, marker string) {
			f.config(t, "uploadpack.packObjectsHook", touch(marker))
			f.config(t, "receive.procReceiveRefs", "refs/for")
			f.config(t, "receivepack.dummy", "true")
		}},
		{"fsmonitor", func(t *testing.T, f sourceAttackFixture, marker string) {
			f.config(t, "core.fsmonitor", touch(marker))
		}},
		{"hooks-path-and-reference-transaction-hook", func(t *testing.T, f sourceAttackFixture, marker string) {
			hooks := filepath.Join(t.TempDir(), "hooks")
			for _, dir := range []string{hooks, filepath.Join(f.gitDir, "hooks")} {
				for _, name := range []string{"reference-transaction", "post-index-change", "pre-auto-gc"} {
					f.write(t, filepath.Join(dir, name), "#!/bin/sh\n"+touch(marker)+"\n", 0o700)
				}
			}
			f.config(t, "core.hooksPath", hooks)
		}},
		{"config-defined-hook", func(t *testing.T, f sourceAttackFixture, marker string) {
			f.config(t, "hook.attacker.command", touch(marker))
			f.config(t, "hook.attacker.event", "reference-transaction")
		}},
		{"filter-and-diff-drivers", func(t *testing.T, f sourceAttackFixture, marker string) {
			f.config(t, "filter.x.clean", touch(marker)+"; cat")
			f.config(t, "filter.x.process", touch(marker))
			f.config(t, "diff.external", touch(marker))
			f.config(t, "diff.x.textconv", touch(marker))
			f.write(t, filepath.Join(f.root, ".gitattributes"), "* filter=x diff=x\n", 0o600)
		}},
		{"include", func(t *testing.T, f sourceAttackFixture, marker string) {
			included := filepath.Join(t.TempDir(), "included")
			f.write(t, included, "[url \"ssh://attacker/\"]\n\tinsteadOf = "+viewParent(f)+"\n[core]\n\tsshCommand = "+touch(marker)+"; false\n\tfsmonitor = "+touch(marker)+"\n", 0o600)
			f.config(t, "include.path", included)
		}},
		{"include-if", func(t *testing.T, f sourceAttackFixture, marker string) {
			included := filepath.Join(t.TempDir(), "included")
			f.write(t, included, "[url \"ssh://attacker/\"]\n\tinsteadOf = "+viewParent(f)+"\n[core]\n\tsshCommand = "+touch(marker)+"; false\n", 0o600)
			f.config(t, "includeIf.gitdir:"+f.gitDir+"/.path", included)
		}},
		{"redirected-work-tree", func(t *testing.T, f sourceAttackFixture, marker string) {
			other := t.TempDir()
			f.write(t, filepath.Join(other, ".gitattributes"), "* filter=x\n", 0o600)
			f.write(t, filepath.Join(other, "file.txt"), "other\n", 0o600)
			f.config(t, "core.worktree", other)
			f.config(t, "filter.x.clean", touch(marker)+"; cat")
		}},
		{"partial-clone-lazy-fetch", func(t *testing.T, f sourceAttackFixture, marker string) {
			f.config(t, "core.repositoryformatversion", "1")
			f.config(t, "extensions.partialClone", "origin")
			f.config(t, "remote.origin.url", "ssh://attacker/repository")
			f.config(t, "remote.origin.promisor", "true")
			f.config(t, "core.sshCommand", touch(marker)+"; false")
			f.missingRefs(t)
		}},
	}
	for _, tc := range attacks {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newSourceAttackFixture(t)
			marker := filepath.Join(t.TempDir(), "MARKER")
			tc.plant(t, f, marker)
			steps := []struct {
				name string
				run  func() error
			}{
				{"baseline freeze", func() error { _, err := InspectCleanGitBaseline(ctx, f.root); return err }},
				{"view creation", func() error { _, err := EnsureGitView(ctx, f.views, "fresh", f.baseline); return err }},
				{"delivery inspection", func() error { _, err := InspectGitViewDelivery(ctx, f.views, f.pending.ID, f.baseline); return err }},
				{"apply delivery", func() error { _, err := DeliverGitViewBranch(ctx, f.views, f.pending.ID, f.baseline); return err }},
				{"keep-ref prune", func() error { _, err := PruneRetiredGitView(ctx, f.views, f.archived.ID, f.baseline, nil); return err }},
				{"pruned inspection", func() error { _, err := InspectPrunedGitView(ctx, f.views, f.archived.ID, f.baseline); return err }},
			}
			for _, step := range steps {
				err := step.run()
				if _, statErr := os.Lstat(marker); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("%s executed a program defined by source repository state (step error: %v)", step.name, err)
				}
			}
		})
	}
}

func TestSourceRepositoryAlternatesAddedAfterBaselineBlockDelivery(t *testing.T) {
	for _, file := range []string{"alternates", "http-alternates"} {
		t.Run(file, func(t *testing.T) {
			ctx := context.Background()
			f := newSourceAttackFixture(t)
			other := cleanGitFixture(t)
			f.write(t, filepath.Join(f.gitDir, "objects", "info", file), filepath.Join(other, ".git", "objects")+"\n", 0o600)
			if _, err := DeliverGitViewBranch(ctx, f.views, f.pending.ID, f.baseline); err == nil || !strings.Contains(err.Error(), "alternates") {
				t.Fatalf("delivery trusted objects borrowed through source %s: %v", file, err)
			}
			if out := baselineGit(t, f.root, "for-each-ref", "--format=%(refname)", "refs/heads/selfmind/"+f.pending.ID); out != "" {
				t.Fatalf("refused delivery still created %s", out)
			}
			if _, err := PruneRetiredGitView(ctx, f.views, f.archived.ID, f.baseline, nil); err == nil || !strings.Contains(err.Error(), "alternates") {
				t.Fatalf("prune protected a commit through source %s: %v", file, err)
			}
			if _, err := os.Stat(filepath.Join(f.views, ".retired", f.archived.ID)); err != nil {
				t.Fatalf("refused prune lost the archive: %v", err)
			}
			if _, err := InspectCleanGitBaseline(ctx, f.root); err == nil || !errors.Is(err, ErrGitViewUnavailable) {
				t.Fatalf("a source with %s was admitted as a view baseline: %v", file, err)
			}
		})
	}
}

// redirectSourceCommonDir adds a .git/commondir that points every unpinned Git
// command at another common directory, the way a Run writing the checkout
// could between the screen and a later read. refs lists the refs the
// redirected directory claims; its objects resolve through the given stores.
func redirectSourceCommonDir(t *testing.T, f sourceAttackFixture, refs map[string]string, objectStores ...string) {
	t.Helper()
	evil := filepath.Join(t.TempDir(), "redirected-common")
	alternates := ""
	for _, store := range objectStores {
		alternates += store + "\n"
	}
	f.write(t, filepath.Join(evil, "objects", "info", "alternates"), alternates, 0o600)
	f.write(t, filepath.Join(evil, "config"), "[core]\n\trepositoryformatversion = 0\n\tbare = false\n", 0o600)
	if err := os.MkdirAll(filepath.Join(evil, "refs"), 0o700); err != nil {
		t.Fatal(err)
	}
	for ref, commit := range refs {
		f.write(t, filepath.Join(evil, filepath.FromSlash(ref)), commit+"\n", 0o600)
	}
	f.write(t, filepath.Join(f.gitDir, "commondir"), evil+"\n", 0o600)
}

// A commondir added after the screen must never make a later source read
// answer from state the redirect chose. Reads are pinned to the admitted Git
// directory and refuse it explicitly, rather than quietly answering wrong.
func TestSourceReadsIgnoreACommonDirAddedAfterBaseline(t *testing.T) {
	ctx := context.Background()
	t.Run("redirect claims an undelivered view was delivered", func(t *testing.T) {
		f := newSourceAttackFixture(t)
		head := baselineGit(t, f.pending.Path, "rev-parse", "HEAD")
		redirectSourceCommonDir(t, f, map[string]string{"refs/heads/selfmind/" + f.pending.ID: head},
			filepath.Join(f.baseline.CommonDir, "objects"), filepath.Join(f.pending.Path, ".git", "objects"))
		info, err := InspectGitViewDelivery(ctx, f.views, f.pending.ID, f.baseline)
		if err == nil || !strings.Contains(err.Error(), "commondir") || info.DeliveredBranch != "" {
			t.Fatalf("a redirected common dir was trusted (delivered %q): %v", info.DeliveredBranch, err)
		}
	})
	t.Run("redirect hides a delivered branch", func(t *testing.T) {
		f := newSourceAttackFixture(t)
		if _, err := DeliverGitViewBranch(ctx, f.views, f.pending.ID, f.baseline); err != nil {
			t.Fatal(err)
		}
		redirectSourceCommonDir(t, f, nil, filepath.Join(f.baseline.CommonDir, "objects"))
		info, err := InspectGitViewDelivery(ctx, f.views, f.pending.ID, f.baseline)
		if err == nil || !strings.Contains(err.Error(), "commondir") {
			t.Fatalf("a redirected common dir silently hid the delivered branch (delivered %q): %v", info.DeliveredBranch, err)
		}
		if err := createGitSourceRef(ctx, f.views, f.baseline, "refs/heads/selfmind/after-redirect", f.baseline.Commit); err == nil {
			t.Fatal("a ref was created in a source whose Git metadata was redirected")
		}
	})
}

// The screen runs before a ref is created, but a Run writing the checkout may
// add a hook between the screen and the update. The ref is created without
// reading the source configuration at all, so no hook of any kind can run.
func TestSourceRefCreationNeverRunsConfiguredHooks(t *testing.T) {
	for _, shape := range []string{"config-defined-hook", "included-config-hook", "hooks-path-and-hook-directory"} {
		t.Run(shape, func(t *testing.T) {
			ctx := context.Background()
			f := newSourceAttackFixture(t)
			marker := filepath.Join(t.TempDir(), "MARKER")
			script := "#!/bin/sh\ntouch '" + marker + "'\n"
			switch shape {
			case "config-defined-hook":
				f.config(t, "hook.late.command", "touch '"+marker+"'")
				f.config(t, "hook.late.event", "reference-transaction")
			case "included-config-hook":
				included := filepath.Join(t.TempDir(), "included")
				f.write(t, included, "[hook \"late\"]\n\tcommand = touch '"+marker+"'\n\tevent = reference-transaction\n", 0o600)
				f.config(t, "include.path", included)
			case "hooks-path-and-hook-directory":
				hooks := filepath.Join(t.TempDir(), "hooks")
				f.write(t, filepath.Join(hooks, "reference-transaction"), script, 0o700)
				f.write(t, filepath.Join(f.gitDir, "hooks", "reference-transaction"), script, 0o700)
				f.config(t, "core.hooksPath", hooks)
			}
			// A commit the source already has, so a refusal can only be the CAS.
			other := baselineGit(t, f.root, "rev-parse", "refs/heads/selfmind/"+f.archived.ID)
			ref := "refs/heads/selfmind/late-hook"
			if err := createGitSourceRef(ctx, f.views, f.baseline, ref, f.baseline.Commit); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("creating a source ref ran a hook defined by source repository state: %v", err)
			}
			if got := baselineGit(t, f.root, "rev-parse", ref); got != f.baseline.Commit {
				t.Fatalf("ref = %s, want %s", got, f.baseline.Commit)
			}
			if err := createGitSourceRef(ctx, f.views, f.baseline, ref, other); err == nil {
				t.Fatal("an existing ref was overwritten")
			}
			if got := baselineGit(t, f.root, "rev-parse", ref); got != f.baseline.Commit {
				t.Fatalf("refused creation still moved the ref to %s", got)
			}
		})
	}
}

// Creation is compare-and-swap against an absent ref, and a ref that exists
// only in packed-refs is present: it is never overwritten or shadowed by a new
// loose ref. The same commit is an identical earlier creation and succeeds.
func TestSourceRefCreationRespectsPackedRefsAndMissingObjects(t *testing.T) {
	ctx := context.Background()
	f := newSourceAttackFixture(t)
	packed := "refs/heads/selfmind/packed"
	other := baselineGit(t, f.root, "rev-parse", "refs/heads/selfmind/"+f.archived.ID)
	baselineGit(t, f.root, "update-ref", packed, f.baseline.Commit)
	baselineGit(t, f.root, "pack-refs", "--all")
	if err := createGitSourceRef(ctx, f.views, f.baseline, packed, other); err == nil {
		t.Fatal("a ref that exists only in packed-refs was overwritten")
	}
	if got := baselineGit(t, f.root, "rev-parse", packed); got != f.baseline.Commit {
		t.Fatalf("refused creation moved the packed ref to %s", got)
	}
	if err := createGitSourceRef(ctx, f.views, f.baseline, packed, f.baseline.Commit); err != nil {
		t.Fatalf("an identical earlier creation was not recognized: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(f.gitDir, filepath.FromSlash(packed))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a loose ref now shadows the packed one: %v", err)
	}
	if err := createGitSourceRef(ctx, f.views, f.baseline, "refs/heads/selfmind/missing", strings.Repeat("ab", 20)); err == nil {
		t.Fatal("a ref was created for an object the source does not have")
	}
	entries, err := os.ReadDir(f.views)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".refs-") {
			t.Fatalf("ref-creation scratch directory %s was left behind", entry.Name())
		}
	}
}

// Baseline freeze screens the source configuration and then reads the
// checkout. A filter or fsmonitor added in between must not run: the checkout
// is read through daemon-written configuration.
func TestSourceCheckoutStatusNeverRunsSourcePrograms(t *testing.T) {
	for _, shape := range []string{"clean-filter", "included-process-filter", "fsmonitor"} {
		t.Run(shape, func(t *testing.T) {
			ctx := context.Background()
			f := newSourceAttackFixture(t)
			config, err := gitSourceCheckoutConfig(ctx, f.baseline)
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "MARKER")
			switch shape {
			case "clean-filter":
				f.config(t, "filter.late.clean", "touch '"+marker+"'; cat")
			case "included-process-filter":
				included := filepath.Join(t.TempDir(), "included")
				f.write(t, included, "[filter \"late\"]\n\tprocess = touch '"+marker+"'\n\tclean = touch '"+marker+"'; cat\n", 0o600)
				f.config(t, "include.path", included)
			case "fsmonitor":
				f.config(t, "core.fsmonitor", "touch '"+marker+"'")
			}
			f.write(t, filepath.Join(f.root, ".gitattributes"), "* filter=late\n", 0o600)
			// Identical bytes with new stat data force status to rehash.
			f.write(t, filepath.Join(f.root, "file.txt"), "base\n", 0o600)
			status, err := gitSourceCheckoutStatus(ctx, f.root, f.baseline.CommonDir, f.baseline.Commit, config)
			if _, statErr := os.Lstat(marker); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("reading the source checkout ran a program from source repository state (status %q, err %v)", status, err)
			}
			if err != nil || !strings.Contains(status, ".gitattributes") || strings.Contains(status, "file.txt") {
				t.Fatalf("checkout status = %q, %v", status, err)
			}
		})
	}
}

// A real person's repository carries remotes, credential helpers, editors,
// and URL rewrites. Neutralizing transports must not make it undeliverable.
func TestBenignSourceConfigurationStillDeliversAndPrunes(t *testing.T) {
	ctx := context.Background()
	f := newSourceAttackFixture(t)
	for key, value := range map[string]string{
		"remote.origin.url":             "https://example.invalid/team/repository.git",
		"remote.origin.fetch":           "+refs/heads/*:refs/remotes/origin/*",
		"branch.main.remote":            "origin",
		"branch.main.merge":             "refs/heads/main",
		"credential.helper":             "osxkeychain",
		"core.sshCommand":               "ssh -i ~/.ssh/id_work",
		"core.editor":                   "vim",
		"core.autocrlf":                 "input",
		"url.git@github.com:.insteadOf": "https://github.com/",
		"alias.st":                      "status --short",
		"pull.rebase":                   "true",
		"color.ui":                      "auto",
		"user.signingkey":               "ABCDEF",
	} {
		f.config(t, key, value)
	}
	if _, err := InspectCleanGitBaseline(ctx, f.root); err != nil {
		t.Fatalf("benign source was not admitted: %v", err)
	}
	delivered, err := DeliverGitViewBranch(ctx, f.views, f.pending.ID, f.baseline)
	if err != nil || delivered.DeliveredBranch != "selfmind/"+f.pending.ID {
		t.Fatalf("benign source could not receive delivery: %+v %v", delivered, err)
	}
	if got := baselineGit(t, f.root, "rev-parse", delivered.DeliveredBranch); got != delivered.HeadCommit {
		t.Fatalf("delivered branch = %s, want %s", got, delivered.HeadCommit)
	}
	if got := baselineGit(t, f.root, "cat-file", "-p", delivered.HeadCommit+":file.txt"); got != "pending work" {
		t.Fatalf("delivered content = %q", got)
	}
	if again, err := DeliverGitViewBranch(ctx, f.views, f.pending.ID, f.baseline); err != nil || again.DeliveredBranch != delivered.DeliveredBranch {
		t.Fatalf("repeat delivery was not idempotent: %+v %v", again, err)
	}
	pruned, err := PruneRetiredGitView(ctx, f.views, f.archived.ID, f.baseline, nil)
	if err != nil {
		t.Fatalf("benign source could not protect a pruned view: %v", err)
	}
	if got := baselineGit(t, f.root, "rev-parse", pruned.RetainedRef); got != pruned.HeadCommit {
		t.Fatalf("protected ref = %s, want %s", got, pruned.HeadCommit)
	}
}
