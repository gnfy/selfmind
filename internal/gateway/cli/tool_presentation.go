package cli

import (
	"fmt"
	"path/filepath"
	"strings"
)

const (
	maxToolHeaderRows    = 2
	maxCommandOutputRows = 5
)

// commandToolAction converts shell syntax into a compact user-facing action.
// Recognized commands get semantic labels; unknown commands retain a bounded
// first command instead of inventing intent.
func commandToolAction(command string, done bool) string {
	command = strings.TrimSpace(sanitizeTerminalText(command))
	if command == "" {
		if done {
			return "Ran command"
		}
		return "Running command"
	}
	if kind := heredocKind(command); kind != "" {
		if done {
			return "Ran " + kind
		}
		return "Running " + kind
	}

	line := firstMeaningfulCommand(command)
	fields := strings.Fields(line)
	if len(fields) == 0 {
		if done {
			return "Ran command"
		}
		return "Running command"
	}
	base := strings.ToLower(filepath.Base(fields[0]))
	arg := ""
	if len(fields) > 1 {
		arg = strings.ToLower(fields[1])
	}
	present, past := "Running command", "Ran command"
	switch base {
	case "go":
		switch arg {
		case "test":
			present, past = "Running tests", "Ran tests"
		case "build":
			present, past = "Building Go project", "Built Go project"
		case "run":
			present, past = "Running Go program", "Ran Go program"
		}
	case "git":
		switch arg {
		case "status":
			present, past = "Checking repository status", "Checked repository status"
		case "log":
			present, past = "Inspecting recent commits", "Inspected recent commits"
		case "diff", "show":
			present, past = "Inspecting repository changes", "Inspected repository changes"
		case "grep":
			present, past = "Searching repository", "Searched repository"
		}
	case "rg", "grep", "egrep", "fgrep":
		present, past = "Searching files", "Searched files"
	case "find", "fd":
		present, past = "Searching workspace", "Searched workspace"
	case "ls", "dir", "pwd":
		present, past = "Inspecting workspace", "Inspected workspace"
	case "cat", "head", "tail", "sed":
		present, past = "Reading command output", "Read command output"
	case "python", "python3", "py":
		present, past = "Running Python command", "Ran Python command"
	case "bash", "sh", "zsh", "pwsh", "powershell":
		if len(fields) > 1 && !strings.HasPrefix(fields[1], "-") {
			name := filepath.Base(strings.Trim(fields[1], "'\""))
			present, past = "Running "+name, "Ran "+name
		}
	case "gcloud":
		present, past = "Running Google Cloud command", "Ran Google Cloud command"
	case "aws":
		present, past = "Running AWS command", "Ran AWS command"
	case "argocd":
		present, past = "Running Argo CD command", "Ran Argo CD command"
	case "kubectl":
		present, past = "Running Kubernetes command", "Ran Kubernetes command"
	default:
		compact := truncateToWidth(strings.Join(strings.Fields(line), " "), 88)
		present, past = "Running "+compact, "Ran "+compact
	}
	if done {
		return past
	}
	return present
}

func heredocKind(command string) string {
	marker := strings.Index(command, "<<")
	if marker < 0 {
		return ""
	}
	fields := strings.Fields(strings.TrimSpace(command[:marker]))
	if len(fields) == 0 {
		return "script"
	}
	base := strings.ToLower(filepath.Base(fields[0]))
	switch base {
	case "python", "python3", "py":
		return "Python script"
	case "node", "nodejs":
		return "Node.js script"
	case "bash", "sh", "zsh":
		return "shell script"
	case "pwsh", "powershell":
		return "PowerShell script"
	default:
		return "script"
	}
}

func firstMeaningfulCommand(command string) string {
	for _, line := range strings.Split(command, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "set ") {
			continue
		}
		for _, sep := range []string{" && ", " || ", ";"} {
			if idx := strings.Index(line, sep); idx >= 0 {
				line = strings.TrimSpace(line[:idx])
			}
		}
		return line
	}
	return "command"
}

// boundedCommandOutputRows returns a head/tail physical-row preview. Compiler
// and shell errors usually end with the actionable diagnostic, so the tail is
// more useful than a head-only preview.
func boundedCommandOutputRows(content string, width, limit int) []string {
	rows := physicalDisplayLines(content, width)
	if len(rows) <= limit || limit < 3 {
		return rows
	}
	head := (limit - 1) / 2
	tail := limit - 1 - head
	hidden := len(rows) - head - tail
	out := append([]string(nil), rows[:head]...)
	out = append(out, fmt.Sprintf("... %d more lines", hidden))
	out = append(out, rows[len(rows)-tail:]...)
	return out
}
