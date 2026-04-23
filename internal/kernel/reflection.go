package kernel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"selfmind/internal/kernel/llm"
)

// ─────────────────────────────────────────
// 1. 数据结构
// ─────────────────────────────────────────

// TaskHistory represents a completed interaction or tool-calling session
type TaskHistory struct {
	Goal    string
	Context string
	Steps   []string
	Outcome string
}

// ReviewResult is the output of LLM reflection
type ReviewResult struct {
	Action    string // "create" | "update" | "skip"
	SkillName string
	Content   string
	Reason    string
}

// EvolutionConfig defines the agent's autonomous learning behavior
type EvolutionConfig struct {
	Enabled               bool    `mapstructure:"enabled"`
	Mode                  string  `mapstructure:"mode"`
	MinComplexityThreshold int     `mapstructure:"min_complexity_threshold"`
	AutoArchiveConfidence float64 `mapstructure:"auto_archive_confidence"`
	NudgeInterval         int     `mapstructure:"nudge_interval"` // 每 N 次工具调用触发一次
	SkillsDir             string  `mapstructure:"skills_dir"`
}

// ReflectionEngine handles the autonomous reflection and skill generation logic
type ReflectionEngine struct {
	Provider llm.Provider
	Config   EvolutionConfig

	// 通知 channel（连接到 TUI）
	notifyCh chan string
}

// NewReflectionEngine creates a new ReflectionEngine
func NewReflectionEngine(provider llm.Provider, cfg EvolutionConfig) *ReflectionEngine {
	return &ReflectionEngine{
		Provider: provider,
		Config:   cfg,
	}
}

// SetNotifyChannel sets the notification channel for TUI updates
func (r *ReflectionEngine) SetNotifyChannel(ch chan string) {
	r.notifyCh = ch
}

// ─────────────────────────────────────────
// 2. 已有 Skill 扫描（核心：避免重复创建）
// ─────────────────────────────────────────

// ExistingSkill represents a skill found on disk
type ExistingSkill struct {
	Name        string
	Description string
	Content     string // 完整内容，用于对比
	FilePath    string
}

// scanExistingSkills scans the skills directory and returns all existing skills
func (r *ReflectionEngine) scanExistingSkills() ([]ExistingSkill, error) {
	dir := r.Config.SkillsDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("get home dir: %w", err)
		}
		dir = filepath.Join(home, ".selfmind", "skills")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read skills dir: %w", err)
	}

	var skills []ExistingSkill
	for _, entry := range entries {
		var filePath string

		if entry.IsDir() {
			// 目录格式：SkillName/SKILL.md
			filePath = filepath.Join(dir, entry.Name(), "SKILL.md")
			// 如果 SKILL.md 不存在，跳过
			if _, err := os.Stat(filePath); err != nil {
				continue
			}
		} else if strings.HasSuffix(entry.Name(), ".md") {
			// 扁平格式：SkillName.md
			filePath = filepath.Join(dir, entry.Name())
		} else {
			continue
		}

		data, err := os.ReadFile(filePath)
		if err != nil {
			continue
		}

		content := string(data)
		name := extractSkillName(content)
		if name == "" {
			// fallback: 从文件名提取
			name = strings.TrimSuffix(entry.Name(), ".md")
		}

		desc := extractSkillDescription(content)
		skills = append(skills, ExistingSkill{
			Name:        name,
			Description: desc,
			Content:     content,
			FilePath:    filePath,
		})
	}

	return skills, nil
}

// extractSkillName pulls the name field from YAML front matter
func extractSkillName(content string) string {
	lines := strings.Split(content, "\n")
	inFrontMatter := false
	for i, line := range lines {
		if strings.TrimSpace(line) == "---" {
			if !inFrontMatter {
				inFrontMatter = true
				continue
			}
			// 遇到结束的 ---，停止搜索
			break
		}
		if inFrontMatter && i > 0 { // 跳过第一行的 ---
			if strings.HasPrefix(line, "name:") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					return strings.TrimSpace(parts[1])
				}
			}
		}
	}
	return ""
}

