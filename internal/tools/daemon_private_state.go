package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"selfmind/internal/tools/sandbox"
)

// Daemon private state.
//
// Every tool command runs as the same OS user as the daemon, so file modes do
// not keep it away from the daemon's records, credentials, or control
// listeners. The gateway runner installs what is private once at startup;
// every isolated backend applies it to every command, after the command's own
// writable roots and network mode, so neither can reopen it.

func copyPrivateState(state sandbox.PrivateState) sandbox.PrivateState {
	return sandbox.PrivateState{
		Dirs:         append([]string(nil), state.Dirs...),
		Files:        append([]string(nil), state.Files...),
		FilePrefixes: append([]string(nil), state.FilePrefixes...),
		Symlinks:     append([]string(nil), state.Symlinks...),
		Sockets:      append([]string(nil), state.Sockets...),
		TCPPorts:     append([]sandbox.TCPPort(nil), state.TCPPorts...),
	}
}

// existingPrivateState keeps only entries bubblewrap can mount over. A mount
// point must exist under the read-only host root; an absent path holds nothing
// to hide, and naming it would abort the command. A file prefix is kept
// whole: bubblewrap matches it against the directory's entries when the
// command starts, so one that matches nothing costs nothing.
func existingPrivateState(state sandbox.PrivateState) sandbox.PrivateState {
	out := sandbox.PrivateState{FilePrefixes: state.FilePrefixes, Symlinks: state.Symlinks, TCPPorts: state.TCPPorts}
	for _, dir := range state.Dirs {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			out.Dirs = append(out.Dirs, dir)
		}
	}
	for _, file := range state.Files {
		if info, err := os.Stat(file); err == nil && info.Mode().IsRegular() {
			out.Files = append(out.Files, file)
		}
	}
	for _, socket := range state.Sockets {
		socket = strings.TrimSpace(socket)
		if info, err := os.Stat(filepath.Dir(socket)); socket != "" && err == nil && info.IsDir() {
			out.Sockets = append(out.Sockets, socket)
		}
	}
	return out
}

// ProcessGroupRecorder learns the process group of every tool command. The
// daemon refuses local control requests from those groups, which still
// identifies a detached descendant after its parent exits and it is reparented
// away from the daemon.
type ProcessGroupRecorder interface {
	Track(pgid int)
}

// recordSpawnedGroup reports a started command that leads its own process
// group. A command sharing the daemon's group is never recorded: refusing that
// group would refuse whoever started the daemon.
func recordSpawnedGroup(ctx context.Context, cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil || cmd.SysProcAttr == nil || !leadsOwnGroup(cmd) {
		return
	}
	runtime := executionRuntimeFromContext(ctx)
	var recorder ProcessGroupRecorder
	if runtime != nil {
		recorder = runtime.recorder
	}
	if recorder != nil {
		recorder.Track(cmd.Process.Pid)
	}
}
