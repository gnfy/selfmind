package tools

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
)

// SkillLifecycleManageTool is intentionally hidden from model tool discovery.
// It backs explicit management commands for candidate versions and durable
// task affinity; ordinary agent turns use skill_select/skill_fallback instead.
type SkillLifecycleManageTool struct {
	BaseTool
	store *control.Store
}

func NewSkillLifecycleManageTool(store *control.Store) *SkillLifecycleManageTool {
	return &SkillLifecycleManageTool{
		BaseTool: BaseTool{
			name:        "skill_lifecycle_manage",
			description: "Explicitly inspect/promote/reject/rollback Skill versions or bind/unbind the current task.",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"action": {
						Type: "string", Description: "Explicit lifecycle action.",
						Enum: []string{"candidate_create", "candidate_list", "candidate_read", "candidate_promote", "candidate_reject", "rollback", "binding_get", "binding_bind", "binding_unbind"},
					},
					"name":                {Type: "string", Description: "Skill name for binding or optional candidate filtering."},
					"skill_key":           {Type: "string", Description: "Logical Skill key when disambiguation is required."},
					"version_hash":        {Type: "string", Description: "Candidate or previous version hash."},
					"parent_version_hash": {Type: "string", Description: "Exact active parent for a PATCH candidate."},
					"content":             {Type: "string", Description: "Complete immutable candidate body."},
					"resources_json":      {Type: "string", Description: "Optional JSON object of immutable linked resource path to content."},
					"evidence_set_hash":   {Type: "string", Description: "Frozen evidence-set identity."},
					"observation_ids":     {Type: "array", Description: "Source observation ids.", Items: &PropertyDef{Type: "string"}},
					"evidence_json":       {Type: "string", Description: "Frozen bounded evidence digest JSON."},
				},
				Required: []string{"action"},
			},
			metadata: ToolMetadata{Exposure: ToolExposureHidden, RiskLevel: ToolRiskHigh, Category: "skill"},
		},
		store: store,
	}
}

func (t *SkillLifecycleManageTool) Execute(args map[string]interface{}) (string, error) {
	if t == nil || t.store == nil {
		return "", fmt.Errorf("skill lifecycle store is unavailable")
	}
	action := strings.ToLower(strings.TrimSpace(taskStringArg(args, "action")))
	tenantID := skillStorageTenantID(args)
	switch action {
	case "candidate_create":
		if err := authorizeSkillMutation(args, action); err != nil {
			return "", err
		}
		return t.createCandidate(args, tenantID)
	case "candidate_list":
		return t.listCandidates(args, tenantID)
	case "candidate_read":
		return t.readVersion(args, tenantID, "candidate")
	case "candidate_promote":
		if err := authorizeSkillMutation(args, action); err != nil {
			return "", err
		}
		return t.promoteCandidate(args, tenantID)
	case "candidate_reject":
		if err := authorizeSkillMutation(args, action); err != nil {
			return "", err
		}
		version, err := t.resolveVersion(args, tenantID, "candidate")
		if err != nil {
			return "", err
		}
		if err := t.store.RejectSkillCandidate(ContextFromArgs(args), tenantID, version.SkillKey, version.VersionHash); err != nil {
			return "", err
		}
		return fmt.Sprintf("Rejected Skill candidate %s@%s. The active Skill was not changed.", version.SkillName, version.VersionHash), nil
	case "rollback":
		if err := authorizeSkillMutation(args, action); err != nil {
			return "", err
		}
		return t.rollback(args, tenantID)
	case "binding_get":
		return t.getBinding(args, tenantID)
	case "binding_bind":
		return t.bindTask(args, tenantID)
	case "binding_unbind":
		return t.unbindTask(args, tenantID)
	default:
		return "", fmt.Errorf("unknown skill lifecycle action %q", action)
	}
}

