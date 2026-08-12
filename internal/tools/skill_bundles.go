package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

type SkillBundle struct {
	Name        string   `json:"name" yaml:"name"`
	Description string   `json:"description" yaml:"description"`
	Skills      []string `json:"skills" yaml:"skills"`
	Instruction string   `json:"instruction,omitempty" yaml:"instruction,omitempty"`
	Path        string   `json:"path,omitempty" yaml:"-"`
}

type SkillBundleTool struct {
	BaseTool
}

func NewSkillBundleTool() *SkillBundleTool {
	return &SkillBundleTool{
		BaseTool: BaseTool{
			name:        "skill_bundle",
			description: "List, read, create, or delete skill bundles. Bundles load multiple skills as one reusable slash command.",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"action": {
						Type:        "string",
						Description: "Action: list, read, create, or delete.",
						Enum:        []string{"list", "read", "create", "delete"},
					},
					"name": {
						Type:        "string",
						Description: "Bundle name.",
					},
					"description": {
						Type:        "string",
						Description: "Short bundle description for action=create.",
					},
					"skills": {
						Type:        "string",
						Description: "Comma-separated skill names for action=create.",
					},
					"instruction": {
						Type:        "string",
						Description: "Optional extra instruction inserted before loaded skills.",
					},
				},
				Required: []string{"action"},
			},
		},
	}
}

func (t *SkillBundleTool) Execute(args map[string]interface{}) (string, error) {
	tenantID := skillStorageTenantID(args)
	action, _ := args["action"].(string)
	name, _ := args["name"].(string)
	description, _ := args["description"].(string)
	skillsRaw, _ := args["skills"].(string)
	instruction, _ := args["instruction"].(string)
	switch action {
	case "list":
		bundles, err := ListSkillBundlesForTenant(tenantID)
		if err != nil {
			return "", err
		}
		data, _ := json.MarshalIndent(map[string]interface{}{"success": true, "count": len(bundles), "bundles": bundles}, "", "  ")
		return string(data), nil
	case "read":
		b, err := FindSkillBundleForTenant(tenantID, name)
		if err != nil {
			return "", err
		}
		data, _ := json.MarshalIndent(map[string]interface{}{"success": true, "bundle": b}, "", "  ")
		return string(data), nil
	case "create":
		skills := splitBundleSkills(skillsRaw)
		b, err := SaveSkillBundleForTenant(tenantID, SkillBundle{Name: name, Description: description, Skills: skills, Instruction: instruction})
		if err != nil {
			return "", err
		}
		data, _ := json.MarshalIndent(map[string]interface{}{"success": true, "bundle": b}, "", "  ")
		return string(data), nil
	case "delete":
		if err := DeleteSkillBundleForTenant(tenantID, name); err != nil {
			return "", err
		}
		return fmt.Sprintf("Bundle %q deleted.", name), nil
	default:
		return "", fmt.Errorf("unknown action: %s", action)
	}
}

func SkillBundlesDirForTenant(tenantID string) (string, error) {
	tenantDir, err := userTenantDirForTenant(tenantID)
	if err != nil {
		return "", err
	}
	return filepath.Join(tenantDir, "skill-bundles"), nil
}

func ListSkillBundlesForTenant(tenantID string) ([]SkillBundle, error) {
	dir, err := SkillBundlesDirForTenant(tenantID)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var bundles []SkillBundle
	for _, entry := range entries {
		if entry.IsDir() || (!strings.HasSuffix(entry.Name(), ".yaml") && !strings.HasSuffix(entry.Name(), ".yml")) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		b, err := readSkillBundleFile(path)
		if err != nil {
			continue
		}
		bundles = append(bundles, b)
	}
	sort.Slice(bundles, func(i, j int) bool {
		return normalizeSkillCommandName(bundles[i].Name) < normalizeSkillCommandName(bundles[j].Name)
	})
	return bundles, nil
}

func FindSkillBundleForTenant(tenantID, name string) (SkillBundle, error) {
	bundles, err := ListSkillBundlesForTenant(tenantID)
	if err != nil {
		return SkillBundle{}, err
	}
	want := normalizeSkillCommandName(name)
	for _, b := range bundles {
		if normalizeSkillCommandName(b.Name) == want {
			return b, nil
		}
	}
	return SkillBundle{}, fmt.Errorf("bundle not found: %s", name)
}

func SaveSkillBundleForTenant(tenantID string, bundle SkillBundle) (SkillBundle, error) {
	if strings.TrimSpace(bundle.Name) == "" {
		return SkillBundle{}, fmt.Errorf("bundle name is required")
	}
	if len(bundle.Skills) == 0 {
		return SkillBundle{}, fmt.Errorf("bundle skills are required")
	}
	for _, skill := range bundle.Skills {
		if _, err := findSkill(tenantID, skill); err != nil {
			return SkillBundle{}, fmt.Errorf("bundle skill %q not found: %w", skill, err)
		}
	}
	dir, err := SkillBundlesDirForTenant(tenantID)
	if err != nil {
		return SkillBundle{}, err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return SkillBundle{}, err
	}
	safe := normalizeSkillCommandName(bundle.Name)
	if safe == "" {
		return SkillBundle{}, fmt.Errorf("invalid bundle name: %s", bundle.Name)
	}
	path := filepath.Join(dir, safe+".yaml")
	data, err := yaml.Marshal(bundle)
	if err != nil {
		return SkillBundle{}, err
	}
	if err := atomicWriteFile(path, string(data)); err != nil {
		return SkillBundle{}, err
	}
	bundle.Path = path
	return bundle, nil
}

func DeleteSkillBundleForTenant(tenantID, name string) error {
	b, err := FindSkillBundleForTenant(tenantID, name)
	if err != nil {
		return err
	}
	return os.Remove(b.Path)
}

func BuildBundleInvocationMessageForTenant(tenantID, name, instruction string) (string, string, bool, error) {
	b, err := FindSkillBundleForTenant(tenantID, name)
	if err != nil {
		return "", "", false, nil
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[IMPORTANT: The user invoked the %q skill bundle. Load and follow all included skills for this turn unless the user overrides them.]\n\n", b.Name))
	if strings.TrimSpace(b.Instruction) != "" {
		sb.WriteString("## Bundle Instruction\n")
		sb.WriteString(strings.TrimSpace(b.Instruction))
		sb.WriteString("\n\n")
	}
	if strings.TrimSpace(instruction) != "" {
		sb.WriteString("## User Instruction\n")
		sb.WriteString(strings.TrimSpace(instruction))
		sb.WriteString("\n\n")
	}
	var loaded []string
	var missing []string
	for _, skill := range b.Skills {
		msg, display, err := BuildSkillInvocationMessageForTenant(tenantID, skill, "")
		if err != nil {
			missing = append(missing, skill)
			continue
		}
		loaded = append(loaded, display)
		sb.WriteString(msg)
		sb.WriteString("\n\n")
	}
	if len(loaded) == 0 {
		return "", "", true, fmt.Errorf("bundle %q has no loadable skills; missing: %s", b.Name, strings.Join(missing, ", "))
	}
	if len(missing) > 0 {
		sb.WriteString("## Missing Bundle Skills\n")
		for _, skill := range missing {
			sb.WriteString("- " + skill + "\n")
		}
	}
	return sb.String(), b.Name, true, nil
}

func readSkillBundleFile(path string) (SkillBundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SkillBundle{}, err
	}
	var b SkillBundle
	if err := yaml.Unmarshal(data, &b); err != nil {
		return SkillBundle{}, err
	}
	if b.Name == "" {
		b.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	b.Path = path
	return b, nil
}

func splitBundleSkills(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
