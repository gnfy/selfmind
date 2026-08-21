package promptassets

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RevisionUnavailableError identifies a pinned prompt revision that cannot be
// loaded or validated. Durable work must pause visibly rather than silently
// switching to the daemon's current prompt contract.
type RevisionUnavailableError struct {
	Hash string
	Err  error
}

func (e *RevisionUnavailableError) Error() string {
	return fmt.Sprintf("prompt revision %s is unavailable: %v", e.Hash, e.Err)
}

func (e *RevisionUnavailableError) Unwrap() error { return e.Err }

func IsRevisionUnavailable(err error) bool {
	var target *RevisionUnavailableError
	return errors.As(err, &target)
}

func revisionUnavailable(hash string, err error) error {
	return &RevisionUnavailableError{Hash: hash, Err: err}
}

type revisionFile struct {
	CatalogVersion int                  `json:"catalog_version"`
	SnapshotHash   string               `json:"snapshot_hash"`
	Files          map[string]FileState `json:"files"`
}

// SaveRevision persists the validated, content-addressed prompt snapshot used
// by durable background jobs. Revision files live below the operator prompt
// root and contain static prompt assets only, never runtime context.
func SaveRevision(snapshot *Snapshot) error {
	if snapshot == nil {
		return fmt.Errorf("prompt snapshot is required")
	}
	dir := filepath.Join(snapshot.Root(), ".revisions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create prompt revision directory: %w", err)
	}
	path := filepath.Join(dir, snapshot.Hash()+".json")
	if _, err := os.Stat(path); err == nil {
		if _, loadErr := LoadRevision(snapshot.Root(), snapshot.Hash()); loadErr == nil {
			return nil
		}
		// The active workspace was already validated and hashes to this exact
		// revision name, so it is authoritative enough to repair the daemon-owned
		// cache entry atomically.
	} else if !os.IsNotExist(err) {
		return err
	}
	files := make(map[string]FileState)
	for _, state := range snapshot.Files() {
		state.Path = ""
		files[state.ID] = state
	}
	data, err := json.MarshalIndent(revisionFile{
		CatalogVersion: CatalogVersion,
		SnapshotHash:   snapshot.Hash(),
		Files:          files,
	}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ".prompt-revision-*")
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
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("publish prompt revision: %w", err)
	}
	return nil
}

func LoadRevision(root, hash string) (*Snapshot, error) {
	hash = strings.ToLower(strings.TrimSpace(hash))
	if len(hash) != 64 || strings.Trim(hash, "0123456789abcdef") != "" {
		return nil, revisionUnavailable(hash, fmt.Errorf("invalid revision hash"))
	}
	path := filepath.Join(filepath.Clean(root), ".revisions", hash+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, revisionUnavailable(hash, fmt.Errorf("read revision: %w", err))
	}
	var revision revisionFile
	if err := json.Unmarshal(data, &revision); err != nil {
		return nil, revisionUnavailable(hash, fmt.Errorf("decode revision: %w", err))
	}
	if revision.CatalogVersion != CatalogVersion || revision.SnapshotHash != hash {
		return nil, revisionUnavailable(hash, fmt.Errorf("incompatible catalog or hash"))
	}
	if len(revision.Files) != len(Catalog()) {
		return nil, revisionUnavailable(hash, fmt.Errorf("incomplete file catalog"))
	}
	for _, spec := range Catalog() {
		state, ok := revision.Files[spec.ID]
		if !ok || len(state.Sections) != len(spec.Sections) {
			return nil, revisionUnavailable(hash, fmt.Errorf("missing %s sections", spec.ID))
		}
		state.ID = spec.ID
		state.Path = filepath.Join(filepath.Clean(root), filepath.FromSlash(spec.RelativePath))
		for _, policy := range spec.Sections {
			value, ok := state.Sections[policy.Name]
			if !ok || (value.Mode != ModeDefault && value.Mode != ModeCustom && value.Mode != ModeOff) {
				return nil, revisionUnavailable(hash, fmt.Errorf("invalid section %s/%s", spec.ID, policy.Name))
			}
			if value.Mode == ModeOff && !policy.AllowOff {
				return nil, revisionUnavailable(hash, fmt.Errorf("disables locked section %s/%s", spec.ID, policy.Name))
			}
			if len([]byte(value.Text)) > policy.MaxBytes {
				return nil, revisionUnavailable(hash, fmt.Errorf("exceeds section limit %s/%s", spec.ID, policy.Name))
			}
		}
		revision.Files[spec.ID] = state
	}
	if got := hashSnapshot(revision.Files); got != hash {
		return nil, revisionUnavailable(hash, fmt.Errorf("failed integrity validation"))
	}
	return &Snapshot{root: filepath.Clean(root), hash: hash, files: revision.Files}, nil
}