// extractSkillDescription pulls the description field from YAML front matter
func extractSkillDescription(content string) string {
	lines := strings.Split(content, "\n")
	inFrontMatter := false
	for i, line := range lines {
		if strings.TrimSpace(line) == "---" {
			if !inFrontMatter {
				inFrontMatter = true
				continue
			}
			break
		}
		if inFrontMatter && i > 0 {
			if strings.HasPrefix(line, "description:") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					return strings.TrimSpace(parts[1])
				}
			}
		}
	}
	return ""
}

// ─────────────────────────────────────────
// 3. 复杂度评估（更智能的过滤）
// ─────────────────────────────────────────

type complexityLevel int

const (
	complexityTrivial complexityLevel = iota
	complexityLow
	complexityMedium
	complexityHigh
)

// assessComplexity evaluates task complexity to decide if review is worth triggering
func (r *ReflectionEngine) assessComplexity(steps []string) complexityLevel {
	if len(steps) < 2 {
		return complexityTrivial
	}

	// 统计工具类型多样性
	toolTypes := make(map[string]int)
	for _, step := range steps {
		// 从 step 中提取工具名
		// 格式: "Executed tool: terminal, result: ..." or "Executed tool: read_file, result: ..."
		step = strings.ToLower(step)
		if idx := strings.Index(step, "tool:"); idx >= 0 {
			rest := step[idx+5:]
			// 找到第一个逗号或结果描述
			if end := strings.IndexAny(rest, ","); end >= 0 {
				toolName := strings.TrimSpace(rest[:end])
				if toolName != "" {
					toolTypes[toolName]++
				}
			}
		}
	}

	// 超过3种不同工具 或 超过6步 = 高复杂度
	if len(toolTypes) >= 3 || len(steps) >= 6 {
		return complexityHigh
	}
	// 2-3种工具 或 4-5步 = 中等复杂度
	if len(toolTypes) >= 2 || len(steps) >= 4 {
		return complexityMedium
	}
	// 2步以上 = 低复杂度
	if len(steps) >= 2 {
		return complexityLow
	}

	return complexityTrivial
}

// shouldTriggerReview decides whether to trigger a review
func (r *ReflectionEngine) shouldTriggerReview(history TaskHistory) bool {
	if !r.Config.Enabled {
		return false
	}

	// 先做简单的步数过滤
	if len(history.Steps) < r.Config.MinComplexityThreshold {
		return false
	}

	// 再做复杂度评估
	level := r.assessComplexity(history.Steps)
	// 只有中高复杂度才触发
	return level >= complexityMedium
}

// ─────────────────────────────────────────
// 4. LLM 反思（带上下文的决策）
// ─────────────────────────────────────────

// Reflect analyzes task history and decides if a skill should be created/updated
func (r *ReflectionEngine) Reflect(ctx context.Context, history TaskHistory) (*ReviewResult, error) {
	// 1. 复杂度检查
	if !r.shouldTriggerReview(history) {
		return &ReviewResult{Action: "skip", Reason: "complexity too low"}, nil
	}

	// 2. 扫描已有 skills
	existing, err := r.scanExistingSkills()
	if err != nil {
		// 非致命错误，继续但日志
		existing = nil
	}

	// 3. 构建带上下文的 prompt
	prompt := r.buildReviewPrompt(history, existing)

	// 4. 调用 LLM
	resp, err := r.Provider.ChatCompletion(ctx, []llm.Message{
		{Role: "user", Content: prompt},
	})
	if err != nil {
		return nil, fmt.Errorf("llm chat completion: %w", err)
	}

	// 5. 解析结果
	result := r.parseReviewResponse(strings.TrimSpace(resp), existing)

	return result, nil
}

