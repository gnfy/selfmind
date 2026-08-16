package eval

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/tools"
)

// Setup describes the initial world a scenario runs against: seed files in the
// isolated workspace, seed memory facts, and an optional pre-created current
// task. All of it is applied before the first turn.
type Setup struct {
	Files  map[string]string `yaml:"files,omitempty" json:"files,omitempty"`
	Memory []SeedFact        `yaml:"memory,omitempty" json:"memory,omitempty"`
	Task   *SeedTask         `yaml:"task,omitempty" json:"task,omitempty"`
}

type SeedFact struct {
	Target    string `yaml:"target" json:"target"`
	Content   string `yaml:"content" json:"content"`
	Scope     string `yaml:"scope,omitempty" json:"scope,omitempty"`
	Category  string `yaml:"category,omitempty" json:"category,omitempty"`
	Canonical bool   `yaml:"canonical,omitempty" json:"canonical,omitempty"`
}

type SeedTask struct {
	Title        string `yaml:"title" json:"title,omitempty"`
	Status       string `yaml:"status" json:"status,omitempty"`
	DefaultSkill string `yaml:"default_skill,omitempty" json:"default_skill,omitempty"`
}

// StatePredicate is a single assertion over the world state after a scenario
// runs. It is intentionally a flat struct: the evaluator dispatches on On and
// applies whichever operators are set. This keeps YAML readable and Go parsing
// trivial.
type StatePredicate struct {
	On     string `yaml:"on" json:"on"`                           // task|handoff|events|artifact|approval|run|file|memory
	Field  string `yaml:"field,omitempty" json:"field,omitempty"` // task/handoff field
	Path   string `yaml:"path,omitempty" json:"path,omitempty"`   // file
	Target string `yaml:"target,omitempty" json:"target,omitempty"`
	Type   string `yaml:"type,omitempty" json:"type,omitempty"`     // events
	Status string `yaml:"status,omitempty" json:"status,omitempty"` // approval

	Eq              *string `yaml:"eq,omitempty" json:"eq,omitempty"`
	Contains        *string `yaml:"contains,omitempty" json:"contains,omitempty"`
	NotContains     *string `yaml:"not_contains,omitempty" json:"not_contains,omitempty"`
	Matches         *string `yaml:"matches,omitempty" json:"matches,omitempty"`
	PayloadContains *string `yaml:"payload_contains,omitempty" json:"payload_contains,omitempty"`
	Exists          *bool   `yaml:"exists,omitempty" json:"exists,omitempty"`
	Empty           *bool   `yaml:"empty,omitempty" json:"empty,omitempty"`
	LenGte          *int    `yaml:"len_gte,omitempty" json:"len_gte,omitempty"`
	LenLte          *int    `yaml:"len_lte,omitempty" json:"len_lte,omitempty"`
	CountGte        *int    `yaml:"count_gte,omitempty" json:"count_gte,omitempty"`
	CountLte        *int    `yaml:"count_lte,omitempty" json:"count_lte,omitempty"`
	MinBytes        *int    `yaml:"min_bytes,omitempty" json:"min_bytes,omitempty"`
}

// needsWorkspaceIsolation reports whether a case must run in a fresh temp
// WORKSPACE (seeded from setup). Data-dir isolation is no longer tied to this:
// every case gets a throwaway data dir by default (see runSingle), so this only
// decides whether tool calls should hit a seeded scratch workspace instead of
// the case's declared workspace path. Scenarios that seed or assert world
// state always need the scratch workspace so they neither write into the repo
// nor read stale files.
func needsWorkspaceIsolation(c *Case) bool {
	if c == nil {
		return false
	}
	if c.Setup != nil || len(c.AssertState) > 0 {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(c.Workspace), "isolated")
}

// applyFileSeeds writes setup files into the (already created) workspace root,
// refusing any path that escapes the root.
func applyFileSeeds(workspaceRoot string, files map[string]string) error {
	for rel, content := range files {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			continue
		}
		clean := filepath.Clean(rel)
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
			return fmt.Errorf("setup file path escapes workspace: %s", rel)
		}
		dst := filepath.Join(workspaceRoot, clean)
		if relCheck, err := filepath.Rel(workspaceRoot, dst); err != nil || strings.HasPrefix(relCheck, "..") {
			return fmt.Errorf("setup file path escapes workspace: %s", rel)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// applyStateSeeds seeds memory facts and an optional current task before the
// first turn. Files are handled separately (they must land before the harness
// starts using the workspace).
func applyStateSeeds(ctx context.Context, store *control.Store, mem *memory.MemoryManager, identity *control.IdentityContext, workspaceID, workspaceRoot string, setup *Setup) error {
	if setup == nil || identity == nil {
		return nil
	}
	for _, f := range setup.Memory {
		target := strings.TrimSpace(f.Target)
		if target == "" {
			target = "user"
		}
		content := strings.TrimSpace(f.Content)
		if mem != nil && content != "" && f.Canonical {
			canonical, ok := mem.Canonical()
			if !ok {
				return fmt.Errorf("seed canonical memory: provider has no canonical store")
			}
			partition := strings.TrimSpace(identity.PersonID)
			if partition == "" {
				partition = strings.TrimSpace(identity.TenantID)
			}
			scope := strings.TrimSpace(f.Scope)
			if scope == "" {
				scope = "global"
			}
			if err := canonical.ApplyIntakeWrite(ctx, partition, memory.IntakeWrite{
				Decision:    "ADD",
				DecisionKey: "eval-seed:" + memory.NormalizedContentHash(content),
				Target:      target,
				Scope:       scope,
				Source:      memory.SourceUser,
				Content:     content,
				Category:    strings.TrimSpace(f.Category),
				Confidence:  1,
			}); err != nil {
				return err
			}
			continue
		}
		if mem != nil && content != "" {
			if err := mem.AddFact(ctx, identity.TenantID, target, f.Content); err != nil {
				return err
			}
		}
	}
	if setup.Task != nil && store != nil {
		task, err := store.CreateTask(ctx, control.TaskCreate{
			TenantID:    identity.TenantID,
			PersonID:    identity.PersonID,
			WorkspaceID: workspaceID,
			Title:       firstNonEmpty(setup.Task.Title, "seeded task"),
			Channel:     "eval",
		})
		if err != nil {
			return err
		}
		if err := store.SetCurrentTask(ctx, identity.TenantID, identity.PersonID, task.ID); err != nil {
			return err
		}
		if skillName := kernel.SanitizeSkillName(setup.Task.DefaultSkill); skillName != "" {
			skillsRoot := filepath.Join(workspaceRoot, ".selfmind", "skills")
			skillPath := filepath.Join(skillsRoot, skillName, "SKILL.md")
			content, err := os.ReadFile(skillPath)
			if err != nil {
				return fmt.Errorf("seed default Skill %s: %w", skillName, err)
			}
			digest := sha256.Sum256(content)
			_, err = store.BindTaskSkill(ctx, control.BindTaskSkillInput{
				IdentityTenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
				ControlTenantID: identity.TenantID, WorkspaceID: workspaceID,
				SkillKey: control.SkillKey(identity.TenantID, skillName, tools.SkillScopeWorkspace,
					"workspace", skillsRoot, filepath.Join(skillName, "SKILL.md")),
				SkillName: skillName, BindingSource: "eval_setup",
				VersionHash: fmt.Sprintf("%x", digest[:]),
			})
			if err != nil {
				return fmt.Errorf("bind seeded default Skill %s: %w", skillName, err)
			}
		}
	}
	return nil
}
