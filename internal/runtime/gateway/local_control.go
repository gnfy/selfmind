package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"selfmind/internal/gateway/weixin"
	"selfmind/internal/platform/config"
	"selfmind/internal/tools/sandbox"
)

// Local control channel.
//
// The person's own CLI and TUI reach the daemon over a Unix domain socket in a
// directory only the daemon's user can enter. The daemon identifies each
// connecting process from kernel peer credentials and grants CLI identity only
// to a process of its own user that it did not start (internal/platform/peercred).
// The TCP listener stays for IM webhooks and HTTP integrations and never
// carries CLI identity. There is no shared secret: the earlier local-control
// token file could be read by any tool running as the same user.

const (
	localControlSocketName = "control.sock"
	// maxSocketPathLen is the portable sun_path limit: 104 bytes on macOS
	// including the terminating NUL (108 on Linux).
	maxSocketPathLen = 103
	// legacyLocalControlTokenName is the retired shared-secret file.
	legacyLocalControlTokenName = "local_control.token"
	socketFallbackRoot          = "/tmp"
)

// LocalControlSocketPath is where the daemon for dataDir accepts its person's
// CLI. It lives in the daemon's private runtime directory unless that path is
// too long for a socket address; then it moves to a per-user directory under
// /tmp named by a hash of the data directory, so daemon and CLI derive the same
// path independently.
func LocalControlSocketPath(dataDir string) string {
	primary := filepath.Join(dataDir, "gateway", localControlSocketName)
	if len(primary) <= maxSocketPathLen {
		return primary
	}
	absolute, err := filepath.Abs(dataDir)
	if err != nil {
		absolute = dataDir
	}
	sum := sha256.Sum256([]byte(filepath.Clean(absolute)))
	return filepath.Join(socketFallbackRoot, "selfmind-"+strconv.Itoa(os.Getuid()), hex.EncodeToString(sum[:8])+".sock")
}

// ErrLocalControlUnavailable reports that no daemon answers on the local
// control socket.
var ErrLocalControlUnavailable = errors.New("local control socket unavailable")

// LocalControlDirState classifies the directory that holds the socket.
type LocalControlDirState int

const (
	// LocalControlDirPrivate is a directory only this user can enter.
	LocalControlDirPrivate LocalControlDirState = iota
	// LocalControlDirMissing means no daemon has opened a socket here.
	LocalControlDirMissing
	// LocalControlDirUnsafe is a directory another user owns or can enter; the
	// client never trusts a socket in it.
	LocalControlDirUnsafe
)

// InspectLocalControlDir reports the state of the socket directory for dataDir
// and a one-line description, for diagnostics.
func InspectLocalControlDir(dataDir string) (LocalControlDirState, string) {
	return inspectSocketDir(filepath.Dir(LocalControlSocketPath(dataDir)))
}

// HasLegacyLocalControlToken reports whether the retired shared-secret file is
// still on disk. Current daemons delete it at startup, so its presence means a
// daemon older than the local control socket wrote it.
func HasLegacyLocalControlToken(dataDir string) bool {
	_, err := os.Stat(filepath.Join(dataDir, "gateway", legacyLocalControlTokenName))
	return err == nil
}

// LocalControlClient sends every request to the daemon's local control socket
// for dataDir, whatever host the request URL names. There is no TCP fallback:
// CLI identity exists only on this channel. The socket directory is checked
// before each dial so a socket planted by another user is never trusted.
func LocalControlClient(dataDir string) *http.Client {
	path := LocalControlSocketPath(dataDir)
	return &http.Client{Transport: &http.Transport{
		// No proxy: the request never leaves this host.
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if err := checkPrivateSocketDir(filepath.Dir(path)); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrLocalControlUnavailable, err)
			}
			var dialer net.Dialer
			conn, err := dialer.DialContext(ctx, "unix", path)
			if err != nil {
				return nil, fmt.Errorf("%w at %s: %v (restart the daemon with `selfmind gateway restart` if it predates the local control socket)", ErrLocalControlUnavailable, path, err)
			}
			return conn, nil
		},
		MaxIdleConnsPerHost: 4,
	}}
}

