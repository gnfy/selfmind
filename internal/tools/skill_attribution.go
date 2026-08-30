package tools

import (
	"path/filepath"
	"strings"
	"sync"

	"selfmind/internal/control"
	"selfmind/internal/platform/log"
)

// skillAttributionPathArgKeys are the structured argument names attribution
// inspects. The set is explicit rather than a pattern: a shell command string is
// deliberately not parsed, because reconstructing a path from a command line is
// guesswork this runtime does not need when its file tools take real path
// arguments.
var skillAttributionPathArgKeys = map[string]bool{
	"path": true, "file_path": true, "filepath": true, "dir": true,
	"directory": true, "paths": true, "files": true, "file_paths": true,
}

// skillPackageEntry is one discovered package reduced to what attribution needs.
type skillPackageEntry struct {
	Path        string
	SkillKey    string
	Name        string
	PackageName string
	Scope       string
	Provenance  string
}

type skillPackageIndex struct {
	entries []skillPackageEntry
	byName  map[string]skillPackageEntry
}

// Discovery walks the filesystem, so the index is built once per work unit
// rather than once per tool call. The cache is bounded because a long-lived
// daemon serves many work units.
const skillAttributionCacheCap = 16

var (
	skillAttributionMu    sync.Mutex
	skillAttributionCache = map[string]*skillPackageIndex{}
	skillAttributionOrder []string
)

// NewSkillAttributionObserver records implicit Skill use: a work unit read a
// package's content without activating it. It is attribution, not activation. It
// consumes no Skill context budget, freezes no delivery receipt, and does not
// occupy the one-Skill-per-work-unit budget, so viewing continues not to count
// as activation.
func NewSkillAttributionObserver(store *control.Store) SkillAttributionObserver {
	if store == nil {
		return nil
	}
	return func(toolName string, args map[string]interface{}) {
		scope, ok := InvocationScopeFromArgs(args)
		if !ok {
			return
		}
		runID := strings.TrimSpace(scope.RunID)
		workUnitID := strings.TrimSpace(scope.WorkUnitID)
		if runID == "" || workUnitID == "" {
			// Without work-unit identity there is nothing to attribute to, and
			// the work unit is the granularity outcome and evidence already use.
			return
		}
		tenantID := skillStorageTenantID(args)
		index := skillPackageIndexForWorkUnit(tenantID, runID, workUnitID, args)
		if index == nil || len(index.entries) == 0 {
			return
		}
		matched := index.match(toolName, args)
		if len(matched) == 0 {
			return
		}
		ctx := ContextFromArgs(args)
		activated, err := store.WorkUnitActivatedSkillKeys(ctx, scope.ControlTenantID, runID, workUnitID)
		if err != nil {
			log.Debug("skill attribution: activation lookup failed", "error", err)
			return
		}
		for _, entry := range matched {
			if activated[entry.SkillKey] {
				// Reading an activated Skill's own resources is that
				// activation's progressive disclosure and is already recorded in
				// its resource manifest.
				continue
			}
			if _, err := store.RecordSkillAttribution(ctx, control.SkillAttribution{
				ControlTenantID: scope.ControlTenantID,
				PersonID:        scope.PersonID,
				RunID:           runID,
				WorkUnitID:      workUnitID,
				SkillKey:        entry.SkillKey,
				SkillName:       entry.Name,
				PackagePath:     entry.Path,
				PackageName:     entry.PackageName,
				Scope:           entry.Scope,
				Provenance:      entry.Provenance,
				ToolName:        toolName,
			}); err != nil {
				log.Debug("skill attribution: record failed", "skill", entry.Name, "error", err)
			}
		}
	}
}

func skillPackageIndexForWorkUnit(tenantID, runID, workUnitID string, args map[string]interface{}) *skillPackageIndex {
	key := tenantID + "|" + runID + "|" + workUnitID
	skillAttributionMu.Lock()
	if cached, ok := skillAttributionCache[key]; ok {
		skillAttributionMu.Unlock()
		return cached
	}
	skillAttributionMu.Unlock()

	skills, err := ListSkillsForTenant(tenantID, false, args)
	if err != nil {
		log.Debug("skill attribution: discovery failed", "error", err)
		return nil
	}
	index := &skillPackageIndex{byName: make(map[string]skillPackageEntry, len(skills))}
	for _, info := range skills {
		skillKey, keyErr := resolvedSkillKey(tenantID, info)
		if keyErr != nil {
			continue
		}
		entry := skillPackageEntry{
			Path: info.Path, SkillKey: skillKey, Name: info.Name,
			PackageName: info.PackageName, Scope: info.Scope, Provenance: info.Provenance,
		}
		index.entries = append(index.entries, entry)
		normalized := normalizeSkillCommandName(info.Name)
		if _, exists := index.byName[normalized]; !exists {
			index.byName[normalized] = entry
		}
	}

	skillAttributionMu.Lock()
	defer skillAttributionMu.Unlock()
	if len(skillAttributionOrder) >= skillAttributionCacheCap {
		evict := skillAttributionOrder[0]
		skillAttributionOrder = skillAttributionOrder[1:]
		delete(skillAttributionCache, evict)
	}
	skillAttributionCache[key] = index
	skillAttributionOrder = append(skillAttributionOrder, key)
	return index
}

// ResetSkillAttributionIndexCache clears the per-work-unit package index. Tests
// use it so one case cannot observe another's inventory.
func ResetSkillAttributionIndexCache() {
	skillAttributionMu.Lock()
	defer skillAttributionMu.Unlock()
	skillAttributionCache = map[string]*skillPackageIndex{}
	skillAttributionOrder = nil
}

// match returns the packages one completed call used. A Skill-name argument is
// resolved through the same index rather than re-resolved, and a path argument
// matches the package whose directory contains it.
func (idx *skillPackageIndex) match(toolName string, args map[string]interface{}) []skillPackageEntry {
	var out []skillPackageEntry
	seen := map[string]bool{}
	add := func(entry skillPackageEntry) {
		if entry.Path == "" || seen[entry.Path] {
			return
		}
		seen[entry.Path] = true
		out = append(out, entry)
	}
	if toolName == "skill_view" {
		if name, ok := args["name"].(string); ok {
			if entry, found := idx.byName[normalizeSkillCommandName(name)]; found {
				add(entry)
			}
		}
	}
	root := ""
	if scope, ok := currentExecutionScopeAny(args); ok {
		root = strings.TrimSpace(scope.WorkspaceRoot)
	}
	for key, value := range args {
		if !skillAttributionPathArgKeys[key] {
			continue
		}
		for _, candidate := range skillAttributionArgPaths(value) {
			resolved := candidate
			if !filepath.IsAbs(resolved) && root != "" {
				resolved = filepath.Join(root, resolved)
			}
			resolved = skillPathKey(resolved)
			for _, entry := range idx.entries {
				packageKey := skillPathKey(entry.Path)
				if resolved == packageKey || strings.HasPrefix(resolved, packageKey+string(filepath.Separator)) {
					add(entry)
				}
			}
		}
	}
	return out
}

func skillAttributionArgPaths(value interface{}) []string {
	switch typed := value.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return []string{typed}
	case []string:
		return typed
	case []interface{}:
		var out []string
		for _, item := range typed {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				out = append(out, text)
			}
		}
		return out
	}
	return nil
}
