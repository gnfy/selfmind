package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"selfmind/internal/kernel"
	"selfmind/internal/platform/log"
	"selfmind/internal/tools/envprofiles"
	"selfmind/internal/tools/sandbox"
)

// A command that writes the operator's credential caches in place is guarded
// on both sides. Just before it starts, whatever the caches hold that a host
// tool must never meet is moved into quarantine. From a deferred step that
// runs however the attempt ends — success, failure, timeout, cancellation, or
// a panic after the start — every process still running under that one
// command's sandbox is stopped, found by the per-command canary its profile
// carries even when it left the command's process group, and the caches are
// scanned again. The daemon repairs the recorded write set at startup, for a
// command a crash or kill interrupted, and waits for in-flight repairs at
// shutdown.

const (
	// hostStateNoticeEntries and hostStateNoticeEntryBytes bound what one
	// notice lists; hostStateEventEntries bounds the evidence event. A cache
	// can hold thousands of planted entries with attacker-chosen names.
	hostStateNoticeEntries    = 10
	hostStateNoticeEntryBytes = 160
	hostStateEventEntries     = 20
	// hostStateQuarantineKeep and hostStateQuarantineMaxAge bound how long
	// quarantined copies — which can hold the operator's tokens — are kept.
	hostStateQuarantineKeep   = 20
	hostStateQuarantineMaxAge = 7 * 24 * time.Hour
)

// afterGuardedStart runs, in tests only, between a guarded command's start
// and its repair.
var afterGuardedStart func()

// stopLeftovers and leftoverStopAvailable are replaced only by tests.
var (
	stopLeftovers         = sandbox.StopProcessesUnderCanary
	leftoverStopAvailable = sandbox.LeftoverStopSupported
)

// HostStateRecordsDir holds the runtime's records of what sandboxed commands
// left: canary markers and the quarantine. A seatbelt policy keeps it
// read-only even inside a command's writable root.
func (r *ExecutionRuntime) HostStateRecordsDir() string {
	root := ""
	if r != nil {
		root = r.runtimeRoot
	}
	if root == "" {
		return ""
	}
	return filepath.Join(root, "host-state")
}

func (r *ExecutionRuntime) trackHostStateCommand(delta int) {
	r.commands.Lock()
	r.commands.inFlight += delta
	r.commands.Unlock()
}