func (t *SkillLifecycleManageTool) createCandidate(args map[string]interface{}, tenantID string) (string, error) {
	skillKey := strings.TrimSpace(taskStringArg(args, "skill_key"))
	name := kernel.SanitizeSkillName(taskStringArg(args, "name"))
	content := strings.TrimSpace(taskStringArg(args, "content"))
	evidenceSetHash := strings.TrimSpace(taskStringArg(args, "evidence_set_hash"))
	if skillKey == "" || name == "" || content == "" || evidenceSetHash == "" {
		return "", fmt.Errorf("candidate_create requires skill_key, name, content, and evidence_set_hash")
	}
	var observationIDs []string
	switch raw := args["observation_ids"].(type) {
	case []string:
		observationIDs = append(observationIDs, raw...)
	case []interface{}:
		for _, value := range raw {
			if id, ok := value.(string); ok && strings.TrimSpace(id) != "" {
				observationIDs = append(observationIDs, strings.TrimSpace(id))
			}
		}
	}
	evidenceJSON := strings.TrimSpace(taskStringArg(args, "evidence_json"))
	if !json.Valid([]byte(evidenceJSON)) {
		return "", fmt.Errorf("candidate_create requires valid evidence_json")
	}
	resources := map[string]string{}
	if raw := strings.TrimSpace(taskStringArg(args, "resources_json")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &resources); err != nil {
			return "", fmt.Errorf("candidate_create resources_json must be a JSON object: %w", err)
		}
	}
	for path := range resources {
		if _, err := safeSupportPath("skill-package", path); err != nil {
			return "", err
		}
	}
	versionHash, packageHash, manifest := BuildSkillPackageIdentity(content, resources)
	manifestJSON, _ := json.Marshal(manifest)
	packageResources := make([]control.SkillPackageResource, 0, len(manifest))
	for _, entry := range manifest {
		packageResources = append(packageResources, control.SkillPackageResource{
			Path: entry.Path, ContentHash: entry.ContentHash, ContentBody: resources[entry.Path], Bytes: entry.Bytes,
		})
	}
	if err := t.store.RecordSkillPackageResources(ContextFromArgs(args), tenantID, skillKey, packageHash, packageResources); err != nil {
		return "", err
	}
	createdVersionHash, err := t.store.CreateSkillPackageCandidateVersion(ContextFromArgs(args), tenantID, skillKey, name,
		taskStringArg(args, "parent_version_hash"), content, packageHash, manifestJSON,
		evidenceSetHash, observationIDs, json.RawMessage(evidenceJSON))
	if err != nil {
		return "", err
	}
	if createdVersionHash != versionHash {
		return "", fmt.Errorf("candidate version identity mismatch")
	}
	data, _ := json.Marshal(map[string]string{"skill_key": skillKey, "name": name, "version_hash": versionHash, "package_hash": packageHash, "state": "candidate"})
	return string(data), nil
}

func (t *SkillLifecycleManageTool) listCandidates(args map[string]interface{}, tenantID string) (string, error) {
	versions, err := t.store.ListSkillVersions(ContextFromArgs(args), tenantID, taskStringArg(args, "skill_key"), "candidate", 50)
	if err != nil {
		return "", err
	}
	name := kernel.SanitizeSkillName(taskStringArg(args, "name"))
	var lines []string
	for _, version := range versions {
		if name != "" && kernel.SanitizeSkillName(version.SkillName) != name {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s@%s (parent: %s, evidence: %s, created: %s)",
			version.SkillName, version.VersionHash, emptyDefault(version.ParentVersionHash, "new"),
			emptyDefault(version.EvidenceSetHash, "manual"), version.CreatedAt.Format("2006-01-02 15:04:05")))
	}
	if len(lines) == 0 {
		return "No pending Skill candidates.", nil
	}
	sort.Strings(lines)
	return fmt.Sprintf("## Skill candidates (%d)\n\n%s", len(lines), strings.Join(lines, "\n")), nil
}

func (t *SkillLifecycleManageTool) readVersion(args map[string]interface{}, tenantID, state string) (string, error) {
	version, err := t.resolveVersion(args, tenantID, state)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("# %s@%s\n\nState: %s\nParent: %s\nEvidence: %s\n\n%s", version.SkillName,
		version.VersionHash, version.State, emptyDefault(version.ParentVersionHash, "none"),
		emptyDefault(version.EvidenceSetHash, "none"), version.ContentBody), nil
}

