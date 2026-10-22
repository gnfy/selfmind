package tools

import (
	"sort"
	"strings"
)

// Only literal standard route variables qualify. Removing PATH, a credential,
// a function, or an expanded name is not bounded environment plumbing.
func proxyUnsetNames(args []string) ([]string, bool) {
	if len(args) > 0 && args[0] == "-v" {
		args = args[1:]
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return nil, false
	}
	for _, name := range args {
		if !standardProxyVariable(name) {
			return nil, false
		}
	}
	return args, true
}

func standardProxyVariable(name string) bool {
	return isProxyRouteVariable(name) && (name == strings.ToUpper(name) || name == strings.ToLower(name)) && name == strings.TrimSpace(name)
}

// env is transparent to observation proof only for exact proxy removals. In
// particular -i, assignments, PATH overrides and split-string remain unknown.
func proxyEnvCommand(args []string) (inner, removed []string, ok bool) {
	for len(args) > 0 {
		arg := args[0]
		args = args[1:]
		var name string
		switch {
		case arg == "-u" || arg == "--unset":
			if len(args) == 0 {
				return nil, nil, false
			}
			name, args = args[0], args[1:]
		case strings.HasPrefix(arg, "--unset="):
			name = strings.TrimPrefix(arg, "--unset=")
		case strings.HasPrefix(arg, "-u"):
			name = strings.TrimPrefix(arg, "-u")
		default:
			if arg == "--" && len(args) > 0 {
				arg, args = args[0], args[1:]
			}
			if len(removed) == 0 || strings.HasPrefix(arg, "-") || strings.Contains(arg, "=") {
				return nil, nil, false
			}
			return append([]string{arg}, args...), removed, true
		}
		if !standardProxyVariable(name) {
			return nil, nil, false
		}
		removed = append(removed, name)
	}
	return nil, nil, false
}

// proxyRemovalVariables names the standard route variables a payload removes,
// through the same identification as the safety floor: behind any known
// wrapper (`exec env -u`, `nohup env -u`), inside -c scripts, by `unset`, by
// `env -u`, or by assigning a variable empty (`HTTPS_PROXY= gh ...`). Unlike
// proof, detection also reads a path-qualified wrapper by its name: reporting
// a removal the file might not perform only asks for review.
func proxyRemovalVariables(payload string) []string {
	seen := map[string]bool{}
	commands, _ := shellCommands(payload, 0)
	for _, read := range commands {
		for _, name := range commandRemovedVariables(read) {
			if standardProxyVariable(name) {
				seen[name] = true
			}
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// commandRemovedVariables lists the variables one command removes. A value
// expanded at run time is not known to be empty, so it is not a removal.
func commandRemovedVariables(read readCommand) []string {
	var names []string
	for _, assignment := range read.id.assignments() {
		if name, value, _ := strings.Cut(assignment, "="); value == "" && !strings.HasSuffix(name, "+") {
			names = append(names, name)
		}
	}
	for _, layer := range read.id.Layers {
		if layer.Name == "env" {
			names = append(names, envUnsetVariables(layer)...)
		}
	}
	if program := read.id.program(); program.Name == "unset" {
		names = append(names, unsetVariables(program.Args)...)
	}
	return names
}

// envUnsetVariables reads env's own -u/--unset options, never the command's,
// with the same option grammar identification uses (`env -iu NAME`,
// `env --uns NAME`, `env -S '-u NAME'`).
func envUnsetVariables(layer commandLayer) []string {
	var names []string
	for _, option := range prefixWrappers["env"].wrapperOptions(layer.Args).options {
		if (option.name == "-u" || option.name == "--unset") && option.hasValue {
			names = append(names, option.value)
		}
	}
	return names
}

// unsetVariables lists the variables `unset` removes; `unset -f` removes
// functions instead.
func unsetVariables(args []string) []string {
	var names []string
	for _, arg := range args {
		switch {
		case arg == "-f":
			return nil
		case strings.HasPrefix(arg, "-"):
		default:
			names = append(names, arg)
		}
	}
	return names
}

func configuredProxyOverrideReason(toolName string, args map[string]interface{}) string {
	if !isExecTool(toolName) || !commandWantsNetwork(toolName, args) {
		return ""
	}
	removed := proxyRemovalVariables(execCommandPayload(toolName, args))
	if len(removed) == 0 {
		return ""
	}
	configured := map[string]bool{}
	for _, entry := range leaseProcessEnv(args) {
		name, value, _ := strings.Cut(entry, "=")
		if strings.TrimSpace(value) != "" {
			configured[name] = true
		}
	}
	var affected []string
	for _, name := range removed {
		if configured[name] {
			affected = append(affected, name)
		}
	}
	if len(affected) == 0 {
		return ""
	}
	return "this command removes configured proxy variables (" + strings.Join(affected, ", ") + ") and changes the operator's network route; assess whether direct access is supported by the user's request or current diagnostic evidence. The runtime already preserves reachable proxies and omits unusable loopback proxies per invocation"
}
