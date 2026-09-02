package eval

import (
	"bytes"
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
	// ModelRequired distinguishes agent-backed scenarios from deterministic
	// gateway/control-plane scenarios. It defaults to true. A false value lets
	// selfcheck execute the case strictly offline without a VCR cassette; if the
	// message path unexpectedly reaches the model, replay still fails closed.
	ModelRequired *bool `yaml:"model_required,omitempty" json:"model_required,omitempty"`
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
	// Slow marks a case whose MEASURED replay cost is high enough to keep it
	// out of the fast local loop (`selfmind selfcheck --fast`). It is set from
	// a measurement, never a guess: max_duration_seconds is an author-chosen
	// ceiling and a poor proxy — continuity_resume declares 420s and replays in
	// one. The full gate always runs every case; --fast is for the loop you run
	// after each change, and the count it skips is always reported.
	Slow       bool          `yaml:"slow" json:"slow,omitempty"`
	SharedData bool          `yaml:"shared_data" json:"shared_data,omitempty"`
	CI         CISettings    `yaml:"ci,omitempty" json:"ci,omitempty"`
	Requires   Requirements  `yaml:"requires,omitempty" json:"requires,omitempty"`
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

type CISettings struct {
	// Required marks a case as complementary CI evidence. It does not make the
	// case mandatory in the local fast/full profiles; RequireCassette owns that
	// independent concern.
	Required  bool     `yaml:"required" json:"required,omitempty"`
	Reason    string   `yaml:"reason" json:"reason,omitempty"`
	Platforms []string `yaml:"platforms" json:"platforms,omitempty"`
}

type Requirements struct {
	// Commands lists host tools used by the replayed tool calls. Selfcheck
	// validates these before starting a case so a missing or incompatible host
	// tool is reported as an environment error, not a product regression.
	Commands []string `yaml:"commands" json:"commands,omitempty"`
}

var validCIReasons = map[string]struct{}{
	"clean_checkout": {},
	"cross_platform": {},
	"credentialless": {},
	"concurrency":    {},
	"timing":         {},
}

var validCIPlatforms = map[string]struct{}{
	"linux":  {},
	"darwin": {},
}

type Turn struct {
	Input           string   `yaml:"input" json:"input"`
	Channel         string   `yaml:"channel" json:"channel,omitempty"`
	AdditionalRoots []string `yaml:"additional_roots,omitempty" json:"additional_roots,omitempty"`
	// PlatformUserID overrides the eval identity for this turn only. It lets a
	// case simulate a *different* platform user (a "stranger") mid-case to assert
	// identity isolation: person-scoped task/run state must not leak across
	// accounts. Empty means the case's default eval identity.
	PlatformUserID string `yaml:"platform_user_id" json:"platform_user_id,omitempty"`
	// ReplyToTurn marks this turn as a platform reply to the run started by an
	// EARLIER turn (1-based turn number). The runner resolves it to that turn's
	// actual run id and sends it as MessageRequest.ReplyToRunID — the
	// structured reply edge cross-endpoint cases assert. 0 means no reply
	// metadata.
	ReplyToTurn int    `yaml:"reply_to_turn" json:"reply_to_turn,omitempty"`
	ApprovalID  string `yaml:"approval_id,omitempty" json:"approval_id,omitempty"`
	ClarifyID   string `yaml:"clarify_id,omitempty" json:"clarify_id,omitempty"`
	// WaitForMaintenance runs one immediately-due post-run maintenance pass
	// before the next turn. It is opt-in because wiring maintenance into every
	// historical case would change the model-call cassette contract. Use it for
	// scenarios whose next turn must observe asynchronous preference intake.
	WaitForMaintenance bool `yaml:"wait_for_maintenance,omitempty" json:"wait_for_maintenance,omitempty"`
}

type Expectations struct {
	Status              string   `yaml:"status" json:"status,omitempty"`
	HTTPStatus          int      `yaml:"http_status" json:"http_status,omitempty"`
	CompletionReason    string   `yaml:"completion_reason" json:"completion_reason,omitempty"`
	Resumable           *bool    `yaml:"resumable" json:"resumable,omitempty"`
	VerificationState   string   `yaml:"verification_state" json:"verification_state,omitempty"`
	Contains            []string `yaml:"contains" json:"contains,omitempty"`
	MustNotContain      []string `yaml:"must_not_contain" json:"must_not_contain,omitempty"`
	MaxDurationSeconds  int      `yaml:"max_duration_seconds" json:"max_duration_seconds,omitempty"`
	MaxToolErrors       *int     `yaml:"max_tool_errors" json:"max_tool_errors,omitempty"`
	MaxToolCalls        *int     `yaml:"max_tool_calls" json:"max_tool_calls,omitempty"`
	RequireToolEvents   bool     `yaml:"require_tool_events" json:"require_tool_events,omitempty"`
	MinToolCalls        int      `yaml:"min_tool_calls" json:"min_tool_calls,omitempty"`
	MinProgressUpdates  int      `yaml:"min_progress_updates" json:"min_progress_updates,omitempty"`
	RequireSameTask     bool     `yaml:"require_same_task" json:"require_same_task,omitempty"`
	RequireContinuation bool     `yaml:"require_continuation" json:"require_continuation,omitempty"`
	RequireNoTask       bool     `yaml:"require_no_task" json:"require_no_task,omitempty"`
	RequireNoRun        bool     `yaml:"require_no_run" json:"require_no_run,omitempty"`
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
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&c); err != nil {
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
		for j := range c.Turns[i].AdditionalRoots {
			c.Turns[i].AdditionalRoots[j] = strings.TrimSpace(c.Turns[i].AdditionalRoots[j])
		}
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
	for i, turn := range c.Turns {
		if turn.ReplyToTurn < 0 || turn.ReplyToTurn > i {
			return fmt.Errorf("turn %d: reply_to_turn must name an earlier turn (1..%d)", i+1, i)
		}
	}
	if !c.RequiresModel() && c.RequireCassette {
		return fmt.Errorf("model_required: false cannot be combined with require_cassette: true")
	}
	// shared_data reuses the configured control.db, which contradicts scenarios
	// that seed or assert a fresh world; reject the combination early so a case
	// author gets a clear error instead of surprising cross-run state bleed.
	if c.SharedData && needsWorkspaceIsolation(c) {
		return fmt.Errorf("shared_data cannot be combined with setup, assert_state, or workspace: isolated")
	}
	if err := c.normalizeCI(); err != nil {
		return err
	}
	if err := c.normalizeRequirements(); err != nil {
		return err
	}
	if !c.Checks.NoMojibake && !c.Checks.NoRawJSONLeak && !c.Checks.NoToolXMLLeak &&
		!c.Checks.NoEmptyResponse && !c.Checks.NoProviderStackDump &&
		!c.Checks.ToolFailureShouldRecover && !c.Checks.WorkspaceShouldMatch &&
		!c.Checks.ContextNotExceeded {
		c.Checks = DefaultCheckSettings()
	}
	return nil
}

// RequiresModel reports whether the case is expected to enter the agent/model
// path. The default is deliberately true so a missing annotation cannot make a
// provider-backed regression silently pass without replay evidence.
func (c *Case) RequiresModel() bool {
	return c == nil || c.ModelRequired == nil || *c.ModelRequired
}

func (c *Case) normalizeCI() error {
	c.CI.Reason = strings.ToLower(strings.TrimSpace(c.CI.Reason))
	if !c.CI.Required {
		if c.CI.Reason != "" || len(c.CI.Platforms) > 0 {
			return fmt.Errorf("ci.reason/ci.platforms require ci.required: true")
		}
		return nil
	}
	if _, ok := validCIReasons[c.CI.Reason]; !ok {
		return fmt.Errorf("ci.required cases need ci.reason in clean_checkout, cross_platform, credentialless, concurrency, timing")
	}
	if len(c.CI.Platforms) == 0 {
		return fmt.Errorf("ci.required cases need at least one ci.platforms entry")
	}
	seen := make(map[string]struct{}, len(c.CI.Platforms))
	platforms := make([]string, 0, len(c.CI.Platforms))
	for _, raw := range c.CI.Platforms {
		platform := strings.ToLower(strings.TrimSpace(raw))
		if platform == "macos" {
			platform = "darwin"
		}
		if _, ok := validCIPlatforms[platform]; !ok {
			return fmt.Errorf("unsupported ci platform %q (expected linux or darwin)", raw)
		}
		if _, ok := seen[platform]; ok {
			continue
		}
		seen[platform] = struct{}{}
		platforms = append(platforms, platform)
	}
	c.CI.Platforms = platforms
	return nil
}

func (c *Case) normalizeRequirements() error {
	seen := make(map[string]struct{}, len(c.Requires.Commands))
	commands := make([]string, 0, len(c.Requires.Commands))
	for _, raw := range c.Requires.Commands {
		command := strings.TrimSpace(raw)
		if command == "" {
			return fmt.Errorf("requires.commands cannot contain an empty command")
		}
		if filepath.Base(command) != command {
			return fmt.Errorf("requires.commands entry %q must be a command name, not a path", raw)
		}
		key := strings.ToLower(command)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		commands = append(commands, command)
	}
	c.Requires.Commands = commands
	return nil
}

// RequiredOnCI reports whether this case belongs to the complementary CI gate
// on goos. Local profiles intentionally ignore this ownership marker.
func (c *Case) RequiredOnCI(goos string) bool {
	if c == nil || !c.CI.Required {
		return false
	}
	goos = strings.ToLower(strings.TrimSpace(goos))
	for _, platform := range c.CI.Platforms {
		if platform == goos {
			return true
		}
	}
	return false
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
