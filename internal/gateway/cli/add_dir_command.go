package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// maxSessionAdditionalRoots mirrors the gateway's own bound
// (httpapi.maxClientAdditionalRoots). Refusing here means the person is told
// which directory was rejected instead of having the whole turn refused later.
const maxSessionAdditionalRoots = 8

// handleAddDir backs the /add-dir command: grant this session access to another
// directory without restarting.
//
// It deliberately carries the SAME name as the `--add-dir` flag rather than a
// new verb. The flag already had to be learned; a session command that means
// the same thing should not cost a second name. Bare `/add-dir` lists what the
// session currently carries.
//
// The overlay stays invocation-local: the gateway validates and freezes it per
// run, so this only decides what THIS terminal offers, never what a workspace
// durably allows.
func (m *uiModel) handleAddDir(args []string) tea.Cmd {
	path := strings.TrimSpace(strings.Join(args, " "))
	if path == "" {
		return m.reportAddDirState("")
	}
	expanded, err := expandAddDirPath(path)
	if err != nil {
		m.addMessage("assistant", err.Error())
		return nil
	}
	for _, existing := range m.additionalRoots {
		if existing == expanded {
			m.addMessage("assistant", fmt.Sprintf("Already available this session: %s", expanded))
			return nil
		}
	}
	if len(m.additionalRoots) >= maxSessionAdditionalRoots {
		m.addMessage("assistant", fmt.Sprintf(
			"This session already carries %d extra directories, the maximum. Restart with the directories you need, or use /ws to switch workspace.",
			maxSessionAdditionalRoots))
		return nil
	}
	m.additionalRoots = append(m.additionalRoots, expanded)
	return m.reportAddDirState(expanded)
}

func (m *uiModel) reportAddDirState(added string) tea.Cmd {
	var sb strings.Builder
	if added != "" {
		fmt.Fprintf(&sb, "Added for this session: %s\n", added)
	}
	if len(m.additionalRoots) == 0 {
		sb.WriteString("No extra directories in this session. Use /add-dir <path> to add one.")
	} else {
		sb.WriteString("Extra directories this session:")
		for _, root := range m.additionalRoots {
			fmt.Fprintf(&sb, "\n  %s", root)
		}
	}
	m.addMessage("assistant", sb.String())
	return nil
}

// expandAddDirPath resolves the path the person typed the way they meant it: a
// leading ~ is their home, a relative path is relative to where this session
// was started, and the result must actually be a directory. Refusing here is
// kinder than letting a typo become an unexplained tool refusal mid-turn.
func expandAddDirPath(path string) (string, error) {
	if strings.HasPrefix(path, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot resolve %s: no home directory", path)
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(currentWorkingDir(), path)
	}
	resolved, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("cannot resolve %s: %v", path, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("not added: %s does not exist", resolved)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not added: %s is a file, not a directory", resolved)
	}
	return resolved, nil
}
