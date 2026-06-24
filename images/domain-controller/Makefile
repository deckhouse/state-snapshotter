SHELL := /bin/bash
.PHONY: help test test-unit build clean deps fmt lint verify run fox_build_and_push redeploy switch_to_local_dev switch_from_local_dev

# Default target
.DEFAULT_GOAL := help

# Local cluster dev build/deploy
repo_local := localhost:5000
repo_fox := registry.flant.com/deckhouse/storage
image_name := localbuild
deployment_name := domain-controller
namespace := d8-state-snapshotter
module_name := state-snapshotter
pull_secret_name := fox-registry
USER := $(shell whoami)
image_tag := $(USER)-domain-controller

# Variables
GO := go
GOFMT := gofmt
GOLINT := golangci-lint
TEST_TIMEOUT := 10m

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

# The domain-controller is a self-contained module that depends only on api/ + k8s. It is tested with
# fake-client unit tests exclusively (no envtest); cross-controller interaction and apiserver semantics
# are covered by e2e in the core module.
test: test-unit ## Run all tests (fake-client unit tests)

test-unit: ## Run unit tests
	@echo "$(BLUE)Running unit tests...$(NC)"
	$(GO) test -v -timeout $(TEST_TIMEOUT) ./... -coverprofile=coverage.out
	@echo "$(GREEN)Unit tests completed!$(NC)"

test-coverage: test-unit ## Run tests with coverage report
	@echo "$(BLUE)Generating coverage report...$(NC)"
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "$(GREEN)Coverage report generated: coverage.html$(NC)"

build: ## Build the domain-controller binary
	@echo "$(BLUE)Building domain-controller...$(NC)"
	$(GO) build -o bin/domain-controller ./cmd/domain-controller
	@echo "$(GREEN)Build completed: bin/domain-controller$(NC)"

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

run: ## Run domain-controller locally
	$(GO) run -race ./cmd/domain-controller

fox_build_and_push: ## Build dev image and push to fox registry
	DOCKER_BUILDKIT=1 docker build -t $(repo_fox)/$(image_name):$(image_tag) -f dev/Dockerfile-dev ../../.
	docker push $(repo_fox)/$(image_name):$(image_tag)

redeploy: ## Restart domain-controller deployment in the cluster
	kubectl -n $(namespace) rollout restart deployment $(deployment_name)

switch_to_local_dev: ## Pause module reconciliation (maintenance) and patch deployment to local dev image
	kubectl patch moduleconfig $(module_name) --type=merge \
		-p '{"spec": {"maintenance": "NoResourceReconciliation"}}'
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
