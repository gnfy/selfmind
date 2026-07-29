package executionenv

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Run scratch.
//
// Isolated commands used to get a fresh private /tmp per invocation, so a file
// written by one command was gone by the next, and host-mode commands saw the
// real /tmp instead — two different namespaces for the same literal path. Work
// that naturally spans commands (write a kubeconfig, then use it; dump JSON,
// then parse it) silently lost its intermediate state.
//
// A lease therefore owns one durable scratch directory for the whole run:
//
//	<runtime>/leases/<lease-id>/tmp     shared temp space for the run
//	<runtime>/leases/<lease-id>/state   credential/state overlays
//	<runtime>/toolchain/<person-id>     person-level caches (no credentials)
//
// $SELFMIND_RUN_TMP is the ONLY cross-mode promise: it is the same absolute path
// under host and isolated execution. Isolated execution additionally binds it at
// /tmp for convenience, but literal /tmp continuity is never promised because
// host mode cannot provide it.

const (
	// ScratchTmpEnvVar is the stable, mode-independent path for work that must
	// survive from one command of a run to the next.
	ScratchTmpEnvVar = "SELFMIND_RUN_TMP"
	// ScratchStateEnvVar exposes the run's state overlay root. Tool
	// environment profiles redirect their own variables into subdirectories of
	// it; this is the escape hatch for anything not covered by a profile.
	ScratchStateEnvVar = "SELFMIND_RUN_STATE"

	scratchDirPerm = 0o700
)

// LeaseScratch is one run's scratch space.
type LeaseScratch struct {
	LeaseID  string
	Root     string
	TmpDir   string
	StateDir string
}

// EnvOverrides returns the environment entries that expose the scratch space.
// TMPDIR is redirected too, so tools that honor it inherit run-scoped temp
// space without knowing anything about SelfMind.
func (s LeaseScratch) EnvOverrides() []string {
	if s.TmpDir == "" {
		return nil
	}
	return []string{
		ScratchTmpEnvVar + "=" + s.TmpDir,
		ScratchStateEnvVar + "=" + s.StateDir,
		"TMPDIR=" + s.TmpDir,
	}
}

var (
	runtimeRootMu sync.RWMutex
	runtimeRoot   string
)

// SetRuntimeRoot installs the execution runtime directory. Ownership is
// explicit: application wiring sets it at startup.
//
// The root must not live under the system temp directory. Isolated execution
// binds a lease's tmp directory at /tmp, and that bind shadows every real path
// beneath /tmp — a scratch root inside /tmp would make $SELFMIND_RUN_TMP
// resolve to a path that does not exist inside the sandbox (verified: the
// command fails with "Directory nonexistent").
func SetRuntimeRoot(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("runtime root is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve runtime root: %w", err)
	}
	abs = filepath.Clean(abs)
	if withinDir(os.TempDir(), abs) || withinDir("/tmp", abs) {
		return fmt.Errorf("runtime root %s must not live under the system temp directory: "+
			"binding a lease tmp directory at /tmp would shadow it", abs)
	}
	if err := os.MkdirAll(abs, scratchDirPerm); err != nil {
		return fmt.Errorf("create runtime root: %w", err)
	}
	runtimeRootMu.Lock()
	runtimeRoot = abs
	runtimeRootMu.Unlock()
	return nil
}

// RuntimeRoot returns the configured execution runtime directory, or "" when
// none is installed (scratch is then unavailable and callers degrade to the
// previous private-/tmp behaviour).
func RuntimeRoot() string {
	runtimeRootMu.RLock()
	defer runtimeRootMu.RUnlock()
	return runtimeRoot
}

// EnsureLeaseScratch creates (idempotently) the scratch space for a lease.
func EnsureLeaseScratch(leaseID string) (LeaseScratch, error) {
	root := RuntimeRoot()
	if root == "" {
		return LeaseScratch{}, fmt.Errorf("execution runtime root is not configured")
	}
	component, err := safePathComponent(leaseID)
	if err != nil {
		return LeaseScratch{}, err
	}
	scratch := LeaseScratch{
		LeaseID: leaseID,
		Root:    filepath.Join(root, "leases", component),
	}
	scratch.TmpDir = filepath.Join(scratch.Root, "tmp")
	scratch.StateDir = filepath.Join(scratch.Root, "state")
	for _, dir := range []string{scratch.Root, scratch.TmpDir, scratch.StateDir} {
		if err := os.MkdirAll(dir, scratchDirPerm); err != nil {
			return LeaseScratch{}, fmt.Errorf("create run scratch: %w", err)
		}
		// MkdirAll honours umask, so tighten explicitly: this tree can hold
		// copies of credential state.
		if err := os.Chmod(dir, scratchDirPerm); err != nil {
			return LeaseScratch{}, fmt.Errorf("secure run scratch: %w", err)
		}
	}
	return scratch, nil
}

