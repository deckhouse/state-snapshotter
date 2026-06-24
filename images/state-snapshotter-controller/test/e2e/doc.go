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

// Package e2e contains end-to-end tests for the state-snapshotter controller.
//
// These tests use build tags (//go:build e2e) to exclude them from regular test runs.
// To run these tests, use: go test -tags e2e ./test/e2e
//
// The actual test files are in e2e_test.go, setup.go, helpers.go, and fixtures.go.
package e2e
