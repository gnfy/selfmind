package tools

import "testing"

func networkPlausible(command string) bool {
	return commandPlausiblyNeedsEgress("terminal", map[string]interface{}{"command": command})
}

// One predicate decides whether a command reaches a remote service. Its
// positive side is data: the fetch, install, publish and remote verbs of the
// package managers, VCS remotes, and infrastructure tools.
func TestPlausibleNetworkVerbsOfPackageManagers(t *testing.T) {
	for _, command := range []string{
		"npm install", "npm i left-pad", "npm ci", "npm add left-pad", "npm update", "npm publish",
		"npm view react version", "npm audit", "npm outdated",
		"yarn add left-pad", "yarn install --frozen-lockfile", "yarn upgrade", "yarn publish", "yarn info react", "yarn audit",
		"pnpm install", "pnpm i", "pnpm add left-pad", "pnpm update", "pnpm publish", "pnpm view react", "pnpm outdated",
		"pip install -r requirements.txt", "pip3 install flask", "pip download flask", "pip3.12 install flask",
		"python3 -m pip install flask", "python -m pip download flask",
		"uv pip install flask", "uv add flask", "uv sync", "uv lock", "poetry install", "poetry add flask", "poetry update",
		"go get example.com/pkg@latest", "go install example.com/cmd/tool@latest", "go install -v example.com/cmd/tool@v1.2.3",
		"go mod download", "go list -m -u all", "go list -u -m all",
		"cargo fetch", "cargo install ripgrep", "cargo install --locked ripgrep", "cargo install --git https://example.com/x.git",
		"cargo update", "cargo publish", "cargo search serde",
		"gem install rails", "gem update", "bundle install", "bundle update",
		"composer install", "composer update",
		"brew install jq", "brew update", "brew upgrade", "apt install -y jq", "apt-get update", "apt-get install -y jq",
		"apt-get upgrade -y", "dnf install -y jq", "dnf upgrade", "yum install -y jq", "yum update", "apk add jq",
		"apk update", "pacman -Syu", "pacman -S jq",
	} {
		if !networkPlausible(command) {
			t.Errorf("%q fetches from a registry but was not read as plausibly network-bound", command)
		}
	}
}

func TestPlausibleNetworkVerbsOfVersionControlAndInfrastructure(t *testing.T) {
	for _, command := range []string{
		"git fetch origin", "git pull --rebase", "git push origin main", "git clone https://example.com/r.git",
		"git ls-remote origin", "git remote update", "git remote prune origin", "git remote show origin",
		"git submodule update --init", "git lfs pull", "git lfs push origin main",
		"yarn", "yarn --frozen-lockfile", "yarn --cwd web",
		"hg pull", "hg push", "hg clone https://example.com/r", "svn update", "svn checkout https://example.com/r", "svn commit -m x",
		"terraform init", "terraform plan", "terraform apply -auto-approve", "terraform destroy", "terraform refresh",
		"terraform import aws_s3_bucket.b b", "terraform providers", "tofu init", "tofu plan", "tofu apply",
		"pulumi up", "pulumi preview", "ansible all -m ping", "ansible-playbook site.yml", "ansible-galaxy install x.y",
		"docker pull alpine", "docker push registry.example.com/app:1", "docker login", "podman pull alpine", "podman push x",
		"helm repo update", "helm pull stable/nginx", "helm install web ./chart", "helm upgrade web ./chart",
		"kubectl get pods", "oc get pods", "oc apply -f x.yaml",
	} {
		if !networkPlausible(command) {
			t.Errorf("%q talks to a remote service but was not read as plausibly network-bound", command)
		}
	}
}

// Building, testing, linting, and running local code never names a remote
// service; reaching for the network is then the command's own business and the
// post-failure path covers a cold cache.
func TestLocalCommandsAreNotPlausiblyNetworkBound(t *testing.T) {
	for _, command := range []string{
		"go build ./...", "go test ./...", "go test -race -count=1 ./internal/tools/", "go vet ./...", "go fmt ./...",
		"go list ./...", "go list -m", "go mod tidy", "go env GOPATH", "go run ./cmd/tool",
		// Installing the module in front of you is a build; installing a remote one is not.
		"go install ./cmd/tool", "go install -v ./...", "cargo install --path .", "cargo install --path=./tool --locked",
		"cargo build", "cargo build --release", "cargo test", "cargo check", "cargo clippy", "cargo fmt",
		"npm test", "npm run build", "npm run install-deps", "npm run test -- --watch", "npm ls", "npm pack",
		"yarn test", "yarn build", "yarn run lint", "pnpm test", "pnpm run build",
		"pytest", "pytest -q tests/", "python script.py", "python3 -m pytest", "python -m venv .venv", "pip list", "pip freeze",
		"node x.js", "node --test", "make", "make test", "make install", "mvn test", "mvn package", "gradle build", "./gradlew test",
		"git status", "git diff", "git log --oneline", "git add -A", "git commit -m wip", "git show HEAD", "git branch",
		"git checkout -b feature", "git merge main", "git rebase main", "git stash", "git remote -v", "git config user.name",
		"git remote show -n origin", "git remote add origin git@example.com:x/y.git", "git remote get-url origin",
		"yarn --version", "yarn -v", "yarn --help",
		"ls -la", "cat go.mod", "rg TODO .", "jq . package.json", "sed -n '1,20p' main.go", "grep -r foo .", "find . -name '*.go'",
		"terraform fmt", "terraform validate",
	} {
		if networkPlausible(command) {
			t.Errorf("%q only builds, tests, or runs local code but was read as plausibly network-bound", command)
		}
	}
}

