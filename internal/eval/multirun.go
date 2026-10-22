package eval

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"selfmind/internal/platform/config"
)

var endpointNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// validEndpointName accepts platform and MCP server names: lowercase letters,
// digits, '_' and '-'.
func validEndpointName(name string) bool {
	return endpointNamePattern.MatchString(name)
}

// normalizeMultiRun validates the runtime block, the endpoint and concurrency
// turn fields, and the endpoint seeds.
func (c *Case) normalizeMultiRun() error {
	if err := c.normalizeRuntime(); err != nil {
		return err
	}
	for i := range c.Turns {
		turn := &c.Turns[i]
		turn.Platform = strings.ToLower(strings.TrimSpace(turn.Platform))
		turn.CWD = strings.TrimSpace(turn.CWD)
		if turn.Platform != "" && !validEndpointName(turn.Platform) {
			return fmt.Errorf("turn %d: platform %q is not a platform name", i+1, turn.Platform)
		}
		if turn.Platform != "" && turn.Platform != "eval" && c.SharedData {
			// The binding would give the eval person a real-platform account
			// that `eval clean` must never select.
			return fmt.Errorf("turn %d: platform needs the isolated data directory", i+1)
		}
		if turn.CWD != "" {
			if turn.Platform != "cli" {
				return fmt.Errorf("turn %d: cwd needs platform: cli, the endpoint that sends a working directory", i+1)
			}
			if !needsWorkspaceIsolation(c) {
				return fmt.Errorf("turn %d: cwd needs an isolated workspace", i+1)
			}
			if _, err := workspaceSubdir("", turn.CWD); err != nil {
				return fmt.Errorf("turn %d: %w", i+1, err)
			}
		}
		if turn.TurnRunOrdinal < 0 || turn.TurnRunOrdinal > i {
			return fmt.Errorf("turn %d: turn_run_ordinal must name an earlier turn (1..%d)", i+1, i)
		}
		if turn.HoldWorker {
			if c.SharedData {
				return fmt.Errorf("turn %d: hold_worker needs the isolated data directory", i+1)
			}
			if turn.WaitForMaintenance || turn.WaitForExternalWatches {
				return fmt.Errorf("turn %d: hold_worker cannot wait for maintenance or external watches", i+1)
			}
		}
		if turn.Scheduled {
			if c.SharedData {
				return fmt.Errorf("turn %d: scheduled needs the isolated data directory", i+1)
			}
			// A job is stored text and an account; everything else a turn can
			// carry belongs to a person at an endpoint.
			if turn.HoldWorker || turn.ReplyToTurn > 0 || turn.ApprovalID != "" || turn.ClarifyID != "" ||
				turn.NativeReplyMessageID != "" || len(turn.AdditionalRoots) > 0 || turn.CWD != "" ||
				turn.Channel != "" || turn.WaitForExternalWatches {
				return fmt.Errorf("turn %d: a scheduled turn carries only its text, platform and platform_user_id", i+1)
			}
		}
		for j, p := range turn.AssertState {
			if err := c.validateRuntimePredicate(p); err != nil {
				return fmt.Errorf("turn %d: assert_state[%d]: %w", i+1, j, err)
			}
		}
	}
	for j, p := range c.AssertState {
		if err := c.validateRuntimePredicate(p); err != nil {
			return fmt.Errorf("assert_state[%d]: %w", j, err)
		}
	}
	if err := c.validateApprovalSeeds(); err != nil {
		return err
	}
	return c.validateEndpointSeeds()
}

// runStatuses are the statuses the gateway stores on a Run.
var runStatuses = []string{
	"running", "done", "completed", "failed", "cancelled", "interrupted",
	"waiting_user", "waiting_external", "verification_partial", "blocked",
}

// turnChoiceStatuses are what turn_choices can count.
var turnChoiceStatuses = []string{"pending", "resolved"}

// mcpStubTools are the tools the MCP stub serves.
var mcpStubTools = []string{"echo", "rendezvous", "record"}