func (t *SkillLifecycleManageTool) resolveVersion(args map[string]interface{}, tenantID, requiredState string) (*control.SkillVersion, error) {
	versionHash := strings.TrimSpace(taskStringArg(args, "version_hash"))
	if versionHash == "" {
		return nil, fmt.Errorf("version_hash is required")
	}
	skillKey := strings.TrimSpace(taskStringArg(args, "skill_key"))
	if skillKey != "" {
		version, err := t.store.GetSkillVersion(ContextFromArgs(args), tenantID, skillKey, versionHash)
		if err != nil {
			return nil, err
		}
		if version == nil || (requiredState != "" && version.State != requiredState) {
			return nil, fmt.Errorf("%s Skill version not found", requiredState)
		}
		return version, nil
	}
	versions, err := t.store.ListSkillVersions(ContextFromArgs(args), tenantID, "", requiredState, 100)
	if err != nil {
		return nil, err
	}
	var match *control.SkillVersion
	for i := range versions {
		if versions[i].VersionHash != versionHash {
			continue
		}
		if match != nil && match.SkillKey != versions[i].SkillKey {
			return nil, fmt.Errorf("version hash is ambiguous; provide skill_key")
		}
		copy := versions[i]
		match = &copy
	}
	if match == nil {
		return nil, fmt.Errorf("%s Skill version not found", requiredState)
	}
	return match, nil
}

func (t *SkillLifecycleManageTool) promoteCandidate(args map[string]interface{}, tenantID string) (string, error) {
	version, err := t.resolveVersion(args, tenantID, "candidate")
	if err != nil {
		return "", err
	}
	if version.CreatedBy != "skill_curator" {
		return "", fmt.Errorf("only curator-created candidates can be promoted through this surface")
	}
	active, err := t.store.ActiveSkillVersion(ContextFromArgs(args), tenantID, version.SkillKey)
	if err != nil {
		return "", err
	}
	expectedCurrentHash := ""
	if version.ParentVersionHash == "" {
		if active != nil {
			return "", fmt.Errorf("parentless Skill candidate cannot replace active version %s", active.VersionHash)
		}
	} else {
		if active == nil || active.VersionHash != version.ParentVersionHash {
			return "", fmt.Errorf("skill candidate parent %s is not the current active version", version.ParentVersionHash)
		}
		expectedCurrentHash = active.VersionHash
	}
	path, err := writeLifecycleVersionFile(t.store, tenantID, version, expectedCurrentHash, args)
	if err != nil {
		return "", err
	}
	if err := t.store.PromoteSkillCandidate(ContextFromArgs(args), tenantID, version.SkillKey, version.VersionHash, path); err != nil {
		return "", err
	}
	reloadSkillToolsFromArgs(tenantID, args)
	return fmt.Sprintf("Promoted Skill candidate %s@%s. Existing activations keep their pinned version; future activations use this one.", version.SkillName, version.VersionHash), nil
}

func (t *SkillLifecycleManageTool) rollback(args map[string]interface{}, tenantID string) (string, error) {
	version, err := t.resolveVersion(args, tenantID, "previous")
	if err != nil {
		return "", err
	}
	active, err := t.store.ActiveSkillVersion(ContextFromArgs(args), tenantID, version.SkillKey)
	if err != nil {
		return "", err
	}
	if active == nil {
		return "", fmt.Errorf("active Skill version is unavailable for rollback")
	}
	path, err := writeLifecycleVersionFile(t.store, tenantID, version, active.VersionHash, args)
	if err != nil {
		return "", err
	}
	if err := t.store.ActivatePreviousSkillVersion(ContextFromArgs(args), tenantID, version.SkillKey, version.VersionHash, path); err != nil {
		return "", err
	}
	reloadSkillToolsFromArgs(tenantID, args)
	return fmt.Sprintf("Rolled back Skill %s to %s. Existing activations remain unchanged.", version.SkillName, version.VersionHash), nil
}

