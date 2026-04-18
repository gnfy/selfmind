package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// SkillDefinition 定义一个技能的元数据（从 front matter 解析）
type SkillDefinition struct {
	Name        string   `json:"name"`                   // skill name
	Description string   `json:"description"`            // skill description for LLM
	Trigger     []string `json:"trigger,omitempty"`      // slash command aliases, e.g. ["/skill", "/s"]
	Parameters  []string `json:"parameters,omitempty"`   // required parameter names
	Examples    []string `json:"examples,omitempty"`     // usage examples
	Confidence  float64  `json:"confidence,omitempty"`   // 0.0-1.0, auto-archive threshold
	Source      string   `json:"source,omitempty"`       // source file path
	ToolName    string   `json:"tool_name,omitempty"`    // 对应的工具名（如果有）
	Handler     string   `json:"handler,omitempty"`      // Go function reference (for codegen)
}

// SkillLoader 动态加载 skills 目录下的 YAML + Markdown 文件
type SkillLoader struct {
	skillsDir string
	registry  *Registry
}

func NewSkillLoader(skillsDir string, registry *Registry) *SkillLoader {
	return &SkillLoader{skillsDir: skillsDir, registry: registry}
}

// LoadAll 扫描 skillsDir 并加载所有有效的 skill 文件
func (sl *SkillLoader) LoadAll() ([]SkillDefinition, error) {
	var loaded []SkillDefinition

	entries, err := os.ReadDir(sl.skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return loaded, nil
		}
		return nil, fmt.Errorf("reading skills dir: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(sl.skillsDir, entry.Name())
		def, err := sl.LoadFile(path)
		if err != nil {
			fmt.Printf("[SkillLoader] WARN: failed to load %s: %v\n", path, err)
			continue
		}
		loaded = append(loaded, def)
	}
	return loaded, nil
}

// LoadFile 加载单个 Markdown 文件，解析 front matter
func (sl *SkillLoader) LoadFile(path string) (SkillDefinition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SkillDefinition{}, err
	}

	// 解析 front matter（YAML between ---）
	def, body, err := parseFrontMatter(string(data))
	if err != nil {
		return SkillDefinition{}, fmt.Errorf("front matter parse error: %w", err)
	}
	def.Source = path

	// 动态注册为工具
	if def.ToolName == "" {
		def.ToolName = "skill:" + def.Name
	}

	// 用 Markdown body 构造 handler
	steps := extractSkillSteps(body)
	sl.registerSkillTool(def, body, steps)

	return def, nil
}

// parseFrontMatter 提取 YAML front matter 和 Markdown body
func parseFrontMatter(content string) (SkillDefinition, string, error) {
	lines := strings.Split(content, "\n")
	if len(lines) < 3 || lines[0] != "---" {
		return SkillDefinition{}, content, nil // 没有 front matter，整体当 body
	}

	var yamlLines []string
	bodyStart := -1
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			bodyStart = i + 1
			break
		}
		yamlLines = append(yamlLines, lines[i])
	}

	if bodyStart == -1 {
		return SkillDefinition{}, content, nil
	}

	yamlStr := strings.Join(yamlLines, "\n")
	var def SkillDefinition
	// 简单的 YAML 解析（避免引入外部依赖）
	if err := parseSimpleYAML(yamlStr, &def); err != nil {
		return SkillDefinition{}, "", err
	}

	body := strings.Join(lines[bodyStart:], "\n")
	return def, strings.TrimSpace(body), nil
}

// parseSimpleYAML 轻量级 YAML 解析（支持 SkillDefinition 的字段）
func parseSimpleYAML(yaml string, def *SkillDefinition) error {
	lines := strings.Split(yaml, "\n")
	var currentKey string
	var inArray bool
	var arrayValues []string

	flushArray := func() {
		if currentKey != "" && len(arrayValues) > 0 {
			switch currentKey {
			case "trigger":
				def.Trigger = append(def.Trigger, arrayValues...)
			case "parameters":
				def.Parameters = append(def.Parameters, arrayValues...)
			case "examples":
				def.Examples = append(def.Examples, arrayValues...)
			}
			arrayValues = nil
		}
		currentKey = ""
		inArray = false
	}

	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			if !inArray {
				continue
			}
			// blank line inside array — flush and continue
			if len(arrayValues) > 0 {
				flushArray()
			}
			continue
		}

		// Indent detection for array items
		if inArray {
			if strings.HasPrefix(line, "- ") {
				val := strings.TrimSpace(strings.TrimPrefix(line, "- "))
				val = strings.Trim(val, "\"' ")
				arrayValues = append(arrayValues, val)
				continue
			}
			flushArray()
		}

		// Array key detected (trigger:, parameters:, examples:)
		if strings.HasSuffix(line, ":") && !strings.Contains(line, " ") {
			key := strings.TrimSpace(strings.TrimSuffix(line, ":"))
			if key == "trigger" || key == "parameters" || key == "examples" {
				currentKey = key
				inArray = true
				arrayValues = nil
				continue
			}
		}

		// Key:value pair
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		val = strings.Trim(val, "\"' ")

		switch key {
		case "name":
			def.Name = val
		case "description":
			def.Description = val
		case "tool_name":
			def.ToolName = val
		case "confidence":
			fmt.Sscanf(val, "%f", &def.Confidence)
		case "handler":
			def.Handler = val
		case "source":
			def.Source = val
		}
	}

	flushArray()
	return nil
}

