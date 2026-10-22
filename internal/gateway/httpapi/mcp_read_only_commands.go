package httpapi

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/tools"
)

const (
	mcpReadOnlyUsage = "Usage: /tools readonly [<server>/<tool> on|off]"
	// mcpReadOnlyListLimit bounds the listing for IM.
	mcpReadOnlyListLimit = 40
	// mcpReadOnlyMarksUnavailableReply explains a database an earlier
	// development build stamped v26 before the marking table existed.
	mcpReadOnlyMarksUnavailableReply = "MCP read-only markings are unavailable: this control database is marked schema v26 " +
		"but has no mcp_read_only_marks table, because an earlier development build upgraded it. Restore the backup " +
		"taken before that upgrade with `selfmind maintenance restore-control --backup PATH --yes`, then restart " +
		"SelfMind so this build applies v26. Until then your markings cannot be read or changed; operator " +
		"read_only_tools still apply."
)

// toolsCommandReply lists and changes a person's explicit read-only markings
// of MCP tools (owner decision D6). It is model-free: the person's word is the
// authority, and the server's description of its own tools is never asked.
func (d *Server) toolsCommandReply(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, args []string) (string, error) {
	reply, err := d.mcpReadOnlyCommand(ctx, identity, req, args)
	if errors.Is(err, control.ErrMCPReadOnlyMarksUnavailable) {
		return mcpReadOnlyMarksUnavailableReply, nil
	}
	return reply, err
}

func (d *Server) mcpReadOnlyCommand(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, args []string) (string, error) {
	if d == nil || d.Control == nil || identity == nil {
		return "MCP tool markings are unavailable.", nil
	}
	if len(args) == 0 || (len(args) == 1 && strings.EqualFold(args[0], "readonly")) {
		return d.mcpReadOnlyListing(ctx, identity)
	}
	if len(args) != 3 || !strings.EqualFold(args[0], "readonly") {
		return mcpReadOnlyUsage, nil
	}
	action := strings.ToLower(args[2])
	if action != "on" && action != "off" {
		return mcpReadOnlyUsage, nil
	}
	// A marking is the person's own word. Text the daemon replays, such as a
	// scheduled prompt the model stored, may hold this command and may even
	// name another person's endpoint; it changes nothing. The gateway refuses
	// such text before any control runs; this check holds on its own as well.
	if !isUserOriginTurn(ctx, req) {
		return "Only you can change MCP read-only markings, by typing the command yourself; a scheduled or other daemon-started turn cannot. Nothing was changed.", nil
	}
	if action == "on" {
		return d.markMCPToolReadOnly(ctx, identity, req.Channel, args[1])
	}
	return d.unmarkMCPToolReadOnly(ctx, identity, req.Channel, args[1])
}

func (d *Server) mcpServers() []tools.MCPServerStatus {
	if d.MCPServersFunc == nil {
		return nil
	}
	return d.MCPServersFunc()
}

// mcpToolReading is one way a <server>/<tool> reference splits into a named
// server and an exact tool name.
type mcpToolReading struct{ server, tool string }

// mcpToolReferenceReadings lists every split of ref at a named server. A
// server name may itself contain "/", so a reference can have several.
func mcpToolReferenceReadings(ref string, servers []string) []mcpToolReading {
	var readings []mcpToolReading
	seen := make(map[string]bool, len(servers))
	for _, name := range servers {
		if seen[name] {
			continue
		}
		seen[name] = true
		if len(ref) > len(name)+1 && strings.HasPrefix(ref, name+"/") {
			readings = append(readings, mcpToolReading{server: name, tool: ref[len(name)+1:]})
		}
	}
	slices.SortFunc(readings, func(a, b mcpToolReading) int { return strings.Compare(a.server, b.server) })
	return readings
}

// ambiguousMCPToolReference refuses to guess between several readings.
func ambiguousMCPToolReference(ref string, readings []mcpToolReading) string {
	options := make([]string, 0, len(readings))
	for _, reading := range readings {
		options = append(options, fmt.Sprintf("server %s, tool %s", reading.server, reading.tool))
	}
	return fmt.Sprintf("The reference %q is ambiguous: it reads as %s. SelfMind does not guess; rename one of these servers in mcp.servers so each tool has one reference. Nothing was changed.",
		ref, strings.Join(options, ", or "))
}

