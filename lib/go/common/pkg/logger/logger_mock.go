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

package logger

import (
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/mock"
)

// MockLogger is a mock implementation of LoggerInterface.
type MockLogger struct {
	mock.Mock
}

// GetLogger returns a mock logrus.Logger.
func (m *MockLogger) GetLogger() *logrus.Logger {
	args := m.Called()
	return args.Get(0).(*logrus.Logger)
}

// Error mocks the Error method.
func (m *MockLogger) Error(_ error, message string, keysAndValues ...interface{}) {
	args := []interface{}{message}
	args = append(args, keysAndValues...)
	m.Called(args...)
}

// Warning mocks the Warning method.
func (m *MockLogger) Warning(message string, keysAndValues ...interface{}) {
	args := []interface{}{message}
	args = append(args, keysAndValues...)
	m.Called(args...)
}

// Info mocks the Info method.
func (m *MockLogger) Info(message string, keysAndValues ...interface{}) {
	args := []interface{}{message}
	args = append(args, keysAndValues...)
	m.Called(args...)
}

// Debug mocks the Debug method.
func (m *MockLogger) Debug(message string, keysAndValues ...interface{}) {
	args := []interface{}{message}
	args = append(args, keysAndValues...)
	m.Called(args...)
}

// Trace mocks the Trace method.
func (m *MockLogger) Trace(message string, keysAndValues ...interface{}) {
	args := []interface{}{message}
	args = append(args, keysAndValues...)
	m.Called(args...)
}