// WaitForHostStateRepairs waits, up to timeout, until no guarded command is
// still between its start and the end of its repair. The daemon calls it
// before exiting so an interrupted command's caches are scanned first.
func (r *ExecutionRuntime) WaitForHostStateRepairs(timeout time.Duration) bool {
	if r == nil {
		return true
	}
	deadline := time.Now().Add(timeout)
	for {
		r.commands.Lock()
		idle := r.commands.inFlight == 0
		r.commands.Unlock()
		if idle {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// runGuardedCommand runs one attempt of a command, guarding the credential
// caches it writes in place (see above). Notices for what was repaired or
// stopped are appended to the command's output and recorded in result.
func runGuardedCommand(ctx context.Context, req ExecutionRequest, material execMaterial, cmd *exec.Cmd,
	result *ExecutionResult, run func() (string, error)) (out string, err error) {
	if len(material.HostStatePlan.Writes) == 0 {
		return run()
	}
	r := executionRuntimeFromContext(ctx)
	if r == nil {
		return "", errors.New("sandbox recovery runtime unavailable; command was not started")
	}
	// The deferred repair is installed before anything it undoes: the
	// in-flight count and the canary record.
	tracked := false
	marker := ""
	var before []envprofiles.QuarantinedStateEntry
	beforeNotice := ""
	defer func() {
		if !tracked {
			return
		}
		defer r.trackHostStateCommand(-1)
		if cmd != nil && cmd.Process != nil {
			stopProcessGroup(cmd)
		}
		stopped, stopErr := stopLeftovers(material.SandboxCanary)
		after, afterNotice := r.repairHostState(ctx, req, material, "after this command", stopped, stopErr)
		unresolved := false
		for _, repair := range after {
			unresolved = unresolved || strings.Contains(repair.Error, "left in place")
		}
		if stopErr == nil && !unresolved {
			// Kept otherwise, so the next startup sweep tries again.
			r.forgetSandboxCanary(marker)
		}
		result.HostStateRepairs = append(result.HostStateRepairs, before...)
		result.HostStateRepairs = append(result.HostStateRepairs, after...)
		for _, notice := range []string{beforeNotice, afterNotice} {
			if notice != "" {
				out = appendRuntimeNotice(out, notice)
			}
		}
	}()
	if leftoverStopAvailable() {
		// Persist recovery scope before changing caches or starting a process.
		marker, err = r.recordSandboxCanary(material.SandboxCanary, material.HostStatePlan)
		if err != nil {
			return "", fmt.Errorf("record sandbox recovery scope before execution: %w", err)
		}
	}
	r.trackHostStateCommand(1)
	tracked = true
	before, beforeNotice = r.repairHostState(ctx, req, material, "before this command", nil, nil)
	out, err = run()
	if afterGuardedStart != nil {
		afterGuardedStart()
	}
	return out, err
}

// newSandboxCanary is a fresh, unguessable mark for one command's sandbox.
func newSandboxCanary() string { return sandbox.NewCanary() }

// sandboxCanaryDir records the canaries of this daemon's guarded commands
// that have not finished, so it can stop what they left after a crash.
func (r *ExecutionRuntime) sandboxCanaryDir() string {
	records := r.HostStateRecordsDir()
	if records == "" {
		return ""
	}
	return filepath.Join(records, "canaries", r.instanceName)
}

func (r *ExecutionRuntime) forgetSandboxCanary(marker string) {
	if marker != "" {
		_ = os.Remove(marker)
	}
}

// repairHostState scans the material's caches, records what moved as the
// call's evidence, and returns the notice the tool result carries ("" when
// nothing was repaired or stopped).
func (r *ExecutionRuntime) repairHostState(ctx context.Context, req ExecutionRequest, material execMaterial, phase string,
	stopped []int, stopErr error) ([]envprofiles.QuarantinedStateEntry, string) {
	quarantine := r.hostStateQuarantineDir()
	repairs := envprofiles.RepairHostStateWrites(material.HostStatePlan, quarantine)
	if len(repairs) == 0 && len(stopped) == 0 && stopErr == nil {
		return nil, ""
	}
	notice := hostStateRepairNotice(envValue(material.Env, "HOME"), quarantine, phase, repairs, stopped, stopErr)
	log.Warn("guarded an in-place credential cache around a sandboxed command",
		"tool", req.ToolName, "phase", phase, "entries", len(repairs), "stopped", len(stopped), "quarantine", quarantine)
	emitToolProgress(kernel.EventChannelFromContext(ctx), "tool.environment", hostStateRepairEvidence(req, phase, quarantine, repairs, stopped, notice), notice)
	if len(repairs) > 0 {
		r.sweepHostStateQuarantine(time.Now())
	}
	return repairs, notice
}

// hostStateRepairEvidence is the bounded event payload for one repair.
func hostStateRepairEvidence(req ExecutionRequest, phase, quarantine string, repairs []envprofiles.QuarantinedStateEntry,
	stopped []int, notice string) map[string]interface{} {
	listed := repairs
	if len(listed) > hostStateEventEntries {
		listed = listed[:hostStateEventEntries]
	}
	return map[string]interface{}{
		"tool_name":           req.ToolName,
		"tool_call_id":        req.ToolCallID,
		"phase":               phase,
		"state_repairs":       listed,
		"state_repairs_total": len(repairs),
		"stopped_processes":   len(stopped),
		"quarantine":          quarantine,
		"notes":               []string{notice},
	}
}

// hostStateQuarantineRoot holds every quarantine; no tool reads it and it is
// never under a tool's own state root.
func (r *ExecutionRuntime) hostStateQuarantineRoot() string {
	records := r.HostStateRecordsDir()
	if records == "" {
		return ""
	}
	return filepath.Join(records, "quarantine")
}

// hostStateQuarantineDir is a fresh directory for one repair, or "" when no
// runtime root is installed (entries that cannot be moved are then removed
// or reported by the scan).
func (r *ExecutionRuntime) hostStateQuarantineDir() string {
	root := r.hostStateQuarantineRoot()
	if root == "" {
		return ""
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	return filepath.Join(root, time.Now().UTC().Format("20060102T150405.000000000Z")+"-"+hex.EncodeToString(suffix))
}

// sweepHostStateQuarantine deletes quarantines older than the retention age
// and all but the newest hostStateQuarantineKeep, returning how many it
// removed. Quarantine names begin with their UTC creation time.
func (r *ExecutionRuntime) sweepHostStateQuarantine(now time.Time) int {
	root := r.hostStateQuarantineRoot()
	if root == "" {
		return 0
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })
	removed := 0
	for index, entry := range entries {
		expired := index >= hostStateQuarantineKeep
		if info, err := entry.Info(); err == nil && now.Sub(info.ModTime()) > hostStateQuarantineMaxAge {
			expired = true
		}
		if expired && os.RemoveAll(filepath.Join(root, entry.Name())) == nil {
			removed++
		}
	}
	return removed
}

// hostStateRepairNotice tells the model and the person what was repaired or
// stopped, in the tool result itself. It is bounded, and every path is quoted
// with control characters and brackets escaped, so a planted name can neither
// flood the result nor pass for a runtime notice.
func hostStateRepairNotice(home, quarantine, phase string, repairs []envprofiles.QuarantinedStateEntry,
	stopped []int, stopErr error) string {
	var parts []string
	if len(repairs) > 0 {
		items := make([]string, 0, hostStateNoticeEntries)
		moved := false
		for index, repair := range repairs {
			moved = moved || repair.MovedTo != ""
			if index >= hostStateNoticeEntries {
				continue
			}
			item := quoteStateName(compactPathsUnderHome(home, []string{repair.Path})[0]) + " (" + boundedStateText(repair.Kind) + ")"
			if repair.Error != "" {
				item += ": " + boundedStateText(repair.Error)
			}
			items = append(items, item)
		}
		listing := strings.Join(items, "; ")
		if more := len(repairs) - len(items); more > 0 {
			listing += fmt.Sprintf("; and %d more", more)
		}
		part := fmt.Sprintf("selfmind repaired the credential cache %s: %d %s that host tools must never meet %s found where the sandbox writes in place — %s",
			phase, len(repairs), pluralWord(len(repairs), "entry", "entries"), pluralWord(len(repairs), "was", "were"), listing)
		if moved {
			part += ". Moved to the quarantine at " + quarantine
		}
		parts = append(parts, part)
	}
	if len(stopped) > 0 {
		parts = append(parts, fmt.Sprintf("selfmind stopped %d %s this command left running under its sandbox",
			len(stopped), pluralWord(len(stopped), "process", "processes")))
	}
	if stopErr != nil {
		parts = append(parts, "selfmind could not confirm that every process this command left under its sandbox was stopped: "+boundedStateText(stopErr.Error()))
	}
	return strings.Join(parts, ". ") + "."
}

// quoteStateName renders a path a sandboxed process may have named: quoted,
// ASCII, with brackets escaped, and bounded.
func quoteStateName(path string) string {
	if len(path) > hostStateNoticeEntryBytes {
		path = path[:hostStateNoticeEntryBytes] + "…"
	}
	quoted := strconv.QuoteToASCII(path)
	return strings.NewReplacer("[", `\x5b`, "]", `\x5d`).Replace(quoted)
}

// boundedStateText is runtime text that may embed a planted name (an error
// message): one line, brackets escaped, bounded.
func boundedStateText(text string) string {
	quoted := strconv.QuoteToASCII(text)
	quoted = strings.NewReplacer("[", `\x5b`, "]", `\x5d`).Replace(quoted[1 : len(quoted)-1])
	if len(quoted) > 2*hostStateNoticeEntryBytes {
		quoted = quoted[:2*hostStateNoticeEntryBytes] + "…"
	}
	return quoted
}

func pluralWord(count int, one, many string) string {
	if count == 1 {
		return one
	}
	return many
}

// appendRuntimeNotice adds a runtime notice after a command's own output.
func appendRuntimeNotice(output, notice string) string {
	if strings.TrimSpace(output) == "" {
		return "[" + notice + "]"
	}
	return strings.TrimRight(output, "\n") + "\n[" + notice + "]"
}

// HostStateSweep is what the daemon's startup sweep did.
type HostStateSweep struct {
	At            time.Time
	Quarantine    string
	Repairs       []envprofiles.QuarantinedStateEntry
	Stopped       []int
	Expired       int
	LegacyRecords int
	Err           error
}

// inPlaceStateHost reports whether this host's isolation backend writes tool
// state in place, so there are caches a sandbox may have written.
func inPlaceStateHost() bool {
	backend := SandboxBackendForMode(SandboxIsolated)
	return backend != nil && backend.Available() && !backend.MountsToolState()
}

// SweepHostStateAtStartup stops whatever a previous daemon's guarded commands
// left running, then repairs only their recorded cache paths with the same
// scan and quarantine as a command's repair, and applies quarantine retention.
// A crash or kill never reaches a command's own repair.
func (r *ExecutionRuntime) SweepHostStateAtStartup() HostStateSweep {
	sweep := HostStateSweep{At: time.Now()}
	if r == nil || !inPlaceStateHost() {
		return sweep
	}
	if dir := r.sandboxCanaryDir(); dir != "" {
		markers, readErr := os.ReadDir(dir)
		if readErr != nil && !os.IsNotExist(readErr) {
			sweep.Err = readErr
		}
		for _, marker := range markers {
			// Only a canary this runtime generated names anything to stop.
			if marker.IsDir() || !sandbox.ValidCanary(marker.Name()) {
				continue
			}
			stopped, err := stopLeftovers(marker.Name())
			sweep.Stopped = append(sweep.Stopped, stopped...)
			if err != nil {
				sweep.Err = errors.Join(sweep.Err, err)
				continue
			}
			path := filepath.Join(dir, marker.Name())
			plan, legacy, err := readSandboxCanary(path)
			if err != nil {
				sweep.Err = errors.Join(sweep.Err, err)
				continue
			}
			if legacy {
				sweep.LegacyRecords++
			}
			if len(plan.Writes) > 0 && sweep.Quarantine == "" {
				sweep.Quarantine = r.hostStateQuarantineDir()
			}
			repairs := envprofiles.RepairHostStateWrites(plan, sweep.Quarantine)
			sweep.Repairs = append(sweep.Repairs, repairs...)
			unresolved := false
			for _, repair := range repairs {
				if repair.Error != "" && strings.Contains(repair.Error, "left in place") {
					unresolved = true
					sweep.Err = errors.Join(sweep.Err, errors.New(repair.Error))
				}
			}
			if !unresolved {
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					sweep.Err = errors.Join(sweep.Err, err)
				}
			}
		}
	}
	sweep.Expired = r.sweepHostStateQuarantine(time.Now())
	if len(sweep.Repairs) > 0 || len(sweep.Stopped) > 0 || sweep.Err != nil || sweep.LegacyRecords > 0 {
		log.Warn("startup sweep guarded the in-place credential caches",
			"entries", len(sweep.Repairs), "stopped", len(sweep.Stopped), "quarantine", sweep.Quarantine, "legacy_records_without_cache_scope", sweep.LegacyRecords, "error", sweep.Err)
	}
	r.lastSweep.Store(&sweep)
	return sweep
}

// HostStateQuarantineStatus describes the quarantine for /diag execution:
// where it is, how many quarantines it keeps, its retention, and the last
// startup sweep. Paths and counts only, never what an entry contains.
func (r *ExecutionRuntime) HostStateQuarantineStatus() string {
	root := r.hostStateQuarantineRoot()
	if root == "" || !inPlaceStateHost() {
		return ""
	}
	kept := 0
	if entries, err := os.ReadDir(root); err == nil {
		kept = len(entries)
	}
	status := fmt.Sprintf("%s (%d kept; each is deleted after %d days or once %d newer exist)",
		root, kept, int(hostStateQuarantineMaxAge/(24*time.Hour)), hostStateQuarantineKeep)
	if sweep := r.lastSweep.Load(); sweep != nil {
		status += fmt.Sprintf("; startup sweep %s: %d %s quarantined, %d %s stopped",
			sweep.At.UTC().Format(time.RFC3339), len(sweep.Repairs), pluralWord(len(sweep.Repairs), "entry", "entries"),
			len(sweep.Stopped), pluralWord(len(sweep.Stopped), "process", "processes"))
		if sweep.Err != nil {
			status += " (" + boundedStateText(sweep.Err.Error()) + ")"
		}
		if sweep.LegacyRecords > 0 {
			status += fmt.Sprintf("; %d legacy records had no cache scope; their cache repair was skipped", sweep.LegacyRecords)
		}
	}
	return status
}
