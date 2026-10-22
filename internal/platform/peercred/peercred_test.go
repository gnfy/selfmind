//go:build !windows

package peercred

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func socketDir(t *testing.T) string {
	t.Helper()
	// Test temp directories exceed sun_path on macOS.
	dir, err := os.MkdirTemp("/tmp", "smpc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func requirePeerCredentials(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("peer credentials are implemented for darwin and linux")
	}
}

// acceptVerdicts serves one verdict per accepted connection and answers with a
// minimal HTTP response so curl exits cleanly.
func acceptVerdicts(t *testing.T, verifier Verifier) (string, <-chan Verdict) {
	t.Helper()
	path := filepath.Join(socketDir(t), "control.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	verdicts := make(chan Verdict, 8)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			verdicts <- verifier.Check(conn)
			// Raw socket clients close after receiving their verdict. HTTP
			// clients must finish sending before we respond and close, or
			// curl can fail with a broken pipe while writing its request.
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := http.ReadRequest(bufio.NewReader(conn)); err == nil {
				_, _ = conn.Write([]byte("HTTP/1.0 200 OK\r\nContent-Length: 2\r\n\r\nok"))
			}
			conn.Close()
		}
	}()
	return path, verdicts
}

func waitVerdict(t *testing.T, verdicts <-chan Verdict) Verdict {
	t.Helper()
	select {
	case verdict := <-verdicts:
		return verdict
	case <-time.After(10 * time.Second):
		t.Fatal("no connection reached the listener")
		return Verdict{}
	}
}

func TestOfReportsTheConnectingProcess(t *testing.T) {
	requirePeerCredentials(t)
	path, verdicts := acceptVerdicts(t, Verifier{DaemonPID: -1, OwnerUID: os.Getuid()})
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	verdict := waitVerdict(t, verdicts)
	if verdict.UID != os.Getuid() || verdict.PID != os.Getpid() {
		t.Fatalf("credentials = uid %d pid %d, want uid %d pid %d", verdict.UID, verdict.PID, os.Getuid(), os.Getpid())
	}
	if !verdict.Verified {
		t.Fatalf("a same-user process outside the daemon tree must be verified: %+v", verdict)
	}
}

func TestOfRejectsNonUnixConnections(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	if _, err := Of(server); err == nil {
		t.Fatal("a connection without kernel peer credentials must not be identified")
	}
	if verdict := (Verifier{OwnerUID: os.Getuid()}).Check(server); verdict.Verified {
		t.Fatal("an unidentified peer must be refused")
	}
}

func curlPath(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl unavailable")
	}
	return path
}

func TestVerifierRefusesTheDaemonAndItsDescendants(t *testing.T) {
	requirePeerCredentials(t)
	curl := curlPath(t)
	// This test process plays the daemon.
	daemon := Verifier{DaemonPID: os.Getpid(), OwnerUID: os.Getuid()}
	path, verdicts := acceptVerdicts(t, daemon)

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if verdict := waitVerdict(t, verdicts); verdict.Verified || !strings.Contains(verdict.Reason, "daemon") {
		t.Fatalf("the daemon itself must be refused: %+v", verdict)
	}
	conn.Close()

	// A tool the daemon starts, one shell level down.
	if out, err := exec.Command("/bin/sh", "-c", curl+" -sS -m 5 --unix-socket "+path+" http://daemon/").CombinedOutput(); err != nil {
		t.Fatalf("child client failed: %v %s", err, out)
	}
	if verdict := waitVerdict(t, verdicts); verdict.Verified || !strings.Contains(verdict.Reason, "daemon") {
		t.Fatalf("a descendant of the daemon must be refused: %+v", verdict)
	}

	// The same client, with a daemon that is not its ancestor, is verified:
	// the walk, not the uid, refused it above.
	sibling := exec.Command("/bin/sleep", "30")
	if err := sibling.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sibling.Process.Kill(); _ = sibling.Wait() }()
	otherPath, otherVerdicts := acceptVerdicts(t, Verifier{DaemonPID: sibling.Process.Pid, OwnerUID: os.Getuid()})
	if out, err := exec.Command("/bin/sh", "-c", curl+" -sS -m 5 --unix-socket "+otherPath+" http://daemon/").CombinedOutput(); err != nil {
		t.Fatalf("child client failed: %v %s", err, out)
	}
	if verdict := waitVerdict(t, otherVerdicts); !verdict.Verified {
		t.Fatalf("a process outside the daemon tree must be verified: %+v", verdict)
	}
}

