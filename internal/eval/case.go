package eval

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

type Case struct {
	ID            string `yaml:"id" json:"id"`
	Title         string `yaml:"title" json:"title,omitempty"`
	Suite         string `yaml:"suite" json:"suite,omitempty"`
	Workspace     string `yaml:"workspace" json:"workspace,omitempty"`
	Provider      string `yaml:"provider" json:"provider,omitempty"`
	Model         string `yaml:"model" json:"model,omitempty"`
	Channel       string `yaml:"channel" json:"channel,omitempty"`
	RecordContent bool   `yaml:"record_content" json:"record_content,omitempty"`
	// RequireCassette marks a case as mandatory for the offline gate: selfcheck
	// must FAIL (not skip) when no recorded cassette exists for the case ID.
	// Use it for north-star scenarios that must never silently drop out of CI.
	RequireCassette bool `yaml:"require_cassette" json:"require_cassette,omitempty"`
	// SharedData opts a case OUT of the default data-dir isolation, running it
	// against the configured data dir (real control.db + memory). Almost no case
	// should set this: each eval run creates its own eval-<id> identity, so
	// there is no shared state to inherit — only pollution to leave behind. It
	// cannot be combined with setup/assert_state/workspace:isolated, which
	// require a fresh world.
	SharedData bool          `yaml:"shared_data" json:"shared_data,omitempty"`
	Turns      []Turn        `yaml:"turns" json:"turns"`
	Expect     Expectations  `yaml:"expect" json:"expect,omitempty"`
	Checks     CheckSettings `yaml:"checks" json:"checks,omitempty"`

	// State-oracle additions (v1): initial world, world-state assertions, and
	// sampling controls for non-deterministic real-model runs.
	Setup       *Setup           `yaml:"setup,omitempty" json:"setup,omitempty"`
	AssertState []StatePredicate `yaml:"assert_state,omitempty" json:"assert_state,omitempty"`
	Tier        string           `yaml:"tier,omitempty" json:"tier,omitempty"`
	Repeat      int              `yaml:"repeat,omitempty" json:"repeat,omitempty"`
	PassRate    float64          `yaml:"pass_rate,omitempty" json:"pass_rate,omitempty"`

	path string
}

type Turn struct {
	Input   string `yaml:"input" json:"input"`
	Channel string `yaml:"channel" json:"channel,omitempty"`
	// PlatformUserID overrides the eval identity for this turn only. It lets a
	// case simulate a *different* platform user (a "stranger") mid-case to assert
	// identity isolation: person-scoped task/run state must not leak across
	// accounts. Empty means the case's default eval identity.
	PlatformUserID string `yaml:"platform_user_id" json:"platform_user_id,omitempty"`
}

type Expectations struct {
	Status              string   `yaml:"status" json:"status,omitempty"`
	CompletionReason    string   `yaml:"completion_reason" json:"completion_reason,omitempty"`
	Resumable           *bool    `yaml:"resumable" json:"resumable,omitempty"`
	VerificationState   string   `yaml:"verification_state" json:"verification_state,omitempty"`
	Contains            []string `yaml:"contains" json:"contains,omitempty"`
	MustNotContain      []string `yaml:"must_not_contain" json:"must_not_contain,omitempty"`
	MaxDurationSeconds  int      `yaml:"max_duration_seconds" json:"max_duration_seconds,omitempty"`
	MaxToolErrors       int      `yaml:"max_tool_errors" json:"max_tool_errors,omitempty"`
	MaxToolCalls        *int     `yaml:"max_tool_calls" json:"max_tool_calls,omitempty"`
	RequireToolEvents   bool     `yaml:"require_tool_events" json:"require_tool_events,omitempty"`
	MinToolCalls        int      `yaml:"min_tool_calls" json:"min_tool_calls,omitempty"`
	RequireSameTask     bool     `yaml:"require_same_task" json:"require_same_task,omitempty"`
	RequireContinuation bool     `yaml:"require_continuation" json:"require_continuation,omitempty"`
	// RequireTaskSwitch is the inverse of RequireSameTask: a multi-turn case
	// must use MORE than one task ID. It asserts the task-attach semantics —
	// a new request without continuation evidence creates its own task
	// instead of attaching to the parked current one.
	RequireTaskSwitch       bool     `yaml:"require_task_switch" json:"require_task_switch,omitempty"`
	RequireWorkspaceMatch   bool     `yaml:"require_workspace_match" json:"require_workspace_match,omitempty"`
	AllowedErrorCategories  []string `yaml:"allowed_error_categories" json:"allowed_error_categories,omitempty"`
	DisallowedErrorCategory []string `yaml:"disallowed_error_category" json:"disallowed_error_category,omitempty"`
}

