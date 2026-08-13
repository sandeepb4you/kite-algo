// Package logger provides a configured structured logger (log/slog) for the
// trading platform. Structured logging is essential in trading: every tick,
// order, and fill should be queryable after the fact.
package logger

import (
	"io"
	"log/slog"
	"os"
	"sync"
)

var (
	defaultLogger *slog.Logger
	once          sync.Once
)

// New returns a slog.Logger writing to w at the given level.
// format "json" produces JSON output (good for ingestion); anything else
// produces a human-readable text format (good for the console).
func New(w io.Writer, level slog.Level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level:     level,
		AddSource: false,
	}
	var handler slog.Handler
	if format == "json" {
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}
	return slog.New(handler)
}

// Default returns a process-wide logger, initialized once.
// Defaults to text format at Info level on stderr. Call Init once at startup
// from main to override.
func Default() *slog.Logger {
	once.Do(func() {
		defaultLogger = New(os.Stderr, slog.LevelInfo, "text")
	})
	return defaultLogger
}

// Init sets the process-wide default logger. Should be called once from main.
// After calling Init, Default() returns this logger.
func Init(l *slog.Logger) {
	defaultLogger = l
	slog.SetDefault(l)
}
