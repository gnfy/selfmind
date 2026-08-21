package cliapp

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	appcore "selfmind/internal/app"
	"selfmind/internal/buildinfo"
	"selfmind/internal/control"
	"selfmind/internal/platform/config"
	"selfmind/internal/promptassets"
	gatewayrt "selfmind/internal/runtime/gateway"
)

func (a *App) runPromptCommandIfRequested() (bool, int) {
	if len(a.args) < 2 || a.args[1] != "prompt" {
		return false, 0
	}
	action := "list"
	args := a.args[2:]
	if len(args) > 0 {
		action, args = args[0], args[1:]
	}
	switch action {
	case "list", "status":
		return true, a.promptList(args)
	case "show":
		return true, a.promptShow(args)
	case "edit":
		return true, a.promptEdit(args)
	case "diff":
		return true, a.promptDiff(args)
	case "validate", "test":
		return true, a.promptValidate(args, action == "test")
	case "reset":
		return true, a.promptReset(args)
	case "apply":
		return true, a.promptApply(args)
	case "help", "-h", "--help":
		printPromptUsage(a.stdout)
		return true, 0
	default:
		fmt.Fprintf(a.stderr, "unknown prompt command: %s\n", action)
		printPromptUsage(a.stderr)
		return true, 2
	}
}

func (a *App) loadPromptCommandSnapshot() (*config.Config, *promptassets.Snapshot, error) {
	cfg, root, err := a.loadPromptCommandConfig()
	if err != nil {
		return nil, nil, err
	}
	snapshot, err := promptassets.Load(root)
	if err != nil {
		return cfg, nil, err
	}
	return cfg, snapshot, nil
}

func (a *App) loadPromptCommandConfig() (*config.Config, string, error) {
	cfg, err := config.LoadConfig(config.Options{Path: a.configPath})
	if err != nil {
		return nil, "", err
	}
	return cfg, promptassets.PromptRoot(cfg.Path, appcore.ResolveDataDir(cfg)), nil
}