// listenLocalControl opens the socket. The daemon holds gateway.lock, so a
// socket file already at the path was left by a daemon that is gone; any
// other kind of file is never replaced.
func listenLocalControl(path string) (net.Listener, error) {
	if err := preparePrivateSocketDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("refusing to replace %s: it is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale local control socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on local control socket %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}

// removeLegacyLocalControlToken deletes the retired shared secret so a copy
// left by an older daemon is not mistaken for authority.
func removeLegacyLocalControlToken(dataDir string) error {
	err := os.Remove(filepath.Join(ResolvePaths(dataDir).RuntimeDir, legacyLocalControlTokenName))
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// daemonPrivateState lists what no confined tool may reach, derived from the
// data-directory layout, the configuration the daemon loaded, and its own
// listeners: the runtime directory (state, lock, log, socket) and the socket's
// directory, the control store and its backups, the credential store, the
// configuration file with the backups and temporary copies written beside it
// (it holds every provider key and chat token), the chat accounts' credentials,
// and both listeners.
//
// The sandbox pins every directory above these inside a command's writable
// roots, so none of them can be moved aside and reached by another name.
func daemonPrivateState(dataDir string, cfg *config.Config, socketPath, addr string) sandbox.PrivateState {
	paths := ResolvePaths(dataDir)
	runtimeDir := policyPath(paths.RuntimeDir)
	private := sandbox.PrivateState{
		Dirs: []string{runtimeDir, policyPath(filepath.Join(dataDir, "backups"))},
	}
	// The socket's directory is private too: inside the runtime directory it
	// already is; in the /tmp fallback it is shared by every daemon of the
	// person, and a confined command must reach none of their sockets. Bubblewrap
	// never masks beneath /tmp (the sandbox package skips it), where the run's
	// scratch is bound.
	if socketDir := policyPath(filepath.Dir(socketPath)); !withinPath(runtimeDir, socketDir) {
		private.Dirs = append(private.Dirs, socketDir)
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		private.Files = append(private.Files, policyPath(filepath.Join(dataDir, "control.db"+suffix)))
	}
	// Every path the daemon and the CLI spell, so the symlinks on them can be named.
	spelled := []string{dataDir, paths.RuntimeDir, filepath.Join(dataDir, "backups"), socketPath}
	if cfg != nil {
		if credentials := strings.TrimSpace(cfg.Auth.CredentialsFile); credentials != "" {
			target, link := fileNames(credentials)
			private.Files = append(private.Files, target)
			// Everything the stores write beside the file: the OAuth refresh's
			// temporary file (`auth.json.<random>.tmp`), its lock, and the credential
			// store's own `.auth-*` temporary files. The names are random or are
			// created later, so only a prefix can hide them, and with it a command
			// can plant none of them either. The writers also create their
			// temporary files exclusively, which is what keeps a planted name from
			// being written through; this is the second layer. They write beside the
			// path as spelled, which for a link is not beside its target.
			private.FilePrefixes = appendUnique(private.FilePrefixes,
				target+".", link+".",
				filepath.Join(filepath.Dir(target), ".auth-"), filepath.Join(filepath.Dir(link), ".auth-"))
			spelled = append(spelled, credentials)
		}
		if configPath := strings.TrimSpace(cfg.Path); configPath != "" {
			target, link := fileNames(configPath)
			private.Files = append(private.Files, target)
			// config.yaml.tmp while it is saved, config.yaml.bak-<time> after an edit,
			// beside the path as spelled, which for a link is not beside its target.
			private.FilePrefixes = appendUnique(private.FilePrefixes, target+".", link+".")
			spelled = append(spelled, configPath)
		}
	}
	// The chat adapters keep each account's token under the SelfMind home, not in
	// the data directory, and whether or not the adapter is enabled now.
	var weixinConfig config.WeixinConfig
	if cfg != nil {
		weixinConfig = cfg.Gateway.Weixin
	}
	accounts := weixin.AccountDir(weixin.RuntimeConfigFrom(weixinConfig, dataDir, "").HomeDir)
	private.Dirs = append(private.Dirs, policyPath(accounts))
	spelled = append(spelled, accounts)
	// Every private name above is a real path. A symlink on the way to one (a
	// dotfiles layout: ~/.selfmind pointing into a repository, a config file that
	// is a link) can be swapped for a tree of the command's own, and the daemon,
	// the CLI and the next configuration load follow it there while every private
	// name still guards the old tree. The links are named so the sandbox denies
	// replacing them.
	for _, path := range spelled {
		private.Symlinks = appendUnique(private.Symlinks, symlinkNames(path)...)
	}
	private.Sockets = []string{policyPath(socketPath)}
	if port, allHosts, ok := listenerPort(addr); ok {
		private.TCPPorts = []sandbox.TCPPort{{Port: port, AllHosts: allHosts}}
	}
	return private
}

// fileNames names a file the way the sandbox needs it twice: its real path, which
// a read resolves to, and its own name in its resolved directory, which is where
// a writer that creates a file beside it puts the copy. They differ when the file
// is itself a symlink.
func fileNames(raw string) (target, link string) {
	absolute, err := filepath.Abs(raw)
	if err != nil {
		absolute = filepath.Clean(raw)
	}
	return policyPath(absolute), filepath.Join(policyPath(filepath.Dir(absolute)), filepath.Base(absolute))
}

// maxLinksFollowed bounds how many symlinks symlinkNames follows for one path,
// the order of the limit the kernel itself applies (ELOOP), so a cycle or an
// endless chain ends.
const maxLinksFollowed = 40

// symlinkNames lists every symlink met on the way to a path, each as its own
// path in its real directory: the name a rename or removal would act on. It walks
// the path as the operating system does: a link met on the path is followed, its
// target read relative to the link's own directory, and the links met on the way
// through that target are named too, so a link to a link, a link into a synced
// folder that is itself a link, and a relative chain are all covered. A component
// that does not exist yet ends the walk; at most maxLinksFollowed links are
// followed, so a cycle ends.
func symlinkNames(raw string) []string {
	absolute, err := filepath.Abs(raw)
	if err != nil {
		return nil
	}
	var links []string
	seen := map[string]bool{}
	// resolved is the real path walked so far; pending the components still to walk.
	resolved := string(filepath.Separator)
	pending := strings.Split(absolute, string(filepath.Separator))
	for followed := 0; len(pending) > 0; {
		part := pending[0]
		pending = pending[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}
		candidate := filepath.Join(resolved, part)
		info, err := os.Lstat(candidate)
		if err != nil {
			break
		}
		if info.Mode()&os.ModeSymlink == 0 {
			resolved = candidate
			continue
		}
		if !seen[candidate] {
			seen[candidate] = true
			links = append(links, candidate)
		}
		if followed++; followed > maxLinksFollowed {
			break
		}
		target, err := os.Readlink(candidate)
		if err != nil {
			break
		}
		if filepath.IsAbs(target) {
			resolved = string(filepath.Separator)
		}
		pending = append(strings.Split(target, string(filepath.Separator)), pending...)
	}
	return links
}

func appendUnique(list []string, items ...string) []string {
	for _, item := range items {
		found := false
		for _, existing := range list {
			found = found || existing == item
		}
		if !found {
			list = append(list, item)
		}
	}
	return list
}

// policyPath is the absolute, symlink-resolved path a sandbox rule must name:
// seatbelt matches the real path, so /var/... must become /private/var/...
// A path that does not exist yet is resolved through its parent.
func policyPath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	// Resolve the deepest ancestor that exists and append the rest: a path that
	// does not exist yet still has to be named the way seatbelt will see it.
	var missing []string
	for dir := absolute; ; {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(append([]string{resolved}, missing...)...)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return absolute
		}
		missing = append([]string{filepath.Base(dir)}, missing...)
		dir = parent
	}
}

func withinPath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// listenerPort returns the TCP port and whether the listener accepts beyond
// loopback.
func listenerPort(addr string) (int, bool, bool) {
	host, rawPort, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return 0, false, false
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port <= 0 || port > 65535 {
		return 0, false, false
	}
	host = strings.TrimSpace(host)
	if strings.EqualFold(host, "localhost") {
		return port, false, true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return port, false, true
	}
	return port, true, true
}