// The same invariant when the verb is spelled differently: through wrappers,
// assignments, global options, paths, case, or compound commands. A compound
// command is plausible when any segment is.
func TestPlausibleNetworkVerbsThroughWrappersAndCompoundCommands(t *testing.T) {
	for _, command := range []string{
		"FOO=1 npm ci", "NODE_ENV=production yarn install", "cd web && npm install", "(cd services/api; yarn add left-pad)",
		"sudo apt-get install -y jq", "sudo -n dnf install -y jq", "env GIT_SSH_COMMAND=ssh git push", "time git clone https://example.com/r.git",
		"nice -n 5 npm ci", "nohup git pull &", "bash -c 'git fetch --all'", "sh -lc \"cd app && npm ci && npm test\"",
		"/usr/bin/git push origin main", "/opt/homebrew/bin/npm install", "GIT push origin main",
		"git -C repo pull", "git -c http.proxy=http://127.0.0.1:7890 push", "git --git-dir=.git fetch",
		"npm --prefix web install", "npm --silent ci", "pnpm --filter web add left-pad", "cargo +nightly install ripgrep",
		"npm test && git push origin main", "go build ./... && go mod download", "echo start; terraform plan; echo done",
		"for r in a b; do git -C $r pull; done", "if [ -d .git ]; then git fetch; fi",
		"xargs -n1 git fetch < repos.txt", `find . -name .git -execdir git pull {} \;`,
		"git status && git diff | head && git push",
	} {
		if !networkPlausible(command) {
			t.Errorf("%q contains a remote verb but was not read as plausibly network-bound", command)
		}
	}
	for _, command := range []string{
		"cd web && npm test", "FOO=1 go test ./...", "echo npm install", "echo 'git push origin main'",
		"git commit -m \"fix pull request template\"", "git commit -m 'push the button'", "grep -r 'git fetch' docs/",
		"cat install.md", "ls fetch", "go test -run TestInstall ./...", "pytest -k install", "make fetch-data-offline",
		"git log --grep=push", "rg 'npm install' README.md", "sh -c 'echo hello'", "npm run push-release-notes",
	} {
		if networkPlausible(command) {
			t.Errorf("%q only mentions a remote verb but was read as plausibly network-bound", command)
		}
	}
}

// Malformed and truncated payloads read as no remote service rather than
// panicking or guessing: a verb that is not there cannot be read.
func TestPlausibleNetworkReadsTruncatedAndMalformedPayloads(t *testing.T) {
	for _, command := range []string{
		"", " ", ";", "&&", "|", "$(", "'unterminated", "git", "git -C", "git -c", "git -C repo", "npm --prefix", "npm",
		"pacman -", "pacman", "python -m", "python -m pip", "python3 -m pip --version", "go", "go list -m", "cargo +",
		"cargo +nightly", "FOO=", "sudo", "env -i", "xargs", "bash -c", "sh -c ''", "pip", "pip install", "uv pip",
	} {
		want := command == "pip install" // the verb is there, its operands are not needed
		if got := networkPlausible(command); got != want {
			t.Errorf("%q: plausible=%v, want %v", command, got, want)
		}
	}
}

// The predicate belongs to execution tools; another tool's arguments never make
// a call network-bound.
func TestPlausibleNetworkIsOnlyAboutExecutionTools(t *testing.T) {
	for _, tool := range []string{"write_file", "read_file", "patch", "web_search"} {
		if commandPlausiblyNeedsEgress(tool, map[string]interface{}{"command": "git push origin main", "path": "x"}) {
			t.Errorf("%s was read as network-bound from a command-shaped argument", tool)
		}
	}
	if !commandPlausiblyNeedsEgress("watch_external", map[string]interface{}{"command": "true"}) {
		t.Error("a durable external watch always reaches a remote service")
	}
	// Python source is not shell: a string that mentions a command is not one.
	for _, code := range []string{"print('npm install is slow')", "message = \"run git push when done\"\nprint(message)"} {
		if commandPlausiblyNeedsEgress("execute_code", map[string]interface{}{"code": code}) {
			t.Errorf("execute_code was read as network-bound from a string in its source: %q", code)
		}
	}
}