// validateRuntimePredicate refuses a runtime assertion that could pass without
// evidence: one with no count operator, or with an operator, status or target
// its target does not read or know. A mistyped value would otherwise count
// zero and satisfy a negative assertion.
func (c *Case) validateRuntimePredicate(p StatePredicate) error {
	on := strings.ToLower(strings.TrimSpace(p.On))
	if !runtimePredicateTargets[on] {
		return nil
	}
	if p.Exists == nil && p.CountGte == nil && p.CountLte == nil {
		return fmt.Errorf("on: %s needs count_gte, count_lte or exists", on)
	}
	if (p.CountGte != nil && *p.CountGte < 0) || (p.CountLte != nil && *p.CountLte < 0) {
		return fmt.Errorf("on: %s counts are never negative", on)
	}
	if p.Exists == nil && p.CountLte == nil && *p.CountGte == 0 {
		return fmt.Errorf("on: %s count_gte: 0 alone cannot fail", on)
	}
	if p.Field != "" || p.Path != "" || p.Type != "" || p.Eq != nil || p.Contains != nil || p.NotContains != nil ||
		p.Matches != nil || p.PayloadContains != nil || len(p.PayloadFields) > 0 || p.Empty != nil || p.LenGte != nil || p.LenLte != nil || p.MinBytes != nil {
		return fmt.Errorf("on: %s counts; use only status, target, count_gte, count_lte and exists", on)
	}
	status := strings.ToLower(strings.TrimSpace(p.Status))
	target := strings.TrimSpace(p.Target)
	var statuses []string
	needsTarget := false
	switch on {
	case "runs":
		statuses = runStatuses
	case "queued":
		statuses = queueStatuses
	case "turn_choices":
		statuses = turnChoiceStatuses
	case "mcp_server_starts", "worker_tool", "worker_tool_calls":
		needsTarget = true
	}
	if status != "" && !containsString(statuses, status) {
		if statuses == nil {
			return fmt.Errorf("on: %s takes no status", on)
		}
		return fmt.Errorf("on: %s status %q is not one of %s", on, p.Status, strings.Join(statuses, ", "))
	}
	if !needsTarget {
		if target != "" {
			return fmt.Errorf("on: %s takes no target", on)
		}
		return nil
	}
	if target == "" {
		return fmt.Errorf("on: %s needs a target", on)
	}
	server, tool := target, ""
	if on != "mcp_server_starts" {
		rest, isMCP := strings.CutPrefix(target, "mcp:")
		if !isMCP {
			return nil
		}
		var found bool
		if server, tool, found = strings.Cut(rest, "/"); !found || tool == "" {
			return fmt.Errorf("on: %s target %q must be mcp:<server>/<tool>", on, target)
		}
		if !containsString(mcpStubTools, tool) {
			return fmt.Errorf("on: %s target %q names no tool of the MCP stub (%s)", on, target, strings.Join(mcpStubTools, ", "))
		}
	}
	// Exact match: the registered tool name hashes the server name as written,
	// so a server spelled in other case would name another tool.
	if !c.declaresMCPStub(server) {
		return fmt.Errorf("on: %s target %q names no runtime.mcp_stubs server", on, target)
	}
	return nil
}

func (c *Case) declaresMCPStub(name string) bool {
	if c.Runtime == nil {
		return false
	}
	for _, stub := range c.Runtime.MCPStubs {
		if stub.Name == name {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (c *Case) normalizeRuntime() error {
	runtime := c.Runtime
	if runtime == nil {
		return nil
	}
	if runtime.MaxActiveWorkRuns < 0 || runtime.MaxActiveWorkRuns > 3 {
		return fmt.Errorf("runtime.max_active_work_runs must be between 1 and 3")
	}
	if runtime.Workers < 0 || runtime.Workers > maxEvalWorkers {
		return fmt.Errorf("runtime.workers must be between 1 and %d", maxEvalWorkers)
	}
	if runtime.workers() < runtime.capacity() {
		return fmt.Errorf("runtime.workers (%d) is below runtime.max_active_work_runs (%d): every admitted Run needs its own agent worker", runtime.workers(), runtime.capacity())
	}
	if runtime.Cron && c.SharedData {
		return fmt.Errorf("runtime.cron needs the isolated data directory for its scheduler store")
	}
	if len(runtime.MCPStubs) > 0 && c.SharedData {
		return fmt.Errorf("runtime.mcp_stubs needs the isolated data directory for the stub binary and its logs")
	}
	seen := map[string]bool{}
	for i := range runtime.MCPStubs {
		stub := &runtime.MCPStubs[i]
		stub.Name = strings.ToLower(strings.TrimSpace(stub.Name))
		if !validEndpointName(stub.Name) {
			return fmt.Errorf("runtime.mcp_stubs[%d].name %q must use lowercase letters, digits, '_' or '-'", i, stub.Name)
		}
		if seen[stub.Name] {
			return fmt.Errorf("runtime.mcp_stubs repeats %q", stub.Name)
		}
		seen[stub.Name] = true
	}
	return nil
}

// workspaceSubdir resolves a relative directory inside root and refuses one
// that escapes it.
func workspaceSubdir(root, rel string) (string, error) {
	clean := filepath.Clean(strings.TrimSpace(rel))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("cwd %q must be a subdirectory of the workspace", rel)
	}
	return filepath.Join(root, clean), nil
}

// applyRuntimeConfig gives the case its declared runtime: the cron scheduler
// (never started) and the MCP stub servers, added to any configured ones. It
// runs before InitGateway so every worker is built with them. casePath locates
// the source checkout that builds the stub.
func applyRuntimeConfig(cfg *config.Config, runtime *RuntimeSettings, stubDir, casePath string) error {
	cfg.Gateway.MaxActiveWorkRuns = runtime.capacity()
	// Eval runs are explicit foreground checks: a case gets the cron tool only
	// when it asks for one, and the scheduler loop never starts.
	cfg.Cron.Enabled = runtime != nil && runtime.Cron
	cfg.Cron.Canary.Enabled = false
	if runtime == nil || len(runtime.MCPStubs) == 0 {
		return nil
	}
	for _, existing := range cfg.MCP.Servers {
		for _, stub := range runtime.MCPStubs {
			if strings.EqualFold(strings.TrimSpace(existing.Name), stub.Name) {
				return fmt.Errorf("runtime.mcp_stubs %q collides with a configured MCP server", stub.Name)
			}
		}
	}
	binary, err := buildMCPStub(stubDir, casePath)
	if err != nil {
		return err
	}
	for _, stub := range runtime.MCPStubs {
		cfg.MCP.Servers = append(cfg.MCP.Servers, config.MCP_SERVER{
			Name: stub.Name, Transport: "stdio", Command: binary,
			Args: []string{mcpStubStartLogFlag, mcpStubStartLog(stubDir, stub.Name)},
		})
	}
	return nil
}

const (
	// mcpStubPackage is the stdio command of internal/tools/mcptest.
	mcpStubPackage = "selfmind/internal/tools/mcptest/cmd/mcpstub"
	// mcpStubStartLogFlag makes every stub process record its process id.
	mcpStubStartLogFlag = "-start-log"
)

// ErrRequirementUnavailable marks a case whose environment cannot provide what
// the case needs, such as a buildable MCP stub. Selfcheck reports such a case
// as unavailable rather than as a regression.
var ErrRequirementUnavailable = errors.New("eval requirement unavailable")

// mcpStubBuildTimeout bounds the stub build, which normally takes seconds.
const mcpStubBuildTimeout = 2 * time.Minute

// processHome is HOME when the process started. A test may point HOME at a
// temporary directory, but the Go tool keeps its build caches under the real
// one; building there would recompile and download every module.
var processHome = os.Getenv("HOME")

// buildMCPStub compiles the stub command into dir with the Go tool, from the
// SelfMind module that contains the case file or the working directory.
func buildMCPStub(dir, casePath string) (string, error) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("%w: runtime.mcp_stubs needs the Go tool to build the MCP stub (declare requires.commands: [go]): %v", ErrRequirementUnavailable, err)
	}
	root, err := selfmindModuleRoot(casePath)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrRequirementUnavailable, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	binary := filepath.Join(dir, "mcpstub")
	ctx, cancel := context.WithTimeout(context.Background(), mcpStubBuildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, goTool, "build", "-o", binary, mcpStubPackage)
	cmd.Dir = root
	cmd.Env = mcpStubBuildEnv(os.Environ(), processHome, evalOffline())
	if output, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("%w: build the MCP stub: no result within %s", ErrRequirementUnavailable, mcpStubBuildTimeout)
		}
		failure := fmt.Errorf("build the MCP stub: %v\n%s", err, bytes.TrimSpace(output))
		if mcpStubEnvironmentFailure.Match(output) {
			return "", fmt.Errorf("%w: %v", ErrRequirementUnavailable, failure)
		}
		// Anything else, such as a compile error in the stub, is a regression.
		return "", failure
	}
	return binary, nil
}

