package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type skillListEntry struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	State       string   `json:"state"`
	Source      string   `json:"source"`
	Pinned      bool     `json:"pinned"`
	LastUsed    string   `json:"last_used,omitempty"`
	Path        string   `json:"path"`
	Format      string   `json:"format"`
	Files       []string `json:"linked_files,omitempty"`
}

type SkillsListTool struct {
	BaseTool
}

func NewSkillsListTool() *SkillsListTool {
	return &SkillsListTool{
		BaseTool: BaseTool{
			name:        "skills_list",
			description: "List available skills as compact metadata. Use skill_view to load full instructions or linked files.",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"query": {
						Type:        "string",
						Description: "Optional text filter for skill name, description, or content.",
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
}

func (t *SkillsListTool) Execute(args map[string]interface{}) (string, error) {
	tenantID, _ := args["_tenant_id"].(string)
	if tenantID == "" {
		tenantID = "default"
	}
	query, _ := args["query"].(string)
	includeArchived, _ := args["include_archived"].(bool)
	return SkillsListJSONForTenant(tenantID, query, includeArchived)
}

type SkillViewTool struct {
	BaseTool
}

func NewSkillViewTool() *SkillViewTool {
	return &SkillViewTool{
		BaseTool: BaseTool{
			name:        "skill_view",
			description: "Load a skill's full SKILL.md content or a specific linked file under references/, templates/, scripts/, or assets/.",
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
				},
				Required: []string{"name"},
			},
		},
	}
}

func (t *SkillViewTool) Execute(args map[string]interface{}) (string, error) {
	tenantID, _ := args["_tenant_id"].(string)
	if tenantID == "" {
		tenantID = "default"
	}
	name, _ := args["name"].(string)
	filePath, _ := args["file_path"].(string)
	return SkillViewJSONForTenant(tenantID, name, filePath)
}

func SkillsListJSONForTenant(tenantID, query string, includeArchived bool) (string, error) {
	var skills []SkillInfo
	var err error
	if strings.TrimSpace(query) != "" {
		skills, err = SearchSkillsForTenant(tenantID, query)
	} else {
		skills, err = ListSkillsForTenant(tenantID, includeArchived)
	}
	if err != nil {
		return "", err
	}

	entries := make([]skillListEntry, 0, len(skills))
	for _, s := range skills {
		entry := skillListEntry{
			Name:        s.Name,
			Description: s.Description,
			State:       s.State,
			Source:      s.Source,
			Pinned:      s.Pinned,
			LastUsed:    s.LastUsed,
			Path:        s.Path,
			Format:      s.Format,
		}
		if s.Format == "dir" {
			entry.Files = listSupportFiles(s.Path)
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})
	out := map[string]interface{}{
		"success": true,
		"count":   len(entries),
		"skills":  entries,
		"hint":    "Use skill_view(name) to load SKILL.md, or skill_view(name, file_path) for linked files.",
	}
	data, _ := json.MarshalIndent(out, "", "  ")
	return string(data), nil
}

func SkillViewJSONForTenant(tenantID, name, filePath string) (string, error) {
	info, content, files, err := ReadSkillPayloadForTenant(tenantID, name, filePath)
	if err != nil {
		return "", err
	}
	_ = MarkSkillViewed(tenantID, info.Name)
	out := map[string]interface{}{
		"success":     true,
		"name":        info.Name,
		"description": info.Description,
		"state":       info.State,
		"source":      info.Source,
		"pinned":      info.Pinned,
		"path":        info.Path,
		"content":     content,
	}
	if filePath != "" {
		out["file"] = filepath.ToSlash(filePath)
	} else if len(files) > 0 {
		out["linked_files"] = files
		out["hint"] = "Load linked files with skill_view(name, file_path)."
	}
	data, _ := json.MarshalIndent(out, "", "  ")
	return string(data), nil
}

func ReadSkillPayloadForTenant(tenantID, name, filePath string) (SkillInfo, string, []string, error) {
	if strings.TrimSpace(name) == "" {
		return SkillInfo{}, "", nil, fmt.Errorf("name is required")
	}
	info, err := findSkill(tenantID, name)
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

func BuildSkillInvocationMessageForTenant(tenantID, name, instruction string) (string, string, error) {
	info, content, files, err := ReadSkillPayloadForTenant(tenantID, name, "")
	if err != nil {
		return "", "", err
	}
	_ = MarkSkillUsed(tenantID, info.Name)
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[IMPORTANT: The user invoked the %q skill. Follow its instructions for this turn unless the user explicitly overrides them.]\n\n", info.Name))
	sb.WriteString("## Loaded Skill: " + info.Name + "\n\n")
	sb.WriteString(content)
	if len(files) > 0 {
		sb.WriteString("\n\n## Linked Files\n")
		for _, f := range files {
			sb.WriteString("- " + f + "\n")
		}
		sb.WriteString("\nLoad linked files with skill_view if needed before using them.\n")
	}
	if strings.TrimSpace(instruction) != "" {
		sb.WriteString("\n\n## User Instruction\n")
		sb.WriteString(strings.TrimSpace(instruction))
	}
	return sb.String(), info.Name, nil
}

func ResolveSkillInvocationForTenant(tenantID, slashCommand, instruction string) (string, string, bool, error) {
	name := strings.TrimPrefix(strings.TrimSpace(slashCommand), "/")
	if name == "" {
		return "", "", false, nil
	}
	if msg, display, ok, err := BuildBundleInvocationMessageForTenant(tenantID, name, instruction); ok || err != nil {
		return msg, display, ok, err
	}
	skill, err := findSkillByCommand(tenantID, name)
	if err != nil {
		return "", "", false, nil
	}
	msg, display, err := BuildSkillInvocationMessageForTenant(tenantID, skill.Name, instruction)
	return msg, display, err == nil, err
}

func findSkillByCommand(tenantID, command string) (SkillInfo, error) {
	skills, err := ListSkillsForTenant(tenantID, false)
	if err != nil {
		return SkillInfo{}, err
	}
	want := normalizeSkillCommandName(command)
	for _, s := range skills {
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

func ReloadSkillToolsForTenant(tenantID string, registry *Registry) ([]SkillDefinition, error) {
	if registry == nil {
		return nil, fmt.Errorf("registry is required")
	}
	for _, name := range registry.List() {
		if strings.HasPrefix(name, "skill:") {
			registry.Unregister(name)
		}
	}
	dir, err := getSkillsDir(tenantID)
	if err != nil {
		return nil, err
	}
	loader := NewSkillLoader(dir, registry)
	return loader.LoadAll()
}
