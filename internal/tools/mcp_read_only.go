package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/platform/log"
)

// Who explicitly marked an external tool read-only. A server's own
// description of its tools, such as the MCP readOnlyHint annotation, is never
// one of them.
const (
	ReadOnlyMarkedByOperator = "operator"
	ReadOnlyMarkedByPerson   = "person"
)

// MCPReadOnlyMarks answers whether a person marked one exact tool of one
// configured MCP server identity read-only, counting only markings made at or
// before notAfter when it is set. control.Store implements it.
type MCPReadOnlyMarks interface {
	MCPToolMarkedReadOnly(ctx context.Context, tenantID, personID, serverName, serverIdentity, toolName string, notAfter time.Time) (bool, error)
}

// MCPServerIdentity is the configured identity a read-only marking belongs
// to: the transport and what it starts or reaches. A different server
// configured under the same name has another identity and inherits no
// marking. Credentials, headers and the environment filter are not part of
// it, so rotating a token keeps the server's markings.
func MCPServerIdentity(config MCPServerConfig) string {
	transport := normalizedMCPTransport(config.Transport)
	material := []string{transport}
	if transport == "stdio" {
		material = append(material, strings.TrimSpace(config.Command))
		material = append(material, config.Args...)
	} else {
		material = append(material, strings.TrimSpace(config.URL))
	}
	encoded, _ := json.Marshal(material)
	sum := sha256.Sum256(encoded)
	return "mcpsrv_" + hex.EncodeToString(sum[:12])
}

func normalizedMCPTransport(transport string) string {
	transport = strings.ToLower(strings.TrimSpace(transport))
	switch transport {
	case "http", "streamable_http", "streamable-http":
		return "streamable_http"
	default:
		return transport
	}
}

// mcpReadOnlyPolicy is one configured server's read-only markings: the
// operator's exact tool names from configuration and each person's own
// markings for this server identity. It is immutable after Connect.
type mcpReadOnlyPolicy struct {
	server   string
	identity string
	operator map[string]struct{}
	marks    MCPReadOnlyMarks
}

func newMCPReadOnlyPolicy(config MCPServerConfig, marks MCPReadOnlyMarks) *mcpReadOnlyPolicy {
	operator := make(map[string]struct{}, len(config.ReadOnlyTools))
	for _, name := range config.ReadOnlyTools {
		operator[name] = struct{}{}
	}
	return &mcpReadOnlyPolicy{server: config.Name, identity: MCPServerIdentity(config), operator: operator, marks: marks}
}

// marking names who marked this exact tool read-only for the call's person,
// or "" when nobody did. A person's marking is a standing decision, so a Run
// uses it under the same policy as remembered approval classes: a scheduled
// Run only the markings that existed when its schedule was authorised. A
// call without a person scope can use only the operator's marking, and a
// lookup failure is no marking.
func (p *mcpReadOnlyPolicy) marking(toolName string, args map[string]interface{}) string {
	if p == nil {
		return ""
	}
	if _, ok := p.operator[toolName]; ok {
		return ReadOnlyMarkedByOperator
	}
	if p.marks == nil {
		return ""
	}
	scope, ok := currentExecutionScopeAny(args)
	if !ok || scope.TenantID == "" || scope.PersonID == "" || !scope.StandingGrants.Allowed {
		return ""
	}
	marked, err := p.marks.MCPToolMarkedReadOnly(contextFromArgs(args), scope.TenantID, scope.PersonID,
		p.server, p.identity, toolName, scope.StandingGrants.NotAfter)
	if err != nil {
		// An incomplete schema is reported once by schema health, not per call.
		if !errors.Is(err, control.ErrMCPReadOnlyMarksUnavailable) {
			log.Warn("mcp: read-only marking lookup failed; treating the tool as unmarked", "server", p.server, "tool", toolName, "error", err)
		}
		return ""
	}
	if marked {
		return ReadOnlyMarkedByPerson
	}
	return ""
}

// readOnlyMarkedTool is an external tool that an operator or a person can
// explicitly mark read-only. Only that marking, never the tool's own metadata
// or annotations, gives an external tool a read-only execution policy.
type readOnlyMarkedTool interface {
	readOnlyMarking(args map[string]interface{}) string
}

// executionPolicyForCall is the registered tool's policy for one call. An
// external tool is read-only only when the operator or the call's person
// marked it; the marking does not change its origin, so replay and every
// other external-tool rule still apply. A marked tool still reads through the
// network, as web_search does, so it keeps that operation class: the person's
// explicit "do not use the network" reaches it.
func executionPolicyForCall(t Tool, args map[string]interface{}) toolExecutionPolicy {
	policy := executionPolicyForTool(t)
	if policy.Origin != ToolSchemaOriginExternal {
		return policy
	}
	if marked, ok := t.(readOnlyMarkedTool); ok {
		if by := readOnlyMarkingForCall(marked, args); by != "" {
			policy.ReadOnly = true
			policy.ReadOnlyMarkedBy = by
			if !slices.Contains(policy.OperationClasses, OpClassNetwork) {
				policy.OperationClasses = append(policy.OperationClasses, OpClassNetwork)
			}
		}
	}
	return policy
}

// toolReadOnlyMarkingArg carries one call's marking decision. It is
// daemon-owned: the kernel removes underscore arguments a model supplies
// before it adds runtime ones.
const toolReadOnlyMarkingArg = "_tool_read_only_marking"

// readOnlyMarkingDecision is read once per call and then carried, so the
// batch decision, approval, effect claim and recorded metadata of one call
// agree even if the person unmarks the tool while it is being dispatched.
type readOnlyMarkingDecision struct {
	once sync.Once
	tool string
	by   string
}

func readOnlyMarkingForCall(t readOnlyMarkedTool, args map[string]interface{}) string {
	if args == nil {
		return t.readOnlyMarking(nil)
	}
	name := ""
	if tool, ok := t.(Tool); ok {
		name = tool.Name()
	}
	decision, ok := args[toolReadOnlyMarkingArg].(*readOnlyMarkingDecision)
	if !ok || decision.tool != name {
		decision = &readOnlyMarkingDecision{tool: name}
		args[toolReadOnlyMarkingArg] = decision
	}
	decision.once.Do(func() { decision.by = t.readOnlyMarking(args) })
	return decision.by
}
