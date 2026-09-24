package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// request_permissions is the REVERSE channel of the approval funnel (batch C3).
//
// Every other path asks the person one question per operation, discovered one
// failure at a time: write outside the workspace → ask; reach a host → ask; run
// one release command → ask; run the next already-known command → ask again. For
// work whose shape is known up front, that is the wrong shape of conversation —
// the person answers five questions that were really one.
//
// This tool lets the agent state the bundle ONCE, before starting, and receive a
// single decision. It is not a new authority: every permission it can request is
// expressed as one of the same narrow rule keys a per-operation ask would offer
// (approval_rules.go), stored in the same approval_grants ledger, bounded by the
// same TTL policy, and revocable the same way. What changes is only WHEN the
// person is asked and how many times.
//
// Safety properties, in order of importance:
//
//   - No broad new authority. Paths and hosts use the existing narrow rules.
//     Commands are exact, run-local, environment-bound declarations; arbitrary
//     code, opaque shells, destructive host operations, and external tools stay
//     on their ordinary single-call path.
//   - Refusal is a decision. A refused bundle returns the user-rejection contract
//     so the model does not retry a variant or fall back to per-call asks.
//   - Already-granted requests do not re-ask. The tool reports them as satisfied,
//     so a retrying agent cannot use it as an approval-spamming loop.
//   - The hard floor is untouched: a granted path rule cannot authorize a
//     hardline operation, which is refused before any grant is consulted.

// requestPermissionsMaxItems bounds one request. A bundle larger than this is not
// a plan, it is a fishing expedition, and it would not fit on an approval surface.
const requestPermissionsMaxItems = 8

// Exact declarations are displayed and stored with the approval request. A
// large payload belongs on the ordinary one-call surface, where it cannot
// multiply the size and review burden of a bundle.
const requestPermissionsMaxEffectBytes = 8 * 1024

type requestedPermissionItem struct {
	Key     string
	Label   string
	RunOnly bool
	Effect  map[string]interface{}
}

// RequestPermissionsTool is the tool registration. It runs under the same
// middleware, scope, and safety layers as every other tool.
type RequestPermissionsTool struct {
	BaseTool
}

func NewRequestPermissionsTool() *RequestPermissionsTool {
	return &RequestPermissionsTool{
		BaseTool: BaseTool{
			name: "request_permissions",
			description: "Ask the person ONCE for a bounded phase whose permissions are already known: filesystem roots, " +
				"network hosts, and exact statically known commands. Exact commands are valid only in this run and only while " +
				"their arguments, workspace, identity, and execution environment remain unchanged. Do not declare commands " +
				"that depend on earlier output. Arbitrary code, opaque shell scripts, destructive host commands, and external " +
				"tools must use their ordinary single-call approval. A refusal is the person's decision: do not retry a variant.",
			schema: ToolSchema{
				Type:                 "object",
				AdditionalProperties: rejectAdditionalProperties(),
				Properties: map[string]PropertyDef{
					"paths": {
						Type:        "array",
						Description: "Absolute directories the task needs to write under. Omit for workspace-only work.",
						Items:       &PropertyDef{Type: "string"},
					},
					"hosts": {
						Type:        "array",
						Description: "Hostnames the task needs to reach (bare host, no scheme or path), e.g. api.github.com.",
						Items:       &PropertyDef{Type: "string"},
					},
					"effects": {
						Type:        "array",
						Description: "Exact commands known before this phase starts. Each item names a registered built-in exec tool and its complete public arguments. Do not include values produced by an earlier command.",
						Items: &PropertyDef{
							Type:                 "object",
							AdditionalProperties: rejectAdditionalProperties(),
							Properties: map[string]PropertyDef{
								"tool":           {Type: "string", Description: "Registered built-in exec tool, such as terminal or verify."},
								"arguments_json": {Type: "string", Description: "JSON object containing the complete public arguments for the exact future call."},
							},
							Required: []string{"tool", "arguments_json"},
						},
					},
					"reason": {
						Type:        "string",
						Description: "One line: what the task will do with these permissions.",
					},
				},
				Required: []string{"reason"},
			},
			handler: requestPermissionsExecutor,
		},
	}
}

