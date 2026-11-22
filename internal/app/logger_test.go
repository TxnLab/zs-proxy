/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/TxnLab/zs-proxy/internal/config"
)

func TestNewLogger_Levels(t *testing.T) {
	cases := []struct {
		level string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"unknown", slog.LevelInfo}, // default fallback
		{"DEBUG", slog.LevelDebug},  // case-insensitive
	}
	for _, c := range cases {
		t.Run(c.level, func(t *testing.T) {
			logger := newLogger(config.LoggingConfig{Level: c.level, Format: "text"})
			if !logger.Handler().Enabled(context.Background(), c.want) {
				t.Errorf("level %q did not enable %v", c.level, c.want)
			}
			// Verify the level below it is NOT enabled (i.e. threshold is right).
			if c.want > slog.LevelDebug {
				if logger.Handler().Enabled(context.Background(), c.want-4) {
					t.Errorf("level %q unexpectedly enabled %v", c.level, c.want-4)
				}
			}
		})
	}
}

func TestNewLogger_FormatJSON(t *testing.T) {
	// Divert stderr to a pipe so we can observe the output.
	oldStderr := os.Stderr
	r, wpipe, _ := os.Pipe()
	os.Stderr = wpipe
	defer func() { os.Stderr = oldStderr }()

	logger := newLogger(config.LoggingConfig{Level: "info", Format: "json"})
	logger.Info("test message", slog.String("k", "v"))
	_ = wpipe.Close()

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("output not JSON: %v (raw: %q)", err, buf.String())
	}
	// JSON output uses Google Cloud Logging field names (see proto/go/gcplog):
	// message instead of msg, severity instead of level.
	if rec["message"] != "test message" {
		t.Errorf("message = %v", rec["message"])
	}
	if rec["severity"] != "INFO" {
		t.Errorf("severity = %v, want INFO", rec["severity"])
	}
	if _, ok := rec["msg"]; ok {
		t.Errorf("msg key should have been renamed to message")
	}
	if _, ok := rec["level"]; ok {
		t.Errorf("level key should have been renamed to severity")
	}
	if rec["k"] != "v" {
		t.Errorf("k = %v", rec["k"])
	}
}

func TestNewLogger_FormatText(t *testing.T) {
	oldStderr := os.Stderr
	r, wpipe, _ := os.Pipe()
	os.Stderr = wpipe
	defer func() { os.Stderr = oldStderr }()

	logger := newLogger(config.LoggingConfig{Level: "info", Format: "text"})
	logger.Info("hello text")
	_ = wpipe.Close()

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	out := buf.String()
	if !strings.Contains(out, "hello text") {
		t.Errorf("missing message: %q", out)
	}
	// Text format isn't strict JSON.
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("text format looks like JSON: %q", out)
	}
}

// TestNewLoggerTo_Sink covers the explicit-sink path the Windows SCM service
// uses (its stderr is discarded, so it logs to a file): output must land in the
// provided writer, not stderr.
func TestNewLoggerTo_Sink(t *testing.T) {
	var buf bytes.Buffer
	logger := newLoggerTo(config.LoggingConfig{Level: "info", Format: "text"}, &buf)
	logger.Info("service log line", slog.String("k", "v"))
	out := buf.String()
	if !strings.Contains(out, "service log line") || !strings.Contains(out, "k=v") {
		t.Errorf("log not written to the provided sink: %q", out)
	}
}

// TestNewLoggerTo_NilSink: a nil writer must default to stderr without panicking.
func TestNewLoggerTo_NilSink(t *testing.T) {
	logger := newLoggerTo(config.LoggingConfig{Level: "info", Format: "text"}, nil)
	if logger == nil {
		t.Fatal("nil logger")
	}
	logger.Info("ok") // writing to stderr must not panic
}
