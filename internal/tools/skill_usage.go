package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const (
	SkillStateActive   = "active"
	SkillStateStale    = "stale"
	SkillStateDisabled = "disabled"
	SkillStateArchived = "archived"

	SkillSourceAgentCreated = "agent-created"
	SkillSourceCatalog      = "catalog-installed"
	SkillSourceManual       = "manual"
)

// SkillUsageRecord stores operational metadata outside SKILL.md so user-authored
// skill content does not fight with lifecycle bookkeeping.
type SkillUsageRecord struct {
	Name        string `json:"name"`
	Source      string `json:"source"`
	CreatedBy   string `json:"created_by"`
	State       string `json:"state"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	LastUsed    string `json:"last_used"`
	LastViewed  string `json:"last_viewed,omitempty"`
	LastPatched string `json:"last_patched,omitempty"`
	UseCount    int    `json:"use_count,omitempty"`
	ViewCount   int    `json:"view_count,omitempty"`
	PatchCount  int    `json:"patch_count"`
	Pinned      bool   `json:"pinned"`
}

func usageFilePath(skillsDir string) string {
	return filepath.Join(skillsDir, ".usage.json")
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func loadSkillUsageForDir(skillsDir string) (map[string]SkillUsageRecord, error) {
	records := map[string]SkillUsageRecord{}
	data, err := os.ReadFile(usageFilePath(skillsDir))
	if err != nil {
		if os.IsNotExist(err) {
			return records, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return records, nil
	}
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, err
	}
	return records, nil
}

func saveSkillUsageForDir(skillsDir string, records map[string]SkillUsageRecord) error {
	if err := os.MkdirAll(skillsDir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	tmp := usageFilePath(skillsDir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, usageFilePath(skillsDir)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func updateSkillUsageForDir(skillsDir, name string, mutator func(*SkillUsageRecord)) error {
	records, err := loadSkillUsageForDir(skillsDir)
	if err != nil {
		return err
	}
	rec := records[name]
	if rec.Name == "" {
		rec.Name = name
		rec.Source = SkillSourceManual
		rec.State = SkillStateActive
		rec.CreatedAt = nowRFC3339()
	}
	if rec.State == "" {
		rec.State = SkillStateActive
	}
	mutator(&rec)
	if rec.UpdatedAt == "" {
		rec.UpdatedAt = nowRFC3339()
	}
	records[name] = rec
	return saveSkillUsageForDir(skillsDir, records)
}

// LoadSkillUsage returns usage metadata for the tenant's skill directory.
func LoadSkillUsage(tenantID string) (map[string]SkillUsageRecord, error) {
	dir, err := getSkillsDir(tenantID)
	if err != nil {
		return nil, err
	}
	return loadSkillUsageForDir(dir)
}

func MarkSkillCreated(tenantID, name, source, createdBy string) error {
	dir, err := getSkillsDir(tenantID)
	if err != nil {
		return err
	}
	return updateSkillUsageForDir(dir, name, func(rec *SkillUsageRecord) {
		now := nowRFC3339()
		if source == "" {
			source = SkillSourceAgentCreated
		}
		if createdBy == "" {
			createdBy = "skill_manage"
		}
		rec.Source = source
		rec.CreatedBy = createdBy
		rec.State = SkillStateActive
		if rec.CreatedAt == "" {
			rec.CreatedAt = now
		}
		rec.UpdatedAt = now
	})
}

func MarkSkillPatched(tenantID, name string) error {
	dir, err := getSkillsDir(tenantID)
	if err != nil {
		return err
	}
	return updateSkillUsageForDir(dir, name, func(rec *SkillUsageRecord) {
		now := nowRFC3339()
		rec.PatchCount++
		rec.LastPatched = now
		rec.State = SkillStateActive
		rec.UpdatedAt = now
	})
}

func MarkSkillUsed(tenantID, name string) error {
	dir, err := getSkillsDir(tenantID)
	if err != nil {
		return err
	}
	return updateSkillUsageForDir(dir, name, func(rec *SkillUsageRecord) {
		now := nowRFC3339()
		rec.LastUsed = now
		rec.UseCount++
		rec.UpdatedAt = now
		if rec.State == SkillStateStale {
			rec.State = SkillStateActive
		}
	})
}

func MarkSkillViewed(tenantID, name string) error {
	dir, err := getSkillsDir(tenantID)
	if err != nil {
		return err
	}
	return updateSkillUsageForDir(dir, name, func(rec *SkillUsageRecord) {
		now := nowRFC3339()
		rec.LastViewed = now
		rec.ViewCount++
		rec.UpdatedAt = now
		if rec.State == SkillStateStale {
			rec.State = SkillStateActive
		}
	})
}

func SetSkillPinned(tenantID, name string, pinned bool) error {
	dir, err := getSkillsDir(tenantID)
	if err != nil {
		return err
	}
	return updateSkillUsageForDir(dir, name, func(rec *SkillUsageRecord) {
		rec.Pinned = pinned
		rec.UpdatedAt = nowRFC3339()
	})
}

func SetSkillState(tenantID, name, state string) error {
	dir, err := getSkillsDir(tenantID)
	if err != nil {
		return err
	}
	return updateSkillUsageForDir(dir, name, func(rec *SkillUsageRecord) {
		rec.State = state
		rec.UpdatedAt = nowRFC3339()
	})
}
