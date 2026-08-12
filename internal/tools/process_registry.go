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
	id := uuid.New().String()
	cmd := shellCommand(command)
	cmd.Dir = cwd
	if len(env) > 0 {
		cmd.Env = env
	}

	output := &synchronizedBuffer{}
	cmd.Stdout = output
	cmd.Stderr = output

	if err := cmd.Start(); err != nil {
		return "", err
	}

	info := &ProcessInfo{
		ID:        id,
		Command:   command,
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
	}()

	return id, nil
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
