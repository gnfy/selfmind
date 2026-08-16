package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// request_permissions is the REVERSE channel of the approval funnel (batch C3).
//
// Every other path asks the person one question per operation, discovered one
// failure at a time: write outside the workspace → ask; reach a host → ask; touch
// a second directory → ask again. For work whose shape is known up front ("I need
// to write under /srv/site and fetch from api.github.com"), that is the wrong
// shape of conversation — the person answers five questions that were really one.
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
//   - No new authority. Only path-root and network-host rules; never host
//     execution, never credential access, never a broader class than the rules
//     the per-call path would have offered anyway.
//   - Refusal is a decision. A refused bundle returns the user-rejection contract
//     so the model does not retry a variant or fall back to per-call asks.
//   - Already-granted requests do not re-ask. The tool reports them as satisfied,
//     so a retrying agent cannot use it as an approval-spamming loop.
//   - The hard floor is untouched: a granted path rule cannot authorize a
//     hardline operation, which is refused before any grant is consulted.

// requestPermissionsMaxItems bounds one request. A bundle larger than this is not
// a plan, it is a fishing expedition, and it would not fit on an approval surface.
const requestPermissionsMaxItems = 8

// RequestPermissionsTool is the tool registration. It runs under the same
// middleware, scope, and safety layers as every other tool.
type RequestPermissionsTool struct {
	BaseTool
}

func NewRequestPermissionsTool() *RequestPermissionsTool {
	return &RequestPermissionsTool{
		BaseTool: BaseTool{
			name: "request_permissions",
			description: "Ask the person ONCE for the filesystem roots and network hosts this task needs, " +
				"instead of being interrupted per command. Use it when you already know the work needs to write " +
				"outside the workspace or reach specific hosts. Requests already granted are reported as satisfied " +
				"without asking again. A refusal is the person's decision: do not retry a variant.",
			schema: ToolSchema{
				Type: "object",
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
	requested, err := requestedPermissionRules(args, scope)
	if err != nil {
		return "", err
	}
	if len(requested) == 0 {
		return "No permissions requested: workspace-scoped writes and sandboxed execution need no grant. Proceed.", nil
	}

	ctx := contextFromArgs(args)
	var already, pending []ApprovalRuleCandidate
	for _, rule := range requested {
		granted := scope.runGrants != nil && scope.runGrants.has(rule.Key)
		if scope.Grants != nil {
			persisted, _ := scope.Grants.IsApprovalGranted(ctx, scope.TenantID, scope.PersonID, scope.TaskID, rule.Key)
			granted = granted || persisted
		}
		if granted {
			already = append(already, rule)
			continue
		}
		pending = append(pending, rule)
	}
	if len(pending) == 0 {
		return "Already granted: " + describePermissionRules(already) + ". Proceed without asking again.", nil
	}
	if scope.Approval == nil {
		return "", fmt.Errorf("operation rejected: these permissions need approval and no approval surface is attached to this run")
	}

	// ONE ask for the whole bundle. The candidates travel with it, so the person's
	// answer can pick the bundle exactly as offered — the same offered-only rule
	// the per-operation path enforces.
	decision, approvalErr := scope.Approval(ctx, ToolApprovalRequest{
		TenantID:   scope.TenantID,
		PersonID:   scope.PersonID,
		TaskID:     scope.TaskID,
		RunID:      scope.RunID,
		Channel:    scope.Channel,
		ToolName:   "request_permissions",
		Reason:     reason,
		Args:       map[string]interface{}{"permissions": describePermissionRules(pending)},
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
		return "", fmt.Errorf("operation rejected: %s", fallbackReason(decision.Reason, "the requested permissions were refused"))
	}

	// Scope: the person's answer decides how long. An empty scope means "this once",
	// which for a pre-declared bundle is a task-scoped grant — the work it
	// authorizes is this task's work.
	if decision.Scope != "run" || strings.TrimSpace(decision.GrantKey) != "" {
		return "", fmt.Errorf("operation rejected: the permission bundle was not approved for this run")
	}
	grantScope := "run"
	for _, rule := range pending {
		recordApprovalGrant(ctx, scope, grantScope, rule.Key, approvalGrantExpiry(grantScope, args))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Granted (%s): %s.", grantScope, describePermissionRules(pending))
	if len(already) > 0 {
		fmt.Fprintf(&sb, " Already held: %s.", describePermissionRules(already))
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
