package tools

import "testing"

func TestObservationProofMutationAndExecutionFlagsAreNotReadOnly(t *testing.T) {
	for _, s := range []string{"date 101521352026", "git diff --output=out.txt", "git diff --output out.txt", "git log --output=out.txt", "git show --output=out.txt", "yq -s . input.yaml", "file -C", "file --compile", "sort --compress-program=unreviewed-program input.txt", "rg --hostname-bin=unreviewed-program x input.txt", "rg --pre unreviewed-program x input.txt", "date -f %Y 2026", "sort --compress-program unreviewed-program input.txt", "yq --split-exp . input.yaml"} {
		t.Run(s, func(t *testing.T) {
			if deterministicObservationExec("terminal", map[string]interface{}{"command": s}) {
				t.Error("effectful or command-executing invocation classified as read-only")
			}
		})
	}
}
func TestObservationProofOrdinaryReadControlsRemainReadable(t *testing.T) {
	for _, s := range []string{"date", "git diff --stat", "git log -1 --oneline", "git show --stat HEAD", "sort input.txt", "rg value input.txt"} {
		if !deterministicObservationExec("terminal", map[string]interface{}{"command": s}) {
			t.Errorf("ordinary read rejected: %s", s)
		}
	}
}
