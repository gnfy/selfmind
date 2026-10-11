// Package scratchhome provides throwaway per-user home directories. SelfMind
// derives its state, caches and configuration from the per-user directories
// (HOME, and on Linux the XDG base directories) unless SELF_CONFIG names the
// config file. A process that must leave the operator's real state alone — a
// test binary, or a child that selfcheck starts — runs with those variables
// pointed at a scratch home instead.
package scratchhome

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// persistentTemp survives reboots and is neither /tmp nor the default system
// temp directory on either supported platform.
const persistentTemp = "/var/tmp"

// Parent returns the directory that holds scratch homes. It prefers the
// persistent temp directory: an execution sandbox binds a private /tmp over
// the host's, which would hide a home beneath it, and the execution runtime
// root — which tests place under HOME, as a person's daemon does — refuses the
// system temp directory. The system temp directory is the fallback when the
// persistent one is missing or lies inside the system temp directory.
func Parent() string {
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(persistentTemp); err == nil && info.IsDir() && !within(os.TempDir(), persistentTemp) {
			return persistentTemp
		}
	}
	return os.TempDir()
}

// New creates a private scratch home whose name starts with prefix and
// returns its canonical path.
func New(prefix string) (string, error) {
	home, err := os.MkdirTemp(Parent(), prefix)
	if err != nil && Parent() != os.TempDir() {
		home, err = os.MkdirTemp("", prefix)
	}
	if err != nil {
		return "", err
	}
	return prepare(home)
}

// Claim is a scratch home that one run holds until Release.
type Claim struct {
	// Home is the claimed home's canonical path.
	Home string
	// Busy reports that another run held the stable path.
	Busy bool
	// Fallback says why Home is not the stable path; empty when it is.
	Fallback string
	release  func() error
}

// Release removes the home and gives up the stable path.
func (c *Claim) Release() error { return c.release() }

// Fresh claims, for this run, a private scratch home at a stable path named
// name under Parent. A stable path keeps HOME-derived cache keys stable — Go's
// test cache records HOME whenever a test reads it — so a repeated run still
// recognizes unchanged packages; whatever an interrupted run of this user left
// there is removed first.
//
// The path is predictable and its parent shared, so Fresh uses it only when
// it is safe and free: the parent is sticky and owned by root or this user,
// the exclusive lock beside the home is this user's own file and no other run
// holds it, and anything already at the path is this user's own directory.
// Otherwise Fresh claims a new randomly named home, which costs only the cache
// benefit, and the Claim says why.
func Fresh(name string) (*Claim, error) {
	return fresh(Parent(), name, uint32(os.Getuid()), fileOwner)
}

func fresh(parent, name string, me uint32, ownerOf func(fs.FileInfo) (uint32, bool)) (*Claim, error) {
	if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
		return nil, fmt.Errorf("scratch home name %q must be a single path element", name)
	}
	if err := trustParent(parent, me, ownerOf); err != nil {
		return claimNew(name, false, err)
	}
	unlock, busy, err := lockStable(filepath.Join(parent, name+".lock"), me, ownerOf)
	if err != nil {
		return claimNew(name, false, err)
	}
	if busy {
		return claimNew(name, true, fmt.Errorf("another run holds %s", filepath.Join(parent, name)))
	}
	home, err := makeStable(filepath.Join(parent, name), me, ownerOf)
	if err != nil {
		unlock()
		return claimNew(name, false, err)
	}
	return &Claim{Home: home, release: func() error {
		err := Remove(home)
		unlock()
		return err
	}}, nil
}

// trustParent accepts a shared parent only when it keeps other users from
// removing or replacing what this user creates there: sticky, and owned by
// root or this user.
func trustParent(parent string, me uint32, ownerOf func(fs.FileInfo) (uint32, bool)) error {
	info, err := os.Stat(parent)
	if err != nil {
		return err
	}
	owner, known := ownerOf(info)
	switch {
	case !info.IsDir():
		return fmt.Errorf("%s is not a directory", parent)
	case info.Mode()&fs.ModeSticky == 0:
		return fmt.Errorf("%s is not sticky", parent)
	case !known:
		return fmt.Errorf("cannot tell who owns %s", parent)
	case owner != 0 && owner != me:
		return fmt.Errorf("%s belongs to uid %d", parent, owner)
	}
	return nil
}

