package kernel

import "testing"

// A strategy rebuilt from the identity an admission froze is the strategy the
// request built, for every tool, budget and policy a Run can observe, and
// whichever request produced it. The gateway restores continuations through
// this constructor, so any drift would silently change what a continued Run
// may do.
func TestAdmittedTaskStrategyEqualsTheStrategyItWasFrozenFrom(t *testing.T) {
	probes := []string{"update_plan", "web_search", "web_extract", "terminal", "read_file", "finish_run", "ask_user"}
	prompts := []string{"", "who are you", "look up today's release notes", "write a go function", "fix the failing build",
		"查询最新的发布说明", "整理一下这个仓库的设计笔记"}
	for _, prompt := range prompts {
		for _, channel := range []string{"cli", "telegram", "6f1d1c9e-4f0b-4d55-9b3e-52b3b1f2a9c1", ""} {
			for _, web := range []bool{false, true} {
				for _, verifyOnly := range []bool{false, true} {
					built := BuildTaskStrategy(prompt, channel)
					if web {
						built = built.WithWebEnabled()
					}
					if verifyOnly {
						// Verification-only narrows the tool surface and nothing else:
						// it neither changes the planning policy nor re-derives which
						// tools are hidden, so the rebuild must agree on every tool
						// without the test supplying that derivation for it.
						built.VerificationOnly, built.ToolMode = true, ToolModeLocalRead
					}
					rebuilt := AdmittedTaskStrategy(built.Class, built.ToolMode, built.PlanPolicy, built.WebPolicy, built.ChannelMode, built.VerificationOnly)
					for _, tool := range probes {
						if built.AllowsTool(tool) != rebuilt.AllowsTool(tool) {
							t.Fatalf("%q on %q web=%t verify=%t: %s allowed=%t, rebuilt %t", prompt, channel, web, verifyOnly, tool, built.AllowsTool(tool), rebuilt.AllowsTool(tool))
						}
					}
					if built.Class != rebuilt.Class || built.ToolMode != rebuilt.ToolMode || built.PlanPolicy != rebuilt.PlanPolicy ||
						built.WebPolicy != rebuilt.WebPolicy || built.ChannelMode != rebuilt.ChannelMode ||
						built.VerificationOnly != rebuilt.VerificationOnly || built.MaxActionTools != rebuilt.MaxActionTools ||
						built.ActionToolBudgetStep != rebuilt.ActionToolBudgetStep || built.ActionToolBudgetLimit != rebuilt.ActionToolBudgetLimit ||
						built.MaxBudgetExtensions != rebuilt.MaxBudgetExtensions || built.RequireProgressEvents != rebuilt.RequireProgressEvents ||
						built.MayWriteWorkspace() != rebuilt.MayWriteWorkspace() {
						t.Fatalf("%q on %q web=%t verify=%t: rebuilt %+v, built %+v", prompt, channel, web, verifyOnly, rebuilt, built)
					}
				}
			}
		}
	}
}