func (sl *SkillLoader) registerSkillTool(def SkillDefinition, body string, steps []string) {
	tool := &SkillTool{
		BaseTool: BaseTool{
			name:        def.ToolName,
			description: def.Description,
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"input": {
						Type:        "string",
						Description: "User input for this skill",
					},
				},
				Required: []string{},
			},
		},
		content: body,
		steps:   steps,
	}
	sl.registry.Register(tool)
}

// SkillTool 是从 Markdown skill 文件生成的工具
type SkillTool struct {
	BaseTool
	content string // Markdown body 作为 skill 的执行逻辑
	steps   []string // 从 body 中提取的步骤（代码块）
}

// SetSkillSteps 设置 skill 的执行步骤（由 SkillLoader.LoadAll 填充）
func (t *SkillTool) SetSkillSteps(steps []string) {
	t.steps = steps
}

// Execute 解析 markdown 中的 code block 并依次执行每个步骤
func (t *SkillTool) Execute(args map[string]interface{}) (string, error) {
	// 如果没有预解析的 steps，则延迟解析
	if len(t.steps) == 0 {
		t.steps = extractSkillSteps(t.content)
	}
	if len(t.steps) == 0 {
		return fmt.Sprintf("[Skill: %s] 无可执行步骤，请检查 skill 文件格式（需要 ``` 代码块）", t.Name()), nil
	}

	var results []string
	for i, step := range t.steps {
		if step == "" {
			continue
		}
		// 去掉步骤编号前缀（如 "1. " 或 "step 1:"）
		cmd := strings.TrimLeft(step, " \t0123456789.-:")
		cmd = strings.TrimSpace(cmd)
		if cmd == "" {
			continue
		}

		// 通过 terminal 工具执行 shell 命令
		result, err := globalRegistry.Dispatch("terminal", map[string]interface{}{
			"command": cmd,
		})
		if err != nil {
			results = append(results, fmt.Sprintf("步骤 %d 执行失败: %v", i+1, err))
			break
		}
		results = append(results, fmt.Sprintf("步骤 %d:\n%s\n=> %s", i+1, cmd, trimResult(result)))
	}

	return strings.Join(results, "\n\n"), nil
}

// extractSkillSteps 从 markdown body 中提取所有 fenced code block 内容
func extractSkillSteps(body string) []string {
	var steps []string
	lines := strings.Split(body, "\n")
	inCodeBlock := false
	var block []string

	for _, line := range lines {
		if strings.HasPrefix(line, "```") {
			if inCodeBlock {
				// 结束当前 code block
				if len(block) > 0 {
					steps = append(steps, strings.Join(block, "\n"))
				}
				block = nil
			}
			inCodeBlock = !inCodeBlock
			continue
		}
		if inCodeBlock {
			block = append(block, line)
		}
	}
	return steps
}

// trimResult 截断过长结果用于显示
func trimResult(s string) string {
	if len(s) > 300 {
		return s[:300] + "\n...(truncated)"
	}
	return s
}

// SkillsIndex 返回所有加载的 skills 的概要索引
type SkillsIndex struct {
	Skills []SkillDefinition `json:"skills"`
}

func (sl *SkillLoader) BuildIndex() (*SkillsIndex, error) {
	defs, err := sl.LoadAll()
	if err != nil {
		return nil, err
	}
	return &SkillsIndex{Skills: defs}, nil
}

// Search 搜索 skills by keyword
func (idx *SkillsIndex) Search(keyword string) []SkillDefinition {
	var results []SkillDefinition
	kw := strings.ToLower(keyword)
	for _, s := range idx.Skills {
		if strings.Contains(strings.ToLower(s.Name), kw) ||
			strings.Contains(strings.ToLower(s.Description), kw) {
			results = append(results, s)
		}
	}
	return results
}

// LLMer 导出 skills 为 LLM 可消费的 system prompt 片段
func (idx *SkillsIndex) ToLLMPrompt() string {
	var lines []string
	lines = append(lines, "## Available Skills")
	for _, s := range idx.Skills {
		triggers := ""
		if len(s.Trigger) > 0 {
			triggers = fmt.Sprintf(" (aliases: %s)", strings.Join(s.Trigger, ", "))
		}
		lines = append(lines, fmt.Sprintf("- **%s**: %s%s", s.Name, s.Description, triggers))
	}
	return strings.Join(lines, "\n")
}

// skillsDirForTenant 返回指定租户的 skills 目录
func SkillsDirForTenant(baseDir, tenantID string) string {
	return filepath.Join(baseDir, tenantID, "skills")
}

// ---- 工具注册助手 ----

// ToolDefFromSkill 将 SkillDefinition 转换为 LLM ToolDefinition
func ToolDefFromSkill(s SkillDefinition) map[string]interface{} {
	props := make(map[string]interface{})
	for _, p := range s.Parameters {
		props[p] = map[string]interface{}{
			"type":        "string",
			"description": fmt.Sprintf("Parameter: %s", p),
		}
	}
	return map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        s.ToolName,
			"description": s.Description,
			"parameters": map[string]interface{}{
				"type":       "object",
				"properties": props,
				"required":   s.Parameters,
			},
		},
	}
}

// toolCallRe matches tool call patterns in LLM response
var toolCallRe = regexp.MustCompile(`\[TOOL:([^\]:]+)(?::([^\]]+))?\]`)

// ExtractToolCalls extracts tool calls from LLM response text
func ExtractToolCalls(text string) []struct{ Name, Args string } {
	var calls []struct{ Name, Args string }
	matches := toolCallRe.FindAllStringSubmatch(text, -1)
	for _, m := range matches {
		calls = append(calls, struct{ Name, Args string }{m[1], m[2]})
	}
	return calls
}
