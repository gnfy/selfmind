package tools

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/log"
)

// ProcessInfo holds information about a background process
type ProcessInfo struct {
	ID        string
	Command   string
	CWD       string
	Cmd       *exec.Cmd
	Output    *synchronizedBuffer
	StartedAt time.Time
	mu        sync.Mutex
	status    string
	// finished closes when the process is reaped, so the ceiling watchdog exits
	// instead of lingering for its full duration on every completed process.
	finished chan struct{}
}

type synchronizedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ProcessRegistry manages background processes
type ProcessRegistry struct {
	processes map[string]*ProcessInfo
	mem       *memory.MemoryManager
	tenantID  string // For now simplified, could be dynamic
	mu        sync.RWMutex
}

func NewProcessRegistry(tenantID string) *ProcessRegistry {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		tenantID = "default"
	}
	return &ProcessRegistry{
		processes: make(map[string]*ProcessInfo),
		tenantID:  tenantID,
	}
}

var processRegistries sync.Map
var processRegistryRuntime struct {
	sync.RWMutex
	mem *memory.MemoryManager
}

func GetProcessRegistry() *ProcessRegistry {
	return GetProcessRegistryForTenant("default")
}

func GetProcessRegistryForTenant(tenantID string) *ProcessRegistry {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		tenantID = "default"
	}
	registry, _ := processRegistries.LoadOrStore(tenantID, NewProcessRegistry(tenantID))
	return registry.(*ProcessRegistry)
}

func ProcessRegistryForArgs(args map[string]interface{}) *ProcessRegistry {
	key := processRegistryScopeID(args)
	persistenceTenantID := "default"
	if scope, ok := InvocationScopeFromArgs(args); ok && strings.TrimSpace(scope.PersonID) != "" {
		persistenceTenantID = strings.TrimSpace(scope.PersonID)
	} else if tenantID, _ := args["_tenant_id"].(string); strings.TrimSpace(tenantID) != "" {
		persistenceTenantID = strings.TrimSpace(tenantID)
	}
	if registry, ok := processRegistries.Load(key); ok {
		return registry.(*ProcessRegistry)
	}
	registry := NewProcessRegistry(persistenceTenantID)
	processRegistryRuntime.RLock()
	registry.mem = processRegistryRuntime.mem
	processRegistryRuntime.RUnlock()
	actual, _ := processRegistries.LoadOrStore(key, registry)
	return actual.(*ProcessRegistry)
}

func (r *ProcessRegistry) Init(mem *memory.MemoryManager, tenantID string) {
	processRegistryRuntime.Lock()
	processRegistryRuntime.mem = mem
	processRegistryRuntime.Unlock()
	r.mu.Lock()
	r.mem = mem
	if tenantID != "" {
		r.tenantID = tenantID
	}
	r.mu.Unlock()
	r.Recover()
}

// StartProcess launches a detached background command.
//
// env must be the RUN's prepared environment, not the process default. A
// background command used to fall back to whatever the current snapshot was, so
// a run whose foreground gcloud succeeded through its state overlay could have
// its background gcloud fail — same run, same command, different environment.
// Passing nil keeps the previous behaviour for callers with no request context.
func (r *ProcessRegistry) StartProcess(command string, cwd string, env []string, ceiling time.Duration) (string, error) {
	return r.StartObservedProcess(command, cwd, env, ceiling, nil)
}

// StartObservedProcess is StartProcess with an exit callback. onExit runs once,
// after the process is reaped, with the exit code (-1 when no exit status was
// observed) and the wait error; it never runs if the start fails.
func (r *ProcessRegistry) StartObservedProcess(command string, cwd string, env []string, ceiling time.Duration,
	onExit func(processID string, exitCode int, waitErr error)) (string, error) {
	return r.StartObservedProcessInContext(context.Background(), command, cwd, env, ceiling, onExit)
}

// StartObservedProcessInContext binds the process receipt to its owning runtime.
func (r *ProcessRegistry) StartObservedProcessInContext(ctx context.Context, command string, cwd string, env []string, ceiling time.Duration,
	onExit func(processID string, exitCode int, waitErr error)) (string, error) {
	id := uuid.New().String()
	cmd := shellCommand(command)
	cmd.Dir = cwd
	if len(env) > 0 {
		cmd.Env = env
	}

	output := &synchronizedBuffer{}
	cmd.Stdout = output
	cmd.Stderr = output
	// A background command leads its own process group, like a foreground one,
	// so the daemon can still recognise anything it leaves behind after it is
	// reparented away from the daemon.
	applySandboxLimits(cmd)

	if err := cmd.Start(); err != nil {
		return "", err
	}
	recordSpawnedGroup(ctx, cmd)

	info := &ProcessInfo{
		ID:        id,
		Command:   command,
		CWD:       cwd,
		Cmd:       cmd,
		Output:    output,
		StartedAt: time.Now(),
		status:    "running",
		finished:  make(chan struct{}),
	}

	r.mu.Lock()
	r.processes[id] = info
	r.mu.Unlock()

	// Persist to DB
	if r.mem != nil {
		r.mem.SaveProcess(context.Background(), r.tenantID, memory.ProcessRecord{
			ID:        id,
			Command:   command,
			CWD:       cwd,
			PID:       cmd.Process.Pid,
			Status:    "running",
			StartedAt: info.StartedAt,
		})
	}

	// A background process outlives its run, so nothing else will ever stop it.
	// Without a ceiling a wedged command holds a process (and its scratch, and
	// its copied credential state) until the daemon restarts. The ceiling is
	// generous — this exists to stop a leak, not to bound legitimate work; a
	// genuinely long external wait belongs in watch_external, which is durable
	// and observable.
	if ceiling > 0 {
		go func() {
			timer := time.NewTimer(ceiling)
			defer timer.Stop()
			select {
			case <-info.finished:
				return
			case <-timer.C:
			}
			if cmd.Process != nil {
				log.Warn("background process exceeded its ceiling and was stopped",
					"process", id, "ceiling", ceiling)
				_ = cmd.Process.Kill()
			}
		}()
	}

	// Handle cleanup in background
	go func() {
		err := cmd.Wait()
		exitCode := 0
		status := "exited"
		if err != nil {
			status = "failed"
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = -1
			}
		}
		info.mu.Lock()
		info.status = status
		info.mu.Unlock()
		close(info.finished)

		if r.mem != nil {
			r.mem.UpdateProcessStatus(context.Background(), r.tenantID, id, status, exitCode)
		}
		if onExit != nil {
			onExit(id, exitCode, err)
		}
	}()

	return id, nil
}