type CheckSettings struct {
	NoMojibake               bool `yaml:"no_mojibake" json:"no_mojibake,omitempty"`
	NoRawJSONLeak            bool `yaml:"no_raw_json_leak" json:"no_raw_json_leak,omitempty"`
	NoToolXMLLeak            bool `yaml:"no_tool_xml_leak" json:"no_tool_xml_leak,omitempty"`
	NoEmptyResponse          bool `yaml:"no_empty_response" json:"no_empty_response,omitempty"`
	NoProviderStackDump      bool `yaml:"no_provider_stack_dump" json:"no_provider_stack_dump,omitempty"`
	ToolFailureShouldRecover bool `yaml:"tool_failure_should_recover" json:"tool_failure_should_recover,omitempty"`
	WorkspaceShouldMatch     bool `yaml:"workspace_should_match" json:"workspace_should_match,omitempty"`
	ContextNotExceeded       bool `yaml:"context_not_exceeded" json:"context_not_exceeded,omitempty"`
}

func LoadCase(path string) (*Case, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("case path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Case
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse case %s: %w", path, err)
	}
	c.path = path
	if err := c.normalize(); err != nil {
		return nil, fmt.Errorf("invalid case %s: %w", path, err)
	}
	return &c, nil
}

func (c *Case) normalize() error {
	c.ID = strings.TrimSpace(c.ID)
	if c.ID == "" {
		return fmt.Errorf("id is required")
	}
	if strings.TrimSpace(c.Title) == "" {
		c.Title = c.ID
	}
	if err := c.validateTextEncoding(); err != nil {
		return err
	}
	if strings.TrimSpace(c.Channel) == "" {
		c.Channel = "cli"
	}
	for i := range c.Turns {
		c.Turns[i].Input = strings.TrimSpace(c.Turns[i].Input)
		c.Turns[i].Channel = strings.TrimSpace(c.Turns[i].Channel)
		c.Turns[i].PlatformUserID = strings.TrimSpace(c.Turns[i].PlatformUserID)
	}
	filtered := c.Turns[:0]
	for _, t := range c.Turns {
		if t.Input != "" {
			filtered = append(filtered, t)
		}
	}
	c.Turns = filtered
	if len(c.Turns) == 0 {
		return fmt.Errorf("at least one turn is required")
	}
	// shared_data reuses the configured control.db, which contradicts scenarios
	// that seed or assert a fresh world; reject the combination early so a case
	// author gets a clear error instead of surprising cross-run state bleed.
	if c.SharedData && needsWorkspaceIsolation(c) {
		return fmt.Errorf("shared_data cannot be combined with setup, assert_state, or workspace: isolated")
	}
	if !c.Checks.NoMojibake && !c.Checks.NoRawJSONLeak && !c.Checks.NoToolXMLLeak &&
		!c.Checks.NoEmptyResponse && !c.Checks.NoProviderStackDump &&
		!c.Checks.ToolFailureShouldRecover && !c.Checks.WorkspaceShouldMatch &&
		!c.Checks.ContextNotExceeded {
		c.Checks = DefaultCheckSettings()
	}
	return nil
}

func (c *Case) validateTextEncoding() error {
	check := func(field, value string) error {
		if hasMojibake(value) {
			return fmt.Errorf("%s appears to contain mojibake; save eval fixtures as UTF-8 and replace garbled text", field)
		}
		return nil
	}
	if err := check("title", c.Title); err != nil {
		return err
	}
	for i, turn := range c.Turns {
		if err := check(fmt.Sprintf("turns[%d].input", i), turn.Input); err != nil {
			return err
		}
	}
	for i, value := range c.Expect.Contains {
		if err := check(fmt.Sprintf("expect.contains[%d]", i), value); err != nil {
			return err
		}
	}
	for i, value := range c.Expect.MustNotContain {
		if err := check(fmt.Sprintf("expect.must_not_contain[%d]", i), value); err != nil {
			return err
		}
	}
	return nil
}

func DefaultCheckSettings() CheckSettings {
	return CheckSettings{
		NoMojibake:               true,
		NoRawJSONLeak:            true,
		NoToolXMLLeak:            true,
		NoEmptyResponse:          true,
		NoProviderStackDump:      true,
		ToolFailureShouldRecover: true,
		ContextNotExceeded:       true,
	}
}

func (c *Case) Path() string {
	if c == nil {
		return ""
	}
	return c.path
}

func ListCaseFiles(root string) ([]string, error) {
	if strings.TrimSpace(root) == "" {
		root = "evalcases"
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{root}, nil
	}
	var out []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if strings.HasPrefix(name, ".") || name == "node_modules" || name == "dist" {
				if path != root {
					return filepath.SkipDir
				}
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext == ".yaml" || ext == ".yml" {
			out = append(out, path)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}
