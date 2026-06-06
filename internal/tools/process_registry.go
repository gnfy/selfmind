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
)

// ProcessInfo holds information about a background process
type ProcessInfo struct {
	ID        string
	Command   string
	Cmd       *exec.Cmd
	Output    *bytes.Buffer
	StartedAt time.Time
	mu        sync.Mutex
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
	if scope, ok := currentExecutionScope(args); ok && scope.TenantID != "" {
		return GetProcessRegistryForTenant(scope.TenantID)
	}
	if tenantID, _ := args["_tenant_id"].(string); strings.TrimSpace(tenantID) != "" {
		return GetProcessRegistryForTenant(tenantID)
	}
	return GetProcessRegistry()
}

func (r *ProcessRegistry) Init(mem *memory.MemoryManager, tenantID string) {
	r.mu.Lock()
	r.mem = mem
	if tenantID != "" {
		r.tenantID = tenantID
	}
	r.mu.Unlock()
	r.Recover()
}

func (r *ProcessRegistry) StartProcess(command string, cwd string) (string, error) {
	id := uuid.New().String()
	cmd := shellCommand(command)
	cmd.Dir = cwd

	output := &bytes.Buffer{}
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
		status := "running"
		if p.Cmd.ProcessState != nil && p.Cmd.ProcessState.Exited() {
			status = "exited"
		}
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
	defer p.mu.Unlock()

	status := "running"
	if p.Cmd.ProcessState != nil && p.Cmd.ProcessState.Exited() {
		status = "exited"
	}

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
