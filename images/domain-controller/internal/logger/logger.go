/*
Copyright 2026 Flant JSC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package logger is the domain controller's self-contained logger: a thin wrapper over the standard
// library log/slog. It keeps images/domain-controller copyable — the only first-party dependency of
// the module is api/, with no shared logging utility from lib/go/common.
package logger

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

// Verbosity represents the log level.
type Verbosity string

const (
	ErrorLevel   Verbosity = "error"
	WarningLevel Verbosity = "warn"
	InfoLevel    Verbosity = "info"
	DebugLevel   Verbosity = "debug"
	TraceLevel   Verbosity = "trace"
)

// levelTrace is below slog.LevelDebug (-4) so "trace" verbosity emits while "debug" filters it out.
const levelTrace = slog.Level(-8)

// LoggerInterface defines the logging surface used across the domain controller.
type LoggerInterface interface {
	Error(err error, message string, keysAndValues ...interface{})
	Warning(message string, keysAndValues ...interface{})
	Info(message string, keysAndValues ...interface{})
	Debug(message string, keysAndValues ...interface{})
	Trace(message string, keysAndValues ...interface{})
}

// Logger implements LoggerInterface over log/slog.
type Logger struct {
	log *slog.Logger
}

// NewLogger creates a Logger at the given level. The level may be one of
// "error", "warn", "info", "debug", "trace", or a numeric string 0..6 (higher = more verbose),
// matching the legacy logrus numeric mapping.
func NewLogger(level string) (*Logger, error) {
	lvl, err := parseLevel(level)
	if err != nil {
		return nil, err
	}
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return &Logger{log: slog.New(handler)}, nil
}

func parseLevel(level string) (slog.Level, error) {
	switch strings.ToLower(level) {
	case "error":
		return slog.LevelError, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "trace":
		return levelTrace, nil
	}
	n, err := strconv.Atoi(level)
	if err != nil {
		return 0, fmt.Errorf("invalid log level: %s", level)
	}
	if n < 0 || n > 6 {
		return 0, fmt.Errorf("numeric log level must be between 0 and 6, got: %d", n)
	}
	switch {
	case n <= 2:
		return slog.LevelError, nil
	case n == 3:
		return slog.LevelWarn, nil
	case n == 4:
		return slog.LevelInfo, nil
	case n == 5:
		return slog.LevelDebug, nil
	default:
		return levelTrace, nil
	}
}

func (l *Logger) Error(err error, message string, keysAndValues ...interface{}) {
	if err != nil {
		keysAndValues = append([]interface{}{"error", err.Error()}, keysAndValues...)
	}
	l.log.Error(message, keysAndValues...)
}

func (l *Logger) Warning(message string, keysAndValues ...interface{}) {
	l.log.Warn(message, keysAndValues...)
}

func (l *Logger) Info(message string, keysAndValues ...interface{}) {
	l.log.Info(message, keysAndValues...)
}

func (l *Logger) Debug(message string, keysAndValues ...interface{}) {
	l.log.Debug(message, keysAndValues...)
}

func (l *Logger) Trace(message string, keysAndValues ...interface{}) {
	l.log.Log(context.Background(), levelTrace, message, keysAndValues...)
}
