package executionenv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PrunedGitView records the durable source ref protecting a deleted view's
// committed work. Pruning is explicit; archived views are never aged out.
type PrunedGitView struct {
	Version        int    `json:"version"`
	SourceRoot     string `json:"source_root"`
	BaselineCommit string `json:"baseline_commit"`
	HeadCommit     string `json:"head_commit"`
	RetainedRef    string `json:"retained_ref"`
}

func retainedGitViewRef(viewID string) string { return "refs/selfmind/retained/" + viewID }

// PruneRetiredGitView reclaims one archived checkout only after proving that
// every object it stores is reachable from its delivered HEAD, with no
// working, ignored, hidden-index, linked-worktree, submodule, or extra-ref
// work. A protected ref in the source keeps that HEAD reachable even if the
// delivery branch later moves. The guard covers the rename that takes the
// archive out of restore's path; a staged directory and marker make a crash
// during deletion retryable.
func PruneRetiredGitView(ctx context.Context, viewsDir, viewID string, baseline GitBaseline, guard GitViewMoveGuard) (PrunedGitView, error) {
	if !safeGitViewID(viewID) {
		return PrunedGitView{}, fmt.Errorf("%w: invalid view identity", ErrGitViewUnavailable)
	}
	archive, err := retiredGitViewDir(viewsDir)
	if err != nil {
		return PrunedGitView{}, err
	}
	liveExists, err := directoryExists(filepath.Join(viewsDir, viewID))
	if err != nil {
		return PrunedGitView{}, err
	}
	if liveExists {
		return PrunedGitView{}, fmt.Errorf("%w: live view still exists", ErrGitViewUnavailable)
	}
	staging, err := privateGitViewSubdir(archive, ".pruning")
	if err != nil {
		return PrunedGitView{}, err
	}
	markerDir, err := privateGitViewSubdir(archive, ".pruned")
	if err != nil {
		return PrunedGitView{}, err
	}
	retired := filepath.Join(archive, viewID)
	staged := filepath.Join(staging, viewID)
	retiredExists, err := directoryExists(retired)
	if err != nil {
		return PrunedGitView{}, err
	}
	stagedExists, err := directoryExists(staged)
	if err != nil {
		return PrunedGitView{}, err
	}
	if retiredExists && stagedExists {
		return PrunedGitView{}, fmt.Errorf("%w: both retained and pruning views exist", ErrGitViewUnavailable)
	}
	marker := filepath.Join(markerDir, viewID+".json")
	if _, err := os.Lstat(marker); err == nil {
		if retiredExists {
			return PrunedGitView{}, fmt.Errorf("%w: prune record conflicts with retained view", ErrGitViewUnavailable)
		}
		record, err := InspectPrunedGitView(ctx, viewsDir, viewID, baseline)
		if err != nil {
			return PrunedGitView{}, err
		}
		// A crash during RemoveAll can leave an incomplete staged directory.
		// The durable marker and protected source ref were written first, so
		// only now is it safe to finish removing that partial directory.
		if stagedExists {
			if err := os.RemoveAll(staged); err != nil {
				return PrunedGitView{}, err
			}
		}
		return record, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return PrunedGitView{}, err
	}
	if !retiredExists && !stagedExists {
		return PrunedGitView{}, fmt.Errorf("%w: retained view is unavailable", ErrGitViewUnavailable)
	}
	location := archive
	if stagedExists {
		location = staging
	}
	info, err := inspectPrunableGitView(ctx, location, viewID, baseline)
	if err != nil {
		return PrunedGitView{}, err
	}
	record := PrunedGitView{Version: 1, SourceRoot: baseline.Root,
		BaselineCommit: baseline.Commit, HeadCommit: info.HeadCommit, RetainedRef: retainedGitViewRef(viewID)}
	if err := retainGitViewCommit(ctx, archive, baseline, record, info); err != nil {
		return PrunedGitView{}, err
	}
	if retiredExists {
		// Ref creation and final inspection are separate effects. Recheck all
		// content before atomically taking the view out of restore's path.
		current, err := inspectPrunableGitView(ctx, archive, viewID, baseline)
		if err != nil || current.HeadCommit != record.HeadCommit {
			return PrunedGitView{}, fmt.Errorf("%w: retained view changed during prune", ErrGitViewUnavailable)
		}
		// The protected ref is what makes deleting the checkout safe, so it is
		// checked against the source again as the last step before the
		// checkout leaves restore's path; a failure keeps the archive.
		if err := guard.run(func() error {
			if err := verifyGitSourceRef(ctx, archive, baseline, record.RetainedRef, record.HeadCommit); err != nil {
				return fmt.Errorf("protected source ref could not be verified, so the archive was kept: %w", err)
			}
			return os.Rename(retired, staged)
		}); err != nil {
			return PrunedGitView{}, err
		}
	}
	if err := writePrunedGitViewMarker(archive, viewID, record); err != nil {
		return PrunedGitView{}, err
	}
	if err := os.RemoveAll(staged); err != nil {
		return PrunedGitView{}, err
	}
	return record, nil
}