// buildReviewPrompt 构建带已有 skill 上下文的 prompt
func (r *ReflectionEngine) buildReviewPrompt(history TaskHistory, existing []ExistingSkill) string {
	var sb strings.Builder

	sb.WriteString("You are a skill review assistant. Analyze the task history and decide whether to create/update a reusable skill.\n\n")

	// 已有 skills 上下文（关键差异：LLM 知道已有什么）
	if len(existing) > 0 {
		sb.WriteString("## Existing Skills\n")
		sb.WriteString("The following skills already exist:\n")
		for _, s := range existing {
			if s.Description != "" {
				sb.WriteString(fmt.Sprintf("- %s: %s\n", s.Name, s.Description))
			} else {
				sb.WriteString(fmt.Sprintf("- %s\n", s.Name))
			}
		}
		sb.WriteString("\nIf a similar skill already exists, prefer UPDATE over CREATE.\n\n")
	} else {
		sb.WriteString("## Existing Skills\nNo skills exist yet.\n\n")
	}

	// 任务历史
	sb.WriteString("## Task History\n")
	sb.WriteString(fmt.Sprintf("Goal: %s\n", history.Goal))
	sb.WriteString(fmt.Sprintf("Steps (%d):\n", len(history.Steps)))
	for i, step := range history.Steps {
		// 截断过长的 step 避免 token 膨胀
		displayStep := step
		if len(displayStep) > 300 {
			displayStep = displayStep[:300] + "...(truncated)"
		}
		sb.WriteString(fmt.Sprintf("  %d. %s\n", i+1, displayStep))
	}
	sb.WriteString(fmt.Sprintf("Outcome: %s\n\n", history.Outcome))

	// 输出格式要求
	sb.WriteString(`## Decision
Based on the above, output ONLY one of:
1. SKIP - if nothing worth saving (most tasks should be SKIP)
2. CREATE|skill-name|content - if a genuinely new reusable skill is needed
3. UPDATE|skill-name|content - if updating an existing skill (include full updated content)

Rules:
- Most tasks should be SKIP — don't save trivial commands or one-off queries
- CREATE only for genuinely reusable workflows (5+ steps, multiple tools, error handling)
- UPDATE existing skills if they cover similar ground — avoid creating duplicates
- Skill content format:
---
name: skill-name
description: One line description
---
# What this does
...step by step instructions...
# Pitfalls
...common mistakes to avoid...
`)

	return sb.String()
}

// parseReviewResponse parses the LLM's response
func (r *ReflectionEngine) parseReviewResponse(resp string, existing []ExistingSkill) *ReviewResult {
	if resp == "" || strings.TrimSpace(resp) == "" {
		return &ReviewResult{Action: "skip", Reason: "empty response"}
	}

	lines := strings.Split(resp, "\n")
	firstLine := strings.TrimSpace(lines[0])

	// 格式: SKIP
	if strings.HasPrefix(firstLine, "SKIP") {
		return &ReviewResult{Action: "skip", Reason: "LLM decision"}
	}

	// 格式: CREATE|skill-name|content
	if strings.HasPrefix(firstLine, "CREATE|") {
		rest := strings.TrimPrefix(firstLine, "CREATE|")
		parts := strings.SplitN(rest, "|", 2)
		if len(parts) >= 2 {
			name := strings.TrimSpace(parts[0])
			content := strings.TrimSpace(parts[1])
			// 如果 content 太短，可能是多行格式
			if len(content) < 50 && len(lines) > 1 {
				content = strings.Join(lines[1:], "\n")
			}
			if name == "" {
				return &ReviewResult{Action: "skip", Reason: "empty skill name"}
			}
			return &ReviewResult{
				Action:    "create",
				SkillName: name,
				Content:   content,
				Reason:    "new reusable workflow",
			}
		}
	}

	// 格式: UPDATE|skill-name|content
	if strings.HasPrefix(firstLine, "UPDATE|") {
		rest := strings.TrimPrefix(firstLine, "UPDATE|")
		parts := strings.SplitN(rest, "|", 2)
		if len(parts) >= 2 {
			name := strings.TrimSpace(parts[0])
			content := strings.TrimSpace(parts[1])
			if len(content) < 50 && len(lines) > 1 {
				content = strings.Join(lines[1:], "\n")
			}
			// 验证目标 skill 是否存在
			if name != "" {
				return &ReviewResult{
					Action:    "update",
					SkillName: name,
					Content:   content,
					Reason:    "updating existing skill",
				}
			}
		}
	}

	// 兜底：如果 LLM 生成了完整的 SKILL.md 格式，尝试解析
	if strings.Contains(resp, "---") && strings.Contains(resp, "name:") {
		name := extractSkillName(resp)
		if name != "" {
			return &ReviewResult{
				Action:    "create",
				SkillName: name,
				Content:   resp,
				Reason:    "LLM generated skill content",
			}
		}
	}

	// 解析失败，skip
	return &ReviewResult{Action: "skip", Reason: fmt.Sprintf("parse failure: %s", firstLine)}
}

