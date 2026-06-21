.PHONY: help test test-unit build clean deps fmt lint verify

# Default target
.DEFAULT_GOAL := help

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
