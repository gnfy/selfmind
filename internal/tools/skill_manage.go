package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"selfmind/internal/kernel"
)

// SkillManageTool allows the agent to actively create, update, or delete skills.
type SkillManageTool struct {
	BaseTool
}

// NewSkillManageTool creates the skill_manage tool.
func NewSkillManageTool() *SkillManageTool {
	return &SkillManageTool{
		BaseTool: BaseTool{
			name:        "skill_manage",
			description: "Create, update, or delete a reusable skill. Skills are saved as markdown files in ~/.selfmind/skills/ and can be loaded in future sessions.",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"action": {
						Type:        "string",
						Description: "The action to perform: 'create', 'update', or 'delete'.",
						Enum:        []string{"create", "update", "delete"},
					},
					"name": {
						Type:        "string",
						Description: "The skill name (kebab-case or plain English). Used as the filename.",
					},
					"content": {
						Type:        "string",
						Description: "The full markdown content of the skill (including YAML front matter). Required for create/update.",
					},
					"description": {
						Type:        "string",
						Description: "A short one-line description of what this skill does. Used if content does not include YAML front matter.",
					},
				},
				Required: []string{"action", "name"},
			},
		},
	}
}

func (t *SkillManageTool) Execute(args map[string]interface{}) (string, error) {
	action, _ := args["action"].(string)
	name, _ := args["name"].(string)
	content, _ := args["content"].(string)
	description, _ := args["description"].(string)

	if action == "" || name == "" {
		return "", fmt.Errorf("action and name are required")
	}

	tenantID, _ := args["_tenant_id"].(string)
	if tenantID == "" {
		tenantID = "default"
	}
	skillDir, err := getSkillsDir(tenantID)
	if err != nil {
		return "", err
	}

	safeName := kernel.SanitizeSkillName(name)
	targetPath := filepath.Join(skillDir, safeName+".md")

	switch action {
	case "create":
		if content == "" {
			return "", fmt.Errorf("content is required for create")
		}
		if _, err := os.Stat(targetPath); err == nil {
			return "", fmt.Errorf("skill %q already exists; use 'update' instead", name)
		}
		// Auto-wrap with front matter if missing
		content = ensureFrontMatter(content, name, description)
		if err := kernel.ScanSkillForDangers(content); err != nil {
			return "", fmt.Errorf("security scan failed: %w", err)
		}
		if err := atomicWriteFile(targetPath, content); err != nil {
			return "", err
		}
		return fmt.Sprintf("Skill %q created successfully at %s", name, targetPath), nil

	case "update":
		if content == "" {
			return "", fmt.Errorf("content is required for update")
		}
		if _, err := os.Stat(targetPath); os.IsNotExist(err) {
			return "", fmt.Errorf("skill %q does not exist; use 'create' instead", name)
		}
		content = ensureFrontMatter(content, name, description)
		if err := kernel.ScanSkillForDangers(content); err != nil {
			return "", fmt.Errorf("security scan failed: %w", err)
		}
		if err := atomicWriteFile(targetPath, content); err != nil {
			return "", err
		}
		return fmt.Sprintf("Skill %q updated successfully at %s", name, targetPath), nil

	case "delete":
		if err := os.Remove(targetPath); err != nil {
			if os.IsNotExist(err) {
				return "", fmt.Errorf("skill %q not found", name)
			}
			return "", fmt.Errorf("failed to delete skill: %w", err)
		}
		return fmt.Sprintf("Skill %q deleted successfully", name), nil

	default:
		return "", fmt.Errorf("unknown action: %s", action)
	}
}

func getSkillsDir(tenantID string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	baseDir := filepath.Join(home, ".selfmind")
	dir := SkillsDirForTenant(baseDir, tenantID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create skills dir: %w", err)
	}
	return dir, nil
}

func atomicWriteFile(path, content string) error {
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("write tmp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename file: %w", err)
	}
	return nil
}

// ensureFrontMatter wraps raw content with YAML front matter if missing.
func ensureFrontMatter(content, name, description string) string {
	if strings.HasPrefix(strings.TrimSpace(content), "---") {
		return content
	}
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("name: %s\n", name))
	if description != "" {
		sb.WriteString(fmt.Sprintf("description: %s\n", description))
	} else {
		sb.WriteString(fmt.Sprintf("description: %s\n", name))
	}
	sb.WriteString("---\n\n")
	sb.WriteString(content)
	return sb.String()
}
