package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"selfmind/internal/kernel"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/config"
	"selfmind/internal/promptassets"
)

const promptLastKnownGoodFile = "prompt-last-good.json"

const (
	PromptSourceActive        = "active"
	PromptSourceLastKnownGood = "last_known_good"
	PromptSourceBuiltIn       = "builtin"
)

// PromptSnapshotStatus describes how the runtime resolved its immutable prompt
// snapshot. Active prompt validation remains strict; only the daemon resolver
// is allowed to fall back after recording enough state for an operator to
// diagnose the invalid workspace.
type PromptSnapshotStatus struct {
	Source          string
	ActiveRoot      string
	SnapshotHash    string
	ActiveErrorKind string
	ActiveError     string
	FallbackError   string
	ActivationError string
}

func (s PromptSnapshotStatus) Degraded() bool {
	return s.Source != PromptSourceActive || s.ActiveError != "" || s.ActivationError != ""
}

type promptActivation struct {
	CatalogVersion int       `json:"catalog_version"`
	PromptRoot     string    `json:"prompt_root"`
	SnapshotHash   string    `json:"snapshot_hash"`
	ActivatedAt    time.Time `json:"activated_at"`
}

// LoadPromptSnapshot resolves the operator-owned prompt workspace next to the
// active config file and freezes it for one process lifetime. Missing files
// mean built-in defaults; malformed active files return an error. This strict
// entrypoint is used by prompt validation and apply commands.
func LoadPromptSnapshot(cfg *config.Config) (*promptassets.Snapshot, error) {
	if cfg == nil {
		return promptassets.Load(promptassets.PromptRoot("", ""))
	}
	root := promptassets.PromptRoot(cfg.Path, ResolveDataDir(cfg))
	return promptassets.Load(root)
}

// InspectRuntimePromptSnapshot reports the same resolution decision without
// updating the revision cache or last-known-good activation record. Doctor uses
// it to stay read-only.
func InspectRuntimePromptSnapshot(cfg *config.Config, dataDir string) (*promptassets.Snapshot, PromptSnapshotStatus) {
	return resolveRuntimePromptSnapshot(cfg, dataDir)
}

// ActivateRuntimePromptSnapshot persists the already selected runtime snapshot
// after the daemon has acquired process ownership. Callers must not activate a
// snapshot from a competing process that failed to acquire the gateway lock.
func ActivateRuntimePromptSnapshot(snapshot *promptassets.Snapshot, status PromptSnapshotStatus, dataDir string) PromptSnapshotStatus {
	if snapshot == nil {
		status.ActivationError = "activate prompt snapshot: snapshot is required"
		return status
	}
	if status.SnapshotHash != snapshot.Hash() || cleanAbsolutePath(status.ActiveRoot) != cleanAbsolutePath(snapshot.Root()) {
		status.ActivationError = "activate prompt snapshot: resolution metadata does not match the selected snapshot"
		return status
	}
	switch status.Source {
	case PromptSourceActive:
		if saveErr := promptassets.SaveRevision(snapshot); saveErr != nil {
			status.ActivationError = fmt.Sprintf("save prompt revision: %v", saveErr)
		} else if writeErr := writePromptActivation(dataDir, promptActivation{
			CatalogVersion: promptassets.CatalogVersion,
			PromptRoot:     status.ActiveRoot,
			SnapshotHash:   snapshot.Hash(),
			ActivatedAt:    time.Now().UTC(),
		}); writeErr != nil {
			status.ActivationError = fmt.Sprintf("save last-known-good activation: %v", writeErr)
		}
	case PromptSourceBuiltIn:
		// Do not promote built-ins to the operator's last-known-good choice, but
		// do persist the exact fallback hash when possible. Maintenance jobs
		// created during this daemon lifetime must still be able to resume after
		// the active workspace is repaired and the daemon restarts.
		if saveErr := promptassets.SaveRevision(snapshot); saveErr != nil {
			status.ActivationError = fmt.Sprintf("save built-in fallback revision: %v", saveErr)
		}
	case PromptSourceLastKnownGood:
		// Loading the selected snapshot already verified its content-addressed
		// revision. Never rewrite the activation pointer while active files are
		// invalid.
	default:
		status.ActivationError = fmt.Sprintf("activate prompt snapshot: unknown source %q", status.Source)
	}
	return status
}

func resolveRuntimePromptSnapshot(cfg *config.Config, dataDir string) (*promptassets.Snapshot, PromptSnapshotStatus) {
	root := runtimePromptRoot(cfg)
	status := PromptSnapshotStatus{Source: PromptSourceActive, ActiveRoot: root}
	snapshot, err := promptassets.Load(root)
	if err == nil {
		status.SnapshotHash = snapshot.Hash()
		return snapshot, status
	}

	status.ActiveError = err.Error()
	status.ActiveErrorKind = promptWorkspaceErrorKind(err)
	if fallback, fallbackErr := loadLastKnownGoodPrompt(dataDir, root); fallbackErr == nil {
		status.Source = PromptSourceLastKnownGood
		status.SnapshotHash = fallback.Hash()
		return fallback, status
	} else {
		status.FallbackError = fallbackErr.Error()
	}

	snapshot = promptassets.Empty(root)
	status.Source = PromptSourceBuiltIn
	status.SnapshotHash = snapshot.Hash()
	return snapshot, status
}