func (d *Server) markMCPToolReadOnly(ctx context.Context, identity *control.IdentityContext, channel, ref string) (string, error) {
	servers := d.mcpServers()
	names := make([]string, 0, len(servers))
	for _, server := range servers {
		names = append(names, server.Name)
	}
	readings := mcpToolReferenceReadings(ref, names)
	if len(readings) == 0 {
		return fmt.Sprintf("No configured MCP server matches %q. Name one exact tool as <server>/<tool>; /tools readonly lists the servers.", ref), nil
	}
	if len(readings) > 1 {
		return ambiguousMCPToolReference(ref, readings), nil
	}
	serverName, tool := readings[0].server, readings[0].tool
	server := servers[slices.Index(names, serverName)]
	if !server.Connected {
		return fmt.Sprintf("MCP server %s is not connected, so SelfMind cannot check that it exposes %s. Mark it once the server is connected.", serverName, tool), nil
	}
	if !slices.Contains(server.Tools, tool) {
		return fmt.Sprintf("MCP server %s does not expose a tool named %s. Tool names are exact; /tools readonly lists the server.", serverName, tool), nil
	}
	_, created, err := d.Control.MarkMCPToolReadOnly(ctx, control.MCPReadOnlyMarkRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, ServerName: serverName,
		ServerIdentity: server.Identity, ToolName: tool, ActorAccountID: identity.AccountID, ActorChannel: channel,
	})
	if errors.Is(err, control.ErrMCPReadOnlyMarkActor) {
		return "This endpoint cannot change your MCP tool markings.", nil
	}
	if err != nil {
		return "", err
	}
	if !created {
		return fmt.Sprintf("%s/%s is already marked read-only for you.", serverName, tool), nil
	}
	return fmt.Sprintf("Marked %s/%s read-only for you. Its calls now run like other read-only tools: no approval prompt, "+
		"no external effect claim, and they may run alongside other reads. Mark only tools that never change anything. "+
		"Undo with /tools readonly %s/%s off.", serverName, tool, serverName, tool), nil
}

func (d *Server) unmarkMCPToolReadOnly(ctx context.Context, identity *control.IdentityContext, channel, ref string) (string, error) {
	servers := d.mcpServers()
	marks, err := d.Control.ListMCPReadOnlyMarks(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(servers)+len(marks))
	for _, server := range servers {
		names = append(names, server.Name)
	}
	for _, mark := range marks {
		names = append(names, mark.ServerName)
	}
	readings := mcpToolReferenceReadings(ref, names)
	if len(readings) == 0 {
		return fmt.Sprintf("You have no MCP read-only marking matching %q. /tools readonly lists your markings.", ref), nil
	}
	if len(readings) > 1 {
		return ambiguousMCPToolReference(ref, readings), nil
	}
	serverName, tool := readings[0].server, readings[0].tool
	revoked, err := d.Control.UnmarkMCPToolReadOnly(ctx, control.MCPReadOnlyMarkRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, ServerName: serverName, ToolName: tool,
		ActorAccountID: identity.AccountID, ActorChannel: channel,
	})
	if errors.Is(err, control.ErrMCPReadOnlyMarkActor) {
		return "This endpoint cannot change your MCP tool markings.", nil
	}
	if err != nil {
		return "", err
	}
	operatorMarked := false
	for _, server := range servers {
		if server.Name == serverName && slices.Contains(server.OperatorReadOnly, tool) {
			operatorMarked = true
		}
	}
	operator := ""
	if operatorMarked {
		operator = " The operator configuration marks it read-only for everyone; only mcp.servers[].read_only_tools in the config file can change that."
	}
	if revoked == 0 {
		return fmt.Sprintf("You have not marked %s/%s read-only.%s", serverName, tool, operator), nil
	}
	if operatorMarked {
		return fmt.Sprintf("Removed your read-only marking of %s/%s.%s", serverName, tool, operator), nil
	}
	return fmt.Sprintf("Unmarked %s/%s. Its calls are again unclassified external effects, each needing a once-only approval.", serverName, tool), nil
}

