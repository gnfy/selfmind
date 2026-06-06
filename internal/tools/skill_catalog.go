package tools

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"selfmind/internal/kernel"
)

//go:embed official-skills/*/SKILL.md
var officialSkillsFS embed.FS

type SkillCatalogTool struct {
	BaseTool
}

func NewSkillCatalogTool() *SkillCatalogTool {
	return &SkillCatalogTool{
		BaseTool: BaseTool{
			name:        "skill_catalog",
			description: "List, install, and audit skills from SelfMind official starters, local paths, or URLs.",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"action": {
						Type:        "string",
						Description: "Action: list, install, or audit.",
						Enum:        []string{"list", "install", "audit"},
					},
					"source": {
						Type:        "string",
						Description: "For install: official/<name>, local file/dir path, raw URL, or GitHub blob URL.",
					},
					"name": {
						Type:        "string",
						Description: "Optional installed skill name, or skill name for audit.",
					},
					"force": {
						Type:        "boolean",
						Description: "Overwrite an existing installed skill.",
						Default:     false,
					},
				},
				Required: []string{"action"},
			},
		},
	}
}

func (t *SkillCatalogTool) Execute(args map[string]interface{}) (string, error) {
	tenantID, _ := args["_tenant_id"].(string)
	if tenantID == "" {
		tenantID = "default"
	}
	action, _ := args["action"].(string)
	source, _ := args["source"].(string)
	name, _ := args["name"].(string)
	force, _ := args["force"].(bool)
	switch action {
	case "list":
		return OfficialSkillCatalogJSON()
	case "install":
		result, err := InstallSkillFromSource(tenantID, source, name, force)
		if err == nil {
			reloadSkillToolsFromArgs(tenantID, args)
		}
		return result, err
	case "audit":
		return AuditSkillsForTenant(tenantID, name)
	default:
		return "", fmt.Errorf("unknown action: %s", action)
	}
}

func OfficialSkillCatalogJSON() (string, error) {
	items, err := officialSkillCatalog()
	if err != nil {
		return "", err
	}
	data, _ := json.MarshalIndent(map[string]interface{}{
		"success": true,
		"count":   len(items),
		"skills":  items,
		"hint":    "Install with skill_catalog(action=install, source=\"official/<name>\") or /skills install official/<name>.",
	}, "", "  ")
	return string(data), nil
}

func officialSkillCatalog() ([]skillListEntry, error) {
	dirs, err := officialSkillsFS.ReadDir("official-skills")
	if err != nil {
		return nil, err
	}
	var items []skillListEntry
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		path := "official-skills/" + dir.Name() + "/SKILL.md"
		data, err := officialSkillsFS.ReadFile(path)
		if err != nil {
			continue
		}
		def, _, _ := parseFrontMatter(string(data))
		name := def.Name
		if name == "" {
			name = dir.Name()
		}
		items = append(items, skillListEntry{
			Name:        name,
			Description: def.Description,
			Source:      "official",
			Path:        "official/" + dir.Name(),
			Format:      "dir",
		})
	}
	return items, nil
}

func InstallSkillFromSource(tenantID, source, name string, force bool) (string, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", fmt.Errorf("source is required for install")
	}
	var content string
	var support map[string]string
	var err error
	switch {
	case strings.HasPrefix(source, "official/"):
		content, err = readOfficialSkill(strings.TrimPrefix(source, "official/"))
	case isHTTPURL(source):
		content, err = readSkillFromURL(source)
	default:
		content, support, err = readSkillFromLocalPath(source)
	}
	if err != nil {
		return "", err
	}
	def, _, _ := parseFrontMatter(content)
	if name == "" {
		name = def.Name
	}
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(source), filepath.Ext(source))
	}
	safeName := kernel.SanitizeSkillName(name)
	content = ensureFrontMatter(content, safeName, def.Description)
	if err := kernel.ScanSkillForDangers(content); err != nil {
		return "", fmt.Errorf("security scan failed: %w", err)
	}
	for path, data := range support {
		if err := kernel.ScanSkillForDangers(data); err != nil {
			return "", fmt.Errorf("security scan failed for %s: %w", path, err)
		}
	}

	dir, err := getSkillsDir(tenantID)
	if err != nil {
		return "", err
	}
	skillDir := filepath.Join(dir, safeName)
	if _, err := os.Stat(skillDir); err == nil && !force {
		return "", fmt.Errorf("skill %q already exists; pass force=true to overwrite", safeName)
	}
	if force {
		_ = os.RemoveAll(skillDir)
	}
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		return "", err
	}
	if err := atomicWriteFile(filepath.Join(skillDir, "SKILL.md"), content); err != nil {
		return "", err
	}
	for path, data := range support {
		target, err := safeSupportPath(skillDir, path)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return "", err
		}
		if err := atomicWriteFile(target, data); err != nil {
			return "", err
		}
	}
	_ = MarkSkillCreated(tenantID, safeName, SkillSourceManual, "skill_catalog")
	return fmt.Sprintf("Installed skill %q from %s.", safeName, source), nil
}