func (a *App) promptList(args []string) int {
	fs := flag.NewFlagSet("selfmind prompt list", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	_, root, err := a.loadPromptCommandConfig()
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	snapshot, validationErr := promptassets.Load(root)
	fmt.Fprintf(a.stdout, "Prompt workspace: %s\n", root)
	if validationErr != nil {
		fmt.Fprintf(a.stdout, "Disk: INVALID (%v)\n", validationErr)
	} else {
		fmt.Fprintf(a.stdout, "Disk snapshot: %s\n", shortPromptHash(snapshot.Hash()))
	}
	if a.gatewayAppearsRunning() {
		record, recordOK := gatewayrt.NewManager(a.gatewayDataDir(), "").RunningRecord()
		if !recordOK || strings.TrimSpace(record.InstanceID) == "" {
			fmt.Fprintln(a.stdout, "Daemon: running; current instance status unavailable")
		} else if store, openErr := control.OpenExistingStoreReadOnly(a.gatewayDataDir()); openErr != nil {
			fmt.Fprintf(a.stdout, "Daemon: running; prompt status unavailable (%v)\n", openErr)
		} else {
			defer store.Close()
			if event, eventErr := store.GatewayRuntimeEventForInstance(context.Background(), record.InstanceID, "prompt.snapshot.loaded"); eventErr != nil {
				fmt.Fprintf(a.stdout, "Daemon: running; prompt status unavailable for instance %s\n", record.InstanceID)
			} else {
				var payload struct {
					SnapshotHash     string `json:"snapshot_hash"`
					Source           string `json:"source"`
					Degraded         bool   `json:"degraded"`
					BuildFingerprint string `json:"build_fingerprint"`
				}
				if json.Unmarshal(event.Payload, &payload) != nil || strings.TrimSpace(payload.SnapshotHash) == "" {
					fmt.Fprintf(a.stdout, "Daemon: running; prompt status invalid for instance %s\n", record.InstanceID)
				} else if validationErr != nil {
					fmt.Fprintf(a.stdout, "Daemon: loaded %s (%s); disk is invalid\n", promptSourceLabel(payload.Source, payload.Degraded), shortPromptHash(payload.SnapshotHash))
				} else if payload.SnapshotHash == snapshot.Hash() {
					fmt.Fprintf(a.stdout, "Daemon: loaded %s (%s)%s\n", promptSourceLabel(payload.Source, payload.Degraded), shortPromptHash(payload.SnapshotHash), promptBuildRestartNotice(payload.BuildFingerprint, record.Version))
				} else {
					fmt.Fprintf(a.stdout, "Daemon: loaded %s %s; disk is %s (restart required)\n", promptSourceLabel(payload.Source, payload.Degraded), shortPromptHash(payload.SnapshotHash), shortPromptHash(snapshot.Hash()))
				}
			}
		}
	} else {
		fmt.Fprintln(a.stdout, "Daemon: not running")
	}
	fmt.Fprintln(a.stdout, "ID                 State    File")
	for _, spec := range promptassets.Catalog() {
		path := filepath.Join(root, filepath.FromSlash(spec.RelativePath))
		mode := "unresolved"
		if snapshot != nil {
			state, _ := snapshot.File(spec.ID)
			path = state.Path
			mode = "default"
			if state.Customized {
				mode = "custom"
			} else if state.Exists {
				mode = "template"
			}
		}
		fmt.Fprintf(a.stdout, "%-18s %-10s %s\n", spec.ID, mode, path)
	}
	fmt.Fprintln(a.stdout, "Locked and not customizable: approval triage, tool contracts/schemas, task strategy, and project-context trust boundaries.")
	fmt.Fprintln(a.stdout, "Changes are loaded by the daemon at startup. Run `selfmind prompt apply` after editing.")
	if validationErr != nil {
		return 1
	}
	return 0
}

func promptSourceLabel(source string, degraded bool) string {
	source = strings.TrimSpace(source)
	if source == "" {
		source = "snapshot"
	}
	if degraded {
		return source + " (degraded)"
	}
	return source
}

func promptBuildRestartNotice(daemonFingerprint, daemonVersion string) string {
	daemonFingerprint = strings.TrimSpace(daemonFingerprint)
	if daemonFingerprint != "" {
		if daemonFingerprint == buildinfo.Fingerprint() {
			return ""
		}
		return "; daemon build differs from this CLI (restart required for built-in prompt changes)"
	}
	daemonVersion = strings.TrimSpace(daemonVersion)
	if daemonVersion != "" && daemonVersion != strings.TrimSpace(buildinfo.Version) {
		return "; daemon version differs from this CLI (restart required for built-in prompt changes)"
	}
	return ""
}

func (a *App) promptShow(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(a.stderr, "usage: selfmind prompt show <agent|role>")
		return 2
	}
	id, spec, ok := resolvePromptSpec(args[0])
	if !ok {
		fmt.Fprintf(a.stderr, "unknown prompt %q\n", args[0])
		return 2
	}
	cfg, root, err := a.loadPromptCommandConfig()
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	path := filepath.Join(root, filepath.FromSlash(spec.RelativePath))
	snapshot, validationErr := promptassets.Load(root)
	// An invalid workspace has no resolvable per-section state. Rendering every
	// section as "default" made a malformed file indistinguishable from having
	// no customizations at all, and a zero exit code told scripts the workspace
	// was fine even though the daemon would ignore it and select a safe fallback.
	exitCode := 0
	if validationErr != nil {
		exitCode = 1
	}
	fmt.Fprintf(a.stdout, "%s (%s)\nFile: %s\n", spec.Title, id, path)
	if validationErr != nil {
		fmt.Fprintf(a.stdout, "Validation: INVALID (%v)\n", validationErr)
		fmt.Fprintln(a.stdout, "The active file is not loaded. On startup the daemon keeps endpoints available by using the last-known-good snapshot for this workspace, or built-in defaults when none exists. Fix the file, then run `selfmind prompt validate`.")
	}
	fmt.Fprintln(a.stdout, "Meaning: default = built-in only; custom = replace or append according to each section policy.")
	for _, section := range spec.Sections {
		state := "unresolved"
		if snapshot != nil {
			state = string(snapshot.Value(id, section.Name).Mode)
		}
		fmt.Fprintf(a.stdout, "- %s\n", section.Name)
		fmt.Fprintf(a.stdout, "  state: %s\n", state)
		fmt.Fprintf(a.stdout, "  policy: %s\n", section.EditPolicyLabel())
		fmt.Fprintf(a.stdout, "  injection: %s (%s)\n", section.Injection, section.PlacementLabel())
		fmt.Fprintf(a.stdout, "  purpose: %s\n", section.Description)
	}
	preview, previewErr := appcore.PromptBuiltInPreview(cfg, id)
	if previewErr != nil {
		fmt.Fprintln(a.stderr, previewErr)
		return 1
	}
	fmt.Fprintln(a.stdout)
	fmt.Fprintln(a.stdout, "Built-in prompt/contract:")
	fmt.Fprintln(a.stdout)
	fmt.Fprintln(a.stdout, preview)
	fmt.Fprintln(a.stdout)
	fmt.Fprintln(a.stdout, "Local customization file:")
	fmt.Fprintln(a.stdout)
	if _, statErr := os.Lstat(path); statErr == nil {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			fmt.Fprintln(a.stderr, readErr)
			return 1
		}
		fmt.Fprint(a.stdout, string(data))
		if len(data) == 0 || data[len(data)-1] != '\n' {
			fmt.Fprintln(a.stdout)
		}
		return exitCode
	} else if !os.IsNotExist(statErr) {
		fmt.Fprintln(a.stderr, statErr)
		return 1
	}
	template, _ := promptassets.Template(id)
	fmt.Fprintln(a.stdout, "No local file exists; all sections use SelfMind's built-in defaults.")
	fmt.Fprint(a.stdout, template)
	return exitCode
}

