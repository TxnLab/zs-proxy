/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package app

import (
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/TxnLab/zerosignal/go/gcplog"

	"github.com/TxnLab/zs-proxy/internal/config"
)

// newLogger builds the process slog.Logger from the logging config, writing to
// stderr. JSON format uses Google Cloud Logging field names (see proto/go/gcplog);
// text is the human-readable default.
func newLogger(cfg config.LoggingConfig) *slog.Logger {
	return newLoggerTo(cfg, os.Stderr)
}

// newLoggerTo is newLogger with an explicit sink. The Windows SCM service uses
// it to log to a file: an SCM-launched process's stderr is discarded (unlike the
// self-managed daemon, whose parent redirects the child's stderr to proxy.log),
// so without this the service would produce no observable logs. w == nil ⇒ stderr.
func newLoggerTo(cfg config.LoggingConfig, w io.Writer) *slog.Logger {
	if w == nil {
		w = os.Stderr
	}
	var level slog.Level
	switch strings.ToLower(cfg.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if cfg.Format == "json" {
		opts.ReplaceAttr = gcplog.ReplaceAttr
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}
	return slog.New(handler)
}
