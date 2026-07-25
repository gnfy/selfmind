package kernel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	maxProfileProjects = 8
	maxManifestBytes   = 1 << 20
)

// ProjectProfile is a deterministic, read-only view of the build systems
// declared by a workspace. It gives the model evidence-backed verification
// candidates without running repository code during prompt assembly.
type ProjectProfile struct {
	Root     string
	Projects []ProjectDescriptor
}

type ProjectDescriptor struct {
	Directory      string
	Ecosystems     []string
	PackageManager string
	Manifests      []string
	Verification   []ProjectCommand
}

type ProjectCommand struct {
	Purpose string
	Command string
}

// DetectProjectProfile inspects the workspace root and, for a multi-project
// workspace, its immediate child directories. Detection is intentionally
// bounded and never executes project code.
func DetectProjectProfile(root string) ProjectProfile {
	root = strings.TrimSpace(root)
	if root == "" {
		return ProjectProfile{}
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return ProjectProfile{}
	}
	profile := ProjectProfile{Root: filepath.Clean(absolute)}
	if project, ok := detectProjectDescriptor(profile.Root, "."); ok {
		profile.Projects = append(profile.Projects, project)
		return profile
	}

	entries, err := os.ReadDir(profile.Root)
	if err != nil {
		return profile
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if len(profile.Projects) >= maxProfileProjects {
			break
		}
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if project, ok := detectProjectDescriptor(filepath.Join(profile.Root, entry.Name()), entry.Name()); ok {
			profile.Projects = append(profile.Projects, project)
		}
	}
	return profile
}

func (p ProjectProfile) Empty() bool {
	return len(p.Projects) == 0
}

// Prompt renders only detected facts and candidate verification commands.
// Commands remain candidates because a lockfile or manifest can be stale; the
// agent must still inspect the relevant project before executing them.
func (p ProjectProfile) Prompt() string {
	if p.Empty() {
		return ""
	}
	var out strings.Builder
	out.WriteString("# PROJECT PROFILE\n")
	out.WriteString("Deterministic workspace signals detected from manifests and lockfiles. Treat verification commands as candidates: inspect the relevant manifest, use the project's existing workflow, and do not invent unavailable tools or global environment overrides.\n")
	for _, project := range p.Projects {
		out.WriteString("\n## ")
		out.WriteString(projectProfileField(project.Directory))
		out.WriteString("\n")
		if len(project.Ecosystems) > 0 {
			out.WriteString("ecosystems: ")
			out.WriteString(projectProfileField(strings.Join(project.Ecosystems, ", ")))
			out.WriteString("\n")
		}
		if project.PackageManager != "" {
			out.WriteString("package_manager: ")
			out.WriteString(projectProfileField(project.PackageManager))
			out.WriteString("\n")
		}
		if len(project.Manifests) > 0 {
			out.WriteString("evidence: ")
			out.WriteString(projectProfileField(strings.Join(project.Manifests, ", ")))
			out.WriteString("\n")
		}
		if len(project.Verification) > 0 {
			out.WriteString("verification_candidates:\n")
			for _, command := range project.Verification {
				out.WriteString(fmt.Sprintf("- %s: %s\n", projectProfileField(command.Purpose), projectProfileField(command.Command)))
			}
		}
	}
	return strings.TrimSpace(out.String())
}

func projectProfileField(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	const maxRunes = 256
	runes := []rune(value)
	if len(runes) > maxRunes {
		value = string(runes[:maxRunes]) + "..."
	}
	return value
}

