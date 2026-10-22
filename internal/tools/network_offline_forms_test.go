package tools

import (
	"strings"
	"testing"

	"selfmind/internal/executionenv"
)

// Clients that were missing from the table: their purpose or their usual
// invocation is a remote service, so withholding the network from them costs a
// failed first attempt for nothing.
func TestPlausibleNetworkAdditionsAreRemoteClients(t *testing.T) {
	for _, command := range []string{
		"npx prettier --check .", "npx --yes create-react-app app", "npx tsc", "bunx prettier .", "uvx ruff check .",
		"bun install", "bun add left-pad", "bun update", "bun create vite app", "bun upgrade", "bun publish",
		"uv run pytest", "uv tool run ruff", "uv run --with requests script.py",
		"pipx install black", "pipx run black .", "pipx upgrade-all",
		"conda install numpy", "conda create -n x python=3.12", "conda update --all", "conda env create -f environment.yml",
		"mamba install numpy", "micromamba install numpy",
		"rustup update", "rustup toolchain install nightly", "rustup target add wasm32-unknown-unknown",
		"rustup component add clippy", "rustup self update", "rustup check",
		"dotnet restore", "dotnet add package Newtonsoft.Json", "dotnet tool install -g dotnet-ef",
		"corepack prepare pnpm@9 --activate", "corepack use yarn@4", "corepack yarn install", "corepack pnpm install",
		"npm init vite@latest", "npm create vite@latest app", "npm init @vitejs/app", "pnpm create vite", "yarn create react-app app",
		"psql -h db.example.com -U app", "psql --host=db.example.com app", "psql -hdb.example.com app",
		"psql postgresql://app@db.example.com/app", "psql 'host=db.example.com dbname=app'",
		"mysql -h db.example.com -u app", "redis-cli -h cache.example.com ping",
		"ping -c 1 example.com", "dig example.com", "nslookup example.com",
		"sudo -n npx prettier .", "cd web && npx tsc", "FOO=1 uvx ruff check .",
	} {
		if !networkPlausible(command) {
			t.Errorf("%q reaches a remote service but was not read as plausibly network-bound", command)
		}
	}
}

// The local siblings of those clients stay local: the table names verbs and
// forms, not programs, wherever the program has a local half.
func TestLocalSiblingsOfTheAdditionsStayLocal(t *testing.T) {
	for _, command := range []string{
		"npm init -y", "npm init", "npm init --scope=@acme", "pnpm init", "yarn init -y", "npm create",
		"psql", "psql -c 'select 1' app", "psql -d app -U me", "psql --version", "mysql -u root", "redis-cli",
		"uv --version", "uv venv", "uv python list", "uv pip list", "uv pip freeze", "uv run --no-sync pytest",
		"conda list", "conda activate base", "conda config --show", "rustup show", "rustup which cargo",
		"dotnet build", "dotnet test", "dotnet --version", "dotnet new console",
		"corepack enable", "corepack disable", "bun test", "bun run build", "bun --version",
		"pipx list", "pipx ensurepath", "pipx uninstall black",
	} {
		if networkPlausible(command) {
			t.Errorf("%q has a local form that was read as plausibly network-bound", command)
		}
	}
}

// Explicit offline and local forms are not remote, wherever the command would
// otherwise be.
func TestExplicitOfflineFormsAreNotRemote(t *testing.T) {
	for _, command := range []string{
		// The tool's own offline switch.
		"npm install --offline", "npm install --offline=true", "npm ci --offline", "yarn install --offline", "pnpm install --offline",
		"pip install --no-index -r requirements.txt", "pip install --no-index --find-links ./wheels flask", "pip3 install --no-index flask",
		"python3 -m pip install --no-index flask", "uv pip install --offline flask", "uv sync --offline", "uvx --offline ruff",
		"npx --offline prettier .", "npx --no-install tsc", "npx --no tsc", "cargo fetch --offline", "cargo install --offline ripgrep", "cargo update --offline",
		"conda install --offline numpy", "gem install --local ./x.gem", "bundle install --local",
		"cargo install --list",
		// A VCS remote that is a path on this machine.
		"git clone ./repo copy", "git clone ../repo copy", "git clone /srv/repo.git copy", "git clone file:///srv/repo.git copy",
		"git clone --depth 1 ./repo copy", "git fetch ../other", "git pull /srv/repo.git main", "git push file:///srv/r.git main",
		"git push ../mirror main", "git -C work pull ../other main",
		// Asking a program for its help or version is not using it.
		"npm install --help", "npm install -h", "git push --help", "git push -h", "git fetch --help", "kubectl --help",
		"kubectl get pods --help", "kubectl -h", "curl --help", "curl -h", "curl --version", "wget --version", "wget -h",
		"gcloud --version", "gcloud --help", "aws --version", "docker pull --help", "docker --version", "helm --version",
		"ping -h", "dig -h", "pip install --help", "cargo install --help", "terraform plan --help", "npx --version",
		// A compound command is network-bound only through a segment that is.
		"npm install --offline && npm test", "kubectl --help && go test ./...", "git clone ../a b && go build ./...",
	} {
		if networkPlausible(command) {
			t.Errorf("%q is an explicit offline or local form but was read as plausibly network-bound", command)
		}
	}
}

