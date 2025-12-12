/*
Copyright 2025 Flant JSC

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

package logger

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"
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

// LoggerInterface defines the interface for our logger.
type LoggerInterface interface {
	GetLogger() *logrus.Logger
	Error(err error, message string, keysAndValues ...interface{})
	Warning(message string, keysAndValues ...interface{})
	Info(message string, keysAndValues ...interface{})
	Debug(message string, keysAndValues ...interface{})
	Trace(message string, keysAndValues ...interface{})
}

// Logger implements LoggerInterface using logrus.
type Logger struct {
	log *logrus.Logger
}

// NewLogger creates a new Logger instance with the specified log level.
// The log level can be one of: "error", "warn", "info", "debug", "trace",
// or a numeric string (from 0 to 6, where lower values represent higher severity).
func NewLogger(level string) (*Logger, error) {
	logger := logrus.New()

	levelMapping := map[string]logrus.Level{
		"error": logrus.ErrorLevel,
		"warn":  logrus.WarnLevel,
		"info":  logrus.InfoLevel,
		"debug": logrus.DebugLevel,
		"trace": logrus.TraceLevel,
	}

	lowerLevel := strings.ToLower(level)
	if lvl, ok := levelMapping[lowerLevel]; ok {
		logger.SetLevel(lvl)
	} else {
		// Try to parse a numeric log level.
		numericLevel, err := strconv.Atoi(level)
		if err != nil {
			return nil, fmt.Errorf("invalid log level: %s", level)
		}
		// logrus levels: PanicLevel(0), FatalLevel(1), ErrorLevel(2),
		// WarnLevel(3), InfoLevel(4), DebugLevel(5), TraceLevel(6)
		if numericLevel < 0 || numericLevel > 6 {
			return nil, fmt.Errorf("numeric log level must be between 0 and 6, got: %d", numericLevel)
		}
		logger.SetLevel(logrus.Level(numericLevel))
	}

	// Use a text formatter with full timestamp for clarity.
	logger.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})

	return &Logger{log: logger}, nil
}

// GetLogger returns the underlying logrus.Logger.
func (l *Logger) GetLogger() *logrus.Logger {
	return l.log
}

// Error logs an error message along with additional key-value pairs.
func (l *Logger) Error(err error, message string, keysAndValues ...interface{}) {
	fields := extractFields(keysAndValues...)
	l.log.WithFields(fields).WithError(err).Error(message)
}

// Warning logs a warning message along with additional key-value pairs.
func (l *Logger) Warning(message string, keysAndValues ...interface{}) {
	fields := extractFields(keysAndValues...)
	l.log.WithFields(fields).Warn(message)
}

// Info logs an informational message along with additional key-value pairs.
func (l *Logger) Info(message string, keysAndValues ...interface{}) {
	fields := extractFields(keysAndValues...)
	l.log.WithFields(fields).Info(message)
}

// Debug logs a debug message along with additional key-value pairs.
func (l *Logger) Debug(message string, keysAndValues ...interface{}) {
	fields := extractFields(keysAndValues...)
	l.log.WithFields(fields).Debug(message)
}

// Trace logs a trace message along with additional key-value pairs.
func (l *Logger) Trace(message string, keysAndValues ...interface{}) {
	fields := extractFields(keysAndValues...)
	l.log.WithFields(fields).Trace(message)
}

// extractFields converts a list of key-value pairs into logrus.Fields.
func extractFields(keysAndValues ...interface{}) logrus.Fields {
	fields := logrus.Fields{}
	for i := 0; i < len(keysAndValues)-1; i += 2 {
		key, ok := keysAndValues[i].(string)
		if !ok {
			continue
		}
		fields[key] = keysAndValues[i+1]
	}
	return fields
}
