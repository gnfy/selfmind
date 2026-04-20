// Package log provides a structured logger for SelfMind.
// Uses Go 1.21+ slog with text output by default.
// Initialized once at startup via Init(opts).
package log

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
)

// Global logger instance.
var (
	logger *slog.Logger
	mu     sync.RWMutex
)

// Options configures the logger.
type Options struct {
	Level  string // "debug", "info", "warn", "error" (default: "info")
	Output io.Writer
}

// Init sets up the global logger. Must be called once at startup.
// If o.Output is nil, defaults to os.Stderr.
func Init(o Options) {
	mu.Lock()
	defer mu.Unlock()

	var level slog.Level
	switch o.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{
		Level: level,
	}

	w := o.Output
	if w == nil {
		w = os.Stderr
	}

	handler := slog.NewTextHandler(w, opts)
	logger = slog.New(handler)
	slog.SetDefault(logger)
}

// Get returns the global logger, auto-initializing with defaults if nil.
func Get() *slog.Logger {
	mu.RLock()
	if logger != nil {
		mu.RUnlock()
		return logger
	}
	mu.RUnlock()

	// Lazy init: thread-safe auto-initialization with defaults
	mu.Lock()
	if logger == nil {
		handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
		logger = slog.New(handler)
		slog.SetDefault(logger)
	}
	mu.Unlock()
	return logger
}

// Debug logs at debug level.
func Debug(msg string, args ...any) {
	Get().Debug(msg, args...)
}

// Info logs at info level.
func Info(msg string, args ...any) {
	Get().Info(msg, args...)
}

// Warn logs at warning level.
func Warn(msg string, args ...any) {
	Get().Warn(msg, args...)
}

// Error logs at error level.
func Error(msg string, args ...any) {
	Get().Error(msg, args...)
}

// Fatal logs at error level and exits with code 1.
// Use sparingly — prefer returning errors.
func Fatal(msg string, args ...any) {
	Get().Error(msg, args...)
	os.Exit(1)
}

// With returns a logger with the given attributes.
func With(args ...any) *slog.Logger {
	return Get().With(args...)
}

// Ctx returns a logger that includes context-level values.
func Ctx(ctx context.Context) *slog.Logger {
	return Get()
}
