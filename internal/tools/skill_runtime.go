package tools

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/platform/textutil"
)

// RankSkillCandidatesForTenant returns metadata-only candidates for one work
// unit. Ranking is deterministic and bounded; it never loads full bodies into
// the prompt and never calls a model.
func RankSkillCandidatesForTenant(tenantID, query string, limit int, invocation ...map[string]interface{}) ([]SkillInfo, error) {
	if limit <= 0 || limit > 3 {
		limit = 3
	}
	skills, err := ListSkillsForTenant(tenantID, false, invocation...)
	if err != nil {
		return nil, err
	}
	active := make([]SkillInfo, 0, len(skills))
	for _, info := range skills {
		if info.State == SkillStateActive {
			active = append(active, info)
		}
	}
	return rankSkillsBM25F(query, active, limit), nil
}

// CatalogSkillCandidatesForTenant returns every active Skill in deterministic
// presentation order. Query matches are ranked first; non-matches remain in
// the catalogue so hiding the legacy per-Skill native tools does not erase the
// existence of unrelated Skills from the model's discovery surface. Full
// bodies are never loaded here; the prompt renderer independently budgets the
// compact metadata.
func CatalogSkillCandidatesForTenant(tenantID, query string, invocation ...map[string]interface{}) ([]SkillInfo, error) {
	skills, err := ListSkillsForTenant(tenantID, false, invocation...)
	if err != nil {
		return nil, err
	}
	active := make([]SkillInfo, 0, len(skills))
	for _, info := range skills {
		if info.State == SkillStateActive {
			active = append(active, info)
		}
	}
	ranked := rankSkillsBM25F(query, active, len(active))
	seen := make(map[string]bool, len(ranked))
	out := make([]SkillInfo, 0, len(active))
	for _, info := range ranked {
		key := normalizeSkillCommandName(info.Name)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, info)
	}
	for _, info := range active {
		key := normalizeSkillCommandName(info.Name)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, info)
	}
	return out, nil
}