// ToolchainDir returns a person-level persistent cache directory. Toolchain
// caches are not credentials and are expensive to rebuild, so they deliberately
// outlive a single run — a per-lease cache would make every run a cold build.
func ToolchainDir(personID, key string) (string, error) {
	root := RuntimeRoot()
	if root == "" {
		return "", fmt.Errorf("execution runtime root is not configured")
	}
	person, err := safePathComponent(personID)
	if err != nil {
		return "", err
	}
	sub, err := safeRelativePath(key)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "toolchain", person, sub)
	if err := os.MkdirAll(dir, scratchDirPerm); err != nil {
		return "", fmt.Errorf("create toolchain cache: %w", err)
	}
	return dir, nil
}

// scratchSizeCacheTTL bounds how often a lease's scratch is measured.
//
// The quota check runs before EVERY command, and measuring means walking the
// whole tree. A run that writes tens of thousands of intermediate files would
// pay that walk on each subsequent command — a cost proportional to its own
// output. A short cache keeps the check cheap while still noticing growth well
// before the soft limit matters.
const scratchSizeCacheTTL = 30 * time.Second

type scratchSizeEntry struct {
	bytes      int64
	measuredAt time.Time
}

var (
	scratchSizeMu    sync.Mutex
	scratchSizeCache = map[string]scratchSizeEntry{}
)

// ScratchBytesCached reports a lease's scratch size, reusing a recent
// measurement. Use it on the hot path; ScratchBytes always measures.
func ScratchBytesCached(leaseID string) (int64, error) {
	leaseID = strings.TrimSpace(leaseID)
	if leaseID == "" {
		return 0, nil
	}
	now := time.Now()
	scratchSizeMu.Lock()
	if entry, ok := scratchSizeCache[leaseID]; ok && now.Sub(entry.measuredAt) < scratchSizeCacheTTL {
		scratchSizeMu.Unlock()
		return entry.bytes, nil
	}
	scratchSizeMu.Unlock()

	bytes, err := ScratchBytes(leaseID)
	if err != nil {
		return bytes, err
	}
	scratchSizeMu.Lock()
	scratchSizeCache[leaseID] = scratchSizeEntry{bytes: bytes, measuredAt: now}
	scratchSizeMu.Unlock()
	return bytes, nil
}

// ScratchBytes reports the on-disk size of a lease's scratch space. Used for the
// quota notice: a run that accumulates gigabytes of intermediates should be
// visible rather than silently filling the disk.
func ScratchBytes(leaseID string) (int64, error) {
	root := RuntimeRoot()
	if root == "" {
		return 0, nil
	}
	component, err := safePathComponent(leaseID)
	if err != nil {
		return 0, err
	}
	return dirBytes(filepath.Join(root, "leases", component))
}

// CleanupLeaseScratch removes one lease's scratch space.
func CleanupLeaseScratch(leaseID string) error {
	scratchSizeMu.Lock()
	delete(scratchSizeCache, strings.TrimSpace(leaseID))
	scratchSizeMu.Unlock()
	root := RuntimeRoot()
	if root == "" {
		return nil
	}
	component, err := safePathComponent(leaseID)
	if err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(root, "leases", component))
}

// SweepExpiredScratch deletes lease scratch directories older than ttl whose
// lease is not reported active. Cleanup is delayed rather than immediate so a
// finished run's intermediates remain inspectable for a while.
func SweepExpiredScratch(ttl time.Duration, active func(leaseID string) bool, now time.Time) (removed int, err error) {
	root := RuntimeRoot()
	if root == "" || ttl <= 0 {
		return 0, nil
	}
	leasesDir := filepath.Join(root, "leases")
	entries, err := os.ReadDir(leasesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if active != nil && active(entry.Name()) {
			continue
		}
		info, statErr := entry.Info()
		if statErr != nil {
			continue
		}
		if now.Sub(info.ModTime()) < ttl {
			continue
		}
		if rmErr := os.RemoveAll(filepath.Join(leasesDir, entry.Name())); rmErr == nil {
			removed++
		}
	}
	return removed, nil
}

func dirBytes(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return total, err
	}
	return total, nil
}

// safePathComponent rejects anything that is not a single, non-traversing path
// element. Lease and person ids come from the control plane, but a scratch root
// must never be derivable from caller-controlled traversal.
func safePathComponent(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("identifier is required")
	}
	if value != filepath.Base(value) || value == "." || value == ".." ||
		strings.ContainsAny(value, `/\`) || strings.Contains(value, "..") {
		return "", fmt.Errorf("unsafe identifier %q", value)
	}
	return value, nil
}

// safeRelativePath allows nested keys ("gcloud/logs") but never traversal.
func safeRelativePath(value string) (string, error) {
	value = strings.TrimSpace(strings.Trim(strings.ReplaceAll(value, `\`, "/"), "/"))
	if value == "" {
		return "", fmt.Errorf("path key is required")
	}
	clean := filepath.Clean(filepath.FromSlash(value))
	if filepath.IsAbs(clean) || clean == "." || clean == ".." ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe path key %q", value)
	}
	return clean, nil
}

func withinDir(root, target string) bool {
	root = strings.TrimSpace(root)
	if root == "" {
		return false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(absRoot), filepath.Clean(target))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