// LiveBackgroundProcess is a detached command that is still running. RunID
// or LeaseID names the Run scope that started it when this daemon did; one
// recorded by an earlier daemon carries only its working directory.
type LiveBackgroundProcess struct {
	ID      string
	Command string
	CWD     string
	RunID   string
	LeaseID string
}

// LiveBackgroundProcesses lists one person's detached commands that are still
// running, so a caller can refuse to act while one may still write a root.
// Background commands are scoped per Run or lease and persisted per person;
// a command an earlier daemon started survives only in that persisted record.
func LiveBackgroundProcesses(ctx context.Context, personID string) []LiveBackgroundProcess {
	personID = strings.TrimSpace(personID)
	if personID == "" {
		return nil
	}
	var live []LiveBackgroundProcess
	seen := make(map[string]bool)
	processRegistries.Range(func(key, value any) bool {
		registry := value.(*ProcessRegistry)
		registry.mu.RLock()
		owner := registry.tenantID
		processes := make([]*ProcessInfo, 0, len(registry.processes))
		for _, process := range registry.processes {
			processes = append(processes, process)
		}
		registry.mu.RUnlock()
		if owner != personID {
			return true
		}
		scope, _ := key.(string)
		for _, process := range processes {
			process.mu.Lock()
			running := process.status == "running"
			process.mu.Unlock()
			if !running {
				continue
			}
			item := LiveBackgroundProcess{ID: process.ID, Command: process.Command, CWD: process.CWD}
			if runID, ok := strings.CutPrefix(scope, "run:"); ok {
				item.RunID = runID
			} else if leaseID, ok := strings.CutPrefix(scope, "lease:"); ok {
				item.LeaseID = leaseID
			}
			seen[process.ID] = true
			live = append(live, item)
		}
		return true
	})
	processRegistryRuntime.RLock()
	mem := processRegistryRuntime.mem
	processRegistryRuntime.RUnlock()
	if mem == nil {
		return live
	}
	records, err := mem.ListProcesses(ctx, personID)
	if err != nil {
		return live
	}
	for _, record := range records {
		if record.Status == "running" && !seen[record.ID] && processAlive(record.PID) {
			live = append(live, LiveBackgroundProcess{ID: record.ID, Command: record.Command, CWD: record.CWD})
		}
	}
	return live
}

func (r *ProcessRegistry) Recover() {
	if r.mem == nil {
		return
	}
	records, err := r.mem.ListProcesses(context.Background(), r.tenantID)
	if err != nil {
		return
	}

	for _, rec := range records {
		if rec.Status == "running" {
			if !processAlive(rec.PID) {
				r.mem.UpdateProcessStatus(context.Background(), r.tenantID, rec.ID, "lost", -1)
			}
		}
	}
}

func (r *ProcessRegistry) List() []map[string]interface{} {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var list []map[string]interface{}

	// Add currently running in-memory processes
	for id, p := range r.processes {
		p.mu.Lock()
		status := p.status
		p.mu.Unlock()
		list = append(list, map[string]interface{}{
			"id":         id,
			"command":    p.Command,
			"status":     status,
			"started_at": p.StartedAt,
			"source":     "memory",
		})
	}

	// Add persisted records from DB that are not in memory
	if r.mem != nil {
		records, _ := r.mem.ListProcesses(context.Background(), r.tenantID)
		for _, rec := range records {
			exists := false
			for _, item := range list {
				if item["id"] == rec.ID {
					exists = true
					break
				}
			}
			if !exists {
				list = append(list, map[string]interface{}{
					"id":          rec.ID,
					"command":     rec.Command,
					"status":      rec.Status,
					"started_at":  rec.StartedAt,
					"finished_at": rec.FinishedAt,
					"exit_code":   rec.ExitCode,
					"source":      "database",
				})
			}
		}
	}
	return list
}

func (r *ProcessRegistry) Poll(id string) (string, string, error) {
	r.mu.RLock()
	p, ok := r.processes[id]
	r.mu.RUnlock()

	if !ok {
		return "", "", io.EOF
	}

	p.mu.Lock()
	status := p.status
	p.mu.Unlock()

	return p.Output.String(), status, nil
}

func (r *ProcessRegistry) Kill(id string) error {
	r.mu.Lock()
	p, ok := r.processes[id]
	delete(r.processes, id)
	r.mu.Unlock()

	if !ok {
		return io.EOF
	}

	if p.Cmd.Process != nil {
		return p.Cmd.Process.Kill()
	}
	return nil
}
