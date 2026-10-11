package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"selfmind/internal/tools/envprofiles"
	"selfmind/internal/tools/sandbox"
)

const sandboxRecoveryRecordLimit = 64 << 10

// Recovery records contain execution scope, never credentials or process env.
type sandboxRecoveryRecord struct {
	Version int                          `json:"version"`
	Writes  []envprofiles.HostStateWrite `json:"writes"`
}

func (r *ExecutionRuntime) recordSandboxCanary(canary string, plan envprofiles.HostStatePlan) (string, error) {
	dir := r.sandboxCanaryDir()
	if dir == "" || !sandbox.ValidCanary(canary) {
		return "", errors.New("sandbox recovery directory or canary unavailable")
	}
	if err := validateRecoveryWrites(plan.Writes); err != nil {
		return "", err
	}
	data, err := json.Marshal(sandboxRecoveryRecord{Version: 1, Writes: plan.Writes})
	if err != nil {
		return "", err
	}
	if len(data) > sandboxRecoveryRecordLimit {
		return "", errors.New("sandbox recovery scope exceeds record limit")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	marker := filepath.Join(dir, canary)
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	_, writeErr := f.Write(data)
	err = errors.Join(writeErr, f.Sync(), f.Close())
	if err == nil {
		parent, openErr := os.Open(dir)
		if openErr == nil {
			err = errors.Join(parent.Sync(), parent.Close())
		} else {
			err = openErr
		}
	}
	if err != nil {
		_ = os.Remove(marker) // Nothing has started under this canary yet.
		return "", err
	}
	return marker, nil
}

func readSandboxCanary(marker string) (envprofiles.HostStatePlan, bool, error) {
	var plan envprofiles.HostStatePlan
	info, err := os.Lstat(marker)
	if err != nil {
		return plan, false, err
	}
	if !info.Mode().IsRegular() || info.Size() > sandboxRecoveryRecordLimit {
		return plan, false, errors.New("invalid sandbox recovery record")
	}
	f, err := os.Open(marker)
	if err != nil {
		return plan, false, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, sandboxRecoveryRecordLimit+1))
	if err != nil || len(data) > sandboxRecoveryRecordLimit {
		return plan, false, errors.New("cannot read bounded sandbox recovery record")
	}
	if len(data) == 0 {
		// Legacy markers prove only process ownership. Never infer cache scope
		// from the operator's current environment or the whole profile catalog.
		return plan, true, nil
	}
	var record sandboxRecoveryRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return plan, false, fmt.Errorf("invalid sandbox recovery record: %w", err)
	}
	if record.Version != 1 {
		return plan, false, fmt.Errorf("unsupported sandbox recovery record version %d", record.Version)
	}
	if err := validateRecoveryWrites(record.Writes); err != nil {
		return plan, false, err
	}
	plan.Writes = record.Writes
	return plan, false, nil
}

// Reject widened records and replaced ancestors before scanning any cache.
// The final cache path may itself be a planted link: the repair quarantines
// that entry without following it.
func validateRecoveryWrites(writes []envprofiles.HostStateWrite) error {
	for _, write := range writes {
		root, path := write.Root, write.Path
		if !filepath.IsAbs(root) || !filepath.IsAbs(path) || filepath.Clean(root) != root || filepath.Clean(path) != path {
			return errors.New("sandbox recovery paths must be canonical absolute paths")
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return errors.New("sandbox recovery cache must be a declared subpath")
		}
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && resolved != root {
			return errors.New("sandbox recovery state root changed")
		}
		for current := filepath.Dir(path); current != root; current = filepath.Dir(current) {
			info, err := os.Lstat(current)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			if !info.IsDir() {
				return errors.New("sandbox recovery cache ancestor changed")
			}
		}
	}
	return nil
}
