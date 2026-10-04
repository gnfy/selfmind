package tools

import (
	"sort"
	"strings"

	"mvdan.cc/sh/v3/syntax"
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

func proxyRemovalVariables(payload string) []string {
	seen := map[string]bool{}
	var inspect func(string, int)
	var inspectArgv func([]string, int)
	inspectArgv = func(argv []string, depth int) {
		if depth > maxObservationShellDepth || len(argv) == 0 {
			return
		}
		var removed []string
		switch argv[0] {
		case "unset":
			removed, _ = proxyUnsetNames(argv[1:])
		case "env":
			inner, names, valid := proxyEnvCommand(argv[1:])
			removed = names
			if valid {
				inspectArgv(inner, depth+1)
			}
		case "command", "timeout":
			if inner, valid := observationWrappedCommand(argv[0], argv[1:]); valid {
				inspectArgv(inner, depth+1)
			}
		default:
			if _, shell := shellDashCWrappers[argv[0]]; shell {
				if script, valid := exactDashCScript(argv[1:]); valid {
					inspect(script, depth+1)
				}
			}
		}
		for _, name := range removed {
			seen[name] = true
		}
	}
	inspect = func(command string, depth int) {
		if depth > maxObservationShellDepth {
			return
		}
		file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
		if err != nil {
			return
		}
		syntax.Walk(file, func(node syntax.Node) bool {
			call, ok := node.(*syntax.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			var argv []string
			for _, word := range call.Args {
				value, static := staticObservationWord(word)
				if !static {
					return true
				}
				argv = append(argv, value)
			}
			inspectArgv(argv, depth)
			return true
		})
	}
	inspect(payload, 0)
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func configuredProxyOverrideReason(toolName string, args map[string]interface{}) string {
	if !isExecTool(toolName) || !commandPlausiblyNeedsEgress(toolName, args) {
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
