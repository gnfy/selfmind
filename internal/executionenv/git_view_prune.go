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
// its entire local Git object graph is reachable from its delivered HEAD, with
// no working, ignored, or extra-ref work. A protected ref in the source keeps
// that HEAD reachable even if the delivery branch later moves. A staged
// directory and marker make a crash during deletion retryable.
func PruneRetiredGitView(ctx context.Context, viewsDir, viewID string, baseline GitBaseline) (PrunedGitView, error) {
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
	if err := retainGitViewCommit(ctx, baseline, record); err != nil {
		return PrunedGitView{}, err
	}
	if retiredExists {
		// Ref creation and final inspection are separate effects. Recheck all
		// content before atomically taking the view out of restore's path.
		current, err := inspectPrunableGitView(ctx, archive, viewID, baseline)
		if err != nil || current.HeadCommit != record.HeadCommit {
			return PrunedGitView{}, fmt.Errorf("%w: retained view changed during prune", ErrGitViewUnavailable)
		}
		if err := os.Rename(retired, staged); err != nil {
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
	if info.Uncommitted != 0 || info.Untracked != 0 || info.Ignored != 0 ||
		info.HeadCommit != baseline.Commit && info.DeliveredBranch == "" {
		return GitViewDelivery{}, fmt.Errorf("%w: view contains undelivered or uncommitted work", ErrGitViewUnavailable)
	}
	refs, err := gitViewCommand(ctx, info.Path, "for-each-ref", "--format=%(refname)")
	if err != nil || refs != "" {
		return GitViewDelivery{}, fmt.Errorf("%w: view has extra Git refs", ErrGitViewUnavailable)
	}
	// The view has an alternate source object store. --no-full checks only
	// objects owned by this view; --no-reflogs exposes rewound local work.
	unreachable, err := gitViewCommandRawTimeout(ctx, info.Path, 5*time.Minute,
		"fsck", "--no-full", "--no-reflogs", "--unreachable", "--no-progress")
	if err != nil || strings.TrimSpace(unreachable) != "" {
		return GitViewDelivery{}, fmt.Errorf("%w: view has unreachable or invalid Git objects", ErrGitViewUnavailable)
	}
	return info, nil
}

func retainGitViewCommit(ctx context.Context, baseline GitBaseline, record PrunedGitView) error {
	if err := inspectGitDeliverySource(ctx, baseline); err != nil {
		return err
	}
	if _, err := gitViewCommand(ctx, baseline.Root, "cat-file", "-e", record.HeadCommit+"^{commit}"); err != nil {
		return fmt.Errorf("%w: delivered commit is missing from source", ErrGitViewUnavailable)
	}
	zero := strings.Repeat("0", len(record.HeadCommit))
	if _, err := gitViewCommand(ctx, baseline.Root, "update-ref", record.RetainedRef, record.HeadCommit, zero); err != nil {
		if existing, checkErr := gitViewCommand(ctx, baseline.Root, "rev-parse", "--verify", record.RetainedRef+"^{commit}"); checkErr != nil || existing != record.HeadCommit {
			return fmt.Errorf("%w: protected source ref differs from view HEAD", ErrGitViewUnavailable)
		}
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
	if _, err := gitViewCommand(ctx, baseline.Root, "cat-file", "-e", record.HeadCommit+"^{commit}"); err != nil {
		return PrunedGitView{}, fmt.Errorf("%w: retained source commit is unavailable", ErrGitViewUnavailable)
	}
	if head, err := gitViewCommand(ctx, baseline.Root, "rev-parse", "--verify", record.RetainedRef+"^{commit}"); err != nil || head != record.HeadCommit {
		return PrunedGitView{}, fmt.Errorf("%w: protected source ref changed", ErrGitViewUnavailable)
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
