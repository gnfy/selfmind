package eval

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	Skills []SeedSkill       `yaml:"skills,omitempty" json:"skills,omitempty"`
	Task   *SeedTask         `yaml:"task,omitempty" json:"task,omitempty"`
	// Deliveries seeds outbound results already parked on a stale IM session.
	// Declaring any also gives the case a delivery service, which cases without
	// it deliberately do not get: a delivery surface changes which notification
	// paths a run takes, and that must stay opt-in.
	Deliveries []SeedDelivery `yaml:"deliveries,omitempty" json:"deliveries,omitempty"`
}

// SeedSkill creates a managed user Skill before the first turn. It exists so
// message-path regressions can exercise an actual agent-created asset instead
// of silently proving only workspace-file discovery.
type SeedSkill struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description"`
	Content     string `yaml:"content" json:"content"`
}

// SeedDelivery is one undeliverable final result that has been waiting for a
// fresh IM session. AgeHours reproduces an accumulated backlog: the automatic
// catch-up window only covers recent messages, so age is what separates work
// the daemon retries by itself from work that needs explicit recovery.
type SeedDelivery struct {
	Content        string `yaml:"content" json:"content"`
	AgeHours       int    `yaml:"age_hours,omitempty" json:"age_hours,omitempty"`
	TaskID         string `yaml:"task_id,omitempty" json:"task_id,omitempty"`
	RunID          string `yaml:"run_id,omitempty" json:"run_id,omitempty"`
	Reason         string `yaml:"reason,omitempty" json:"reason,omitempty"`
	PlatformUserID string `yaml:"platform_user_id,omitempty" json:"platform_user_id,omitempty"`
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
	// ParkedRuns seeds unclaimed resumable runs on the task before the first
	// turn, so cases can exercise the deterministic continuation ladder
	// (unique parent claim, cross-task candidates) through the production
	// message path without a live model producing the waiting state first.
	ParkedRuns []SeedParkedRun `yaml:"parked_runs,omitempty" json:"parked_runs,omitempty"`
}

// SeedParkedRun is one pre-existing run parked in a resumable status.
type SeedParkedRun struct {
	Input string `yaml:"input" json:"input"`
	// Status must be one of the resumable statuses (interrupted, waiting_user,
	// verification_partial, blocked); empty defaults to waiting_user.
	Status string `yaml:"status,omitempty" json:"status,omitempty"`
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

// applySkillSeeds runs before app tool registration, so an eval fixture proves
// both hidden compatibility registration and provider-catalog exclusion.
func applySkillSeeds(tenantID, skillsBaseDir string, seeds []SeedSkill) error {
	if len(seeds) == 0 {
		return nil
	}
	storage, err := tools.NewSkillStorage(skillsBaseDir)
	if err != nil {
		return fmt.Errorf("seed agent-created Skills: %w", err)
	}
	root := tools.SkillsDirForTenant(storage.BaseDir(), tenantID)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("seed agent-created Skills: %w", err)
	}
	invocation := tools.WithSkillStorage(map[string]interface{}{"_tenant_id": tenantID}, storage)
	for index, seed := range seeds {
		name := kernel.SanitizeSkillName(seed.Name)
		description := strings.TrimSpace(seed.Description)
		body := strings.TrimSpace(seed.Content)
		if name == "" || description == "" || body == "" || strings.ContainsAny(description, "\r\n") {
			return fmt.Errorf("seed agent-created Skill %d requires a safe name, one-line description, and content", index)
		}
		content := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n%s\n", name, description, body)
		if err := tools.ValidateManagedSkillDescription(content); err != nil {
			return fmt.Errorf("seed agent-created Skill %s: %w", name, err)
		}
		dir := filepath.Join(root, name)
		if _, err := os.Stat(dir); err == nil {
			return fmt.Errorf("seed agent-created Skill %s already exists", name)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("seed agent-created Skill %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
			return fmt.Errorf("seed agent-created Skill %s: %w", name, err)
		}
		if err := tools.MarkSkillCreated(tenantID, name, tools.SkillSourceAgentCreated, "eval_setup", invocation); err != nil {
			return fmt.Errorf("seed agent-created Skill %s metadata: %w", name, err)
		}
	}
	return nil
}

// applyStateSeeds seeds memory facts and an optional current task before the
// first turn. Files are handled separately (they must land before the harness
// starts using the workspace).
func applyStateSeeds(ctx context.Context, store *control.Store, mem *memory.MemoryManager, identity *control.IdentityContext, workspaceID, workspaceRoot, channel string, setup *Setup) error {
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
			partition := strings.TrimSpace(identity.PersonID)
			if partition == "" {
				partition = identity.TenantID
			}
			if err := mem.AddFact(ctx, partition, target, f.Content); err != nil {
				return err
			}
		}
	}
	for i, seed := range setup.Deliveries {
		if store == nil {
			break
		}
		content := strings.TrimSpace(seed.Content)
		if content == "" {
			return fmt.Errorf("delivery seed %d has no content", i)
		}
		age := time.Duration(seed.AgeHours) * time.Hour
		if seed.AgeHours <= 0 {
			// Default well past any automatic catch-up window so the seed lands
			// in the state that needs explicit recovery.
			age = 30 * 24 * time.Hour
		}
		if _, err := store.SeedAgedPendingSessionDelivery(ctx, control.Delivery{
			TenantID:       identity.TenantID,
			PersonID:       identity.PersonID,
			Platform:       identity.Platform,
			PlatformUserID: firstNonEmpty(strings.TrimSpace(seed.PlatformUserID), identity.PlatformUserID),
			Channel:        channel,
			TaskID:         strings.TrimSpace(seed.TaskID),
			RunID:          strings.TrimSpace(seed.RunID),
			Content:        content,
			Kind:           "final_result",
		}, age, firstNonEmpty(strings.TrimSpace(seed.Reason), "seeded stale IM session")); err != nil {
			return fmt.Errorf("seed delivery %d: %w", i, err)
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
		for i, parked := range setup.Task.ParkedRuns {
			run, err := store.StartRun(ctx, task, channel, strings.TrimSpace(parked.Input))
			if err != nil {
				return fmt.Errorf("seed parked run %d: %w", i, err)
			}
			status := strings.TrimSpace(parked.Status)
			if status == "" {
				status = "waiting_user"
			}
			switch status {
			case "interrupted", "waiting_user", "verification_partial", "blocked":
			default:
				return fmt.Errorf("seed parked run %d: status %q is not resumable", i, status)
			}
			if err := store.FinishRun(ctx, identity.TenantID, run.ID, status); err != nil {
				return fmt.Errorf("park seeded run %d: %w", i, err)
			}
			if err := store.UpdateTaskStatus(ctx, identity.TenantID, task.ID, status, "", nil); err != nil {
				return fmt.Errorf("seed parked run %d task status: %w", i, err)
			}
		}
		if skillName := kernel.SanitizeSkillName(setup.Task.DefaultSkill); strings.TrimSpace(setup.Task.DefaultSkill) != "" && skillName != "" {
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
