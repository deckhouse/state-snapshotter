.PHONY: help test test-unit test-e2e build clean deps setup-envtest

# Default target
.DEFAULT_GOAL := help

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

test: test-unit ## Run all tests (unit + e2e)

test-unit: ## Run unit tests only
	@echo "$(BLUE)Running unit tests...$(NC)"
	$(GO) test -v -timeout $(TEST_TIMEOUT) ./internal/... -coverprofile=coverage.out
	@echo "$(GREEN)Unit tests completed!$(NC)"

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

