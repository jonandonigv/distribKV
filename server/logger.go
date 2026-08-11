// Logger construction for production: maps -log.level / -log.format
// flags to an slog.Handler. text by default; json for production deploys.

package server

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// buildLogger constructs an slog.Logger from the flag-style level/format
// strings, writing to os.Stderr. Empty strings default to info / text.
// Returns an error for invalid values so the binary can fail fast on
// bad flags.
func buildLogger(level, format string) (*slog.Logger, error) {
	return buildLoggerTo(level, format, os.Stderr)
}

// BuildLogger is the exported form of buildLogger for tests that want
// to exercise flag validation without starting a full node.
func BuildLogger(level, format string) (*slog.Logger, error) {
	return buildLogger(level, format)
}

// buildLoggerTo is buildLogger with a configurable writer, for tests
// that want to capture log output.
func buildLoggerTo(level, format string, w io.Writer) (*slog.Logger, error) {
	lvl, err := parseLevel(level)
	if err != nil {
		return nil, err
	}
	h, err := buildHandler(format, lvl, w)
	if err != nil {
		return nil, err
	}
	return slog.New(h), nil
}

// parseLevel maps the -log.level flag string to an slog.Level.
func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid log level %q (want debug|info|warn|error)", s)
	}
}

// buildHandler returns a text or JSON handler writing to w at lvl.
func buildHandler(format string, lvl slog.Level, w io.Writer) (slog.Handler, error) {
	opts := &slog.HandlerOptions{Level: lvl}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "text":
		return slog.NewTextHandler(w, opts), nil
	case "json":
		return slog.NewJSONHandler(w, opts), nil
	default:
		return nil, fmt.Errorf("invalid log format %q (want text|json)", format)
	}
}
