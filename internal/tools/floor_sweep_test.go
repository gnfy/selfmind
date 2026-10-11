package tools

import "testing"

// GNU parallel composes each command line from its template and its literal
// arguments: substituted for a replacement string, or appended when the
// template has none. Reading the template alone read `parallel rm -rf ::: /`
// as `rm -rf`, which the floor let through to an ordinary approval.
func TestParallelArgumentsReachTheFloor(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	for _, command := range []string{
		"parallel rm -rf ::: /",
		"parallel rm -rf {} ::: /",
		"parallel rm ::: -rf ::: /",
		"parallel rm -rf {2} ::: x ::: /etc",
		"parallel -j4 rm -rf ::: /tmp/a /",
		"parallel --arg-sep ,, rm -rf ,, /etc",
		"parallel -I X rm -rf X ::: /usr",
		"parallel {} -rf / ::: rm",
		"parallel sh -c {} ::: 'rm -rf ~'",
		"parallel -- mkfs.ext4 ::: /dev/sdb1",
		// ${NAME} is the shell's expansion, and a brace group of letters is
		// no replacement string parallel knows: the argument is appended.
		"parallel rm -rf '${EMPTY}' ::: /",
		"parallel rm -rf {HOME} ::: /",
	} {
		if !hardFloorBlocks(t, command) {
			t.Errorf("hard floor missed %q", command)
		}
	}
	// The constraint that must change the result: other arguments, and
	// arguments the call does not show, are not refused.
	for _, command := range []string{
		"parallel rm -rf ::: build dist",
		"parallel rm -rf {} ::: ./build",
		"parallel rm -rf :::: dirs.txt",
		"parallel echo {} ::: /",
		"parallel rm -rf {//} ::: /home/tester/project/build",
		"parallel rm -rf {}/cache ::: /home/tester",
		"parallel rm -rf {.}.d ::: /etc",
	} {
		if hardFloorBlocks(t, command) {
			t.Errorf("hard floor refused %q", command)
		}
	}
}

// The floor reads the selection find evaluates. -maxdepth bounds -delete only
// at 0 or 1, where it removes no directory that still holds entries; an
// always-true test (`-true`, `-name '*'`, `-regex '.*'`) narrows nothing; and
// what a negated test or an earlier -o branch leaves is still nearly
// everything (`find / -path /proc -prune -o -delete` deletes all but /proc).
func TestFindSweepReadsTheExpressionFindEvaluates(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	for _, command := range []string{
		"find / -maxdepth 99 -delete",
		"find /etc -maxdepth 2 -delete",
		"find / -true -delete",
		"find / -name '*' -delete",
		"find / -path '*' -delete",
		"find / -iname '**' -exec rm -rf {} +",
		"find / -regex '.*' -delete",
		"find / ! -name keep -delete",
		"find / -not -name keep -delete",
		"find / -name keep -o -delete",
		"find / -path /proc -prune -o -delete",
		"find / -type d -o -exec rm -rf {} +",
		"find / -print -delete",
		`find / \( -true \) -delete`,
		`find / \( -name a -o -true \) -delete`,
		// A command's exit status may hold or fail for every entry.
		`find / -exec test -f {} \; -delete`,
		`find / -exec false \; -o -delete`,
	} {
		if !hardFloorBlocks(t, command) {
			t.Errorf("hard floor missed %q", command)
		}
	}
	// The constraint that must change the result: a real test, a bound depth,
	// an action find never reaches, or a condition no reading can bound leaves
	// an ordinary dangerous operation.
	for _, command := range []string{
		"find . -name '*.tmp' -delete",
		"find ./build -type f -delete",
		"find / -maxdepth 1 -delete",
		"find / -maxdepth 0 -delete",
		"find / -name '*.tmp' -delete",
		"find / -lname '*' -delete",
		`find / \( -name a -o -name b \) -delete`,
		"find / -true -o -delete",
		"find / -empty -delete",
		"find / -false -delete",
		"find / -delete -quit",
		"find / -name keep -o -name '*.log' -delete",
	} {
		if hardFloorBlocks(t, command) {
			t.Errorf("hard floor refused %q", command)
		}
	}
}

// busybox and toybox run no applet named `-`; reading a lone dash as one of
// their own options must not stop the floor from reading the words after it.
func TestLoneDashKeepsMultiCallCommandsVisible(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	for _, command := range []string{"busybox - rm -rf /", "toybox - rm -rf /"} {
		if !hardFloorBlocks(t, command) {
			t.Errorf("hard floor missed %q", command)
		}
	}
	// env's lone dash is its -i.
	if !execEnvironmentOverrides("env - aws sts get-caller-identity")["*"] {
		t.Error("env - discards the environment")
	}
}

// A find whose expression selects every entry under its start paths — no
// test, operator, -prune or -quit narrows it — deletes those trees with
// -delete, and hands every start path to each command its -exec family runs
// (past -mindepth, every entry under it). Both only asked before.
func TestFindSweepReachesTheFloor(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	for _, command := range []string{
		"find / -delete",
		"find ~ -depth -delete",
		"find /etc -xdev -delete",
		"find -L / -mindepth 1 -delete",
		"find / -exec rm -rf {} +",
		`find / -exec rm -rf {} \;`,
		"find $HOME -execdir rm -rf {} +",
		`find /usr -maxdepth 0 -exec rm -rf {} \;`,
		"find / -mindepth 1 -maxdepth 1 -exec rm -rf {} +",
		`find / -exec sh -c 'rm -rf {}' \;`,
		"sudo find / -delete",
		`find / /tmp -ok rm -r {} \;`,
		"find / -exec find {} -delete ;",
	} {
		if !hardFloorBlocks(t, command) {
			t.Errorf("hard floor missed %q", command)
		}
	}
	// The constraint that must change the result: a narrowed selection, a
	// bounded depth, a harmless command or an ordinary tree is not refused.
	for _, command := range []string{
		"find / -name '*.tmp' -delete",
		"find . -delete",
		"find /tmp/build -delete",
		"find / -maxdepth 1 -delete",
		"find / -type f -exec rm -rf {} +",
		"find / -exec echo {} +",
		"find / -path /proc -prune -o -name '*.tmp' -delete",
		"find /home/tester/project -mindepth 1 -delete",
		"find / -files0-from list -delete",
	} {
		if hardFloorBlocks(t, command) {
			t.Errorf("hard floor refused %q", command)
		}
	}
}