func (a *App) promptEdit(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(a.stderr, "usage: selfmind prompt edit <agent|role>")
		return 2
	}
	id, spec, ok := resolvePromptSpec(args[0])
	if !ok {
		fmt.Fprintf(a.stderr, "unknown prompt %q\n", args[0])
		return 2
	}
	_, root, err := a.loadPromptCommandConfig()
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	path := filepath.Join(root, filepath.FromSlash(spec.RelativePath))
	info, statErr := os.Lstat(path)
	if statErr != nil && !os.IsNotExist(statErr) {
		fmt.Fprintln(a.stderr, statErr)
		return 1
	}
	if statErr == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		fmt.Fprintf(a.stderr, "%s is not a regular prompt file; use `selfmind prompt reset %s` first.\n", path, id)
		return 1
	}
	if os.IsNotExist(statErr) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
		template, _ := promptassets.Template(id)
		if err := os.WriteFile(path, []byte(template), 0o600); err != nil {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
	} else {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			fmt.Fprintln(a.stderr, readErr)
			return 1
		}
		migrated, changed, migrateErr := promptassets.MigrateLegacyContent(id, string(data))
		if migrateErr != nil {
			// Invalid active content is exactly what prompt edit is expected to
			// let the operator repair. Keep the original bytes and open it below,
			// but make the parser diagnosis visible before the editor starts.
			fmt.Fprintf(a.stderr, "Prompt needs manual repair before it can be migrated: %v\n", migrateErr)
		} else if changed {
			stamp := time.Now().UTC().Format("20060102T150405Z")
			backup := uniquePromptBackupPath(path + ".legacy-" + stamp)
			if err := os.WriteFile(backup, data, 0o600); err != nil {
				fmt.Fprintf(a.stderr, "back up legacy prompt: %v\n", err)
				return 1
			}
			if err := writeConfigBytesAtomic(path, []byte(migrated)); err != nil {
				fmt.Fprintf(a.stderr, "migrate legacy prompt: %v\n", err)
				return 1
			}
			fmt.Fprintf(a.stdout, "Migrated legacy prompt markers; backup: %s\n", backup)
		}
	}
	editor := strings.TrimSpace(os.Getenv("VISUAL"))
	if editor == "" {
		editor = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	if !a.interactive || editor == "" {
		fmt.Fprintf(a.stdout, "Prompt file ready: %s\n", path)
		fmt.Fprintln(a.stdout, "Edit it, run `selfmind prompt validate`, then `selfmind prompt apply`.")
		return 0
	}
	cmd := exec.Command("sh", "-c", editor+" \"$1\"", "selfmind-prompt-editor", path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = a.stdin, a.stdout, a.stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(a.stderr, "editor failed: %v\n", err)
		return 1
	}
	if _, err := promptassets.Load(root); err != nil {
		fmt.Fprintf(a.stderr, "Prompt validation failed: %v\n", err)
		return 1
	}
	fmt.Fprintln(a.stdout, "Prompt file is valid. Run `selfmind prompt apply` to restart the daemon and load it.")
	return 0
}