func inspectPrunableGitView(ctx context.Context, dir, viewID string, baseline GitBaseline) (GitViewDelivery, error) {
	info, err := InspectGitViewDelivery(ctx, dir, viewID, baseline)
	if err != nil {
		return GitViewDelivery{}, err
	}
	if err := gitViewRetentionBlocker(info, baseline); err != nil {
		return GitViewDelivery{}, err
	}
	refs, err := gitViewCommand(ctx, info.Path, "for-each-ref", "--format=%(refname)")
	if err != nil || refs != "" {
		return GitViewDelivery{}, fmt.Errorf("%w: view has extra Git refs", ErrGitViewUnavailable)
	}
	unreachable, commits, err := unreachableGitViewObjects(ctx, info.Path, info.HeadCommit)
	if err != nil {
		return GitViewDelivery{}, err
	}
	if unreachable != 0 {
		return GitViewDelivery{}, fmt.Errorf("%w: view has %d unreachable Git objects (%d commits) that its delivered HEAD does not contain, such as rewound or discarded work; the archive was kept",
			ErrGitViewUnavailable, unreachable, commits)
	}
	return info, nil
}

// unreachableGitViewObjects proves that every object the view stores itself,
// loose or packed, is reachable from HEAD without reflogs. Objects reached
// only through alternates belong to the source repository: fsck --full would
// report the source's other history as unreachable from the view, and
// --no-full skips packs, which is where gc moves rewound commits.
//
// The walk reads the view's own objects only. Through its alternates it would
// also read the source, which a Run that can write there could fill with a
// commit whose parents lead to work the view's HEAD left out, and that work
// would count as reachable. A gc in the view leaves out of its own store what
// the source holds, so the walk stops at the first commit the view does not
// store itself; objects reached only past it stay unreached and keep the view.
func unreachableGitViewObjects(ctx context.Context, viewPath, head string) (int, int, error) {
	local, err := localGitViewObjects(ctx, viewPath)
	if err != nil || len(local) == 0 {
		return 0, 0, err
	}
	var unreachable, commits int
	err = withGitViewOwnObjects(viewPath, func(env []string) error {
		if err := streamHostGit(ctx, 5*time.Minute, viewPath, env, nil, func(line string) {
			delete(local, line)
		}, "rev-list", "--objects", "--no-object-names", "--missing=allow-any", head); err != nil {
			return err
		}
		if len(local) == 0 {
			return nil
		}
		ids := make([]string, 0, len(local))
		for id := range local {
			ids = append(ids, id)
		}
		types, err := runHostGit(ctx, time.Minute, viewPath, env, strings.NewReader(strings.Join(ids, "\n")+"\n"),
			"cat-file", "--batch-check=%(objecttype)")
		if err != nil {
			return err
		}
		unreachable, commits = len(ids), strings.Count(types, "commit\n")
		return nil
	})
	return unreachable, commits, err
}

