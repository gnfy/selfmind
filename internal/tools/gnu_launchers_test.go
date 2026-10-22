package tools

import (
	"strings"
	"testing"
)

// Homebrew installs GNU coreutils, findutils, time and inetutils beside the
// BSD tools macOS ships, prefixed with g: grm is rm, gtimeout is timeout.
// Read as unknown programs they hid every command they run.
func TestHomebrewGNUPrefixNamesTheSameProgram(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	for _, command := range []string{
		"grm -rf /",
		"GRM -rf /",
		"/opt/homebrew/bin/grm -rf ~",
		"gtimeout 5 rm -rf /",
		"genv -i rm -rf /",
		"gxargs rm -rf / < /dev/null",
		"gfind / -delete",
		"gnice -n 5 rm -rf ~",
		"gnohup grm -rf /etc",
		"gstdbuf -o0 rm -rf /",
		"gtime rm -rf /",
		"gchroot / rm -rf /",
	} {
		if !hardFloorBlocks(t, command) {
			t.Errorf("hard floor missed %q", command)
		}
	}
	for _, tc := range []struct{ command, reason string }{
		{"gchmod 777 build", "invokes dangerous command: chmod"},
		{"gkill -9 4242", "invokes dangerous command: kill"},
		{"gshred secret.txt", "invokes dangerous command: shred"},
		{"gtelnet example.test 80", "network egress command: telnet"},
	} {
		if dangerous, reason := dangerousCommand(tc.command); !dangerous || reason != tc.reason {
			t.Errorf("%q: dangerous=%v reason=%q, want %q", tc.command, dangerous, reason, tc.reason)
		}
	}
	// The constraint that must change the result: a program that only begins
	// with g is itself, and proof and classes still need the exact name.
	for _, command := range []string{"git status", "gcloud builds list", "go test ./...", "grep -r x ."} {
		word := strings.Fields(command)[0]
		if programs := identifiedPrograms(command); !programs[word] {
			t.Errorf("%q must name %q, identified %v", command, word, programs)
		}
	}
	if provenReadOnly(t, "gls -la") || provenReadOnly(t, "gtimeout 5 cat value.txt") {
		t.Error("a g-prefixed name is not the catalogued name proof vouches for")
	}
	if prefix, ok := grantCommandPrefix("terminal", map[string]interface{}{"command": "gtimeout 5 kubectl get pods"}); ok {
		t.Errorf("a g-prefixed wrapper minted class %v", prefix)
	}
	if IsGrantableCommandFamily("grm -rf") {
		t.Error("grm is rm, which no class may name")
	}
}

// A g-prefixed GNU program follows the bare program's class rules: gsed and
// gmake are sed and make, which no class may name, so neither mints one and a
// key a class once minted for either is withdrawn at boot. A prefixed name is
// never the exact word a class needs, so ggrep and gtar mint none either.
func TestGNUPrefixedProgramsFollowTheBareProgramsClassRules(t *testing.T) {
	for _, command := range []string{
		"gsed -i s/a/b/ notes.txt", "gmake release", "ggrep -r needle .", "gegrep needle notes.txt",
		"gtar -xf a.tar", "gawk '{print $1}' notes.txt",
	} {
		if prefix, ok := grantClassOf(command); ok {
			t.Errorf("%q minted class %v", command, prefix)
		}
	}
	for _, family := range []string{"gsed -i", "gmake release", "ggrep", "gtar"} {
		if IsGrantableCommandFamily(family) {
			t.Errorf("%q must not be a grantable family", family)
		}
		for _, key := range []string{
			"exec:terminal" + grantKeyClassMarker + family,
			hostGrantKeyBase + grantKeyResourceMarker + "workspace:0123456789abcdef" + grantKeyCommandMarker + family,
		} {
			if _, keep := ReviewPersistedGrantKey(key); keep {
				t.Errorf("boot review kept %q", key)
			}
		}
	}
	// The constraint that must change the result: the bare names keep their
	// own rules — sed and make stay banned, tar keeps its class.
	for command, minted := range map[string]bool{
		"sed -i s/a/b/ notes.txt": false, "make release": false, "tar -xf a.tar": true,
	} {
		if _, ok := grantClassOf(command); ok != minted {
			t.Errorf("%q: class minted=%v, want %v", command, ok, minted)
		}
	}
}

