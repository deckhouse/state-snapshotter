# Makefile for state-snapshotter Snapshot export e2e tests.

.PHONY: help
help: ## Show help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'

.PHONY: deps
deps: ## Download dependencies
	go mod download
	go mod tidy
	@$(MAKE) fix-mod-permissions

.PHONY: test
test: ## Run the full nested-cluster Snapshot export e2e suite
	go test -v -count=1 -timeout 180m ./tests/

.PHONY: test-focus
test-focus: ## Run a focused Ginkgo spec (make test-focus FOCUS="exports a namespace snapshot")
	@if [ -z "$(FOCUS)" ]; then \
		echo "Error: specify FOCUS=<Ginkgo focus string>"; \
		exit 1; \
	fi
	go test -v -count=1 -timeout 180m ./tests/ -ginkgo.focus "$(FOCUS)"

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: build
build: ## go build (compile-check only, no cluster)
	go build ./...

.PHONY: lint
lint: ## golangci-lint (if installed)
	golangci-lint run ./...

.PHONY: check-env
check-env: ## Print relevant env vars for the nested flow
	@echo "=== Nested cluster (storage-e2e) ==="
	@echo "TEST_CLUSTER_CREATE_MODE:               $${TEST_CLUSTER_CREATE_MODE}  # alwaysUseExisting | alwaysCreateNew | commander"
	@echo "TEST_CLUSTER_CLEANUP:                   $${TEST_CLUSTER_CLEANUP}      # this suite never deletes the cluster/ns regardless"
	@echo "TEST_CLUSTER_NAMESPACE:                 $${TEST_CLUSTER_NAMESPACE}    # base-cluster VM namespace (thin-SC disk attach)"
	@echo "TEST_CLUSTER_STORAGE_CLASS:             $${TEST_CLUSTER_STORAGE_CLASS}"
	@echo "SSH_HOST:                               $${SSH_HOST}"
	@echo "SSH_USER:                               $${SSH_USER}"
	@echo "SSH_PRIVATE_KEY:                        $${SSH_PRIVATE_KEY}"
	@echo "SSH_PUBLIC_KEY:                         $${SSH_PUBLIC_KEY}  # required for alwaysCreateNew"
	@echo "SSH_VM_USER:                            $${SSH_VM_USER}  # required for alwaysCreateNew"
	@echo "DKP_LICENSE_KEY:                        $${DKP_LICENSE_KEY:+<set>}"
	@echo "REGISTRY_DOCKER_CFG:                    $${REGISTRY_DOCKER_CFG:+<set>}"
	@echo "YAML_CONFIG_FILENAME:                   $${YAML_CONFIG_FILENAME}  # default cluster_config.yml"
	@echo ""
	@echo "=== Snapshot export suite knobs ==="
	@echo "E2E_THIN_STORAGE_CLASS:                 $${E2E_THIN_STORAGE_CLASS}  # name for the created thin SC (default e2e-thin-local)"
	@echo "E2E_PROBE_IMAGE:                        $${E2E_PROBE_IMAGE}  # image with sh for the PVC-mounting pod (default busybox:1.36)"
	@echo "E2E_PVC_SIZE:                           $${E2E_PVC_SIZE}  # app PVC size (default 1Gi)"
	@echo "E2E_NS_PREFIX:                          $${E2E_NS_PREFIX}  # test namespace prefix (default snap-e2e-)"
	@echo "E2E_KEEP_CLUSTER:                       $${E2E_KEEP_CLUSTER}  # informational; this suite never tears the cluster down"

.PHONY: fix-mod-permissions
fix-mod-permissions: ## Make storage-e2e module cache writable (it writes temp/ under the module path)
	@GOMODCACHE=$${GOMODCACHE:-$(CURDIR)/.gomodcache}; \
	GOPATH=$${GOPATH:-$$HOME/go}; \
	for root in "$$GOMODCACHE" "$$GOPATH/pkg/mod"; do \
		[ -d "$$root" ] || continue; \
		for dir in "$$root"/github.com/deckhouse/storage-e2e@*; do \
			if [ -d "$$dir" ]; then \
				echo "Fixing permissions for $$dir"; \
				chmod -R +w "$$dir" 2>/dev/null || true; \
				mkdir -p "$$dir/temp/cluster" 2>/dev/null || true; \
			fi; \
		done; \
	done

.DEFAULT_GOAL := help