// ─────────────────────────────────────────
// 5. ArchiveSkill（带去重 + 安全扫描）
// ─────────────────────────────────────────

// ArchiveSkill writes a new or updated skill to disk
func (r *ReflectionEngine) ArchiveSkill(ctx context.Context, result *ReviewResult) error {
	if result == nil || result.Action == "skip" {
		return nil
	}

	skillDir := r.Config.SkillsDir
	if skillDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("get home dir: %w", err)
		}
		skillDir = filepath.Join(home, ".selfmind", "skills")
	}
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		return fmt.Errorf("create skill dir: %w", err)
	}

	// 安全扫描（P0）
	if err := ScanSkillForDangers(result.Content); err != nil {
		// 危险内容，不写入，发送通知
		if r.notifyCh != nil {
			select {
			case r.notifyCh <- fmt.Sprintf("⚠️ skill %s blocked: %v", result.SkillName, err):
			default:
			}
		}
		return fmt.Errorf("security scan failed: %w", err)
	}

	// 去重检查：确定目标文件路径
	existing, _ := r.scanExistingSkills()
	targetPath := ""

	for _, s := range existing {
		if s.Name == result.SkillName {
			targetPath = s.FilePath
			break
		}
	}

	if targetPath == "" {
		// 新建
		safeName := SanitizeSkillName(result.SkillName)
		targetPath = filepath.Join(skillDir, safeName+".md")
	}

	// 原子写入：先写临时文件，再 rename
	tmpPath := targetPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(result.Content), 0644); err != nil {
		return fmt.Errorf("write tmp file: %w", err)
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename file: %w", err)
	}

	// 发送通知
	action := "created"
	if result.Action == "update" {
		action = "updated"
	}
	if r.notifyCh != nil {
		select {
		case r.notifyCh <- fmt.Sprintf("💾 skill %s %s", result.SkillName, action):
		default:
		}
	}

	return nil
}