// Launchers outside the earlier table run the command their own grammar
// names.
func TestUnlistedLauncherGrammarFidelity(t *testing.T) {
	checkLauncherCases(t, []launcherCase{
		{command: "pkexec kill 1", runs: "kill"},
		{command: "pkexec --user root kill 1", runs: "kill", not: []string{"root"}},
		{command: "pkexec -u root --keep-cwd kill 1", runs: "kill", not: []string{"root"}},
		{command: "pkexec", opaque: true},
		{command: "taskset 0x3 kill 1", runs: "kill", not: []string{"0x3"}},
		{command: "taskset -c 0-3 kill 1", runs: "kill", not: []string{"0-3"}},
		{command: "taskset -p 0x3 4242", not: []string{"0x3", "4242"}},
		{command: "taskset -ap 0x3 4242", not: []string{"0x3", "4242"}},
		{command: "nsenter -t 1 -m -u -n kill 1", runs: "kill"},
		{command: "nsenter --target 1 --mount=/proc/1/ns/mnt kill 1", runs: "kill"},
		{command: "nsenter -a -t 1", opaque: true},
		{command: "runuser -u operator kill 1", runs: "kill", not: []string{"operator"}},
		{command: "runuser -u operator -- kill 1", runs: "kill", not: []string{"operator"}},
		{command: "runuser -l operator -c kill", runs: "kill", not: []string{"operator"}},
		{command: "runuser operator -c -- kill", runs: "kill"},
		{command: "runuser operator", opaque: true},
		{command: "setpriv --reuid 1000 --regid 1000 --init-groups kill 1", runs: "kill", not: []string{"1000"}},
		{command: "setpriv --no-new-privs kill 1", runs: "kill"},
		{command: "setpriv -d"},
		{command: "setpriv --dump"},
		{command: "arch -x86_64 kill 1", runs: "kill"},
		{command: "arch -arm64e kill 1", runs: "kill"},
		{command: "arch -arch x86_64 kill 1", runs: "kill", not: []string{"x86_64"}},
		{command: "arch -e FOO=1 -d BAR kill 1", runs: "kill", not: []string{"bar", "foo=1"}},
		{command: "arch"},
		{command: "caffeinate -i kill 1", runs: "kill"},
		{command: "caffeinate -t 60 kill 1", runs: "kill", not: []string{"60"}},
		{command: "caffeinate -dimsu"},
		{command: "sem --id build -j 2 kill 1", runs: "kill", not: []string{"build", "2"}},
		{command: "sem --fg kill 1", runs: "kill"},
		{command: "sem --wait"},
	})
	// arch -e sets a variable for the command it runs; one that loads code
	// is an injection like any other.
	if dangerous, reason := dangerousCommand("arch -e LD_PRELOAD=/tmp/x.so ls"); !dangerous || !strings.Contains(reason, "LD_PRELOAD") {
		t.Errorf("arch -e LD_PRELOAD: dangerous=%v reason=%q", dangerous, reason)
	}
}

// Powering the host down through systemd, formatting through newfs, and
// erasing a disk through diskutil are the floor's operations under other
// names.
func TestPowerAndDiskEraseVerbsReachTheFloor(t *testing.T) {
	for _, command := range []string{
		"systemctl reboot",
		"systemctl poweroff",
		"systemctl halt",
		"systemctl kexec",
		"systemctl soft-reboot",
		"systemctl --force reboot",
		"systemctl -i --no-wall poweroff",
		"systemctl --message maintenance reboot",
		"systemctl start reboot.target",
		"systemctl isolate poweroff.target",
		"sudo systemctl reboot",
		"telinit 6",
		"newfs_apfs -v Data /dev/disk4s1",
		"newfs_hfs /dev/disk4s1",
		"newfs /dev/ada0p2",
		"diskutil eraseDisk APFS Blank disk4",
		"diskutil erasedisk JHFS+ x disk4",
		"diskutil quiet eraseVolume APFS Data disk4s2",
		"diskutil zeroDisk disk4",
		"diskutil randomDisk 3 disk4",
		"diskutil secureErase 0 disk4",
		"diskutil partitionDisk disk4 GPT APFS Data 100%",
		"diskutil reformat disk4s2",
	} {
		if !hardFloorBlocks(t, command) {
			t.Errorf("hard floor missed %q", command)
		}
	}
	// The constraint that must change the result: other verbs, a status
	// query and a free-space erase leave the host and its data in place.
	for _, command := range []string{
		"systemctl status reboot.target",
		"systemctl restart nginx",
		"systemctl --user restart api",
		"systemctl -t service list-units",
		"diskutil list",
		"diskutil info disk4",
		"diskutil secureErase freespace 0 /Volumes/Data",
		"diskutil eject disk4",
		"telinit q",
	} {
		if hardFloorBlocks(t, command) {
			t.Errorf("hard floor refused %q", command)
		}
	}
}
