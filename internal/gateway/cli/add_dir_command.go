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

// describeAdditionalRoot renders one extra root against the workspace it
// extends. A bare absolute path says where the directory is but not what it
// does to this session's reach, which is the part worth knowing: a root that
// contains the workspace widens it, one already inside it adds nothing, and a
// neighbour is best read as the hop from the workspace to it.
// describeAdditionalRoot is the detail form, used where the person deliberately
// asked: the absolute path is the truth, and the parenthetical says how the root
// sits against the workspace it extends.
func describeAdditionalRoot(root, workspacePath, workspaceName string) string {
	form := addDirFlagForm(root, workspacePath)
	root = filepath.Clean(strings.TrimSpace(root))
	workspacePath = filepath.Clean(strings.TrimSpace(workspacePath))
	if workspacePath == "" || workspacePath == "." || root == "" {
		return root
	}
	if workspaceName = strings.TrimSpace(workspaceName); workspaceName == "" {
		workspaceName = filepath.Base(workspacePath)
	}
	switch {
	case root == workspacePath:
		return root + "  (the workspace itself)"
	case pathContains(workspacePath, root):
		return root + "  (inside " + workspaceName + ")"
	case pathContains(root, workspacePath):
		return root + "  (contains " + workspaceName + ")"
	case form != root:
		return root + "  (" + form + ")"
	}
	return root
}

// addDirFlagForm is the shortest honest way to write one root: exactly what
// could be passed back to --add-dir. A neighbour reads best as the hop from the
// workspace; a distant tree does not, because "../../../../etc/ssl" is longer
// and less clear than the path itself.
func addDirFlagForm(root, workspacePath string) string {
	root = filepath.Clean(strings.TrimSpace(root))
	workspacePath = filepath.Clean(strings.TrimSpace(workspacePath))
	if workspacePath == "" || workspacePath == "." || root == "" {
		return root
	}
	if relative, err := filepath.Rel(workspacePath, root); err == nil && len(relative) < len(root) {
		return relative
	}
	return root
}

// additionalRootReach names the directories this session reaches beyond its
// workspace, each in the shortest form that still says where it sits. Empty
// when the session carries none, so the card stays as it was.
func (m *uiModel) additionalRootReach() string {
	if len(m.additionalRoots) == 0 {
		return ""
	}
	workspacePath := m.workspaceOverridePath
	if workspacePath == "" && m.sessionWorkspace != nil {
		workspacePath = m.sessionWorkspace.Path
	}
	forms := make([]string, 0, len(m.additionalRoots))
	for _, root := range m.additionalRoots {
		forms = append(forms, addDirFlagForm(root, workspacePath))
	}
	return strings.Join(forms, "  ")
}

// pathContains reports whether child sits under parent, comparing whole path
// segments so /repo does not appear to contain /repo-fork.
func pathContains(parent, child string) bool {
	if parent == child {
		return false
	}
	return strings.HasPrefix(child, strings.TrimSuffix(parent, string(filepath.Separator))+string(filepath.Separator))
}

func (m *uiModel) additionalRootDescriptions() []string {
	workspacePath, workspaceName := m.workspaceOverridePath, m.workspaceOverrideName
	if workspacePath == "" && m.sessionWorkspace != nil {
		workspacePath, workspaceName = m.sessionWorkspace.Path, m.sessionWorkspace.Name
	}
	out := make([]string, 0, len(m.additionalRoots))
	for _, root := range m.additionalRoots {
		out = append(out, describeAdditionalRoot(root, workspacePath, workspaceName))
	}
	return out
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
		for _, described := range m.additionalRootDescriptions() {
			fmt.Fprintf(&sb, "\n  %s", described)
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
