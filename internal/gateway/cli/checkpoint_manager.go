package cli

import (
	"selfmind/internal/kernel/memory"
)

// Package-level state for checkpoint tool injection.
// Set by the controller before the tool is used.
var (
	checkpointMemGetter func() (*memory.MemoryManager, string, string)
)

// SetCheckpointMemGetter registers the memory/tenant/channel getter.
// Called from main.go after creating the controller.
func SetCheckpointMemGetter(fn func() (*memory.MemoryManager, string, string)) {
	checkpointMemGetter = fn
}

// GetCheckpointMem returns the current memory manager and tenant context.
func GetCheckpointMem() (*memory.MemoryManager, string, string) {
	if checkpointMemGetter == nil {
		return nil, "", ""
	}
	return checkpointMemGetter()
}

// checkpointMessagesGetter is called by the CheckpointTool to get current conversation messages.
var checkpointMessagesGetter func() ([]ChatMessage, error)

// SetCheckpointMessagesFn registers a function that returns the current conversation messages.
func SetCheckpointMessagesFn(fn func() ([]ChatMessage, error)) {
	checkpointMessagesGetter = fn
}

// GetCheckpointMessages returns the current conversation messages for checkpointing.
func GetCheckpointMessages() ([]ChatMessage, error) {
	if checkpointMessagesGetter == nil {
		return nil, nil
	}
	return checkpointMessagesGetter()
}
