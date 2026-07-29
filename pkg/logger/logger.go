// Package logger builds the application logger on top of log/slog.
//
// It deliberately exposes no package-level logger: every component takes a
// *slog.Logger through its constructor, so nothing depends on global state and
// tests can pass a discarding logger without touching a shared variable.
package logger

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// New builds a logger from the configuration.
func New(config *Config) (*slog.Logger, error) {
	level, err := parseLevel(config.Level)
	if err != nil {
		return nil, err
	}

	options := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	switch strings.ToLower(config.Encoding) {
	case "json":
		handler = slog.NewJSONHandler(os.Stdout, options)
	case "console", "text":
		handler = slog.NewTextHandler(os.Stdout, options)
	default:
		return nil, fmt.Errorf("invalid log encoding %q, expected \"json\" or \"console\"", config.Encoding)
	}

	return slog.New(handler), nil
}

// Discard returns a logger that writes nothing, for tests.
func Discard() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func parseLevel(level string) (slog.Level, error) {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid log level %q", level)
	}
}
