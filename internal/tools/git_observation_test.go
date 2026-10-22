package tools

import "testing"

func TestGitObservationKeepsGlobalAndSubcommandOptionsDistinct(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    bool
	}{
		{"git -C repo log --oneline", true},
		{"git -Crepo --no-pager show HEAD:path.txt", true},
		{"git -C 'another repo' diff -C", true},
		{"git --no-optional-locks -C one -C ../two rev-list --count HEAD", true},
		{"git grep -n needle -- src", true},
		{"git -C repo grep -C 2 needle", true},
		{"git -C repo -c core.pager=sh log", false},
		{"git -C repo log --output=/tmp/not-executed", false},
		{"git grep -n --open-files-in-pager=sh needle", false},
		{"git grep --textconv needle", false},
		{"git -C repo rev-list --output=/tmp/not-executed HEAD", false},
		{"git -C repo checkout main", false},
		{"git -C repo log --unknown-option", false},
	} {
		t.Run(tc.command, func(t *testing.T) {
			if got := deterministicObservationExec("terminal", map[string]interface{}{"command": tc.command}); got != tc.want {
				t.Fatalf("proof=%v want=%v", got, tc.want)
			}
		})
	}
	if !observationRuleByProgram["git"].matches([]string{"-C", "repo", "log", "--oneline"}) {
		t.Fatal("catalog consumers disagree on global option proof")
	}
}
