package kernel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"selfmind/internal/kernel/llm"
)

// TaskHistory represents a completed interaction or tool-calling session
type TaskHistory struct {
	Goal    string
	Context string
	Steps   []string
	Outcome string
}

// EvolutionConfig defines the agent's autonomous learning behavior.
type EvolutionConfig struct {
	Enabled               bool    `mapstructure:"enabled"`
	Mode                  string  `mapstructure:"mode"`
	MinComplexityThreshold int     `mapstructure:"min_complexity_threshold"`
	AutoArchiveConfidence float64 `mapstructure:"auto_archive_confidence"`
}

// ReflectionEngine handles the autonomous reflection and skill generation logic.
type ReflectionEngine struct {
	Provider llm.Provider
	Config   EvolutionConfig
}

// Reflect analyzes task history and determines if a new skill should be created.
func (r *ReflectionEngine) Reflect(ctx context.Context, history TaskHistory) (bool, string, error) {
	if !r.Config.Enabled {
		return false, "", nil
	}

	// 1. 复杂度检查
	if len(history.Steps) < r.Config.MinComplexityThreshold {
		return false, "", nil
	}

	// 2. 调用 LLM 反思
	prompt := fmt.Sprintf(`
Analyze the following task history. Is this a reusable workflow (e.g. system administration, batch file processing, data extraction)? 
If yes, generate a SKILL.md. If no, just return "SKIP".

Task Goal: %s
History: %s
Outcome: %s
`, history.Goal, strings.Join(history.Steps, " -> "), history.Outcome)

	resp, err := r.Provider.ChatCompletion(ctx, []llm.Message{{Role: "user", Content: prompt}})
	if err != nil {
		return false, "", err
	}

	content := strings.TrimSpace(resp)
	if content == "SKIP" || content == "" {
		return false, "", nil
	}

	return true, content, nil
}

// ArchiveSkill autonomously writes a new skill file.
// It parses the front matter to extract the skill name for the filename.
func (r *ReflectionEngine) ArchiveSkill(content string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home dir: %w", err)
	}

	skillDir := filepath.Join(home, ".selfmind", "skills")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		return fmt.Errorf("failed to create skill directory: %w", err)
	}

	// Extract name from front matter for a meaningful filename
	name := extractSkillName(content)
	if name == "" {
		name = fmt.Sprintf("auto-skill-%d", time.Now().Unix())
	}
	// Sanitize filename
	name = strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, name)

	path := filepath.Join(skillDir, name+".md")
	fmt.Printf("[Reflector] Archiving skill to: %s\n", path)
	return os.WriteFile(path, []byte(content), 0644)
}

// extractSkillName pulls the name field from YAML front matter
func extractSkillName(content string) string {
	lines := strings.Split(content, "\n")
	inFrontMatter := false
	for _, line := range lines {
		if strings.TrimSpace(line) == "---" {
			if !inFrontMatter {
				inFrontMatter = true
				continue
			}
			break // end of front matter
		}
		if inFrontMatter && strings.HasPrefix(line, "name:") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}