func requestPermissionsExecutor(args map[string]interface{}) (string, error) {
	scope, hasScope := currentExecutionScopeAny(args)
	if !hasScope {
		return "", fmt.Errorf("request_permissions needs an execution scope; it cannot be used outside a workspace-bound run")
	}
	reason := strings.TrimSpace(stringArg(args, "reason"))
	if reason == "" {
		return "", fmt.Errorf("reason is required: state in one line what the permissions are for")
	}
	rules, err := requestedPermissionRules(args, scope)
	if err != nil {
		return "", err
	}
	requested := make([]requestedPermissionItem, 0, len(rules))
	for _, rule := range rules {
		requested = append(requested, requestedPermissionItem{Key: rule.Key, Label: rule.Label})
	}
	effects, err := requestedPermissionEffects(args, scope)
	if err != nil {
		return "", err
	}
	requested = append(requested, effects...)
	if len(requested) > requestPermissionsMaxItems {
		return "", fmt.Errorf("request at most %d permissions at a time; narrow the list to one reviewable phase", requestPermissionsMaxItems)
	}
	if len(requested) == 0 {
		return "No permissions requested: workspace-scoped writes and sandboxed execution need no grant. Proceed.", nil
	}

	ctx := contextFromArgs(args)
	var already, pending []requestedPermissionItem
	for _, item := range requested {
		granted := scope.runGrants != nil && scope.runGrants.has(item.Key)
		if !item.RunOnly && scope.Grants != nil {
			persisted := false
			if scope.StandingGrants.Allowed {
				persisted, _ = scope.Grants.IsApprovalGranted(ctx, scope.TenantID, scope.PersonID, scope.WorkspaceID, item.Key, scope.StandingGrants.NotAfter)
			}
			granted = granted || persisted
		}
		if granted {
			already = append(already, item)
			continue
		}
		pending = append(pending, item)
	}
	if len(pending) == 0 {
		return "Already granted: " + describePermissionItems(already) + ". Proceed without asking again.", nil
	}
	if scope.Approval == nil {
		return "", rejectOperation(rejectionCodeCapability, "operation rejected: these permissions need approval and no approval surface is attached to this run")
	}

	// ONE ask for the whole bundle. The candidates travel with it, so the person's
	// answer can pick the bundle exactly as offered — the same offered-only rule
	// the per-operation path enforces.
	decision, approvalErr := scope.Approval(ctx, ToolApprovalRequest{
		TenantID: scope.TenantID,
		PersonID: scope.PersonID,
		TaskID:   scope.TaskID,
		RunID:    scope.RunID,
		Channel:  scope.Channel,
		ToolName: "request_permissions",
		Reason:   reason,
		Args: map[string]interface{}{
			"permissions": describePermissionItems(pending),
			"effects":     permissionEffectDisplays(pending),
		},
		GrantClass: "the displayed permission bundle",
		// This tool has no useful one-off side effect. Its only positive answer is
		// the exact displayed bundle, bounded to the live run.
		DecisionPolicy: ApprovalDecisionPolicyRunBundle,
		Environment:    scope.EnvironmentSnapshotID,
		Cwd:            approvalDisplayCwd(scope),
	})
	if approvalErr != nil {
		return "", approvalErr
	}
	if !decision.Approved {
		// Same contract as any other refusal: a decision, not a failure to work
		// around. Without this the model would "helpfully" fall back to asking per
		// command, which is the fatigue this tool exists to remove.
		if decision.Outcome == ApprovalOutcomeTimedOut {
			return "", fmt.Errorf("approval timed out with no answer: nobody is at the keyboard; finish waiting_user instead of retrying")
		}
		return "", rejectOperation(rejectionCodeApproval, "operation rejected: "+fallbackReason(decision.Reason, "the requested permissions were refused"))
	}

	// Scope: the person's answer decides how long. An empty scope means "this once",
	// which for a pre-declared bundle is a task-scoped grant — the work it
	// authorizes is this task's work.
	if decision.Scope != "run" || strings.TrimSpace(decision.GrantKey) != "" {
		return "", rejectOperation(rejectionCodeApproval, "operation rejected: the permission bundle was not approved for this run")
	}
	grantScope := "run"
	for _, item := range pending {
		recordApprovalGrant(ctx, scope, grantScope, item.Key, approvalGrantExpiry(grantScope, args))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Granted (%s): %s.", grantScope, describePermissionItems(pending))
	if len(already) > 0 {
		fmt.Fprintf(&sb, " Already held: %s.", describePermissionItems(already))
	}
	sb.WriteString(" Proceed; these no longer prompt.")
	return sb.String(), nil
}

// requestedPermissionRules validates the requested bundle and maps it to rule
// candidates. Validation is strict on purpose: a root that authorizes everything
// ("/", the bare home directory) or a host that is not a host would create an
// authorization nobody could reason about later.
func requestedPermissionRules(args map[string]interface{}, scope ExecutionScope) ([]ApprovalRuleCandidate, error) {
	paths := stringSliceArg(args, "paths")
	hosts := stringSliceArg(args, "hosts")
	if len(paths)+len(hosts) > requestPermissionsMaxItems {
		return nil, fmt.Errorf("request at most %d permissions at a time; narrow the list to what this task actually needs", requestPermissionsMaxItems)
	}
	seen := map[string]struct{}{}
	var rules []ApprovalRuleCandidate
	add := func(rule ApprovalRuleCandidate) {
		if _, dup := seen[rule.Key]; dup {
			return
		}
		seen[rule.Key] = struct{}{}
		rules = append(rules, rule)
	}
	for _, raw := range paths {
		root, err := validatePermissionRoot(raw, scope)
		if err != nil {
			return nil, err
		}
		if root == "" {
			continue // already inside the workspace: nothing to grant
		}
		add(ApprovalRuleCandidate{
			Kind:  ApprovalRuleKindPathRoot,
			Key:   approvalRuleKey(ApprovalRuleKindPathRoot, root),
			Label: fmt.Sprintf("writes under %s", root),
		})
	}
	for _, raw := range hosts {
		host, ok := hostFromToken(strings.TrimSpace(raw))
		if !ok {
			return nil, fmt.Errorf("%q is not a hostname; pass a bare host such as api.github.com (no scheme, no path)", raw)
		}
		add(ApprovalRuleCandidate{
			Kind:  ApprovalRuleKindNetworkHost,
			Key:   approvalRuleKey(ApprovalRuleKindNetworkHost, host),
			Label: fmt.Sprintf("network access to %s", host),
		})
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].Key < rules[j].Key })
	return rules, nil
}

