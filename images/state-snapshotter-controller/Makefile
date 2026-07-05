SHELL := /bin/bash
.PHONY: help test test-unit test-integration test-e2e build clean deps setup-envtest run fox_build_and_push redeploy switch_to_local_dev switch_from_local_dev

# Default target
.DEFAULT_GOAL := help

# Local cluster dev build/deploy
repo_local := localhost:5000
repo_fox := registry.flant.com/deckhouse/storage
image_name := localbuild
deployment_name := controller
namespace := d8-state-snapshotter
module_name := state-snapshotter
pull_secret_name := fox-registry
# Seconds to wait after enabling maintenance (NoResourceReconciliation) before patching the deployment,
# so Deckhouse stops reconciling the module and does not overwrite the local-dev image patch. Override:
# make switch_to_local_dev maintenance_wait=30
maintenance_wait := 15
USER := $(shell whoami)
image_tag := $(USER)-state-snapshotter-controller

# Build-time version marker injected into the binary (main.version) so the running pod self-identifies at
# startup: git short sha + "-dirty" when the working tree has uncommitted changes + UTC build timestamp.
git_sha := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
git_dirty := $(shell git diff --quiet 2>/dev/null || echo -dirty)
build_ts := $(shell date -u +%Y%m%dT%H%M%SZ)
app_version := $(git_sha)$(git_dirty)-$(build_ts)

# Go toolchain version read from go.mod (the `go X.Y.Z` directive) so the dev image builder always matches
# the required toolchain. Falls back to the Dockerfile ARG default if extraction fails.
go_version := $(shell awk '/^go [0-9]/{print $$2; exit}' go.mod)

# Variables
GO := go
GOFMT := gofmt
GOLINT := golangci-lint
TEST_TIMEOUT := 10m
E2E_TIMEOUT := 30m