func (a *App) promptDiff(args []string) int {
	if len(args) > 1 {
		fmt.Fprintln(a.stderr, "usage: selfmind prompt diff [agent|role]")
		return 2
	}
	_, snapshot, err := a.loadPromptCommandSnapshot()
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	ids := promptassets.IDs()
	if len(args) == 1 {
		id, _, ok := resolvePromptSpec(args[0])
		if !ok {
			fmt.Fprintf(a.stderr, "unknown prompt %q\n", args[0])
			return 2
		}
		ids = []string{id}
	}
	changed := false
	for _, id := range ids {
		spec, _ := promptassets.Spec(id)
		for _, section := range spec.Sections {
			value := snapshot.Value(id, section.Name)
			if value.Mode == promptassets.ModeDefault {
				continue
			}
			changed = true
			fmt.Fprintf(a.stdout, "--- %s / %s (built-in default)\n+++ %s / %s (%s)\n", id, section.Name, id, section.Name, value.Mode)
			fmt.Fprintln(a.stdout, "- default")
			if value.Mode == promptassets.ModeOff {
				fmt.Fprintln(a.stdout, "+ off")
			} else {
				for _, line := range strings.Split(value.Text, "\n") {
					fmt.Fprintf(a.stdout, "+ %s\n", line)
				}
			}
		}
	}
	if !changed {
		fmt.Fprintln(a.stdout, "No prompt customizations.")
	}
	return 0
}

