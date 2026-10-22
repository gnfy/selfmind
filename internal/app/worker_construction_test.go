package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"testing"
)

// The daemon and the eval harness must build their agent workers only through
// InitGateway. Each used to assemble its own agent, tools, cron tool, MCP
// connection and review queue, and the daemon's extra workers silently lacked
// the cron and MCP tools and the durable review queue. A direct call to a
// worker part here would let the two paths drift again.
func TestDaemonAndEvalBuildAgentWorkersOnlyThroughInitGateway(t *testing.T) {
	forbidden := []string{
		"InitAgent", "InitTools", "InitMCP", "RegisterCronTool", "MaybeEnableWorkerPool",
		"NewMCPToolManager", "NewSkillToolManager", "NewCronTool", "SetEnqueue", "SetBackend", "EnableWorkerPool", "RegisterTool",
	}
	for _, path := range []string{"../runtime/gateway/runner.go", "../eval/runner.go"} {
		t.Run(path, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			calls := map[string]int{}
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fn := call.Fun.(type) {
				case *ast.SelectorExpr:
					calls[fn.Sel.Name]++
				case *ast.Ident:
					calls[fn.Name]++
				}
				return true
			})
			if calls["InitGateway"] != 1 {
				t.Fatalf("%s calls InitGateway %d times, want exactly one worker construction", path, calls["InitGateway"])
			}
			var direct []string
			for _, name := range forbidden {
				if calls[name] > 0 {
					direct = append(direct, name)
				}
			}
			sort.Strings(direct)
			if len(direct) > 0 {
				t.Fatalf("%s assembles agent worker parts outside InitGateway: %v", path, direct)
			}
		})
	}
}