func writeLifecycleVersionFile(store *control.Store, tenantID string, version *control.SkillVersion, expectedCurrentHash string, args map[string]interface{}) (string, error) {
	if version == nil || strings.TrimSpace(version.ContentBody) == "" {
		return "", fmt.Errorf("version content is unavailable")
	}
	info, current, _, findErr := ReadSkillPayloadForTenant(tenantID, version.SkillName, "", args)
	if findErr == nil {
		key, err := resolvedSkillKey(tenantID, info)
		if err != nil {
			return "", err
		}
		if key != version.SkillKey {
			return "", fmt.Errorf("resolved Skill identity changed; refusing to overwrite %s", info.Path)
		}
		if info.Source != SkillSourceAgentCreated || info.Pinned || !info.Writable {
			return "", fmt.Errorf("automatic lifecycle writes require a writable, unpinned, agent-created Skill")
		}
		if version.PackageHash != "" {
			currentPackage, err := ReadSkillPackageForTenant(tenantID, version.SkillName, args)
			if err != nil {
				return "", err
			}
			desiredResources, err := versionPackageResources(store, args, version)
			if err != nil {
				return "", err
			}
			if !sameSkillPackageResources(currentPackage.ResourceManifest, desiredResources) {
				return "", fmt.Errorf("automatic PATCH cannot change linked resources; create a reviewed package update instead")
			}
		}
		currentHash := sha256.Sum256([]byte(current))
		currentVersionHash := fmt.Sprintf("%x", currentHash[:])
		if expectedCurrentHash == "" && currentVersionHash != version.VersionHash {
			return "", fmt.Errorf("active Skill exists for a parentless candidate; refusing to overwrite it")
		}
		if expectedCurrentHash != "" && currentVersionHash != expectedCurrentHash && currentVersionHash != version.VersionHash {
			return "", fmt.Errorf("active Skill changed after candidate creation; refusing to overwrite newer content")
		}
		if currentVersionHash != version.VersionHash {
			if _, err := editSkill(tenantID, info.Name, version.ContentBody, "", args); err != nil {
				return "", err
			}
		}
		return verifyLifecycleVersionFile(tenantID, version, args)
	}
	if version.ParentVersionHash != "" {
		return "", fmt.Errorf("active Skill for PATCH/rollback is unavailable: %w", findErr)
	}
	writeRoot, err := ResolveWritableSkillRootForTenant(tenantID, args)
	if err != nil {
		return "", err
	}
	root := writeRoot.Path
	safeName := kernel.SanitizeSkillName(version.SkillName)
	expectedKey := control.SkillKey(tenantID, safeName, writeRoot.Scope, SkillSourceAgentCreated, root, safeName+"/SKILL.md")
	if expectedKey != version.SkillKey {
		return "", fmt.Errorf("candidate identity does not match the selected managed Skill root")
	}
	content := ensureFrontMatter(version.ContentBody, safeName, "")
	if err := validateSkillEnvironmentDeclarations(content); err != nil {
		return "", err
	}
	if err := kernel.ScanSkillForDangers(content); err != nil {
		return "", fmt.Errorf("security scan failed: %w", err)
	}
	dir := filepath.Join(root, safeName)
	if _, err := os.Stat(dir); err == nil {
		return "", fmt.Errorf("skill %q already exists but could not be resolved safely", safeName)
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		return "", err
	}
	resources, err := versionPackageResources(store, args, version)
	if err != nil {
		return "", err
	}
	tmpDir, err := os.MkdirTemp(root, ".skill-package-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)
	path := filepath.Join(tmpDir, "SKILL.md")
	if err := atomicWriteFile(path, content); err != nil {
		return "", err
	}
	for _, resource := range resources {
		target, err := safeSupportPath(tmpDir, resource.Path)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return "", err
		}
		if err := atomicWriteFile(target, resource.ContentBody); err != nil {
			return "", err
		}
	}
	if err := os.Rename(tmpDir, dir); err != nil {
		return "", err
	}
	path = filepath.Join(dir, "SKILL.md")
	_ = MarkSkillCreated(tenantID, safeName, SkillSourceAgentCreated, "skill_lifecycle_manage", args)
	recordSkillLearningChange(tenantID, safeName, "promote", "", content, SkillSourceAgentCreated, args)
	return verifyLifecycleVersionFile(tenantID, version, args)
}

func verifyLifecycleVersionFile(tenantID string, version *control.SkillVersion, args map[string]interface{}) (string, error) {
	pack, err := ReadSkillPackageForTenant(tenantID, version.SkillName, args)
	if err != nil {
		return "", err
	}
	info, content := pack.Info, pack.MainSource
	key, err := resolvedSkillKey(tenantID, info)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(content))
	if key != version.SkillKey || fmt.Sprintf("%x", digest[:]) != version.VersionHash || (version.PackageHash != "" && pack.PackageHash != version.PackageHash) {
		return "", fmt.Errorf("written Skill does not match the candidate identity and content hash")
	}
	return skillMainFilePath(info), nil
}

func versionPackageResources(store *control.Store, args map[string]interface{}, version *control.SkillVersion) ([]control.SkillPackageResource, error) {
	if version == nil || version.PackageHash == "" {
		return nil, nil
	}
	if store == nil {
		return nil, fmt.Errorf("control store is unavailable for Skill package resources")
	}
	return store.ListSkillPackageResources(ContextFromArgs(args), version.ControlTenantID, version.SkillKey, version.PackageHash)
}

