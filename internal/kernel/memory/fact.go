package memory

import (
	"time"
)

// Fact represents a durable piece of information about the user or environment.
type Fact struct {
	ID        string    `json:"id"`
	Target    string    `json:"target"` // 'user' (preferences) or 'memory' (environment/conventions)
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}