// requestedPermissionEffects validates commands without executing them. The
// live call will be normalized again and will pass the hard floor, current
// explicit-deny policy, capability checks, and workspace scope again. This
// function only creates an exact run-local key for that later comparison.
func requestedPermissionEffects(args map[string]interface{}, scope ExecutionScope) ([]requestedPermissionItem, error) {
	raw, present := args["effects"]
	if !present {
		return nil, nil
	}
	values, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("effects must be an array")
	}
	registry, _ := args["_registry"].(*Registry)
	if len(values) > 0 && registry == nil {
		return nil, fmt.Errorf("exact command declarations require the active tool registry")
	}
	seen := map[string]struct{}{}
	items := make([]requestedPermissionItem, 0, len(values))
	for index, value := range values {
		effect, ok := value.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("effects[%d] must be an object", index)
		}
		toolName := strings.TrimSpace(stringArg(effect, "tool"))
		argumentsJSON := strings.TrimSpace(stringArg(effect, "arguments_json"))
		if toolName == "" || argumentsJSON == "" {
			return nil, fmt.Errorf("effects[%d] needs tool and arguments_json", index)
		}
		var publicArgs map[string]interface{}
		if err := json.Unmarshal([]byte(argumentsJSON), &publicArgs); err != nil || publicArgs == nil {
			return nil, fmt.Errorf("effects[%d].arguments_json must be a JSON object", index)
		}
		for name := range publicArgs {
			if strings.HasPrefix(strings.TrimSpace(name), "_") {
				return nil, fmt.Errorf("effects[%d].arguments_json contains reserved runtime argument %q", index, name)
			}
		}
		tool, registered := registry.Get(toolName)
		if !registered {
			return nil, fmt.Errorf("effects[%d] names unavailable tool %q", index, toolName)
		}
		policy := executionPolicyForTool(tool)
		if policy.Origin != ToolSchemaOriginBuiltin || !isExecTool(toolName) || toolName == "execute_code" {
			return nil, fmt.Errorf("effects[%d] %q cannot be phase-approved; use its ordinary single-call approval", index, toolName)
		}
		prepared, prepareErr := registry.PrepareToolArguments(toolName, publicArgs)
		if prepareErr != nil {
			return nil, fmt.Errorf("effects[%d] %s: %w", index, toolName, prepareErr)
		}
		prepared["_tool_name"] = toolName
		prepared[toolExecutionPolicyArg] = policy
		annotateEffectiveSandboxMode(prepared)
		projectRoot := approvalProjectRoot("", scope, prepared)
		if blocked, blockReason := hardlineToolCall(projectRoot, toolName, prepared); blocked {
			return nil, fmt.Errorf("effects[%d] cannot be declared: hard safety policy blocks %s", index, blockReason)
		}
		command := strings.TrimSpace(execCommandPayload(toolName, prepared))
		segments, unparsed := expandCommandSegments(command, 0)
		if command == "" || unparsed {
			return nil, fmt.Errorf("effects[%d] is opaque or incomplete; use its ordinary single-call approval", index)
		}
		if egress, _ := egressCommand(command, segments); egress {
			return nil, fmt.Errorf("effects[%d] is an arbitrary network command; use its ordinary single-call approval", index)
		}
		dangerous, dangerReason := dangerousToolCall(projectRoot, toolName, prepared)
		if dangerous && strings.TrimSpace(dangerReason) != HostEscapeApprovalReason {
			return nil, fmt.Errorf("effects[%d] is destructive or otherwise sensitive; use its ordinary single-call approval", index)
		}
		canonical, marshalErr := json.Marshal(approvalArgs(prepared))
		if marshalErr != nil {
			return nil, fmt.Errorf("effects[%d] arguments cannot be normalized: %w", index, marshalErr)
		}
		if len(canonical) > requestPermissionsMaxEffectBytes {
			return nil, fmt.Errorf("effects[%d] is too large for a reviewable bundle; use its ordinary single-call approval", index)
		}
		key := approvalDeclaredEffectKey(toolName, prepared, scope, true)
		if key == "" {
			return nil, fmt.Errorf("effects[%d] could not be bound to this run", index)
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		items = append(items, requestedPermissionItem{
			Key: key, Label: fmt.Sprintf("exact %s command: %s", toolName, truncateRunes(toSingleLine(RedactSensitive(command)), 180)),
			RunOnly: true,
			Effect: map[string]interface{}{
				"tool":    toolName,
				"args":    approvalDisplayArgs(prepared),
				"summary": ApprovalChangeSummary(toolName, prepared),
			},
		})
	}
	return items, nil
}