func (d *Server) mcpReadOnlyListing(ctx context.Context, identity *control.IdentityContext) (string, error) {
	servers := d.mcpServers()
	marks, err := d.Control.ListMCPReadOnlyMarks(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		return "", err
	}
	if len(servers) == 0 && len(marks) == 0 {
		return "No MCP servers are configured, and you have no MCP read-only markings.", nil
	}
	var lines []string
	listed := make(map[string]bool, len(servers))
	for _, server := range servers {
		listed[server.Name] = true
		state := "connected"
		if !server.Connected {
			state = "not connected"
		}
		lines = append(lines, fmt.Sprintf("%s (%s)", server.Name, state))
		count := len(lines)
		for _, tool := range server.OperatorReadOnly {
			note := ""
			if server.Connected && !slices.Contains(server.Tools, tool) {
				note = " (the server does not expose it; it marks nothing)"
			}
			lines = append(lines, fmt.Sprintf("- %s: operator configuration%s", tool, note))
		}
		for _, mark := range marks {
			if mark.ServerName != server.Name {
				continue
			}
			note := ""
			switch {
			case mark.ServerIdentity != server.Identity:
				note = " (inactive: the server's configuration changed since you marked it; mark it again if the new server's tool only reads)"
			case server.Connected && !slices.Contains(server.Tools, mark.ToolName):
				note = " (the server does not expose it now)"
			}
			lines = append(lines, fmt.Sprintf("- %s: you, %s%s", mark.ToolName, mcpReadOnlyMarkAudit(mark), note))
		}
		if len(lines) == count {
			lines = append(lines, "- none")
		}
	}
	for _, mark := range marks {
		if listed[mark.ServerName] {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s/%s: you, %s (inactive: no MCP server named %s is configured)",
			mark.ServerName, mark.ToolName, mcpReadOnlyMarkAudit(mark), mark.ServerName))
	}
	if len(lines) > mcpReadOnlyListLimit {
		lines = append(lines[:mcpReadOnlyListLimit], fmt.Sprintf("... and %d more", len(lines)-mcpReadOnlyListLimit))
	}
	var sb strings.Builder
	sb.WriteString("MCP tools marked read-only\n")
	sb.WriteString("A read-only tool's calls need no approval, claim no external target, and may run alongside other reads. A server's description of its own tools never counts.\n")
	sb.WriteString(strings.Join(lines, "\n"))
	sb.WriteString("\nMark: /tools readonly <server>/<tool> on   Unmark: /tools readonly <server>/<tool> off")
	return sb.String(), nil
}

func mcpReadOnlyMarkAudit(mark control.MCPReadOnlyMark) string {
	return fmt.Sprintf("marked %s from %s", mark.MarkedAt.UTC().Format("2006-01-02 15:04 UTC"), mark.MarkedChannel)
}

// mcpReadOnlyDiagLines summarizes, for /diag tools, the operator's read-only
// names a server does not expose and the person's markings that no longer
// apply because their server's configuration changed or was removed.
func (d *Server) mcpReadOnlyDiagLines(ctx context.Context, identity *control.IdentityContext) []string {
	var lines []string
	if d.MCPHealthFunc != nil {
		for _, warning := range d.MCPHealthFunc().Warnings {
			lines = append(lines, fmt.Sprintf("MCP %s: %s", warning.Name, warning.Warning))
		}
	}
	if d.Control == nil || identity == nil || identity.PersonID == "" {
		return lines
	}
	marks, err := d.Control.ListMCPReadOnlyMarks(ctx, identity.TenantID, identity.PersonID)
	if errors.Is(err, control.ErrMCPReadOnlyMarksUnavailable) {
		return append(lines, "MCP read-only markings are unavailable: the control database lacks the mcp_read_only_marks table. /tools readonly explains how to restore it.")
	}
	if err != nil || len(marks) == 0 {
		return lines
	}
	identities := make(map[string]string)
	for _, server := range d.mcpServers() {
		identities[server.Name] = server.Identity
	}
	inactive := 0
	for _, mark := range marks {
		if identities[mark.ServerName] != mark.ServerIdentity {
			inactive++
		}
	}
	switch {
	case inactive == 1:
		lines = append(lines, "1 of your MCP read-only markings no longer applies: its server's configuration changed or was removed. See /tools readonly.")
	case inactive > 1:
		lines = append(lines, fmt.Sprintf("%d of your MCP read-only markings no longer apply: their servers' configuration changed or was removed. See /tools readonly.", inactive))
	}
	return lines
}