// withGitViewOwnObjects runs use with the view's Git environment pointed at a
// daemon-made object directory beside the view: links to the view's own loose
// fan-out directories and pack directory, and no alternates, commit-graph, or
// other info files. Only plain directories are linked, as localGitViewObjects
// lists only those.
func withGitViewOwnObjects(viewPath string, use func(env []string) error) error {
	objects := filepath.Join(viewPath, ".git", "objects")
	if info, err := os.Lstat(objects); err != nil || !info.IsDir() {
		return fmt.Errorf("%w: view object directory is not a plain directory", ErrGitViewUnavailable)
	}
	entries, err := os.ReadDir(objects)
	if err != nil {
		return fmt.Errorf("%w: list view objects: %v", ErrGitViewUnavailable, err)
	}
	own, err := os.MkdirTemp(filepath.Dir(viewPath), ".objects-")
	if err != nil {
		return fmt.Errorf("%w: create view object directory: %v", ErrGitViewUnavailable, err)
	}
	defer os.RemoveAll(own)
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || (name != "pack" && !gitObjectFanout(name)) {
			continue
		}
		if err := os.Symlink(filepath.Join(objects, name), filepath.Join(own, name)); err != nil {
			return fmt.Errorf("%w: link view objects: %v", ErrGitViewUnavailable, err)
		}
	}
	return use(append(gitViewEnv(viewPath), "GIT_OBJECT_DIRECTORY="+own))
}

// gitObjectFanout reports whether name is a loose-object fan-out directory.
func gitObjectFanout(name string) bool {
	_, ok := gitObjectIDField(name + strings.Repeat("0", 38))
	return ok
}

// localGitViewObjects lists object IDs stored in the view's own object
// directory: loose fan-out files and every local pack index.
func localGitViewObjects(ctx context.Context, viewPath string) (map[string]struct{}, error) {
	objects := filepath.Join(viewPath, ".git", "objects")
	entries, err := os.ReadDir(objects)
	if err != nil {
		return nil, fmt.Errorf("%w: list view objects: %v", ErrGitViewUnavailable, err)
	}
	local := make(map[string]struct{})
	for _, entry := range entries {
		if !entry.IsDir() || len(entry.Name()) != 2 {
			continue
		}
		files, err := os.ReadDir(filepath.Join(objects, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("%w: list view objects: %v", ErrGitViewUnavailable, err)
		}
		for _, file := range files {
			if id, ok := gitObjectIDField(entry.Name() + file.Name()); ok {
				local[id] = struct{}{}
			}
		}
	}
	indexes, err := filepath.Glob(filepath.Join(objects, "pack", "*.idx"))
	if err != nil {
		return nil, err
	}
	for _, index := range indexes {
		file, err := os.Open(index)
		if err != nil {
			return nil, fmt.Errorf("%w: read view pack index: %v", ErrGitViewUnavailable, err)
		}
		err = streamHostGit(ctx, 5*time.Minute, viewPath, gitViewEnv(viewPath), file, func(line string) {
			// show-index prints "<offset> <object id> (<crc>)".
			if _, rest, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
				if id, ok := gitObjectIDField(rest); ok {
					local[id] = struct{}{}
				}
			}
		}, "show-index")
		_ = file.Close()
		if err != nil {
			return nil, err
		}
	}
	return local, nil
}

// retainGitViewCommit protects the delivered work with a source ref before the
// checkout is deleted. The work is first imported from the archive and the
// source proven to hold every object it adds over the baseline with matching
// content; a commit object, or placeholders under the work's ids, would leave
// the protected ref naming work nobody can rebuild. scratch is daemon-private
// storage for the ref creation's own Git directory.
func retainGitViewCommit(ctx context.Context, scratch string, baseline GitBaseline, record PrunedGitView, work GitViewDelivery) error {
	if err := inspectGitDeliverySource(ctx, baseline); err != nil {
		return err
	}
	if err := ensureGitWorkInSource(ctx, baseline, work); err != nil {
		return fmt.Errorf("the delivered work was not proven complete in the source repository, so the archive was kept: %w", err)
	}
	if _, err := gitSourceCommand(ctx, baseline, "cat-file", "-e", record.HeadCommit+"^{commit}"); err != nil {
		return fmt.Errorf("%w: delivered commit is missing from source", ErrGitViewUnavailable)
	}
	if err := createGitSourceRef(ctx, scratch, baseline, record.RetainedRef, record.HeadCommit); err != nil {
		return fmt.Errorf("protected source ref was not created, so the archive was kept: %w", err)
	}
	return nil
}

