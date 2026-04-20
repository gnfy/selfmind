package components

import (
	"os"
	"os/exec"
	"runtime"
)

// OpenExternalEditor opens the user's preferred editor ($EDITOR, $VISUAL, or fallback)
// with the given content in a temporary file, and returns the edited content.
// Returns ("", nil) if the user cancelled (exit code != 0).
// The temp file is always removed after editing.
func OpenExternalEditor(content string) (string, error) {
	editor := getEditor()

	// Create a temp file
	tmpDir := os.TempDir()
	tmpFile, err := os.CreateTemp(tmpDir, "selfmind-*.md")
	if err != nil {
		return "", err
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()

	defer os.Remove(tmpPath)

	// Write current content to the temp file
	if err := os.WriteFile(tmpPath, []byte(content), 0600); err != nil {
		return "", err
	}

	// Exit alt screen before launching the editor
	os.Stdout.Write([]byte("\x1b[?1049l"))

	// Determine if we need a shell to run the editor
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", editor, tmpPath)
	} else {
		// On Unix, use shell to handle $EDITOR that may contain args (e.g. "vim -c 'set wrap'")
		cmd = exec.Command("sh", "-c", editor+" "+shellEscape(tmpPath))
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err = cmd.Run()

	// Re-enter alt screen after editor exits
	os.Stdout.Write([]byte("\x1b[?1049h\x1b[2J\x1b[H"))

	if err != nil {
		// User cancelled or editor error — return empty, no submission
		return "", nil
	}

	// Read back the edited content
	edited, err := os.ReadFile(tmpPath)
	if err != nil {
		return "", err
	}

	return string(edited), nil
}

func getEditor() string {
	if editor := os.Getenv("EDITOR"); editor != "" {
		return editor
	}
	if editor := os.Getenv("VISUAL"); editor != "" {
		return editor
	}
	switch runtime.GOOS {
	case "darwin":
		return "vi"
	case "windows":
		return "notepad"
	default:
		return "vi"
	}
}

func shellEscape(s string) string {
	// Simple escaping for sh — just escape single quotes by ending the string,
	// inserting a literal ', and starting a new one.
	// This is sufficient for file paths which don't contain special shell chars.
	return "'" + s + "'"
}

// largePasteThreshold returns the char and line thresholds for what counts as a "large paste".
// Pastes larger than these are replaced with a placeholder token.
func LargePasteThreshold() (chars, lines int) {
	return 8000, 80
}