// The veto is about the invocation it is in. It never applies when the command
// names a remote target, when the flag means something else, or when another
// segment of a compound command is remote; and a semantic that is not offline is
// not a veto.
func TestOfflineFormVetoNeverHidesARemoteTarget(t *testing.T) {
	for _, command := range []string{
		// A remote URL or host in the invocation.
		"npm install --offline https://example.com/pkg.tgz", "pip install --no-index https://example.com/p.whl",
		"git clone ../a https://example.com/b.git", "git clone https://example.com/r.git ../copy",
		"git clone git@example.com:org/repo.git ../copy", "git fetch user@example.com:repo.git",
		"git push --force-with-lease git@example.com:org/repo.git main", "curl --help https://example.test",
		"npm install --offline --registry=https://registry.example.com left-pad",
		// A named remote, not a path: it may be anything.
		"git fetch origin", "git pull", "git push origin main", "git push", "git clone origin-mirror copy",
		// Prefer-offline still goes to the network on a miss.
		"npm install --prefer-offline", "pnpm install --prefer-offline", "yarn install --prefer-offline", "npm install --offline=false",
		// -h is a host or a human-readable switch there, not help.
		"psql -h db.example.com", "mysql -h db.example.com", "redis-cli -h cache.example.com", "rsync -h dist/ deploy@host:/srv/",
		"docker run -h build-host alpine true", "podman run -h build-host alpine true",
		// A help flag that belongs to the command a program runs, not to the program.
		"ssh deploy@host df -h", "docker run --rm alpine df -h", "kubectl exec pod -- sh -c 'df -h'",
		// Another segment is remote.
		"curl --version && git push origin main", "git clone ../a b && npm install", "npm install --offline; terraform plan",
		// The remote form of an offline-capable tool.
		"cargo install ripgrep", "cargo fetch", "pip install flask", "npm install", "git clone https://example.com/r.git",
	} {
		if !networkPlausible(command) {
			t.Errorf("%q names a remote target but a veto hid it", command)
		}
	}
}

// An explicit request is never vetoed: the model asked, and permission decides.
// The vetoed command, without the request, stays offline in a granted workspace.
func TestOfflineFormVetoDoesNotOverrideAnExplicitRequest(t *testing.T) {
	h := newNetworkHarness(t, networkHarnessOptions{store: networkOnlyCapabilityStore{}})
	for _, command := range []string{"npm install --offline", "git clone ../repo copy", "curl --version", "cargo install --list"} {
		args, _, err := h.call(command)
		if err != nil || argShared(args) || argDecision(args) != executionenv.NetworkWithheldNotBound {
			t.Errorf("%q: an offline form was networked by a grant: shared=%v decision=%q err=%v", command, argShared(args), argDecision(args), err)
		}
		args, _, err = h.call(command, "network", true)
		if err != nil || !argShared(args) || argDecision(args) != executionenv.NetworkDecisionShared {
			t.Errorf("%q with network:true: the veto overrode an explicit request: shared=%v decision=%q err=%v", command, argShared(args), argDecision(args), err)
		}
	}
	if len(h.asks) != 0 {
		t.Fatalf("a grant answers the request: %d asks", len(h.asks))
	}
}

// A help or version request for an egress program asks nothing in a workspace
// that has no network permission, and an egress program that is not asking for
// help still does.
func TestHelpFlagDoesNotAskForTheNetwork(t *testing.T) {
	h := newNetworkHarness(t, networkHarnessOptions{})
	for _, command := range []string{"curl --version", "curl -h", "wget --help"} {
		if args, _, err := h.call(command); err != nil || argShared(args) || len(h.asks) != 0 {
			t.Errorf("%q asked about the network or was networked: shared=%v asks=%d err=%v", command, argShared(args), len(h.asks), err)
		}
	}
	if args, _, err := h.call("curl https://example.test"); err != nil || !argShared(args) || len(h.asks) != 1 {
		t.Fatalf("a real curl no longer asks: shared=%v asks=%d err=%v", argShared(args), len(h.asks), err)
	}
}

