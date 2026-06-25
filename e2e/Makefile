# Makefile for state-snapshotter e2e tests.

.PHONY: help
help: ## Show help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'

.PHONY: deps
deps: ## Download dependencies and tidy go.mod/go.sum
	go mod download
	go mod tidy

.PHONY: test
test: ## Run the full nested-cluster E2E suite
	go test -v -count=1 -timeout 120m ./tests/ -ginkgo.timeout 120m -ginkgo.v

.PHONY: test-focus
test-focus: ## Run a focused Ginkgo spec (make test-focus FOCUS="captures the demo tree")
	@if [ -z "$(FOCUS)" ]; then \
		echo "Error: specify FOCUS=<Ginkgo focus string>"; \
		exit 1; \
	fi
	go test -v -count=1 -timeout 120m ./tests/ -ginkgo.timeout 120m -ginkgo.v -ginkgo.focus "$(FOCUS)"

.PHONY: clean-env
clean-env: ## Delete leftover e2e resources: namespaces (snap-e2e*) + ObjectKeepers (ret-snap-snap-e2e*)
	@echo "==> Deleting namespaces with prefix 'snap-e2e'..."
	@ns=$$(kubectl get ns -o name | grep -E '^namespace/snap-e2e' || true); \
	if [ -n "$$ns" ]; then echo "$$ns" | xargs kubectl delete --wait=false; else echo "  none found"; fi
	@echo "==> Deleting ObjectKeepers with prefix 'ret-snap-snap-e2e'..."
	@oks=$$(kubectl get objectkeepers.deckhouse.io -o name | grep -E '/ret-snap-snap-e2e' || true); \
	if [ -n "$$oks" ]; then echo "$$oks" | xargs kubectl delete --wait=false; else echo "  none found"; fi

.PHONY: vet
vet: ## go vet (also type-checks the _test.go files -> compile gate)
	go vet ./...

.PHONY: build
build: ## Compile the test binary without running it (compile-only gate)
	go test -c -o /dev/null ./tests/

.PHONY: lint
lint: ## golangci-lint (if installed)
	golangci-lint run ./...

.PHONY: check-env
check-env: ## Print the relevant env vars for the nested flow
	@echo "=== Nested cluster (storage-e2e) ==="
	@echo "TEST_CLUSTER_CREATE_MODE:               $${TEST_CLUSTER_CREATE_MODE}  # alwaysCreateNew | alwaysUseExisting | commander"
	@echo "TEST_CLUSTER_CLEANUP:                   $${TEST_CLUSTER_CLEANUP}      # set true to delete VMs after the run"
	@echo "TEST_CLUSTER_NAMESPACE:                 $${TEST_CLUSTER_NAMESPACE}    # base-cluster VM namespace (phase 3 disk attach)"
	@echo "TEST_CLUSTER_STORAGE_CLASS:             $${TEST_CLUSTER_STORAGE_CLASS}  # base-cluster SC for VM disks (phase 3)"
	@echo "YAML_CONFIG_FILENAME:                   $${YAML_CONFIG_FILENAME}      # default cluster_config.yml"
	@echo "SSH_HOST:                               $${SSH_HOST}"
	@echo "SSH_USER:                               $${SSH_USER}"
	@echo "SSH_PRIVATE_KEY:                        $${SSH_PRIVATE_KEY}"
	@echo "SSH_PUBLIC_KEY:                         $${SSH_PUBLIC_KEY}  # required for alwaysCreateNew"
	@echo "SSH_VM_USER:                            $${SSH_VM_USER}  # required for alwaysCreateNew"
	@echo "DKP_LICENSE_KEY:                        $${DKP_LICENSE_KEY:+<set>}"
	@echo "REGISTRY_DOCKER_CFG:                    $${REGISTRY_DOCKER_CFG:+<set>}"
	@echo "STATE_SNAPSHOTTER_MODULE_PULL_OVERRIDE: $${STATE_SNAPSHOTTER_MODULE_PULL_OVERRIDE}  # overrides modulePullOverride (default main)"
	@echo ""
	@echo "=== state-snapshotter suite knobs ==="
	@echo "E2E_SNAPSHOTTER_NS_PREFIX:              $${E2E_SNAPSHOTTER_NS_PREFIX}  # namespace prefix for source/restore namespaces (default snap-e2e)"
	@echo "E2E_SNAPSHOT_READY_TIMEOUT:             $${E2E_SNAPSHOT_READY_TIMEOUT}  # Go duration, default 10m"
	@echo "E2E_MODULE_READY_TIMEOUT:               $${E2E_MODULE_READY_TIMEOUT}  # Go duration, default 15m"
	@echo "E2E_GC_TTL:                             $${E2E_GC_TTL}  # snapshotRootOkTtl for the GC spec (default 60s)"
	@echo "E2E_VOLUME_DATA:                        $${E2E_VOLUME_DATA}  # true|1|yes -> run phase 3 (full volume-data flow)"
	@echo "E2E_STORAGE_CLASS:                      $${E2E_STORAGE_CLASS}  # thin SC provisioned/used for phase 3 (default e2e-thin)"
	@echo "E2E_PROBE_IMAGE:                        $${E2E_PROBE_IMAGE}  # image with sh+cat for PVC round-trip pods (default busybox:1.36)"
	@echo "E2E_KEEP_CLUSTER_ON_FAILURE:            $${E2E_KEEP_CLUSTER_ON_FAILURE}  # true|1|yes -> skip teardown if a spec failed"

.DEFAULT_GOAL := help