func detectProjectDescriptor(dir, display string) (ProjectDescriptor, bool) {
	project := ProjectDescriptor{Directory: filepath.ToSlash(display)}
	addManifest := func(name, ecosystem string) bool {
		if !regularFile(filepath.Join(dir, name)) {
			return false
		}
		project.Manifests = appendUnique(project.Manifests, name)
		project.Ecosystems = appendUnique(project.Ecosystems, ecosystem)
		return true
	}
	addCommand := func(purpose, command string) {
		for _, existing := range project.Verification {
			if existing.Command == command {
				return
			}
		}
		project.Verification = append(project.Verification, ProjectCommand{Purpose: purpose, Command: command})
	}

	if addManifest("go.mod", "go") {
		addCommand("tests", "go test ./...")
		addCommand("static analysis", "go vet ./...")
	}
	if addManifest("Cargo.toml", "rust") {
		addCommand("format", "cargo fmt --check")
		addCommand("tests", "cargo test")
		addCommand("lint", "cargo clippy --all-targets --all-features -- -D warnings")
	}
	if addManifest("Package.swift", "swift") {
		addCommand("tests", "swift test")
	}

	if addManifest("package.json", "node") {
		project.PackageManager = detectNodePackageManager(dir)
		for _, command := range nodeVerificationCommands(dir, project.PackageManager) {
			addCommand(command.Purpose, command.Command)
		}
	}

	python := false
	for _, manifest := range []string{"pyproject.toml", "setup.cfg", "setup.py", "requirements.txt"} {
		python = addManifest(manifest, "python") || python
	}
	if python {
		if regularFile(filepath.Join(dir, "pytest.ini")) ||
			regularFile(filepath.Join(dir, "conftest.py")) ||
			directory(filepath.Join(dir, "tests")) {
			addCommand("tests", "python -m pytest")
		}
		if regularFile(filepath.Join(dir, "ruff.toml")) || regularFile(filepath.Join(dir, ".ruff.toml")) {
			addCommand("lint", "ruff check .")
		}
		if regularFile(filepath.Join(dir, "mypy.ini")) || regularFile(filepath.Join(dir, ".mypy.ini")) {
			addCommand("type check", "mypy .")
		}
	}

	if addManifest("composer.json", "php") {
		for _, command := range composerVerificationCommands(dir) {
			addCommand(command.Purpose, command.Command)
		}
	}

	gradle := addManifest("build.gradle", "jvm") || addManifest("build.gradle.kts", "jvm")
	if gradle {
		if regularFile(filepath.Join(dir, "gradlew")) {
			addCommand("tests", "./gradlew test")
		} else {
			addCommand("tests", "gradle test")
		}
	}
	if addManifest("pom.xml", "jvm") {
		if regularFile(filepath.Join(dir, "mvnw")) {
			addCommand("tests", "./mvnw test")
		} else {
			addCommand("tests", "mvn test")
		}
	}

	if regularFile(filepath.Join(dir, "Gemfile")) {
		addManifest("Gemfile", "ruby")
		if regularFile(filepath.Join(dir, ".rspec")) || directory(filepath.Join(dir, "spec")) {
			addCommand("tests", "bundle exec rspec")
		} else if regularFile(filepath.Join(dir, "Rakefile")) {
			addCommand("tests", "bundle exec rake test")
		}
	}

	if regularFile(filepath.Join(dir, "CMakeLists.txt")) {
		addManifest("CMakeLists.txt", "cmake")
		if directory(filepath.Join(dir, "build")) {
			addCommand("build", "cmake --build build")
			addCommand("tests", "ctest --test-dir build --output-on-failure")
		}
	}

	for _, match := range []string{"*.sln", "*.csproj", "*.fsproj"} {
		files, _ := filepath.Glob(filepath.Join(dir, match))
		if len(files) == 0 {
			continue
		}
		project.Ecosystems = appendUnique(project.Ecosystems, "dotnet")
		for _, file := range files {
			project.Manifests = appendUnique(project.Manifests, filepath.Base(file))
		}
		addCommand("tests", "dotnet test")
		break
	}

	sort.Strings(project.Ecosystems)
	sort.Strings(project.Manifests)
	return project, len(project.Manifests) > 0
}

func detectNodePackageManager(dir string) string {
	switch {
	case regularFile(filepath.Join(dir, "pnpm-lock.yaml")):
		return "pnpm"
	case regularFile(filepath.Join(dir, "yarn.lock")):
		return "yarn"
	case regularFile(filepath.Join(dir, "bun.lock")), regularFile(filepath.Join(dir, "bun.lockb")):
		return "bun"
	default:
		return "npm"
	}
}

func nodeVerificationCommands(dir, manager string) []ProjectCommand {
	var manifest struct {
		Scripts map[string]string `json:"scripts"`
	}
	if !readJSON(filepath.Join(dir, "package.json"), &manifest) {
		return nil
	}
	run := func(script string) string {
		switch manager {
		case "pnpm":
			return "pnpm run " + script
		case "yarn":
			return "yarn run " + script
		case "bun":
			return "bun run " + script
		default:
			return "npm run " + script
		}
	}
	var commands []ProjectCommand
	for _, script := range []struct {
		Name    string
		Purpose string
	}{
		{Name: "test", Purpose: "tests"},
		{Name: "typecheck", Purpose: "type check"},
		{Name: "lint", Purpose: "lint"},
		{Name: "build", Purpose: "build"},
	} {
		body, ok := manifest.Scripts[script.Name]
		if !ok || strings.Contains(strings.ToLower(body), "no test specified") {
			continue
		}
		commands = append(commands, ProjectCommand{Purpose: script.Purpose, Command: run(script.Name)})
	}
	return commands
}

func composerVerificationCommands(dir string) []ProjectCommand {
	var manifest struct {
		Scripts map[string]interface{} `json:"scripts"`
		Require map[string]string      `json:"require-dev"`
	}
	if !readJSON(filepath.Join(dir, "composer.json"), &manifest) {
		return nil
	}
	var commands []ProjectCommand
	for _, name := range []string{"test", "phpunit", "lint", "analyse", "analyze"} {
		if _, ok := manifest.Scripts[name]; ok {
			commands = append(commands, ProjectCommand{Purpose: name, Command: "composer run " + name})
		}
	}
	if len(commands) == 0 {
		if _, ok := manifest.Require["phpunit/phpunit"]; ok {
			commands = append(commands, ProjectCommand{Purpose: "tests", Command: "vendor/bin/phpunit"})
		}
	}
	return commands
}

func readJSON(path string, target interface{}) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() > maxManifestBytes {
		return false
	}
	data, err := os.ReadFile(path)
	return err == nil && json.Unmarshal(data, target) == nil
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func directory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