// The classifier that gates dangerous egress reads the same commands as before:
// the veto lives in the network decision, not in the approval classifier.
func TestOfflineFormVetoLeavesTheEgressApprovalClassifierAlone(t *testing.T) {
	for _, command := range []string{"curl --version", "wget -h", "curl --help"} {
		if dangerous, reason := dangerousToolCall("", "terminal", map[string]interface{}{"command": command}); !dangerous || !strings.Contains(reason, "egress") {
			t.Errorf("%q stopped being classified as egress: dangerous=%v reason=%q", command, dangerous, reason)
		}
		classes := operationClassesFor("terminal", map[string]interface{}{"_tool_name": "terminal", "command": command}, false)
		found := false
		for _, class := range classes {
			found = found || class == OpClassNetwork
		}
		if !found {
			t.Errorf("%q lost the network operation class: %v", command, classes)
		}
	}
}

// ---- 6. the offline notice names only commands that can fetch ---------------

func TestCommandMayFetchDependencies(t *testing.T) {
	for _, command := range []string{
		"go test ./...", "go build ./...", "go run ./cmd/tool", "go mod tidy", "go mod verify", "go install ./cmd/tool", "go test -race -count=1 ./internal/tools/",
		"cargo build", "cargo build --release", "cargo test", "cargo check", "cargo run -- --flag", "cargo clippy", "cargo clippy --all-targets", "cargo doc",
		"npm test", "npm run build", "npm run test:unit", "npm run e2e", "npm ci", "yarn test", "yarn build", "pnpm test", "pnpm run build", "pnpm run e2e",
		"pip install -r requirements.txt", "poetry run pytest", "uv sync", "mvn test", "./mvnw verify", "gradle build", "./gradlew test",
		"bundle install", "composer test",
		// The dependency fetchers of other ecosystems.
		"dotnet build", "dotnet test", "dotnet run", "dotnet restore", "sbt test", "sbt compile", "bazel build //...", "bazel test //pkg:all", "bazel fetch //...",
		"deno run main.ts", "deno test", "deno cache main.ts", "deno install", "bun test", "bun install", "bun run build",
		"mix deps.get", "mix test", "mix compile", "swift build", "swift test", "swift package resolve",
		"flutter pub get", "flutter test", "flutter build apk", "tox", "tox -e py312", "nox", "nox -s tests",
		"conda install numpy", "mamba create -n x python=3.12", "micromamba run -n x pytest",
		"cd web && npm test", "FOO=1 go test ./...", "sh -c 'go test ./...'", "make lint && go test ./...",
	} {
		if !CommandMayFetchDependencies(command) {
			t.Errorf("%q can fetch dependencies on a cold cache but the notice would not be shown", command)
		}
	}
	for _, command := range []string{
		"go vet ./...", "go fmt ./...", "gofmt -l .", "go list ./...", "go env GOPATH", "go version",
		"cargo fmt", "cargo --version", "cargo install --list", "ls -la", "rg TODO .", "grep -r foo .", "cat go.mod", "git diff", "git status",
		"pytest -q", "python -m pytest", "python script.py", "node x.js", "make test", "make lint", "tsc --noEmit", "eslint .",
		"npm ls", "shellcheck run.sh", "jq . package.json", "echo go test", "true", "",
		// A script whose name says it only reads the sources, and a runner of
		// whatever is already installed, fetch nothing.
		"npm run lint", "npm run lint:fix", "yarn lint", "pnpm run format", "npm run typecheck", "bun run lint", "bundle exec rspec",
		"dotnet --version", "dotnet new console", "swift --version", "mix format", "deno fmt", "deno lint", "bazel info",
	} {
		if CommandMayFetchDependencies(command) {
			t.Errorf("%q cannot fetch dependencies but the notice would be shown", command)
		}
	}
	// Spelled offline, or only asked for its help, it fetches nothing.
	for _, command := range []string{"npm test --offline", "cargo build --offline", "yarn test --offline", "go test --help", "npm test -h", "pnpm --version", "mvn -o test", "gradle --offline build"} {
		if CommandMayFetchDependencies(command) {
			t.Errorf("%q cannot fetch but the notice would be shown", command)
		}
	}
}

// ---- round 3: version requests, runners, switch values, git remotes ---------