// InspectPrunedGitView is also the idempotent result after a completed prune.
func InspectPrunedGitView(ctx context.Context, viewsDir, viewID string, baseline GitBaseline) (PrunedGitView, error) {
	if !safeGitViewID(viewID) {
		return PrunedGitView{}, fmt.Errorf("%w: invalid view identity", ErrGitViewUnavailable)
	}
	archive, err := retiredGitViewDir(viewsDir)
	if err != nil {
		return PrunedGitView{}, err
	}
	markerDir, err := privateGitViewSubdir(archive, ".pruned")
	if err != nil {
		return PrunedGitView{}, err
	}
	marker := filepath.Join(markerDir, viewID+".json")
	markerInfo, err := os.Lstat(marker)
	if err != nil || !markerInfo.Mode().IsRegular() {
		return PrunedGitView{}, fmt.Errorf("%w: prune record is unavailable", ErrGitViewUnavailable)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		return PrunedGitView{}, fmt.Errorf("%w: prune record is unavailable", ErrGitViewUnavailable)
	}
	var record PrunedGitView
	if err := json.Unmarshal(data, &record); err != nil || record.Version != 1 ||
		record.SourceRoot != baseline.Root || record.BaselineCommit != baseline.Commit ||
		record.RetainedRef != retainedGitViewRef(viewID) || record.HeadCommit == "" {
		return PrunedGitView{}, fmt.Errorf("%w: prune record changed", ErrGitViewUnavailable)
	}
	if err := inspectGitDeliverySource(ctx, baseline); err != nil {
		return PrunedGitView{}, err
	}
	if _, err := gitSourceCommand(ctx, baseline, "cat-file", "-e", record.HeadCommit+"^{commit}"); gitInterrupted(err) {
		return PrunedGitView{}, err
	} else if err != nil {
		return PrunedGitView{}, fmt.Errorf("%w: retained source commit is unavailable", ErrGitViewUnavailable)
	}
	// Only a ref that is absent or names other work has changed; a check that
	// could not run says so instead.
	if err := verifyGitSourceRef(ctx, archive, baseline, record.RetainedRef, record.HeadCommit); errors.Is(err, errGitSourceRefOther) {
		return PrunedGitView{}, fmt.Errorf("protected source ref changed: %w", err)
	} else if err != nil {
		return PrunedGitView{}, err
	}
	// The checkout is gone, so the source is the only copy of the work.
	if err := proveGitPrunedWorkInSource(ctx, archive, baseline, record.HeadCommit); gitSourceLacksWork(err) {
		return PrunedGitView{}, fmt.Errorf("protected work is no longer complete in the source repository: %w", err)
	} else if err != nil {
		return PrunedGitView{}, err
	}
	return record, nil
}

func writePrunedGitViewMarker(archive, viewID string, record PrunedGitView) error {
	dir, err := privateGitViewSubdir(archive, ".pruned")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, viewID+".json")
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: prune record changed", ErrGitViewUnavailable)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var previous PrunedGitView
		if json.Unmarshal(data, &previous) != nil || previous != record {
			return fmt.Errorf("%w: prune record conflicts with retained view", ErrGitViewUnavailable)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".prune-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return err
	}
	return syncGitViewDirectory(dir)
}

func syncGitViewDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func privateGitViewSubdir(parent, name string) (string, error) {
	path := filepath.Join(parent, name)
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: private view directory changed", ErrGitViewUnavailable)
	}
	return path, nil
}

func directoryExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("%w: view path changed", ErrGitViewUnavailable)
	}
	return true, nil
}