func runtimePromptRoot(cfg *config.Config) string {
	if cfg == nil {
		return cleanAbsolutePath(promptassets.PromptRoot("", ""))
	}
	return cleanAbsolutePath(promptassets.PromptRoot(cfg.Path, ResolveDataDir(cfg)))
}

func cleanAbsolutePath(path string) string {
	path = filepath.Clean(strings.TrimSpace(path))
	if abs, err := filepath.Abs(path); err == nil {
		return filepath.Clean(abs)
	}
	return path
}

func promptActivationPath(dataDir string) string {
	return filepath.Join(cleanAbsolutePath(dataDir), promptLastKnownGoodFile)
}

func writePromptActivation(dataDir string, activation promptActivation) error {
	if strings.TrimSpace(dataDir) == "" {
		return fmt.Errorf("data directory is required")
	}
	dir := cleanAbsolutePath(dataDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(activation, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ".prompt-last-good-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, promptActivationPath(dataDir)); err != nil {
		return err
	}
	return nil
}

func loadLastKnownGoodPrompt(dataDir, activeRoot string) (*promptassets.Snapshot, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, fmt.Errorf("data directory is required")
	}
	path := promptActivationPath(dataDir)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("read activation record: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("activation record must be a regular file, not a symlink or special file")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("activation record must not be group- or world-writable")
	}
	if info.Size() > 16*1024 {
		return nil, fmt.Errorf("activation record is too large")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read activation record: %w", err)
	}
	var activation promptActivation
	if err := json.Unmarshal(data, &activation); err != nil {
		return nil, fmt.Errorf("decode activation record: %w", err)
	}
	if activation.CatalogVersion != promptassets.CatalogVersion {
		return nil, fmt.Errorf("activation record uses prompt catalog %d, expected %d", activation.CatalogVersion, promptassets.CatalogVersion)
	}
	if cleanAbsolutePath(activation.PromptRoot) != cleanAbsolutePath(activeRoot) {
		return nil, fmt.Errorf("activation record belongs to a different prompt root")
	}
	snapshot, err := promptassets.LoadRevision(activeRoot, activation.SnapshotHash)
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func promptWorkspaceErrorKind(err error) string {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "group- or world-writable"):
		return "unsafe_permissions"
	case strings.Contains(message, "owned by the current user"):
		return "unsafe_permissions"
	case strings.Contains(message, "symlink or special file"), strings.Contains(message, "must be a regular file"):
		return "unsafe_file"
	case strings.Contains(message, "invalid prompt"):
		return "invalid_content"
	case strings.Contains(message, "inspect prompt"), strings.Contains(message, "read prompt"):
		return "io"
	default:
		return "invalid"
	}
}

// PromptBuiltInPreview returns static prompt assets only. Dynamic memory,
// project context, task data, credentials, and tool payloads are never exposed.
func PromptBuiltInPreview(cfg *config.Config, fileID string) (string, error) {
	switch fileID {
	case promptassets.FileAgent:
		soul := ""
		if cfg != nil {
			soul = cfg.Agent.Soul
		}
		return kernel.ForegroundPromptDefaults(soul), nil
	case promptassets.FileMemoryExtract:
		return strings.Join([]string{
			"## Post-run Analysis (locked contract)\n\n" + postRunAnalyzerSystemPrompt,
			"## Batch Post-run Analysis (locked contract)\n\n" + postRunBatchAnalyzerSystemPrompt,
			"## Consolidation (locked contract)\n\n" + memoryJudgeSystemPrompt,
		}, "\n\n"), nil
	case promptassets.FileBackgroundReview:
		return kernel.BackgroundReviewPromptDefaults(), nil
	case promptassets.FileSkillCurator:
		return skillCuratorSystemPrompt, nil
	case promptassets.FileSummarizer:
		return kernel.SummarizerPromptDefaults(), nil
	case promptassets.FileSemanticRecall:
		return memory.SemanticRecallPromptDefaults(), nil
	default:
		return "", fmt.Errorf("unknown prompt %q", fileID)
	}
}

func promptRevision(current *promptassets.Snapshot, hash string) (*promptassets.Snapshot, error) {
	hash = strings.TrimSpace(hash)
	if hash == "" || (current != nil && current.Hash() == hash) {
		return current, nil
	}
	if current == nil || strings.TrimSpace(current.Root()) == "" {
		return nil, &promptassets.RevisionUnavailableError{Hash: hash, Err: fmt.Errorf("prompt revision root is unavailable")}
	}
	return promptassets.LoadRevision(current.Root(), hash)
}