// A tool that detaches leaves the daemon's tree on macOS (no subreaper): its
// parent exits and launchd adopts it. The process group the daemon recorded
// for the tool still identifies it.
func TestVerifierRefusesADetachedMemberOfATrackedToolGroup(t *testing.T) {
	requirePeerCredentials(t)
	curl := curlPath(t)
	run := func(groups *GroupTracker) Verdict {
		t.Helper()
		path, verdicts := acceptVerdicts(t, Verifier{DaemonPID: os.Getpid(), OwnerUID: os.Getuid(), Groups: groups})
		tool := exec.Command("/bin/sh", "-c", "( sleep 0.5; "+curl+" -sS -m 5 --unix-socket "+path+" http://daemon/ >/dev/null 2>&1 ) & exit 0")
		tool.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := tool.Start(); err != nil {
			t.Fatal(err)
		}
		groups.Track(tool.Process.Pid)
		if err := tool.Wait(); err != nil {
			t.Fatal(err)
		}
		return waitVerdict(t, verdicts)
	}
	if verdict := run(NewGroupTracker()); verdict.Verified {
		t.Fatalf("a detached member of a tool's process group must be refused: %+v", verdict)
	}
	if runtime.GOOS == "darwin" {
		// The residual this record exists for: without it, the adopted
		// process is indistinguishable from the person's own terminal.
		if verdict := run(nil); !verdict.Verified {
			t.Fatalf("expected the untracked detached process to pass the ancestry walk on macOS: %+v", verdict)
		}
	}
}

func TestVerifierFailsClosed(t *testing.T) {
	owner := os.Getuid()
	broken := Verifier{DaemonPID: 99999999, OwnerUID: owner, lookup: func(int) (process, error) {
		return process{}, errors.New("hidden by hidepid")
	}}
	if verdict := broken.CheckCredentials(Credentials{UID: owner, PID: 4242}); verdict.Verified || !strings.Contains(verdict.Reason, "ancestry unavailable") {
		t.Fatalf("an unreadable ancestor must refuse the peer: %+v", verdict)
	}
	cases := map[string]Credentials{
		"other user":  {UID: owner + 1, PID: 4242},
		"missing pid": {UID: owner, PID: 0},
	}
	for name, creds := range cases {
		t.Run(name, func(t *testing.T) {
			if verdict := (Verifier{DaemonPID: 1, OwnerUID: owner}).CheckCredentials(creds); verdict.Verified {
				t.Fatalf("%s must be refused: %+v", name, verdict)
			}
		})
	}
	cyclic := Verifier{DaemonPID: 7, OwnerUID: owner, lookup: func(pid int) (process, error) {
		return process{PPID: pid + 1}, nil
	}}
	if verdict := cyclic.CheckCredentials(Credentials{UID: owner, PID: 100}); verdict.Verified {
		t.Fatalf("an unbounded ancestry must be refused: %+v", verdict)
	}
}

func TestGroupTrackerForgetsDeadGroups(t *testing.T) {
	live := map[int]bool{40: true, 41: true}
	tracker := &GroupTracker{groups: map[int]struct{}{}, alive: func(pgid int) bool { return live[pgid] }}
	tracker.Track(40)
	tracker.Track(41)
	tracker.Track(1)
	if !tracker.Contains(40) || !tracker.Contains(41) || tracker.Contains(1) {
		t.Fatal("live tool groups must be tracked; init's group never is")
	}
	live[40] = false
	if tracker.Contains(40) {
		t.Fatal("a dead group must be forgotten so a recycled id is not refused")
	}
	live[40] = true
	if tracker.Contains(40) {
		t.Fatal("a forgotten group must not come back when its id is reused")
	}
	var nilTracker *GroupTracker
	nilTracker.Track(5)
	if nilTracker.Contains(5) {
		t.Fatal("a nil tracker contains nothing")
	}
}

// A group that died is forgotten on a timer, not only when someone next asks. A
// recycled group id that is still on the list while its new owner is alive would
// pass the liveness check and wrongly mark a different process group as a tool's.
func TestGroupTrackerForgetsDeadGroupsOnATimer(t *testing.T) {
	var mu sync.Mutex
	live := map[int]bool{70: true}
	tracker := &GroupTracker{groups: map[int]struct{}{}, alive: func(pgid int) bool {
		mu.Lock()
		defer mu.Unlock()
		return live[pgid]
	}}
	tracker.Track(70)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go PruneEvery(ctx, tracker, 5*time.Millisecond)

	mu.Lock()
	live[70] = false
	mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !tracker.leads(70) {
			// The id comes back to life under a new owner; it must not be a tool's.
			mu.Lock()
			live[70] = true
			mu.Unlock()
			time.Sleep(30 * time.Millisecond)
			if tracker.Contains(70) {
				t.Fatal("a recycled group id was mistaken for a tool's")
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the dead group was never forgotten without a query")
}

func TestParseProcStat(t *testing.T) {
	stat, err := parseProcStat([]byte("1234 (we (ird) name)) S 77 1234 1234 34816 1234 4194560"))
	if err != nil {
		t.Fatal(err)
	}
	if stat.State != 'S' || stat.PPID != 77 || stat.PGID != 1234 {
		t.Fatalf("parsed %+v", stat)
	}
	zombie, err := parseProcStat([]byte("88 (sh) Z 1 88 88 0 -1"))
	if err != nil || zombie.State != 'Z' || zombie.PPID != 1 {
		t.Fatalf("zombie parsed %+v err=%v", zombie, err)
	}
	for _, bad := range []string{"", "12 (x", "12 (x) S", "12 (x) S abc 1", "12 (x) SS 1 1"} {
		if _, err := parseProcStat([]byte(bad)); err == nil {
			t.Fatalf("malformed stat %q must fail", bad)
		}
	}
}
