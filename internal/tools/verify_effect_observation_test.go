package tools

import (
	"errors"
	"strings"
	"testing"

	"selfmind/internal/kernel"
)

func TestVerifyCanBindAnEffectObservation(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		for _, command := range []string{"cat observed.state", "git ls-remote origin", "true", "cat missing.state"} {
			t.Run(command+map[bool]string{true: "/foreign", false: "/own"}[foreign], func(t *testing.T) {
				f := newEffectLifecycleFixture(t, 2)
				f.registry.Register(failThenObserveShell())
				v := newLifecycleExecTool("verify", func(command string) (kernel.ToolDispatchResult, error) {
					if command == "cat missing.state" {
						return exitedResult(1), errors.New("missing file")
					}
					return exitedResult(0), nil
				})
				v.BaseTool = NewVerifyTool().BaseTool
				f.registry.Register(v)
				owner, observer := f.runs[0], f.runs[0]
				if foreign {
					observer = f.runs[1]
				}
				if _, err := f.dispatch(owner, "terminal", "publish", map[string]interface{}{"command": "npm publish", "_network_shared": true}); err == nil {
					t.Fatal("expected failed dispatch")
				}
				claimID := f.heldClaimID(owner, "publish")
				result, err := f.dispatch(observer, "verify", "observe", map[string]interface{}{"command": command, "effect_claim_id": claimID, "criterion": "the affected target is observable", "target": "target"})
				if command == "true" || command == "cat missing.state" {
					if err == nil || len(f.unresolved()) != 1 {
						t.Fatalf("invalid observation released a claim: result=%+v err=%v", result, err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if foreign {
					if len(f.unresolved()) != 1 || !strings.Contains(result.Output, "stays unresolved") {
						t.Fatal("foreign observation falsely settled the claim")
					}
				} else if len(f.unresolved()) != 0 {
					t.Fatal("bound verification did not release its own claim")
				}
			})
		}
	}
}
