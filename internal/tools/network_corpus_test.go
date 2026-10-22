package tools

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"selfmind/internal/executionenv"
)

// networkCorpus is fifty common developer commands, grouped by what they do.
// The first group builds, tests, or inspects; the second names an egress program
// or a credential-bearing service CLI; the third fetches or publishes through a
// package manager, a VCS remote, or an infrastructure tool, or is a cold-cache
// build.
var networkCorpus = []struct {
	command string
	// networked: the call runs with network when a grant (or trust) permits it.
	networked bool
	// observation: a proven read-only observation, which claims no target.
	observation bool
}{
	{"go build ./...", false, false}, {"go test ./...", false, false}, {"go vet ./...", false, false},
	{"npm test", false, false}, {"npm run build", false, false}, {"pytest -q", false, false}, {"make", false, false},
	{"ls -la", false, true}, {"rg TODO .", false, true}, {"jq . package.json", false, true},
	{"sed -n '1,20p' main.go", false, false}, {"git status", false, true}, {"git diff", false, true},
	{"git log --oneline", false, true}, {"git add -A", false, false}, {"git commit -m wip", false, false},
	{"python script.py", false, false}, {"node x.js", false, false},

	{"curl https://example.com/health", true, false}, {"wget https://example.com/file.tgz", true, false},
	{"ssh deploy@host uptime", true, false}, {"scp build.tgz deploy@host:/srv/", true, false},
	{"rsync -a dist/ deploy@host:/srv/app/", true, false}, {"docker build -t app .", true, false},
	{"docker push registry.example.com/app:1", true, false}, {"kubectl get pods", true, true},
	{"kubectl apply -f deploy.yaml", true, false}, {"helm upgrade app ./chart", true, false},
	{"aws s3 ls", true, false}, {"gcloud builds list", true, true}, {"gh pr list", true, true},

	{"go mod download", true, false}, {"go get example.com/pkg@latest", true, false}, {"npm install", true, false},
	{"yarn install", true, false}, {"pnpm install", true, false}, {"pip install -r requirements.txt", true, false},
	{"poetry install", true, false}, {"cargo build", false, false}, {"cargo test", false, false},
	{"mvn test", false, false}, {"gradle build", false, false}, {"git fetch origin", true, false},
	{"git pull", true, false}, {"git push origin main", true, false},
	{"git clone https://example.com/repo.git", true, false}, {"terraform plan", true, false},
	{"terraform apply", true, false}, {"brew install jq", true, false}, {"apt-get install -y jq", true, false},
}

// Fifty common developer commands, through the real capability middleware under
// the two ways a workspace comes to permit network. Network follows the command,
// not the permission, and at parallel capacity only a call that actually runs
// with network claims the person-wide unknown external target.
//
// Before: a stored grant networked 50/50 and 41 of those claimed; the trusted
// path networked 13/50 and 10 claimed, while package managers, VCS remotes and
// infrastructure tools ran offline and failed first.
func TestNetworkFollowsTheCommandNotThePermission(t *testing.T) {
	withExecSandboxPolicy(t, true, false, true)
	if len(networkCorpus) != 50 {
		t.Fatalf("corpus has %d commands, want 50", len(networkCorpus))
	}
	scenarios := []struct {
		name  string
		scope func(person string) ExecutionScope
	}{
		{"stored network grant, untrusted workspace", func(person string) ExecutionScope {
			return ExecutionScope{TenantID: "t", PersonID: person, RunID: "run-" + person, WorkspaceID: "w",
				TrustLevel: executionenv.TrustUntrusted, ParallelWork: true, CapabilityStore: networkOnlyCapabilityStore{}}
		}},
		{"operator policy, trusted workspace", func(person string) ExecutionScope {
			return ExecutionScope{TenantID: "t", PersonID: person, RunID: "run-" + person, WorkspaceID: "w",
				TrustLevel: executionenv.TrustTrusted, ParallelWork: true}
		}},
	}
	for index, sc := range scenarios {
		person := fmt.Sprintf("corpus-%d", index)
		scope := sc.scope(person)
		cleanup := SetExecutionScope(person, scope)
		executor := ExecutionCapabilityMiddleware()(func(args map[string]interface{}) (string, error) { return "ok", nil })
		networked, claims := 0, 0
		var wrong, rows []string
		for _, item := range networkCorpus {
			args := map[string]interface{}{
				"_tenant_id": person, "_tool_name": "terminal", "command": item.command,
				"_effective_sandbox_mode": string(SandboxIsolated),
				toolExecutionPolicyArg:    toolExecutionPolicy{Origin: ToolSchemaOriginBuiltin},
			}
			if _, err := executor(args); err != nil {
				t.Fatalf("%s: %v", item.command, err)
			}
			shared := argShared(args)
			claim := externalEffectPossible(args)
			if shared {
				networked++
			}
			if claim {
				claims++
			}
			rows = append(rows, fmt.Sprintf("  %-42s network=%-5t claims=%-5t %s", item.command, shared, claim, argDecision(args)))
			if shared != item.networked || claim != (item.networked && !item.observation) {
				wrong = append(wrong, fmt.Sprintf("%s: network=%t claims=%t", item.command, shared, claim))
			}
		}
		cleanup()
		t.Logf("%s: networked %d/50, claims %d/50\n%s", sc.name, networked, claims, strings.Join(rows, "\n"))
		sort.Strings(wrong)
		if len(wrong) > 0 {
			t.Errorf("%s: networked %d/50, claims %d/50; unexpected:\n  %s", sc.name, networked, claims, strings.Join(wrong, "\n  "))
		}
		if networked != 28 || claims != 25 {
			t.Errorf("%s: networked %d/50 and claims %d/50, want 28 and 25", sc.name, networked, claims)
		}
	}
}