func (a *App) promptValidate(args []string, test bool) int {
	if (!test && len(args) != 0) || (test && len(args) > 1) {
		if test {
			fmt.Fprintln(a.stderr, "usage: selfmind prompt test [agent|role]")
		} else {
			fmt.Fprintln(a.stderr, "usage: selfmind prompt validate")
		}
		return 2
	}
	cfg, snapshot, err := a.loadPromptCommandSnapshot()
	if err != nil {
		fmt.Fprintf(a.stderr, "Prompt validation failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(a.stdout, "Prompt workspace is valid (catalog %d, snapshot %s).\n", promptassets.CatalogVersion, shortPromptHash(snapshot.Hash()))
	if test {
		ids := promptassets.IDs()
		if len(args) == 1 {
			id, _, ok := resolvePromptSpec(args[0])
			if !ok {
				fmt.Fprintf(a.stderr, "unknown prompt %q\n", args[0])
				return 2
			}
			ids = []string{id}
		}
		for _, id := range ids {
			preview, previewErr := appcore.PromptBuiltInPreview(cfg, id)
			if previewErr != nil || strings.TrimSpace(preview) == "" {
				fmt.Fprintf(a.stderr, "Prompt composition failed for %s: %v\n", id, previewErr)
				return 1
			}
			state, _ := snapshot.File(id)
			spec, _ := promptassets.Spec(id)
			custom := 0
			for _, section := range spec.Sections {
				value := state.Sections[section.Name]
				if value.Mode != promptassets.ModeDefault {
					custom++
				}
				composed := snapshot.Compose(id, preview, section.Name)
				switch value.Mode {
				case promptassets.ModeDefault:
					if composed != preview {
						fmt.Fprintf(a.stderr, "Prompt composition failed for %s / %s: default changed the built-in\n", id, section.Name)
						return 1
					}
				case promptassets.ModeOff:
					if strings.TrimSpace(composed) != "" {
						fmt.Fprintf(a.stderr, "Prompt composition failed for %s / %s: off did not remove the replaceable slot\n", id, section.Name)
						return 1
					}
				case promptassets.ModeCustom:
					if section.Replace {
						if strings.TrimSpace(composed) != strings.TrimSpace(value.Text) {
							fmt.Fprintf(a.stderr, "Prompt composition failed for %s / %s: replacement was not applied\n", id, section.Name)
							return 1
						}
					} else if !strings.Contains(composed, preview) || !strings.Contains(composed, value.Text) {
						fmt.Fprintf(a.stderr, "Prompt composition failed for %s / %s: append guidance was not composed\n", id, section.Name)
						return 1
					}
				}
			}
			fmt.Fprintf(a.stdout, "PASS %-18s built-in=%d bytes custom_sections=%d section_composition=verified\n", id, len([]byte(preview)), custom)
		}
		fmt.Fprintln(a.stdout, "Static section composition passed; locked contracts remain code-owned. No model call was made.")
	}
	return 0
}

func (a *App) promptReset(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(a.stderr, "usage: selfmind prompt reset <agent|role|all>")
		return 2
	}
	_, root, err := a.loadPromptCommandConfig()
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	ids := promptassets.IDs()
	if args[0] != "all" {
		id, _, ok := resolvePromptSpec(args[0])
		if !ok {
			fmt.Fprintf(a.stderr, "unknown prompt %q\n", args[0])
			return 2
		}
		ids = []string{id}
	}
	sort.Strings(ids)
	stamp := time.Now().UTC().Format("20060102T150405Z")
	moved := 0
	for _, id := range ids {
		spec, _ := promptassets.Spec(id)
		path := filepath.Join(root, filepath.FromSlash(spec.RelativePath))
		if _, statErr := os.Lstat(path); os.IsNotExist(statErr) {
			continue
		} else if statErr != nil {
			fmt.Fprintf(a.stderr, "reset %s: %v\n", id, statErr)
			return 1
		}
		backup := uniquePromptBackupPath(path + ".bak-" + stamp)
		if err := os.Rename(path, backup); err != nil {
			fmt.Fprintf(a.stderr, "reset %s: %v\n", id, err)
			return 1
		}
		moved++
		fmt.Fprintf(a.stdout, "Reset %s; backup: %s\n", id, backup)
	}
	if moved == 0 {
		fmt.Fprintln(a.stdout, "Nothing to reset.")
	} else {
		fmt.Fprintln(a.stdout, "Run `selfmind prompt apply` to load built-in defaults.")
	}
	return 0
}

func uniquePromptBackupPath(base string) string {
	if _, err := os.Lstat(base); os.IsNotExist(err) {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if _, err := os.Lstat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

func (a *App) promptApply(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(a.stderr, "usage: selfmind prompt apply")
		return 2
	}
	_, snapshot, err := a.loadPromptCommandSnapshot()
	if err != nil {
		fmt.Fprintf(a.stderr, "Prompt validation failed; daemon was not restarted: %v\n", err)
		return 1
	}
	fmt.Fprintf(a.stdout, "Validated prompt snapshot %s.\n", shortPromptHash(snapshot.Hash()))
	if !a.gatewayAppearsRunning() {
		fmt.Fprintln(a.stdout, "Gateway is not running; this snapshot will load on its next start.")
		return 0
	}
	return a.gatewayRestart(nil)
}

func resolvePromptSpec(value string) (string, promptassets.FileSpec, bool) {
	aliases := map[string]string{
		"main": promptassets.FileAgent, "memory": promptassets.FileMemoryExtract,
		"review": promptassets.FileBackgroundReview, "skills": promptassets.FileSkillCurator,
		"summary": promptassets.FileSummarizer, "recall": promptassets.FileSemanticRecall,
	}
	id := strings.ToLower(strings.TrimSpace(value))
	if alias := aliases[id]; alias != "" {
		id = alias
	}
	spec, ok := promptassets.Spec(id)
	return id, spec, ok
}

func shortPromptHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

func printPromptUsage(w interface{ Write([]byte) (int, error) }) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  selfmind prompt [list|status]")
	fmt.Fprintln(w, "  selfmind prompt show <agent|role>")
	fmt.Fprintln(w, "  selfmind prompt edit <agent|role>")
	fmt.Fprintln(w, "  selfmind prompt diff [agent|role]")
	fmt.Fprintln(w, "  selfmind prompt validate")
	fmt.Fprintln(w, "  selfmind prompt test [agent|role]")
	fmt.Fprintln(w, "  selfmind prompt reset <agent|role|all>")
	fmt.Fprintln(w, "  selfmind prompt apply")
	fmt.Fprintln(w, "Roles: memory_extract, background_review, skill_curator, summarizer, semantic_recall")
}