// makeStable empties the stable path and creates it afresh. Only this user's
// own directory is removed; anything else found there is left alone. The new
// directory is checked again once made, before anything is put in it.
func makeStable(home string, me uint32, ownerOf func(fs.FileInfo) (uint32, bool)) (string, error) {
	if info, err := os.Lstat(home); err == nil {
		if err := notOwned(home, info, me, ownerOf, true); err != nil {
			return "", err
		}
		if err := Remove(home); err != nil {
			return "", fmt.Errorf("clear %s: %w", home, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	// Mkdir, not MkdirAll: a path recreated since the removal makes this
	// fail instead of being adopted.
	if err := os.Mkdir(home, 0o700); err != nil {
		return "", err
	}
	info, err := os.Lstat(home)
	if err == nil {
		err = notOwned(home, info, me, ownerOf, true)
	}
	if err == nil && info.Mode().Perm() != 0o700 {
		err = fmt.Errorf("%s has mode %o, not 700", home, info.Mode().Perm())
	}
	if err != nil {
		return "", err
	}
	return prepare(home)
}

// claimNew claims a new randomly named home in place of the stable one.
func claimNew(name string, busy bool, why error) (*Claim, error) {
	home, err := New(name + "-")
	if err != nil {
		return nil, err
	}
	return &Claim{Home: home, Busy: busy, Fallback: why.Error(), release: func() error { return Remove(home) }}, nil
}

// notOwned refuses path unless it is this user's own directory (wantDir) or
// regular file, never a symbolic link.
func notOwned(path string, info fs.FileInfo, me uint32, ownerOf func(fs.FileInfo) (uint32, bool), wantDir bool) error {
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("%s is a symbolic link", path)
	case wantDir && !info.IsDir():
		return fmt.Errorf("%s is not a directory", path)
	case !wantDir && !info.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file", path)
	}
	owner, ok := ownerOf(info)
	if !ok {
		return fmt.Errorf("cannot tell who owns %s", path)
	}
	if owner != me {
		return fmt.Errorf("%s belongs to uid %d, not %d", path, owner, me)
	}
	return nil
}

// prepare canonicalizes a new home and turns Go telemetry off in it. A `go`
// run under a home without telemetry state records counters there and may
// start a detached upload process that outlives it, recreating the home
// after Remove. Telemetry written to a throwaway home would be deleted
// anyway, so nothing is lost. The mode file is the one `go telemetry off`
// writes, below the user config directory Env yields on this platform.
func prepare(home string) (string, error) {
	resolved, err := canonical(home)
	if err == nil {
		configDir := filepath.Join(resolved, ".config")
		if runtime.GOOS == "darwin" {
			configDir = filepath.Join(resolved, "Library", "Application Support")
		}
		modeFile := filepath.Join(configDir, "go", "telemetry", "mode")
		if err = os.MkdirAll(filepath.Dir(modeFile), 0o700); err == nil {
			err = os.WriteFile(modeFile, []byte("off\n"), 0o600)
		}
	}
	if err != nil {
		_ = Remove(home)
		return "", err
	}
	return resolved, nil
}

// Env returns the assignments that root every per-user location in home the
// way a fresh account would lay it out: HOME, the XDG base directories that
// Linux prefers over HOME (os.UserConfigDir, os.UserCacheDir), and an empty
// SELF_CONFIG so the config file resolves to its per-user default inside home.
func Env(home string) []string {
	return []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"),
		"SELF_CONFIG=",
	}
}

// Remove deletes a scratch home and everything in it. Tools leave read-only
// directories behind (a Go module cache is read-only by design), so after a
// failed removal it makes the tree's directories writable by their owner and
// tries once more. WalkDir does not follow symbolic links.
func Remove(home string) error {
	if err := os.RemoveAll(home); err == nil {
		return nil
	}
	_ = filepath.WalkDir(home, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
	return os.RemoveAll(home)
}

// goDirVars are the Go tool's own per-user locations: build cache, module
// cache, GOPATH, and the settings file that `go env -w` writes (GOPROXY,
// GOTOOLCHAIN, GOFLAGS, ...). All of them default to paths under HOME.
var goDirVars = []string{"GOCACHE", "GOMODCACHE", "GOPATH", "GOENV"}

// GoDirs reports, as assignments, where the Go tool keeps its own state for
// environ. Resolved for the original environment and applied after HOME
// moves, they keep a child `go` on the developer's caches and settings instead
// of rebuilding and re-downloading into a scratch home. They are Go's, not
// SelfMind's, so they need no isolation.
func GoDirs(ctx context.Context, goBin string, environ []string) ([]string, error) {
	cmd := exec.CommandContext(ctx, goBin, append([]string{"env", "-json"}, goDirVars...)...)
	cmd.Env = environ
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(bytes.TrimSpace(exitErr.Stderr)) > 0 {
			return nil, fmt.Errorf("go env: %w: %s", err, bytes.TrimSpace(exitErr.Stderr))
		}
		return nil, fmt.Errorf("go env: %w", err)
	}
	var values map[string]string
	if err := json.Unmarshal(out, &values); err != nil {
		return nil, fmt.Errorf("go env: decode: %w", err)
	}
	pins := make([]string, 0, len(goDirVars))
	for _, key := range goDirVars {
		if value := strings.TrimSpace(values[key]); value != "" {
			pins = append(pins, key+"="+value)
		}
	}
	return pins, nil
}

// canonical resolves symlinks in a home path (on macOS /var is a link to
// /private/var), so paths derived from HOME compare equal to canonicalized
// ones.
func canonical(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}