// `--version` is a request for the program's version only before it has been
// given anything to do. After the subcommand or an operand it is an option with a
// value (`cargo install ripgrep --version 13.0.0` installs that release) and the
// command is as remote as it was.
func TestVersionWithAValueIsNotAVersionRequest(t *testing.T) {
	for _, command := range []string{
		"cargo install ripgrep --version 13.0.0", "cargo install --version 13.0.0 ripgrep", "cargo add serde --version 1",
		"gem install rails --version 7.0.0", "gem install rails -v 7.0.0 --version 7.0.0",
		"helm pull repo/chart --version 1.2.3", "helm upgrade --install foo --version 1.2.3 repo/chart",
		"helm install foo repo/chart --version=1.2.3", "pip download flask --version 1",
	} {
		if !networkPlausible(command) {
			t.Errorf("%q installs a release but its --version was read as a version request", command)
		}
	}
	for _, command := range []string{
		"cargo --version", "helm --version", "gem --version", "npm --version", "pip --version", "curl -V", "wget -V", "ssh -V",
		"gcloud --version", "kubectl --version", "terraform --version", "git --version",
		// Help keeps its reach past a subcommand.
		"helm upgrade --help", "cargo install --help",
	} {
		if networkPlausible(command) {
			t.Errorf("%q only asks for a version or help but was read as plausibly network-bound", command)
		}
	}
}

// Options after the program being run belong to that program, not to the runner
// that starts it.
func TestFlagsAfterTheProgramBeingRunBelongToIt(t *testing.T) {
	for _, command := range []string{
		"docker run alpine --help", "docker run alpine --version", "podman run alpine --help", "docker exec build cmd --help",
		"npx pkg --offline", "npx --yes pkg --offline", "pnpm dlx tool --offline", "yarn dlx tool --offline", "uvx tool --offline",
		"bunx tool --offline", "pipx run tool --help", "npx pkg --help", "uvx tool --version",
	} {
		if !networkPlausible(command) {
			t.Errorf("%q passes a flag to the program it runs but the flag was read as the runner's own", command)
		}
	}
	for _, command := range []string{
		"docker run --help", "docker exec --help", "podman run --help", "npx --offline pkg", "npx --version", "npx --help",
		"pnpm dlx --offline tool", "yarn dlx --offline tool", "uvx --offline tool", "uvx --version", "pipx run --help",
	} {
		if networkPlausible(command) {
			t.Errorf("%q asks the runner itself but was read as plausibly network-bound", command)
		}
	}
	// The fetching table reads the same way.
	if !CommandMayFetchDependencies("npm exec pkg --offline") || CommandMayFetchDependencies("npm exec --offline pkg") {
		t.Error("npm exec: a flag after the package belongs to the package; before it, to npm")
	}
}

// An offline switch given a false value is not the switch, in any spelling.
func TestOfflineSwitchSetToFalseIsNotOffline(t *testing.T) {
	for _, command := range []string{
		"npm install --offline=false", "npm install --offline=FALSE", "npm install --offline=False", "npm install --offline=0",
		"npm install --offline=no", "npm install --offline=NO", "npm install foo --offline false", "npm install foo --offline No",
		"pnpm install --offline=false", "yarn install --offline=0", "cargo fetch --offline=false",
	} {
		if !networkPlausible(command) {
			t.Errorf("%q turns the offline switch off but was read as offline", command)
		}
	}
	for _, command := range []string{
		"npm install --offline", "npm install --offline=true", "npm install --offline=TRUE", "npm install --offline true",
		"npm install foo --offline", "yarn install --offline=1",
	} {
		if networkPlausible(command) {
			t.Errorf("%q is offline but was read as plausibly network-bound", command)
		}
	}
}

// A local first operand is the whole remote only when nothing widens the
// transfer to others: submodules have remotes of their own, and a multiple fetch
// takes every operand as a remote.
func TestLocalGitRemoteIsNotTheWholeTransferWhenSomethingWidensIt(t *testing.T) {
	for _, command := range []string{
		"git clone --recurse-submodules ../a", "git clone --recursive ../a copy", "git fetch --multiple ../a origin",
		"git fetch --recurse-submodules ../a", "git pull --recurse-submodules ../a main", "git push --recurse-submodules=on-demand ../mirror main",
		"git fetch --all",
	} {
		if !networkPlausible(command) {
			t.Errorf("%q can reach remotes beyond the local path but was read as local", command)
		}
	}
	for _, command := range []string{
		"git fetch --recurse-submodules=no ../a", "git clone ../a copy", "git clone --depth 1 ../a copy", "git fetch -o opt ../a",
		"git pull --rebase ../a main", "git push --force ../mirror main",
	} {
		if networkPlausible(command) {
			t.Errorf("%q only touches a path on this machine but was read as plausibly network-bound", command)
		}
	}
}