// scanForDangers performs security scan on skill content
// ScanSkillForDangers checks skill content for dangerous patterns (credentials,
// destructive commands, sensitive paths). It is exported so tools/skill_manage can reuse it.
func ScanSkillForDangers(content string) error {
	dangerousPatterns := []struct {
		pattern *regexp.Regexp
		msg     string
	}{
		// 凭证泄露
		{regexp.MustCompile(`curl\s+[^\n]*\$\{?\w*(KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL|API)`), "curl with secret variable"},
		{regexp.MustCompile(`wget\s+[^\n]*\$\{?\w*(KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL|API)`), "wget with secret variable"},
		{regexp.MustCompile(`\$\{?API_?KEY\}?`), "API key reference"},
		{regexp.MustCompile(`ghp_[A-Za-z0-9]{36}`), "GitHub token"},
		{regexp.MustCompile(`github_pat_[A-Za-z0-9_]{80,}`), "GitHub personal access token"},
		{regexp.MustCompile(`sk-[A-Za-z0-9]{20,}`), "OpenAI API key"},
		{regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{90,}`), "Anthropic API key"},
		{regexp.MustCompile(`AKIA[0-9A-Z]{16}`), "AWS access key"},

		// 危险命令
		{regexp.MustCompile(`rm\s+-rf\s+/`), "recursive delete from root"},
		{regexp.MustCompile(`rm\s+-rf\s+\$HOME|rm\s+-rf\s+~\/`), "delete home directory"},
		{regexp.MustCompile(`;\s*rm\s+|&\s*rm\s+|\|\s*rm\s+`), "command injection (rm)"},
		{regexp.MustCompile(`>\s*/etc/`), "write to /etc"},

		// 敏感路径
		{regexp.MustCompile(`\$HOME/\.ssh|~/.ssh`), "SSH directory access"},
		{regexp.MustCompile(`\$HOME/\.hermes/.*\.env|\.hermes/\.env`), "Hermes env file access"},
		{regexp.MustCompile(`\$HOME/\.aws|~/.aws`), "AWS credentials access"},
		{regexp.MustCompile(`\$HOME/\.docker/.*config|` + "`" + `.docker/config`), "Docker config access"},

		// 环境变量 dump
		{regexp.MustCompile(`printenv|env\s*\|`), "environment dump"},
		{regexp.MustCompile(`os\.environ|process\.env`), "programmatic env access"},
	}

	for _, p := range dangerousPatterns {
		if p.pattern.MatchString(content) {
			return fmt.Errorf("dangerous pattern detected: %s", p.msg)
		}
	}

	return nil
}

// SanitizeSkillName converts a skill name to a safe filename.
// Exported so tools/skill_manage can reuse it.
func SanitizeSkillName(name string) string {
	name = strings.ToLower(name)
	name = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		if r == '-' || r == '_' {
			return r
		}
		return '-' // 其他字符替换为连字符
	}, name)
	// 去除连续连字符
	for strings.Contains(name, "--") {
		name = strings.ReplaceAll(name, "--", "-")
	}
	name = strings.Trim(name, "-")
	if len(name) == 0 {
		name = "unnamed-skill"
	}
	return name
}

// ─────────────────────────────────────────
// 6. 旧接口兼容（供尚未迁移的调用方使用）
// ─────────────────────────────────────────

// ReflectLegacy 旧版 Reflect 接口，返回 (bool, string, error)
// 供尚未迁移的代码使用
func (r *ReflectionEngine) ReflectLegacy(ctx context.Context, history TaskHistory) (bool, string, error) {
	result, err := r.Reflect(ctx, history)
	if err != nil {
		return false, "", err
	}
	if result.Action == "skip" {
		return false, "", nil
	}
	return true, result.Content, nil
}

// ArchiveSkillLegacy 旧版 ArchiveSkill 接口，接收 content string
// 供尚未迁移的代码使用
func (r *ReflectionEngine) ArchiveSkillLegacy(content string) error {
	result := &ReviewResult{
		Action:  "create",
		Content: content,
	}
	// 提取 skill name
	result.SkillName = extractSkillName(content)
	if result.SkillName == "" {
		result.SkillName = fmt.Sprintf("auto-skill-%d", len(content)%10000)
	}
	return r.ArchiveSkill(context.Background(), result)
}

// ListSkills returns all skills for management purposes.
func (r *ReflectionEngine) ListSkills() ([]ExistingSkill, error) {
	return r.scanExistingSkills()
}

// DeleteSkill removes a skill by name. Returns error if not found or delete fails.
func (r *ReflectionEngine) DeleteSkill(name string) error {
	skills, err := r.scanExistingSkills()
	if err != nil {
		return fmt.Errorf("scan skills: %w", err)
	}

	for _, s := range skills {
		if s.Name == name {
			if err := os.Remove(s.FilePath); err != nil {
				return fmt.Errorf("remove %s: %w", s.FilePath, err)
			}
			// 如果是空目录也清理掉
			dir := filepath.Dir(s.FilePath)
			entries, _ := os.ReadDir(dir)
			if len(entries) == 0 {
				os.Remove(dir)
			}
			return nil
		}
	}
	return fmt.Errorf("skill not found: %s", name)
}
