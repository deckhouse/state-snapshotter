# E2E Tests for ManifestCaptureRequest and ManifestCheckpoint

## Overview

This directory contains end-to-end tests for the state-snapshotter controller, verifying all critical aspects of the ADR.

## Quick Start

```bash
# From images/state-snapshotter-controller directory
make e2e
```

This will:
1. Automatically install `setup-envtest` if needed
2. Download Kubernetes test binaries (on first run)
3. Run all e2e tests

## Manual Setup

If you prefer to set up manually:

```bash
# Install setup-envtest
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

# Set up test binaries
eval $(setup-envtest use -p path)

# Run tests (note: e2e tests require -tags e2e build tag)
go test -tags e2e ./test/e2e -v -ginkgo.v
```

**Important:** E2E tests use build tags (`//go:build e2e`) to exclude them from regular test runs. You must use `-tags e2e` when running e2e tests manually.

## Structure

- `e2e_test.go` - Main test file with test scenarios
- `setup.go` - Test environment setup (envtest, manager, controllers)
- `helpers.go` - Helper functions for creating and verifying resources
- `fixtures.go` - Test data and fixtures

## Test Coverage

### Implemented (17 tests):
1. Basic Checkpoint Creation (1 test)
2. Condition Transitions (4 tests)
3. Idempotency (3 tests)
4. Validation (1 test)
5. Retainers (3 tests)
6. Garbage Collection (2 tests)
7. Archive Recovery and Stress Tests (2 tests)
8. Retainer TTL and Migration (1 test)

### TODO (from updated plan):
- ChunkStructureAndContent (chunk content validation)
- EdgeCase tests (4 tests)
- Cache tests (2 tests)
- Cleanup tests (2 tests)

## Notes

- Tests use envtest for creating a test Kubernetes API server
- CRDs are automatically discovered from multiple possible paths
- All tests are isolated (create their own namespaces)
- Uses ginkgo/gomega for structured tests
- Test binaries are cached after first download
- E2E tests are excluded from regular `go test ./...` runs via build tags (`//go:build e2e`)
- Use `make e2e` or `go test -tags e2e ./test/e2e` to run e2e tests

## Troubleshooting

### CRDs not found
If you see "CRDs not found" errors, ensure CRD files are in `../../../../crds/` relative to test directory.

### Test binaries download
On first run, test binaries will be downloaded automatically. This may take a few minutes.

### KUBEBUILDER_ASSETS
If you want to use cached binaries, set `KUBEBUILDER_ASSETS` environment variable:
```bash
export KUBEBUILDER_ASSETS=/path/to/binaries
make test-e2e-fast
```