func AuditSkillsForTenant(tenantID, name string) (string, error) {
	var targets []SkillInfo
	if strings.TrimSpace(name) != "" {
		info, err := findSkill(tenantID, name)
		if err != nil {
			return "", err
		}
		targets = []SkillInfo{info}
	} else {
		skills, err := ListSkillsForTenant(tenantID, false)
		if err != nil {
			return "", err
		}
		targets = skills
	}
	type auditRow struct {
		Name   string `json:"name"`
		Path   string `json:"path"`
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
	}
	var rows []auditRow
	for _, s := range targets {
		status := "ok"
		errText := ""
		if err := auditSkillInfo(s); err != nil {
			status = "blocked"
			errText = err.Error()
		}
		rows = append(rows, auditRow{Name: s.Name, Path: s.Path, Status: status, Error: errText})
	}
	data, _ := json.MarshalIndent(map[string]interface{}{"success": true, "count": len(rows), "results": rows}, "", "  ")
	return string(data), nil
}

func auditSkillInfo(info SkillInfo) error {
	content, err := readSkillContent(info)
	if err != nil {
		return err
	}
	if err := kernel.ScanSkillForDangers(content); err != nil {
		return err
	}
	if info.Format != "dir" {
		return nil
	}
	for _, rel := range listSupportFiles(info.Path) {
		path, err := safeSupportPath(info.Path, rel)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := kernel.ScanSkillForDangers(string(data)); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
	}
	return nil
}

func readOfficialSkill(name string) (string, error) {
	safe := kernel.SanitizeSkillName(name)
	data, err := officialSkillsFS.ReadFile("official-skills/" + safe + "/SKILL.md")
	if err != nil {
		return "", fmt.Errorf("official skill not found: %s", name)
	}
	return string(data), nil
}

func readSkillFromURL(rawURL string) (string, error) {
	url := normalizeSkillURL(rawURL)
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("download failed: %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func readSkillFromLocalPath(source string) (string, map[string]string, error) {
	path := filepath.Clean(source)
	info, err := os.Stat(path)
	if err != nil {
		return "", nil, err
	}
	support := map[string]string{}
	if !info.IsDir() {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", nil, err
		}
		return string(data), support, nil
	}
	data, err := os.ReadFile(filepath.Join(path, "SKILL.md"))
	if err != nil {
		return "", nil, err
	}
	for subdir := range allowedSkillSubdirs {
		root := filepath.Join(path, subdir)
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(path, p)
			if err != nil {
				return nil
			}
			fileData, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			support[filepath.ToSlash(rel)] = string(fileData)
			return nil
		})
	}
	return string(data), support, nil
}

func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")
}

func normalizeSkillURL(rawURL string) string {
	if strings.Contains(rawURL, "github.com/") && strings.Contains(rawURL, "/blob/") {
		trimmed := strings.TrimPrefix(rawURL, "https://github.com/")
		parts := strings.SplitN(trimmed, "/blob/", 2)
		if len(parts) == 2 {
			repo := parts[0]
			rest := parts[1]
			return "https://raw.githubusercontent.com/" + repo + "/" + rest
		}
	}
	return rawURL
}