# Colors for output
BLUE := \033[0;34m
GREEN := \033[0;32m
YELLOW := \033[0;33m
NC := \033[0m # No Color

help: ## Show this help message
	@echo "$(BLUE)Available targets:$(NC)"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  $(GREEN)%-20s$(NC) %s\n", $$1, $$2}'

deps: ## Download Go dependencies
	@echo "$(BLUE)Downloading dependencies...$(NC)"
	$(GO) mod download
	$(GO) mod tidy

setup-envtest: ## Setup envtest binaries (required for e2e tests)
	@echo "$(BLUE)Setting up envtest binaries...$(NC)"
	@echo "$(YELLOW)Installing/updating setup-envtest...$(NC)"
	@$(GO) install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
	@echo "$(BLUE)Downloading Kubernetes test binaries (this may take a while on first run)...$(NC)"
	@GOPATH=$$($(GO) env GOPATH) && \
		KUBEBUILDER_ASSETS=$$($$GOPATH/bin/setup-envtest use -p path 2>&1) && \
		if [ -n "$$KUBEBUILDER_ASSETS" ] && [ -d "$$KUBEBUILDER_ASSETS" ]; then \
			echo "$(YELLOW)Ensuring binaries are executable...$(NC)"; \
			chmod +x $$KUBEBUILDER_ASSETS/etcd $$KUBEBUILDER_ASSETS/kube-apiserver $$KUBEBUILDER_ASSETS/kubectl 2>/dev/null || true; \
			echo "$(GREEN)Envtest binaries ready at: $$KUBEBUILDER_ASSETS$(NC)"; \
		else \
			echo "$(YELLOW)Note: Run 'make test-e2e' to download binaries automatically$(NC)"; \
		fi

test: test-unit test-integration ## Run all tests (unit + integration + e2e)

test-unit: ## Run unit tests only
	@echo "$(BLUE)Running unit tests...$(NC)"
	$(GO) test -v -timeout $(TEST_TIMEOUT) ./pkg/... ./internal/... -coverprofile=coverage.out
	@echo "$(GREEN)Unit tests completed!$(NC)"

test-integration: ## Run integration tests (requires envtest)
	@echo "$(BLUE)Running integration tests...$(NC)"
	@echo "$(YELLOW)Setting up envtest (this may download binaries on first run)...$(NC)"
	@GOPATH=$$($(GO) env GOPATH) && \
		if [ ! -f $$GOPATH/bin/setup-envtest ]; then \
			echo "$(YELLOW)Installing setup-envtest...$(NC)"; \
			$(GO) install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest; \
		fi && \
		KUBEBUILDER_ASSETS=$$($$GOPATH/bin/setup-envtest use -p path) && \
		export KUBEBUILDER_ASSETS && \
		if [ -n "$$KUBEBUILDER_ASSETS" ]; then \
			echo "$(BLUE)KUBEBUILDER_ASSETS=$$KUBEBUILDER_ASSETS$(NC)"; \
			if [ -d "$$KUBEBUILDER_ASSETS" ]; then \
				echo "$(YELLOW)Ensuring binaries are executable...$(NC)"; \
				chmod +x $$KUBEBUILDER_ASSETS/etcd $$KUBEBUILDER_ASSETS/kube-apiserver $$KUBEBUILDER_ASSETS/kubectl 2>/dev/null || true; \
			fi; \
		fi && \
		echo "$(BLUE)Pass 1/2: shared-manager suite (label-filter !isolated)...$(NC)" && \
		KUBEBUILDER_ASSETS=$$KUBEBUILDER_ASSETS $(GO) test -tags integration -v -count=1 -timeout $(TEST_TIMEOUT) ./test/integration -ginkgo.v -ginkgo.label-filter='!isolated' && \
		echo "$(BLUE)Pass 2/2: isolation-sensitive specs in a fresh env (label-filter isolated)...$(NC)" && \
		KUBEBUILDER_ASSETS=$$KUBEBUILDER_ASSETS $(GO) test -tags integration -v -count=1 -timeout $(TEST_TIMEOUT) ./test/integration -ginkgo.v -ginkgo.label-filter='isolated'
	@echo "$(GREEN)Integration tests completed!$(NC)"

test-e2e: ## Run e2e tests (automatically sets up envtest)
	@echo "$(BLUE)Running e2e tests...$(NC)"
	@echo "$(YELLOW)Setting up envtest (this may download binaries on first run)...$(NC)"
	@GOPATH=$$($(GO) env GOPATH) && \
		if [ ! -f $$GOPATH/bin/setup-envtest ]; then \
			echo "$(YELLOW)Installing setup-envtest...$(NC)"; \
			$(GO) install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest; \
		fi && \
		KUBEBUILDER_ASSETS=$$($$GOPATH/bin/setup-envtest use -p path) && \
		export KUBEBUILDER_ASSETS && \
		if [ -n "$$KUBEBUILDER_ASSETS" ]; then \
			echo "$(BLUE)KUBEBUILDER_ASSETS=$$KUBEBUILDER_ASSETS$(NC)"; \
			if [ -d "$$KUBEBUILDER_ASSETS" ]; then \
				echo "$(YELLOW)Ensuring binaries are executable...$(NC)"; \
				chmod +x $$KUBEBUILDER_ASSETS/etcd $$KUBEBUILDER_ASSETS/kube-apiserver $$KUBEBUILDER_ASSETS/kubectl 2>/dev/null || true; \
			fi; \
		fi && \
		KUBEBUILDER_ASSETS=$$KUBEBUILDER_ASSETS $(GO) test -tags e2e -v -count=1 -timeout $(E2E_TIMEOUT) ./test/e2e -ginkgo.v
	@echo "$(GREEN)E2e tests completed!$(NC)"

test-e2e-fast: ## Run e2e tests without downloading binaries (uses cached)
	@echo "$(BLUE)Running e2e tests (fast mode)...$(NC)"
	@GOPATH=$$($(GO) env GOPATH) && \
		if [ -z "$$KUBEBUILDER_ASSETS" ]; then \
			echo "$(YELLOW)KUBEBUILDER_ASSETS not set, setting up envtest...$(NC)"; \
			if [ ! -f $$GOPATH/bin/setup-envtest ]; then \
				$(GO) install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest; \
			fi; \
			KUBEBUILDER_ASSETS=$$($$GOPATH/bin/setup-envtest use -p path) && \
			export KUBEBUILDER_ASSETS; \
		fi && \
		if [ -n "$$KUBEBUILDER_ASSETS" ] && [ -d "$$KUBEBUILDER_ASSETS" ]; then \
			echo "$(YELLOW)Ensuring binaries are executable...$(NC)"; \
			chmod +x $$KUBEBUILDER_ASSETS/etcd $$KUBEBUILDER_ASSETS/kube-apiserver $$KUBEBUILDER_ASSETS/kubectl 2>/dev/null || true; \
		fi && \
		KUBEBUILDER_ASSETS=$$KUBEBUILDER_ASSETS $(GO) test -tags e2e -v -count=1 -timeout $(E2E_TIMEOUT) ./test/e2e -ginkgo.v
	@echo "$(GREEN)E2e tests completed!$(NC)"

test-coverage: test-unit ## Run tests with coverage report
	@echo "$(BLUE)Generating coverage report...$(NC)"
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "$(GREEN)Coverage report generated: coverage.html$(NC)"

build: ## Build the controller binary
	@echo "$(BLUE)Building controller...$(NC)"
	$(GO) build -o bin/state-snapshotter-controller ./cmd/main.go
	@echo "$(GREEN)Build completed: bin/state-snapshotter-controller$(NC)"

fmt: ## Format Go code
	@echo "$(BLUE)Formatting code...$(NC)"
	$(GOFMT) -s -w .
	@echo "$(GREEN)Code formatted!$(NC)"

lint: ## Run linter
	@echo "$(BLUE)Running linter...$(NC)"
	@if ! command -v $(GOLINT) > /dev/null 2>&1; then \
		echo "$(YELLOW)Installing golangci-lint...$(NC)"; \
		$(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@latest; \
	fi
	$(GOLINT) run ./...
	@echo "$(GREEN)Linting completed!$(NC)"

clean: ## Clean build artifacts
	@echo "$(BLUE)Cleaning build artifacts...$(NC)"
	rm -rf bin/
	rm -f coverage.out coverage.html
	$(GO) clean
	@echo "$(GREEN)Clean completed!$(NC)"

verify: fmt lint test-unit ## Verify code (format, lint, test)
	@echo "$(GREEN)Verification completed!$(NC)"

e2e: test-e2e ## Alias for test-e2e (convenience target - recommended)

.PHONY: e2e

run: ## Run controller locally
	$(GO) run -race ./cmd/main.go

fox_build_and_push: ## Build dev image and push to fox registry
	@echo "$(BLUE)Building image with version: $(app_version) (Go $(go_version))$(NC)"
	DOCKER_BUILDKIT=1 docker build --build-arg APP_VERSION=$(app_version) --build-arg GO_VERSION=$(go_version) -t $(repo_fox)/$(image_name):$(image_tag) -f dev/Dockerfile-dev ../../.
	docker push $(repo_fox)/$(image_name):$(image_tag)

redeploy: ## Restart controller deployment, wait for rollout, and print the running build version
	kubectl -n $(namespace) rollout restart deployment $(deployment_name)
	kubectl -n $(namespace) rollout status deployment $(deployment_name) --timeout=300s
	@printf "$(BLUE)Running build version:$(NC)\n"
	@pod=$$(kubectl -n $(namespace) get pods -l app=$(deployment_name) --field-selector=status.phase=Running --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1].metadata.name}'); \
	printf "  pod=%s\n" "$$pod"; \
	kubectl -n $(namespace) logs "$$pod" | grep -m1 '\[main\] Version' \
		|| printf "$(YELLOW)  [main] Version line not found (old image without version logging?)$(NC)\n"

switch_to_local_dev: ## Pause module reconciliation (maintenance) and patch deployment to local dev image
	kubectl patch moduleconfig $(module_name) --type=merge \
		-p '{"spec": {"maintenance": "NoResourceReconciliation"}}'
	@echo "$(YELLOW)Waiting $(maintenance_wait)s for Deckhouse to stop reconciling the module before patching the deployment...$(NC)"
	@sleep $(maintenance_wait)
	kubectl -n $(namespace) patch deployment $(deployment_name) -p \
		'{"spec": {"template": {"spec": {"containers": [{"name": "$(deployment_name)", "image": "$(repo_fox)/$(image_name):$(image_tag)"}]}}}}'
	kubectl -n $(namespace) patch deployment $(deployment_name) -p \
		'{"spec": {"template": {"spec": {"containers": [{"name": "$(deployment_name)", "imagePullPolicy": "Always"}]}}}}'
	kubectl -n $(namespace) patch deployment $(deployment_name) --type='json' \
		-p='[{"op": "add", "path": "/spec/template/spec/imagePullSecrets", "value": [{"name": "$(pull_secret_name)"}]}]'

switch_from_local_dev: ## Resume module reconciliation so Deckhouse restores the module image
	kubectl patch moduleconfig $(module_name) --type=merge \
		-p '{"spec": {"maintenance": null}}'
	@echo "Maintenance disabled for module '$(module_name)'. Deckhouse will restore $(deployment_name) to the module image shortly."

