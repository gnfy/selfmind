package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const skillCatalogLockVersion = 1

// SkillCatalogLock records skills installed through skill_catalog. It gives
// future update/reinstall logic a Hermes-like provenance boundary without
// mixing lifecycle metadata into user-authored SKILL.md files.
type SkillCatalogLock struct {
	Version int                              `json:"version"`
	Skills  map[string]SkillCatalogLockEntry `json:"skills"`
}

// SkillCatalogLockEntry stores enough install metadata to distinguish catalog
// skills from hand-written and background-review skills.
type SkillCatalogLockEntry struct {
	Name           string   `json:"name"`
	Source         string   `json:"source"`
	SourceKind     string   `json:"source_kind"`
	OriginalSource string   `json:"original_source,omitempty"`
	InstallPath    string   `json:"install_path"`
	ContentHash    string   `json:"content_hash"`
	Files          []string `json:"files,omitempty"`
	InstalledAt    string   `json:"installed_at"`
	UpdatedAt      string   `json:"updated_at"`
	LastBackupPath string   `json:"last_backup_path,omitempty"`
}

type skillInstallCollision struct {
	Path string
	Kind string
}

func skillCatalogDir(skillsDir string) string {
	return filepath.Join(skillsDir, ".catalog")
}

func skillCatalogLockPath(skillsDir string) string {
	return filepath.Join(skillCatalogDir(skillsDir), "lock.json")
}

func loadSkillCatalogLockForDir(skillsDir string) (*SkillCatalogLock, error) {
	lock := &SkillCatalogLock{
		Version: skillCatalogLockVersion,
		Skills:  map[string]SkillCatalogLockEntry{},
	}
	data, err := os.ReadFile(skillCatalogLockPath(skillsDir))
	if err != nil {
		if os.IsNotExist(err) {
			return lock, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return lock, nil
	}
	if err := json.Unmarshal(data, lock); err != nil {
		return nil, err
	}
	if lock.Version == 0 {
		lock.Version = skillCatalogLockVersion
	}
	if lock.Skills == nil {
		lock.Skills = map[string]SkillCatalogLockEntry{}
	}
	return lock, nil
}

func saveSkillCatalogLockForDir(skillsDir string, lock *SkillCatalogLock) error {
	if lock == nil {
		lock = &SkillCatalogLock{}
	}
	lock.Version = skillCatalogLockVersion
	if lock.Skills == nil {
		lock.Skills = map[string]SkillCatalogLockEntry{}
	}
	if err := os.MkdirAll(skillCatalogDir(skillsDir), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(skillCatalogLockPath(skillsDir), string(data))
}

func recordSkillCatalogInstall(tenantID string, entry SkillCatalogLockEntry, invocation ...map[string]interface{}) error {
	skillsDir, err := getSkillsDir(tenantID, invocation...)
	if err != nil {
		return err
	}
	lock, err := loadSkillCatalogLockForDir(skillsDir)
	if err != nil {
		return err
	}
	now := nowRFC3339()
	previous := lock.Skills[entry.Name]
	if previous.InstalledAt != "" {
		entry.InstalledAt = previous.InstalledAt
	}
	if entry.InstalledAt == "" {
		entry.InstalledAt = now
	}
	entry.UpdatedAt = now
	lock.Skills[entry.Name] = entry
	return saveSkillCatalogLockForDir(skillsDir, lock)
}

func skillInstallSourceKind(source string) string {
	switch {
	case strings.HasPrefix(source, "official/"):
		return "official"
	case isHTTPURL(source):
		return "url"
	default:
		return "local"
	}
}

func existingSkillInstallCollisions(skillsDir, safeName string) []skillInstallCollision {
	var collisions []skillInstallCollision
	for _, c := range []skillInstallCollision{
		{Path: filepath.Join(skillsDir, safeName), Kind: "directory"},
		{Path: filepath.Join(skillsDir, safeName+".md"), Kind: "legacy-file"},
	} {
		if _, err := os.Stat(c.Path); err == nil {
			collisions = append(collisions, c)
		}
	}
	return collisions
}

func formatInstallCollisionError(name string, collisions []skillInstallCollision) error {
	var kinds []string
	for _, c := range collisions {
		kinds = append(kinds, c.Kind)
	}
	sort.Strings(kinds)
	return fmt.Errorf("skill %q already exists (%s); pass force=true to overwrite", name, strings.Join(kinds, ", "))
}

func backupSkillInstallCollisions(skillsDir, safeName string, collisions []skillInstallCollision) (string, error) {
	if len(collisions) == 0 {
		return "", nil
	}
	backupDir := filepath.Join(skillCatalogDir(skillsDir), "backups", time.Now().UTC().Format("20060102-150405.000000000")+"-"+safeName)
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return "", err
	}
	for _, c := range collisions {
		dest := filepath.Join(backupDir, filepath.Base(c.Path))
		if err := os.Rename(c.Path, dest); err != nil {
			_ = restoreSkillInstallBackup(skillsDir, backupDir)
			return "", err
		}
	}
	return backupDir, nil
}

func restoreSkillInstallBackup(skillsDir, backupDir string) error {
	if backupDir == "" {
		return nil
	}
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		src := filepath.Join(backupDir, entry.Name())
		dest := filepath.Join(skillsDir, entry.Name())
		_ = os.RemoveAll(dest)
		if err := os.Rename(src, dest); err != nil {
			return err
		}
	}
	_ = os.Remove(backupDir)
	return nil
}

func hashSkillDirectory(skillDir string) (string, []string, error) {
	var files []string
	if err := filepath.WalkDir(skillDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(skillDir, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	}); err != nil {
		return "", nil, err
	}
	sort.Strings(files)
	hasher := sha256.New()
	for _, rel := range files {
		path := filepath.Join(skillDir, filepath.FromSlash(rel))
		data, err := os.ReadFile(path)
		if err != nil {
			return "", nil, err
		}
		hasher.Write([]byte(rel))
		hasher.Write([]byte{0})
		hasher.Write(data)
		hasher.Write([]byte{0})
	}
	return hex.EncodeToString(hasher.Sum(nil)), files, nil
}
