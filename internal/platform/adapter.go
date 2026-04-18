package platform

import (
	"context"
)

// Message represents a cross-platform message
type Message struct {
	TenantID string
	Role     string // "user" or "agent"
	Content  string
}

// PlatformAdapter defines the interface for external messaging platforms
type PlatformAdapter interface {
	PlatformName() string
	SendMessage(ctx context.Context, tenantID string, text string) error
	ReceiveMessage(ctx context.Context, msg Message) error
	SetHandler(handler MessageHandler)
}

// MessageHandler is the function type for Agent core to handle messages
type MessageHandler func(ctx context.Context, tenantID, userInput string) (string, error)

// CLIAdapter is the built-in CLI PlatformAdapter implementation
type CLIAdapter struct {
	handler MessageHandler
}

func (c *CLIAdapter) PlatformName() string { return "cli" }

func (c *CLIAdapter) SendMessage(ctx context.Context, tenantID string, text string) error {

	return nil
}

func (c *CLIAdapter) ReceiveMessage(ctx context.Context, msg Message) error {
	if c.handler == nil {
		return nil
	}
	_, err := c.handler(ctx, msg.TenantID, msg.Content)
	return err
}

func (c *CLIAdapter) SetHandler(handler MessageHandler) {
	c.handler = handler
}

// MockAdapter is a test PlatformAdapter implementation
type MockAdapter struct {
	name    string
	handler MessageHandler
	sent    []string
}

func (m *MockAdapter) PlatformName() string { return m.name }

func (m *MockAdapter) SendMessage(ctx context.Context, tenantID string, text string) error {
	m.sent = append(m.sent, text)
	return nil
}

func (m *MockAdapter) ReceiveMessage(ctx context.Context, msg Message) error {
	if m.handler != nil {
		m.handler(ctx, msg.TenantID, msg.Content)
	}
	return nil
}

func (m *MockAdapter) SetHandler(handler MessageHandler) {
	m.handler = handler
}
