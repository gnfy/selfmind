package memory

import (
	"context"
	"fmt"
	"time"
)

// Checkpoint represents a named session snapshot.
type Checkpoint struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	TenantID  string    `json:"tenant_id"`
	Channel   string    `json:"channel"`
	Messages  []byte    `json:"messages"` // JSON-encoded []llm.Message
	CreatedAt time.Time `json:"created_at"`
}

// SaveCheckpoint stores a named snapshot of the current conversation.
func (p *SQLiteProvider) SaveCheckpoint(ctx context.Context, tenantID, channel, name string, messages []byte) error {
	_, err := p.call("SaveCheckpoint", tenantID, channel, name, messages)
	return err
}

// ListCheckpoints returns all checkpoints for a tenant/channel.
func (p *SQLiteProvider) ListCheckpoints(ctx context.Context, tenantID, channel string) ([]Checkpoint, error) {
	val, err := p.call("ListCheckpoints", tenantID, channel)
	if err != nil {
		return nil, err
	}
	if val == nil {
		return nil, nil
	}
	return val.([]Checkpoint), nil
}

// LoadCheckpoint retrieves a specific checkpoint by name.
func (p *SQLiteProvider) LoadCheckpoint(ctx context.Context, tenantID, channel, name string) ([]byte, error) {
	val, err := p.call("LoadCheckpoint", tenantID, channel, name)
	if err != nil {
		return nil, err
	}
	if val == nil {
		return nil, fmt.Errorf("checkpoint %q not found", name)
	}
	return val.([]byte), nil
}

// DeleteCheckpoint removes a checkpoint by name.
func (p *SQLiteProvider) DeleteCheckpoint(ctx context.Context, tenantID, channel, name string) error {
	_, err := p.call("DeleteCheckpoint", tenantID, channel, name)
	return err
}

// checkpointOp is used internally to carry typed results from the worker.
type checkpointOp struct {
	method string
	args   []interface{}
	result chan checkpointResult
}

type checkpointResult struct {
	val interface{}
	err error
}

// runCheckpointOp executes a checkpoint operation synchronously via the worker.
// We reuse the existing worker goroutine but send over a separate channel
// to avoid mixing with regular ops. Since the worker already handles SQLite
// per-tenant with its own mutex, we send the operation over opCh directly
// using the existing mechanism — we just need to add new "SaveCheckpoint",
// "ListCheckpoints", "LoadCheckpoint", "DeleteCheckpoint" cases to the worker.