// mcpStubEnvironmentFailure matches build output that blames the environment
// rather than the source: modules or a toolchain that cannot be fetched, or a
// Go tool older than the module requires.
var mcpStubEnvironmentFailure = regexp.MustCompile(`module lookup disabled|dial tcp|no such host|connection refused|connection reset|i/o timeout|TLS handshake timeout|proxyconnect|toolchain not available|requires go >=`)

// mcpStubBuildEnv keeps the Go caches under the process's original HOME and,
// offline, forbids module downloads: the build then uses only the module
// cache already on disk.
func mcpStubBuildEnv(environ []string, home string, offline bool) []string {
	env := append([]string{}, environ...)
	if home != "" {
		env = append(env, "HOME="+home)
	}
	if offline {
		env = append(env, "GOPROXY=off")
	}
	return env
}

// evalOffline reports strict offline eval, read the way the VCR reads
// SELFMIND_EVAL_OFFLINE.
func evalOffline() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SELFMIND_EVAL_OFFLINE"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// selfmindModuleRoot finds the SelfMind module above the case file, or else
// above the working directory.
func selfmindModuleRoot(casePath string) (string, error) {
	var starts []string
	if strings.TrimSpace(casePath) != "" {
		if abs, err := filepath.Abs(filepath.Dir(casePath)); err == nil {
			starts = append(starts, abs)
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		starts = append(starts, cwd)
	}
	for _, dir := range starts {
		for {
			data, readErr := os.ReadFile(filepath.Join(dir, "go.mod"))
			if readErr == nil {
				firstLine, _, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
				if strings.TrimSpace(firstLine) == "module selfmind" {
					return dir, nil
				}
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", fmt.Errorf("runtime.mcp_stubs builds the MCP stub from the SelfMind source checkout; keep the case inside it or run from it")
}

func mcpStubStartLog(dir, server string) string {
	return filepath.Join(dir, "mcp-"+server+"-starts.log")
}

// countMCPStubStarts reads how many stub processes a server started.
func countMCPStubStarts(dir, server string) (int, error) {
	file, err := os.Open(mcpStubStartLog(dir, server))
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer file.Close()
	count := 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			count++
		}
	}
	return count, scanner.Err()
}