func sameSkillPackageResources(current []SkillResourceManifestEntry, desired []control.SkillPackageResource) bool {
	if len(current) != len(desired) {
		return false
	}
	for index := range current {
		if current[index].Path != desired[index].Path || current[index].ContentHash != desired[index].ContentHash || current[index].Bytes != desired[index].Bytes {
			return false
		}
	}
	return true
}

func resolvedSkillKey(tenantID string, info SkillInfo) (string, error) {
	rel, err := filepath.Rel(info.Root, skillMainFilePath(info))
	if err != nil {
		return "", err
	}
	return control.SkillKey(tenantID, info.Name, info.Scope, info.Source, info.Root, rel), nil
}

func (t *SkillLifecycleManageTool) getBinding(args map[string]interface{}, tenantID string) (string, error) {
	scope, ok := InvocationScopeFromArgs(args)
	if !ok || scope.PersonID == "" || scope.TaskID == "" {
		return "", fmt.Errorf("binding management requires an authenticated current task")
	}
	binding, err := t.store.GetTaskSkillBinding(ContextFromArgs(args), tenantID, scope.PersonID, scope.TaskID)
	if err != nil {
		return "", err
	}
	if binding == nil || binding.State == control.TaskSkillBindingReleased {
		return "The current task has no default Skill binding.", nil
	}
	data, _ := json.MarshalIndent(binding, "", "  ")
	return string(data), nil
}

func (t *SkillLifecycleManageTool) bindTask(args map[string]interface{}, tenantID string) (string, error) {
	if err := authorizeSkillMutation(args, "binding_bind"); err != nil {
		return "", err
	}
	scope, ok := InvocationScopeFromArgs(args)
	if !ok || scope.PersonID == "" || scope.TaskID == "" {
		return "", fmt.Errorf("binding management requires an authenticated current task")
	}
	name := taskStringArg(args, "name")
	info, content, _, err := ReadSkillPayloadForTenant(tenantID, name, "", args)
	if err != nil {
		return "", err
	}
	if info.State != SkillStateActive {
		return "", fmt.Errorf("skill %q is not active", info.Name)
	}
	key, err := resolvedSkillKey(tenantID, info)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(content))
	_, err = t.store.BindTaskSkill(ContextFromArgs(args), control.BindTaskSkillInput{
		IdentityTenantID: tenantID, PersonID: scope.PersonID, TaskID: scope.TaskID,
		ControlTenantID: tenantID, WorkspaceID: scope.WorkspaceID, SkillKey: key,
		SkillName: info.Name, BindingSource: "explicit", VersionHash: fmt.Sprintf("%x", digest[:]),
	})
	if err != nil {
		return "", err
	}
	t.recordBindingEvent(args, scope.TaskID, "skill.binding.changed", map[string]string{"state": "active", "skill_key": key, "name": info.Name})
	return fmt.Sprintf("Bound current task %s to Skill %s. Deterministic continuations will load only this Skill.", scope.TaskID, info.Name), nil
}

func (t *SkillLifecycleManageTool) unbindTask(args map[string]interface{}, tenantID string) (string, error) {
	if err := authorizeSkillMutation(args, "binding_unbind"); err != nil {
		return "", err
	}
	scope, ok := InvocationScopeFromArgs(args)
	if !ok || scope.PersonID == "" || scope.TaskID == "" {
		return "", fmt.Errorf("binding management requires an authenticated current task")
	}
	if err := t.store.SetTaskSkillBindingState(ContextFromArgs(args), tenantID, scope.PersonID, scope.TaskID, control.TaskSkillBindingReleased, "released by user"); err != nil {
		return "", err
	}
	t.recordBindingEvent(args, scope.TaskID, "skill.binding.changed", map[string]string{"state": "released"})
	return fmt.Sprintf("Released the default Skill binding for current task %s.", scope.TaskID), nil
}

func (t *SkillLifecycleManageTool) recordBindingEvent(args map[string]interface{}, taskID, typ string, payload interface{}) {
	raw, _ := json.Marshal(payload)
	_, _ = t.store.AppendEvent(ContextFromArgs(args), control.Event{TaskID: taskID, Type: typ, Visibility: "task", Payload: raw})
}
