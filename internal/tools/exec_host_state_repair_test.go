//go:build darwin

package tools

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"selfmind/internal/executionenv"
	"selfmind/internal/kernel"
)

// requireNoPlantedEntry walks a flat credential cache the way a host tool
// meets it: only single-link regular files may be there, and the cache path
// itself is a real directory or absent.
func requireNoPlantedEntry(t *testing.T, cache string) {
	t.Helper()
	info, err := os.Lstat(cache)
	if os.IsNotExist(err) {
		return
	}
	if err != nil || !info.IsDir() {
		t.Fatalf("the cache path is no longer a real directory: %v %v", info, err)
	}
	_ = filepath.WalkDir(cache, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || path == cache {
			return nil
		}
		entryInfo, infoErr := entry.Info()
		if infoErr != nil {
			return nil
		}
		mode := entryInfo.Mode()
		stat, _ := entryInfo.Sys().(*syscall.Stat_t)
		switch {
		case mode.IsRegular() && (stat == nil || stat.Nlink == 1):
		default:
			t.Errorf("a host tool would meet %s (%v, links=%v) in the cache", path, mode.Type(), stat)
		}
		return nil
	})
}

// D1, the review probe: seatbelt checks only the vnode an operation names, so
// a sandboxed command could RENAME_SWAP a workspace link, pipe or directory
// with a cache file, or move or clone a directory full of links in, and the
// operator's own `aws` would later follow the link out of the cache or hang on
// the pipe. Whatever such a command leaves is moved into the runtime's
// quarantine before the call returns, and the call's result and evidence say
// the cache was repaired. A planter left running in the background is stopped
// with the command, so the window is the command's own duration.
func TestSandboxedCommandLeavesNoPlantedEntryInTheCredentialCache(t *testing.T) {
	helper, err := os.Executable()
	if err != nil {
		t.Skip("no path to this test binary")
	}
	plant := func(operation, from, to string) string {
		return "'" + helper + "' " + hostStatePlantVerb + " " + operation + " '" + from + "' '" + to + "'"
	}
	for _, tc := range []struct {
		name string
		// command plants into the cache; planted says the operation must have
		// succeeded for the probe to mean anything.
		command func(workspace, cache string) string
		planted bool
		// later is how long a background planter would need to act.
		later time.Duration
	}{
		{name: "swap-symlink", planted: true, command: func(ws, cache string) string {
			return plant("swap", ws+"/link", cache+"/stub-sso-dev.json")
		}},
		{name: "swap-fifo", planted: true, command: func(ws, cache string) string {
			return plant("swap", ws+"/pipe", cache+"/stub-sso-dev.json")
		}},
		{name: "swap-directory", planted: true, command: func(ws, cache string) string {
			return plant("swap", ws+"/dir", cache+"/stub-sso-dev.json")
		}},
		{name: "rename-directory-in", command: func(ws, cache string) string {
			return "mv '" + ws + "/dir' '" + cache + "/dir'"
		}},
		{name: "clone-directory", command: func(ws, cache string) string {
			return plant("clone", ws+"/dir", cache+"/clone")
		}},
		{name: "swap-the-cache-itself", command: func(ws, cache string) string {
			return plant("swap", ws+"/link", cache)
		}},
		{name: "background-planter", later: 2 * time.Second, command: func(ws, cache string) string {
			return "{ (sleep 1; " + plant("swap", ws+"/link", cache+"/stub-sso-dev.json") + ") >/dev/null 2>&1 & }"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireInPlaceBackend(t)
			withExecSandboxPolicy(t, true, false, false)
			base := fixtureBase(t)
			home := awsStateHome(t, base, ssoProfileConfig)
			run := newBoundaryRun(t, "repair-"+tc.name, home, writeStub(t, base, "aws", awsCredentialStateStub),
				executionenv.TrustTrusted, ApprovalOnRequest, nil)
			victim := filepath.Join(base, "victim.json")
			ws := run.workspace
			mustWrite(t, victim, "victim")
			mustWrite(t, filepath.Join(ws, "file"), "workspace")
			mustDo(t, os.Symlink(victim, filepath.Join(ws, "link")))
			mustDo(t, exec.Command("mkfifo", filepath.Join(ws, "pipe")).Run())
			mustDo(t, os.MkdirAll(filepath.Join(ws, "dir"), 0o755))
			mustDo(t, os.Symlink(victim, filepath.Join(ws, "dir", "link.json")))
			mustDo(t, exec.Command("mkfifo", filepath.Join(ws, "dir", "pipe")).Run())
			mustDo(t, os.Link(filepath.Join(ws, "file"), filepath.Join(ws, "dir", "hard.json")))
			cache := filepath.Join(canonicalPath(t, home), ".aws", "sso", "cache")

			events := make(chan string, 1024)
			command := "aws sts get-caller-identity --profile sso-dev && " + tc.command(canonicalPath(t, ws), cache)
			result, _ := Execute(kernel.WithEventChannel(context.Background(), events), ExecutionRequest{
				ToolName: "terminal", Payload: command, Shell: true, CWD: ws,
				WorkspaceRoots: []string{ws}, Sandbox: SandboxAuto, Timeout: 30 * time.Second,
				ToolProfile: ToolProfile{Class: ToolExecutionStandard, Timeout: 30 * time.Second},
			}, map[string]interface{}{"_tenant_id": run.tenant, "_tool_name": "terminal", "command": command, "cwd": ws})
			if !result.Started || !strings.Contains(result.Output, "cache write ok: sso") {
				t.Fatalf("fixture: the sandboxed aws must have written its cache: %+v", result)
			}
			if tc.planted && !strings.Contains(result.Output, "planted by") {
				t.Fatalf("fixture: the probe must plant: %s", result.Output)
			}
			time.Sleep(tc.later)

			requireNoPlantedEntry(t, cache)
			requireNoPlantedEntry(t, filepath.Join(canonicalPath(t, home), ".aws", "cli", "cache"))
			if data, _ := os.ReadFile(victim); string(data) != "victim" {
				t.Fatal("the file a planted link pointed at changed")
			}
			if !strings.Contains(result.Output, "planted by") {
				return
			}
			if !strings.Contains(result.Output, "repaired the credential cache") {
				t.Fatalf("the tool result must say the cache was repaired: %s", result.Output)
			}
			quarantined := 0
			_ = filepath.WalkDir(run.runtime.hostStateQuarantineRoot(), func(_ string, entry fs.DirEntry, err error) error {
				if err == nil && !entry.IsDir() {
					quarantined++
				}
				return nil
			})
			if quarantined == 0 {
				t.Fatal("the quarantine does not hold the planted entries")
			}
			close(events)
			evidence := false
			for event := range events {
				evidence = evidence || (strings.Contains(event, "tool.environment") && strings.Contains(event, "state_repairs"))
			}
			if !evidence {
				t.Fatal("the call's evidence does not record the repair")
			}
		})
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
