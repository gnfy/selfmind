package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/atotto/clipboard"
)

func copyToClipboard(text string) error {
	if err := copyToPlatformClipboard(text); err == nil {
		return nil
	} else if isProbablyWSL() {
		// In WSL, generic Linux fallbacks can route through clip.exe without
		// a UTF-8 console setup and corrupt CJK text. Prefer a loud error.
		return err
	}
	return clipboard.WriteAll(text)
}

func copyToPlatformClipboard(text string) error {
	switch runtime.GOOS {
	case "windows":
		return copyToWindowsClipboardUTF8(text, "powershell.exe")
	case "darwin":
		return runClipboardCommand(text, "pbcopy")
	case "linux":
		if isProbablyWSL() {
			return copyToWindowsClipboardUTF8(text, "powershell.exe")
		}
		return copyToUnixClipboard(text)
	case "freebsd", "openbsd", "netbsd":
		return copyToUnixClipboard(text)
	default:
		return fmt.Errorf("no platform clipboard command for %s", runtime.GOOS)
	}
}

func isProbablyWSL() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if os.Getenv("WSL_DISTRO_NAME") != "" || os.Getenv("WSL_INTEROP") != "" {
		return true
	}
	version, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	lower := strings.ToLower(string(version))
	return strings.Contains(lower, "microsoft") || strings.Contains(lower, "wsl")
}

func copyToWindowsClipboardUTF8(text, command string) error {
	var stderr bytes.Buffer
	cmd := exec.Command(
		command,
		"-NoProfile",
		"-Command",
		"[Console]::InputEncoding = [System.Text.Encoding]::UTF8; $ErrorActionPreference = 'Stop'; $text = [Console]::In.ReadToEnd(); Set-Clipboard -Value $text",
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	cmd.Stdin = strings.NewReader(text)
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return fmt.Errorf("%s clipboard copy failed: %w", command, err)
		}
		return fmt.Errorf("%s clipboard copy failed: %s", command, msg)
	}
	return nil
}

func copyToUnixClipboard(text string) error {
	candidates := []struct {
		name string
		args []string
	}{
		{name: "wl-copy"},
		{name: "xclip", args: []string{"-selection", "clipboard"}},
		{name: "xsel", args: []string{"--clipboard", "--input"}},
		{name: "termux-clipboard-set"},
	}
	var errs []string
	for _, candidate := range candidates {
		if _, err := exec.LookPath(candidate.name); err != nil {
			errs = append(errs, candidate.name+": not found")
			continue
		}
		if err := runClipboardCommand(text, candidate.name, candidate.args...); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		return nil
	}
	if len(errs) == 0 {
		return fmt.Errorf("no clipboard command found")
	}
	return fmt.Errorf("%s", strings.Join(errs, "; "))
}

func runClipboardCommand(text, name string, args ...string) error {
	var stderr bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	cmd.Stdin = strings.NewReader(text)
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return fmt.Errorf("%s clipboard copy failed: %w", name, err)
		}
		return fmt.Errorf("%s clipboard copy failed: %s", name, msg)
	}
	return nil
}