// validatePermissionRoot returns the directory a path rule would authorize, ""
// when the path is already inside the scope's roots, or an error when the request
// is too broad to be a rule.
func validatePermissionRoot(raw string, scope ExecutionScope) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("empty path in the permission request")
	}
	if !filepath.IsAbs(trimmed) {
		return "", fmt.Errorf("%q must be an absolute directory", raw)
	}
	root := filepath.Clean(trimmed)
	if root == "/" {
		return "", fmt.Errorf("refusing to request write access to the filesystem root; name the directory the task needs")
	}
	if home := filepath.Clean(strings.TrimRight(os.Getenv("HOME"), "/")); home != "" && home != "." && root == home {
		return "", fmt.Errorf("refusing to request write access to the whole home directory; name the directory the task needs")
	}
	roots := append([]string{}, scope.AllowedRoots...)
	if trimmedRoot := strings.TrimSpace(scope.WorkspaceRoot); trimmedRoot != "" {
		roots = append(roots, trimmedRoot)
	}
	for _, allowed := range roots {
		if isWithin(filepath.Clean(allowed), root) {
			return "", nil
		}
	}
	return root, nil
}

func describePermissionRules(rules []ApprovalRuleCandidate) string {
	labels := make([]string, 0, len(rules))
	for _, rule := range rules {
		labels = append(labels, rule.Label)
	}
	return strings.Join(labels, ", ")
}

func describePermissionItems(items []requestedPermissionItem) string {
	labels := make([]string, 0, len(items))
	for _, item := range items {
		labels = append(labels, item.Label)
	}
	return strings.Join(labels, ", ")
}

func permissionEffectDisplays(items []requestedPermissionItem) []interface{} {
	displays := make([]interface{}, 0, len(items))
	for _, item := range items {
		if item.Effect != nil {
			displays = append(displays, item.Effect)
		}
	}
	if len(displays) == 0 {
		return nil
	}
	return displays
}

// stringSliceArg reads a string array argument, tolerating a single string (a
// common model shorthand) and ignoring blanks.
func stringSliceArg(args map[string]interface{}, key string) []string {
	switch value := args[key].(type) {
	case []interface{}:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				out = append(out, strings.TrimSpace(text))
			}
		}
		return out
	case []string:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if strings.TrimSpace(item) != "" {
				out = append(out, strings.TrimSpace(item))
			}
		}
		return out
	case string:
		if strings.TrimSpace(value) == "" {
			return nil
		}
		return []string{strings.TrimSpace(value)}
	default:
		return nil
	}
}