type skillListEntry struct {
	CandidateRef string   `json:"candidate_ref,omitempty"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	State        string   `json:"state"`
	Source       string   `json:"source"`
	Scope        string   `json:"scope"`
	Writable     bool     `json:"writable"`
	Pinned       bool     `json:"pinned"`
	LastUsed     string   `json:"last_used,omitempty"`
	Path         string   `json:"path"`
	Root         string   `json:"root,omitempty"`
	Format       string   `json:"format"`
	Files        []string `json:"linked_files,omitempty"`
}

type SkillDescriptionDiagnostic struct {
	Name     string
	Path     string
	Scope    string
	Source   string
	Writable bool
	Chars    int
	Bytes    int
}

// InspectSkillDescriptionsForTenant is a read-only authoring-contract check.
// Existing external/read-only assets remain usable, but Doctor can identify the
// exact owning file whose metadata will be presentation-capped.
func InspectSkillDescriptionsForTenant(tenantID string, invocation ...map[string]interface{}) ([]SkillDescriptionDiagnostic, error) {
	skills, err := ListSkillsForTenant(tenantID, true, invocation...)
	if err != nil {
		return nil, err
	}
	var issues []SkillDescriptionDiagnostic
	for _, info := range skills {
		description := strings.TrimSpace(info.Description)
		chars := utf8.RuneCountInString(description)
		bytes := len(description)
		if chars <= SkillDescriptionMaxChars && bytes <= SkillDescriptionMaxBytes {
			continue
		}
		issues = append(issues, SkillDescriptionDiagnostic{
			Name: info.Name, Path: skillMainFilePath(info), Scope: info.Scope, Source: info.Source,
			Writable: info.Writable, Chars: chars, Bytes: bytes,
		})
	}
	return issues, nil
}

const maxSkillViewPageBytes = 8 * 1024

type SkillsListTool struct {
	BaseTool
	store *control.Store
}

func NewSkillsListTool(stores ...*control.Store) *SkillsListTool {
	tool := &SkillsListTool{
		BaseTool: BaseTool{
			name:        "skills_list",
			description: "List available skills as compact metadata. Use skill_view to load full instructions or linked files.",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"query": {
						Type:        "string",
						Description: "Optional relevance query over skill name and description metadata.",
					},
					"include_archived": {
						Type:        "boolean",
						Description: "Include archived skills in the listing.",
						Default:     false,
					},
				},
				Required: []string{},
			},
		},
	}
	if len(stores) > 0 {
		tool.store = stores[0]
	}
	return tool
}

func (t *SkillsListTool) Execute(args map[string]interface{}) (string, error) {
	tenantID := skillStorageTenantID(args)
	query, _ := args["query"].(string)
	includeArchived, _ := args["include_archived"].(bool)
	return skillsListJSONForTenant(t.store, tenantID, query, includeArchived, args)
}

type SkillViewTool struct {
	BaseTool
	store *control.Store
}

// SkillInvocationResolveTool is a hidden thin-client bridge for `/skill-name`.
// Resolution stays inside the daemon so custom evolution.skills_dir roots and
// workspace scope cannot diverge from normal agent turns.
type SkillInvocationResolveTool struct {
	BaseTool
}

func NewSkillInvocationResolveTool() *SkillInvocationResolveTool {
	return &SkillInvocationResolveTool{
		BaseTool: BaseTool{
			name:        "skill_invocation_resolve",
			description: "Resolve an explicit slash Skill or bundle invocation for a thin client.",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"command":     {Type: "string", Description: "Slash command name, with or without the leading slash."},
					"instruction": {Type: "string", Description: "Optional user instruction appended after the loaded Skill context."},
				},
				Required: []string{"command"},
			},
			metadata: ToolMetadata{
				Exposure: ToolExposureHidden, ReadOnly: true, RiskLevel: ToolRiskLow, Category: "skill",
			},
		},
	}
}

func (t *SkillInvocationResolveTool) Execute(args map[string]interface{}) (string, error) {
	tenantID := skillStorageTenantID(args)
	command, _ := args["command"].(string)
	instruction, _ := args["instruction"].(string)
	resolved, prompt, displayName, kind, found, err := ResolveTypedSkillInvocationForTenant(tenantID, command, instruction, args)
	if err != nil {
		return "", err
	}
	data, _ := json.Marshal(map[string]interface{}{
		"found": found, "kind": kind, "display_name": displayName, "prompt": prompt,
		"name": resolved.Name, "skill_key": resolved.SkillKey,
		"version_hash": resolved.VersionHash, "package_hash": resolved.PackageHash,
	})
	return string(data), nil
}

func NewSkillViewTool(stores ...*control.Store) *SkillViewTool {
	tool := &SkillViewTool{
		BaseTool: BaseTool{
			name:        "skill_view",
			description: "Inspect a skill's SKILL.md or one linked file without activating or using it. Never substitute this for execution attribution; call skill_select when applying a Skill to the current work unit.",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"name": {
						Type:        "string",
						Description: "Skill name.",
					},
					"file_path": {
						Type:        "string",
						Description: "Optional linked file path under references/, templates/, scripts/, or assets/.",
					},
					"section": {
						Type:        "string",
						Description: "Optional level-two heading from SKILL.md. Returns that exact section page, including its heading.",
					},
					"offset_bytes": {
						Type:        "integer",
						Description: "Optional UTF-8 byte offset within the selected main section or linked file.",
						Default:     0,
					},
					"limit_bytes": {
						Type:        "integer",
						Description: "Page size in bytes, capped at 8192.",
						Default:     maxSkillViewPageBytes,
					},
				},
				Required: []string{"name"},
			},
		},
	}
	if len(stores) > 0 {
		tool.store = stores[0]
	}
	return tool
}

func (t *SkillViewTool) Execute(args map[string]interface{}) (string, error) {
	tenantID := skillStorageTenantID(args)
	name, _ := args["name"].(string)
	filePath, _ := args["file_path"].(string)
	section, _ := args["section"].(string)
	if t.store != nil {
		if scope, ok := InvocationScopeFromArgs(args); ok && scope.RunID != "" && scope.PersonID != "" {
			workUnitID := strings.TrimSpace(scope.WorkUnitID)
			if workUnitID == "" {
				if unit, err := t.store.CurrentRunWorkUnit(ContextFromArgs(args), scope.ControlTenantID, scope.RunID); err != nil {
					return "", err
				} else if unit != nil {
					workUnitID = unit.ID
				}
			}
			if workUnitID != "" {
				activation, err := t.store.ActiveSkillActivation(ContextFromArgs(args), scope.ControlTenantID, scope.RunID, workUnitID, scope.ExecutionLane)
				if err != nil {
					return "", err
				}
				if activation != nil && kernel.SanitizeSkillName(name) == kernel.SanitizeSkillName(activation.SkillName) && activation.DeliveryContractVersion > 0 {
					return skillViewActivationPageJSON(t.store, activation, filePath, section, integerToolArg(args, "offset_bytes"), integerToolArg(args, "limit_bytes"), args)
				}
			}
		}
	}
	return SkillViewPageJSONForTenant(tenantID, name, filePath, section, integerToolArg(args, "offset_bytes"), integerToolArg(args, "limit_bytes"), args)
}

func skillViewActivationPageJSON(store *control.Store, activation *control.SkillActivation, filePath, section string, offsetBytes, limitBytes int, args map[string]interface{}) (string, error) {
	if store == nil || activation == nil {
		return "", fmt.Errorf("active Skill package is unavailable")
	}
	if strings.TrimSpace(filePath) != "" && strings.TrimSpace(section) != "" {
		return "", fmt.Errorf("section applies only to SKILL.md; omit file_path")
	}
	var manifest []SkillResourceManifestEntry
	if !json.Valid([]byte(activation.ResourceManifestJSON)) || json.Unmarshal([]byte(activation.ResourceManifestJSON), &manifest) != nil {
		return "", fmt.Errorf("active Skill resource manifest is invalid")
	}
	selected := ""
	if strings.TrimSpace(filePath) != "" {
		resource, err := store.SkillPackageResource(ContextFromArgs(args), activation.ControlTenantID, activation.SkillKey, activation.PackageHash, filepath.ToSlash(filePath))
		if err != nil {
			return "", err
		}
		if resource == nil {
			return "", fmt.Errorf("linked resource %q is not part of active Skill package %s", filepath.ToSlash(filePath), activation.PackageHash)
		}
		selected = resource.ContentBody
	} else {
		version, err := store.GetSkillVersion(ContextFromArgs(args), activation.ControlTenantID, activation.SkillKey, activation.VersionHash)
		if err != nil {
			return "", err
		}
		if version == nil || version.ContentBody == "" {
			return "", fmt.Errorf("active Skill main source is unavailable")
		}
		var ok bool
		selected, ok = kernel.SkillSectionPage(version.ContentBody, section)
		if !ok {
			return "", fmt.Errorf("skill %q has no level-two section %q", activation.SkillName, strings.TrimSpace(section))
		}
	}
	page, nextOffset, err := skillViewPage(selected, offsetBytes, limitBytes)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(selected))
	out := map[string]interface{}{
		"success": true, "name": activation.SkillName, "activation_id": activation.ID,
		"version_hash": activation.VersionHash, "package_hash": activation.PackageHash,
		"delivery_contract_version": activation.DeliveryContractVersion,
		"content":                   page, "content_hash": fmt.Sprintf("%x", digest[:]),
		"offset_bytes": offsetBytes, "total_bytes": len(selected), "complete": nextOffset == 0,
	}
	if nextOffset > 0 {
		out["next_offset_bytes"] = nextOffset
		out["hint"] = "Continue the same page with offset_bytes=next_offset_bytes."
	}
	if strings.TrimSpace(filePath) != "" {
		out["file"] = filepath.ToSlash(filePath)
	} else if strings.TrimSpace(section) != "" {
		out["section"] = strings.TrimSpace(section)
	} else if len(manifest) > 0 {
		files := make([]string, 0, len(manifest))
		for _, resource := range manifest {
			files = append(files, resource.Path)
		}
		out["linked_files"] = files
	}
	kernel.EmitAgentEvent(kernel.EventChannelFromContext(ContextFromArgs(args)), kernel.AgentEvent{
		Type: "skill.viewed",
		Payload: map[string]interface{}{
			"activation_id": activation.ID, "name": activation.SkillName,
			"version_hash": activation.VersionHash, "package_hash": activation.PackageHash,
			"file_path": filepath.ToSlash(filePath), "section": strings.TrimSpace(section),
			"view_source": "active_package_snapshot",
		},
	})
	data, _ := json.MarshalIndent(out, "", "  ")
	return string(data), nil
}

func SkillsListJSONForTenant(tenantID, query string, includeArchived bool, invocation ...map[string]interface{}) (string, error) {
	return skillsListJSONForTenant(nil, tenantID, query, includeArchived, invocation...)
}

func skillsListJSONForTenant(store *control.Store, tenantID, query string, includeArchived bool, invocation ...map[string]interface{}) (string, error) {
	var skills []SkillInfo
	totalMatches := 0
	truncated := false
	var err error
	if strings.TrimSpace(query) != "" {
		skills, totalMatches, err = searchSkillsForTenantDetailed(tenantID, query, invocation...)
		truncated = totalMatches > len(skills)
	} else {
		skills, err = ListSkillsForTenant(tenantID, includeArchived, invocation...)
		totalMatches = len(skills)
	}
	if err != nil {
		return "", err
	}

	var scope kernel.ToolInvocationScope
	var currentUnit *control.RunWorkUnit
	if store != nil && len(invocation) > 0 && invocation[0] != nil {
		if resolved, ok := InvocationScopeFromArgs(invocation[0]); ok && resolved.PersonID != "" && resolved.RunID != "" {
			scope = resolved
			unit, unitErr := store.CurrentRunWorkUnit(ContextFromArgs(invocation[0]), resolved.ControlTenantID, resolved.RunID)
			if unitErr != nil {
				return "", unitErr
			}
			currentUnit = unit
		}
	}
	if currentUnit != nil && len(skills) > control.MaxSkillCandidateRefsPerWorkUnit {
		skills = skills[:control.MaxSkillCandidateRefsPerWorkUnit]
		truncated = true
	}
	entries := make([]skillListEntry, 0, len(skills))
	for _, s := range skills {
		entry := skillListEntry{
			Name:        s.Name,
			Description: presentSkillDescription(s.Description),
			State:       s.State,
			Source:      s.Source,
			Scope:       s.Scope,
			Writable:    s.Writable,
			Pinned:      s.Pinned,
			LastUsed:    s.LastUsed,
			Path:        s.Path,
			Root:        s.Root,
			Format:      s.Format,
		}
		if currentUnit != nil && s.State == SkillStateActive {
			pack, packErr := ReadSkillPackageForTenant(tenantID, s.Name, invocation...)
			if packErr != nil {
				return "", packErr
			}
			key, keyErr := resolvedSkillKey(tenantID, pack.Info)
			if keyErr != nil {
				return "", keyErr
			}
			issued, issueErr := store.IssueSkillCandidateRef(ContextFromArgs(invocation[0]), control.IssueSkillCandidateRefInput{
				IdentityTenantID: scope.ControlTenantID, ControlTenantID: tenantID, PersonID: scope.PersonID,
				RunID: scope.RunID, WorkUnitID: currentUnit.ID, SkillKey: key, SkillName: pack.Info.Name,
				VersionHash: pack.VersionHash, PackageHash: pack.PackageHash, DescriptionHash: pack.DescriptionHash,
			})
			if issueErr != nil {
				return "", issueErr
			}
			entry.CandidateRef = issued.CandidateRef
		}
		if s.Format == "dir" {
			entry.Files = listSupportFiles(s.Path)
		}
		entries = append(entries, entry)
	}
	if strings.TrimSpace(query) == "" {
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].Name < entries[j].Name
		})
	}
	presentedEntries := interface{}(entries)
	if currentUnit != nil {
		type discoveryEntry struct {
			CandidateRef string `json:"candidate_ref"`
			Name         string `json:"name"`
			Description  string `json:"description"`
			Scope        string `json:"scope,omitempty"`
			Source       string `json:"source,omitempty"`
		}
		nameCounts := make(map[string]int, len(entries))
		for _, entry := range entries {
			nameCounts[strings.ToLower(strings.TrimSpace(entry.Name))]++
		}
		minimal := make([]discoveryEntry, 0, len(entries))
		for _, entry := range entries {
			item := discoveryEntry{CandidateRef: entry.CandidateRef, Name: entry.Name, Description: entry.Description}
			if nameCounts[strings.ToLower(strings.TrimSpace(entry.Name))] > 1 {
				item.Scope, item.Source = entry.Scope, entry.Source
			}
			minimal = append(minimal, item)
		}
		presentedEntries = minimal
	}
	out := map[string]interface{}{
		"success":       true,
		"count":         len(entries),
		"total_matches": totalMatches,
		"truncated":     truncated,
		"skills":        presentedEntries,
		"hint":          "Use skill_view(name) to load SKILL.md, or skill_view(name, file_path) for linked files.",
	}
	data, _ := json.MarshalIndent(out, "", "  ")
	return string(data), nil
}

func presentSkillDescription(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(value, "\r", " "), "\n", " "))
	runes := []rune(value)
	if len(runes) > SkillDescriptionMaxChars {
		value = string(runes[:SkillDescriptionMaxChars])
	}
	if len(value) > SkillDescriptionMaxBytes {
		value = textutil.TruncateBytes(value, SkillDescriptionMaxBytes)
	}
	return value
}

func SkillViewJSONForTenant(tenantID, name, filePath string, invocation ...map[string]interface{}) (string, error) {
	return SkillViewPageJSONForTenant(tenantID, name, filePath, "", 0, maxSkillViewPageBytes, invocation...)
}

func SkillViewPageJSONForTenant(tenantID, name, filePath, section string, offsetBytes, limitBytes int, invocation ...map[string]interface{}) (string, error) {
	info, content, files, err := ReadSkillPayloadForTenant(tenantID, name, filePath, invocation...)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(filePath) != "" && strings.TrimSpace(section) != "" {
		return "", fmt.Errorf("section applies only to SKILL.md; omit file_path")
	}
	selected := content
	if strings.TrimSpace(filePath) == "" {
		var ok bool
		selected, ok = kernel.SkillSectionPage(content, section)
		if !ok {
			return "", fmt.Errorf("skill %q has no level-two section %q", info.Name, strings.TrimSpace(section))
		}
	}
	page, nextOffset, err := skillViewPage(selected, offsetBytes, limitBytes)
	if err != nil {
		return "", err
	}
	_ = MarkSkillViewed(tenantID, info.Name, invocation...)
	emitSkillViewed(invocation, info, selected, filePath)
	digest := sha256.Sum256([]byte(selected))
	out := map[string]interface{}{
		"success":      true,
		"name":         info.Name,
		"description":  info.Description,
		"state":        info.State,
		"source":       info.Source,
		"scope":        info.Scope,
		"writable":     info.Writable,
		"pinned":       info.Pinned,
		"path":         info.Path,
		"root":         info.Root,
		"content":      page,
		"content_hash": fmt.Sprintf("%x", digest[:]),
		"offset_bytes": offsetBytes,
		"total_bytes":  len(selected),
		"complete":     nextOffset == 0,
	}
	if nextOffset > 0 {
		out["next_offset_bytes"] = nextOffset
		out["hint"] = "Continue the same page with offset_bytes=next_offset_bytes."
	}
	if filePath != "" {
		out["file"] = filepath.ToSlash(filePath)
	} else if strings.TrimSpace(section) != "" {
		out["section"] = strings.TrimSpace(section)
	} else if len(files) > 0 {
		out["linked_files"] = files
		if nextOffset == 0 {
			out["hint"] = "Load linked files with skill_view(name, file_path)."
		}
	}
	data, _ := json.MarshalIndent(out, "", "  ")
	return string(data), nil
}

func integerToolArg(args map[string]interface{}, name string) int {
	switch value := args[name].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	case json.Number:
		n, _ := value.Int64()
		return int(n)
	default:
		return 0
	}
}

func skillViewPage(content string, offsetBytes, limitBytes int) (string, int, error) {
	if offsetBytes < 0 || offsetBytes > len(content) {
		return "", 0, fmt.Errorf("offset_bytes must be between 0 and %d", len(content))
	}
	if offsetBytes < len(content) && !utf8.RuneStart(content[offsetBytes]) {
		return "", 0, fmt.Errorf("offset_bytes must point to a UTF-8 character boundary")
	}
	if limitBytes <= 0 || limitBytes > maxSkillViewPageBytes {
		limitBytes = maxSkillViewPageBytes
	}
	remainder := content[offsetBytes:]
	if len(remainder) <= limitBytes {
		return remainder, 0, nil
	}
	end := limitBytes
	for end > 0 && !utf8.ValidString(remainder[:end]) {
		end--
	}
	if end <= 0 {
		return "", 0, fmt.Errorf("limit_bytes is too small for the next UTF-8 character")
	}
	return remainder[:end], offsetBytes + end, nil
}

func emitSkillViewed(invocation []map[string]interface{}, info SkillInfo, content, filePath string) {
	if len(invocation) == 0 || invocation[0] == nil {
		return
	}
	ctx := ContextFromArgs(invocation[0])
	digest := sha256.Sum256([]byte(content))
	kernel.EmitAgentEvent(kernel.EventChannelFromContext(ctx), kernel.AgentEvent{
		Type: "skill.viewed",
		Payload: map[string]interface{}{
			"name":         info.Name,
			"version_hash": fmt.Sprintf("%x", digest[:]),
			"source":       info.Source,
			"scope":        info.Scope,
			"root":         info.Root,
			"pinned":       info.Pinned,
			"writable":     info.Writable,
			"file_path":    filepath.ToSlash(filePath),
			"view_source":  "skill_view",
		},
	})
}

func ReadSkillPayloadForTenant(tenantID, name, filePath string, invocation ...map[string]interface{}) (SkillInfo, string, []string, error) {
	if strings.TrimSpace(name) == "" {
		return SkillInfo{}, "", nil, fmt.Errorf("name is required")
	}
	info, err := findSkill(tenantID, name, invocation...)
	if err != nil {
		return SkillInfo{}, "", nil, err
	}
	if filePath != "" {
		if info.Format != "dir" {
			return SkillInfo{}, "", nil, fmt.Errorf("skill %q has no linked file directory", info.Name)
		}
		target, err := safeSupportPath(info.Path, filePath)
		if err != nil {
			return SkillInfo{}, "", nil, err
		}
		data, err := os.ReadFile(target)
		if err != nil {
			return SkillInfo{}, "", nil, err
		}
		return info, string(data), listSupportFiles(info.Path), nil
	}
	content, err := readSkillContent(info)
	if err != nil {
		return SkillInfo{}, "", nil, err
	}
	files := []string{}
	if info.Format == "dir" {
		files = listSupportFiles(info.Path)
	}
	return info, content, files, nil
}

func BuildSkillInvocationMessageForTenant(tenantID, name, instruction string, invocation ...map[string]interface{}) (string, string, error) {
	_, prompt, display, err := buildTypedSkillInvocationForTenant(tenantID, name, instruction, invocation...)
	return prompt, display, err
}

func buildTypedSkillInvocationForTenant(tenantID, name, instruction string, invocation ...map[string]interface{}) (kernel.ExplicitSkillInvocation, string, string, error) {
	pack, err := ReadSkillPackageForTenant(tenantID, name, invocation...)
	if err != nil {
		return kernel.ExplicitSkillInvocation{}, "", "", err
	}
	info := pack.Info
	if info.State == SkillStateDisabled {
		return kernel.ExplicitSkillInvocation{}, "", "", fmt.Errorf("skill %q is disabled", info.Name)
	}
	key, err := resolvedSkillKey(tenantID, info)
	if err != nil {
		return kernel.ExplicitSkillInvocation{}, "", "", err
	}
	prompt := strings.TrimSpace(instruction)
	if strings.TrimSpace(instruction) != "" {
		prompt = strings.TrimSpace(instruction)
	} else {
		prompt = "Apply the explicitly invoked Skill to this work unit."
	}
	return kernel.ExplicitSkillInvocation{
		Name: info.Name, SkillKey: key, VersionHash: pack.VersionHash, PackageHash: pack.PackageHash,
	}, prompt, info.Name, nil
}

func ResolveTypedSkillInvocationForTenant(tenantID, slashCommand, instruction string, invocation ...map[string]interface{}) (kernel.ExplicitSkillInvocation, string, string, string, bool, error) {
	name := strings.TrimPrefix(strings.TrimSpace(slashCommand), "/")
	if name == "" {
		return kernel.ExplicitSkillInvocation{}, "", "", "", false, nil
	}
	if msg, display, ok, err := BuildBundleInvocationMessageForTenant(tenantID, name, instruction, invocation...); ok || err != nil {
		return kernel.ExplicitSkillInvocation{}, msg, display, "bundle", ok, err
	}
	skill, err := findSkillByCommand(tenantID, name, invocation...)
	if err != nil {
		return kernel.ExplicitSkillInvocation{}, "", "", "", false, nil
	}
	resolved, prompt, display, err := buildTypedSkillInvocationForTenant(tenantID, skill.Name, instruction, invocation...)
	return resolved, prompt, display, "skill", err == nil, err
}

func ResolveSkillInvocationForTenant(tenantID, slashCommand, instruction string, invocation ...map[string]interface{}) (string, string, bool, error) {
	_, prompt, display, _, ok, err := ResolveTypedSkillInvocationForTenant(tenantID, slashCommand, instruction, invocation...)
	return prompt, display, ok, err
}

func findSkillByCommand(tenantID, command string, invocation ...map[string]interface{}) (SkillInfo, error) {
	skills, err := ListSkillsForTenant(tenantID, false, invocation...)
	if err != nil {
		return SkillInfo{}, err
	}
	want := normalizeSkillCommandName(command)
	for _, s := range skills {
		if s.State == SkillStateDisabled {
			continue
		}
		if normalizeSkillCommandName(s.Name) == want {
			return s, nil
		}
	}
	return SkillInfo{}, fmt.Errorf("skill command not found: /%s", command)
}

func normalizeSkillCommandName(name string) string {
	name = strings.TrimPrefix(strings.TrimSpace(strings.ToLower(name)), "/")
	name = strings.ReplaceAll(name, "_", "-")
	return strings.Trim(strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, name), "-")
}

func ReloadSkillToolsForTenant(tenantID string, registry *Registry, invocation ...map[string]interface{}) ([]SkillDefinition, error) {
	if registry == nil {
		return nil, fmt.Errorf("registry is required")
	}
	for _, name := range registry.List() {
		if strings.HasPrefix(name, "skill:") {
			registry.Unregister(name)
		}
	}
	roots, err := SkillRootsForTenant(tenantID, invocation...)
	if err != nil {
		return nil, err
	}
	var loaded []SkillDefinition
	for i := len(roots) - 1; i >= 0; i-- {
		root := roots[i]
		loader := NewSkillLoader(root.Path, registry)
		defs, err := loader.LoadAll()
		if err != nil {
			return loaded, err
		}
		loaded = append(loaded, defs...)
	}
	return loaded, nil
}

func truncateUTF8ByBytes(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		return s, false
	}
	if max > len(s) {
		max = len(s)
	}
	for max > 0 && !utf8.ValidString(s[:max]) {
		max--
	}
	if max <= 0 {
		return "", true
	}
	return s[:max] + "\n\n...(truncated)", true
}
