package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/atotto/clipboard"
)

// clipboardImageToFile reads an image from the OS clipboard (a pasted/copied
// screenshot), writes it to a temp PNG, and returns the local path. It returns
// an error when the clipboard holds no image or no reader is available.
//
// IMPORTANT: this only works where a GUI clipboard is reachable from this
// process — running the TUI locally (incl. WSL reading the Windows clipboard).
// Over SSH to a headless server there is no clipboard, so this fails cleanly and
// the user should drag/type an image path instead (or send via IM).
func clipboardImageToFile() (string, error) {
	dst := filepath.Join(os.TempDir(), fmt.Sprintf("selfmind-paste-%d.png", time.Now().UnixNano()))
	switch {
	case isProbablyWSL():
		return clipboardImageWindows(dst, true)
	case runtime.GOOS == "windows":
		return clipboardImageWindows(dst, false)
	case runtime.GOOS == "darwin":
		return clipboardImageDarwin(dst)
	case runtime.GOOS == "linux", runtime.GOOS == "freebsd", runtime.GOOS == "openbsd", runtime.GOOS == "netbsd":
		return clipboardImageUnix(dst)
	default:
		return "", fmt.Errorf("clipboard image paste is not supported on %s", runtime.GOOS)
	}
}

// clipboardImageWindows uses PowerShell to save the clipboard image. On WSL the
// file is saved to the Windows temp dir and the path is translated back to a WSL
// path via `wslpath`, so the running Linux process can read it. The file name
// comes from dst's unique timestamped base name so a second paste never
// overwrites the first (attachments are read lazily at submit/inspect time).
func clipboardImageWindows(dst string, wsl bool) (string, error) {
	ps := `Add-Type -AssemblyName System.Windows.Forms,System.Drawing; ` +
		`$img=[System.Windows.Forms.Clipboard]::GetImage(); ` +
		`if($img -eq $null){ Write-Output 'NOIMAGE'; exit 0 }; ` +
		fmt.Sprintf(`$p=[System.IO.Path]::Combine($env:TEMP, '%s'); `, filepath.Base(dst)) +
		`$img.Save($p, [System.Drawing.Imaging.ImageFormat]::Png); Write-Output $p`
	out, err := exec.Command("powershell.exe", "-NoProfile", "-Command", ps).Output()
	if err != nil {
		return "", fmt.Errorf("read clipboard image via powershell: %w", err)
	}
	winPath := strings.TrimSpace(string(out))
	if winPath == "" || winPath == "NOIMAGE" {
		return "", fmt.Errorf("no image on the clipboard")
	}
	if !wsl {
		return winPath, nil
	}
	conv, err := exec.Command("wslpath", "-u", winPath).Output()
	if err != nil {
		return "", fmt.Errorf("translate windows path: %w", err)
	}
	return strings.TrimSpace(string(conv)), nil
}

func clipboardImageDarwin(dst string) (string, error) {
	// AppleScript writes the clipboard PNG to dst when present.
	script := fmt.Sprintf(`set p to POSIX file %q
try
	set d to (the clipboard as «class PNGf»)
on error
	return "NOIMAGE"
end try
set fh to open for access p with write permission
set eof fh to 0
write d to fh
close access fh
return "OK"`, dst)
	out, err := exec.Command("osascript", "-e", script).Output()
	if err != nil {
		return "", fmt.Errorf("read clipboard image via osascript: %w", err)
	}
	if strings.TrimSpace(string(out)) != "OK" {
		return "", fmt.Errorf("no image on the clipboard")
	}
	return dst, nil
}

func clipboardImageUnix(dst string) (string, error) {
	candidates := []struct {
		name string
		args []string
	}{
		{"wl-paste", []string{"--type", "image/png"}},
		{"xclip", []string{"-selection", "clipboard", "-t", "image/png", "-o"}},
	}
	var lastErr error
	for _, c := range candidates {
		if _, err := exec.LookPath(c.name); err != nil {
			continue
		}
		out, err := exec.Command(c.name, c.args...).Output()
		if err != nil {
			lastErr = err
			continue
		}
		if len(out) < 8 { // not a real image payload
			lastErr = fmt.Errorf("no image on the clipboard")
			continue
		}
		if err := os.WriteFile(dst, out, 0o644); err != nil {
			return "", err
		}
		return dst, nil
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("no clipboard image reader found (install wl-clipboard or xclip)")
}

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
