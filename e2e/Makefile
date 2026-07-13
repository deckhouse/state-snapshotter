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

# Directory where the e2e capture timeline writes its per-kind/diff logs (mirrors the default in
# tests/timeline_test.go: $E2E_TIMELINE_DIR, else <go os.TempDir()>/e2e/timeline). Go's os.TempDir()
# uses $TMPDIR and falls back to /tmp when it is unset (typical in CI). Override TIMELINE_DIR for custom
# runs. Double slashes (TMPDIR with a trailing slash) are harmless for rm.
TMP_BASE := $(if $(TMPDIR),$(TMPDIR),/tmp)
TIMELINE_DIR ?= $(if $(E2E_TIMELINE_DIR),$(E2E_TIMELINE_DIR),$(TMP_BASE)/e2e/timeline)
# Directory where dumpFailedSpecDiagnostics writes per-pod/container controller logs on a failed spec
# (mirrors the default in tests/diagnostics_test.go: $E2E_LOGS_DIR, else <tmp>/e2e/logs).
LOGS_DIR ?= $(if $(E2E_LOGS_DIR),$(E2E_LOGS_DIR),$(TMP_BASE)/e2e/logs)

# Timeout for the waited deletions in clean-env (Snapshots cascade + ObjectKeepers).
CLEAN_WAIT_TIMEOUT ?= 180s

.PHONY: clean-env
clean-env: ## Delete leftover e2e resources: Snapshots (wait) -> namespaces (snap-e2e*, no wait) -> ObjectKeepers (by followObjectRef.namespace snap-e2e*, wait) + timeline/controller logs
	@echo "==> Deleting Snapshots in 'snap-e2e' namespaces (waiting for finalizers/cascade)..."
	@ns=$$(kubectl get ns -o name | sed 's|^namespace/||' | grep -E '^snap-e2e' || true); \
	if [ -n "$$ns" ]; then \
		for n in $$ns; do \
			echo "  namespace $$n:"; \
			kubectl -n "$$n" delete snapshots.state-snapshotter.deckhouse.io --all --wait=true --timeout=$(CLEAN_WAIT_TIMEOUT) || true; \
		done; \
	else echo "  none found"; fi
	@echo "==> Deleting namespaces with prefix 'snap-e2e' (no wait)..."
	@ns=$$(kubectl get ns -o name | grep -E '^namespace/snap-e2e' || true); \
	if [ -n "$$ns" ]; then echo "$$ns" | xargs kubectl delete --wait=false; else echo "  none found"; fi
	@echo "==> Deleting ObjectKeepers tracking 'snap-e2e' objects (matched by spec.followObjectRef.namespace, waiting)..."
	@# ObjectKeepers are cluster-scoped and UID-named (nss-ok-<h16(uid)> / nss-import-ok-<h16(uid)> since
	@# wave4C), so there is no namespace slug in the name to grep. Select by the namespaced object each one
	@# follows (spec.followObjectRef.namespace) — this catches every test keeper regardless of its name prefix.
	@oks=$$(kubectl get objectkeepers.deckhouse.io \
		-o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.followObjectRef.namespace}{"\n"}{end}' \
		| awk -F'\t' '$$2 ~ /^snap-e2e/ {print $$1}' || true); \
	if [ -n "$$oks" ]; then \
		echo "$$oks" | xargs kubectl delete objectkeepers.deckhouse.io --wait=true --timeout=$(CLEAN_WAIT_TIMEOUT) \
			|| echo "  WARNING: ObjectKeeper deletion timed out or failed (re-run clean-env)"; \
	else echo "  none found"; fi
	@echo "==> Deleting capture timeline logs in '$(TIMELINE_DIR)'..."
	@if [ -d "$(TIMELINE_DIR)" ]; then rm -rf "$(TIMELINE_DIR)" && echo "  removed"; else echo "  none found"; fi
	@echo "==> Deleting controller diagnostics logs in '$(LOGS_DIR)'..."
	@if [ -d "$(LOGS_DIR)" ]; then rm -rf "$(LOGS_DIR)" && echo "  removed"; else echo "  none found"; fi

# ---- Transition e2e (manual, dev-only; see tests/transition/README.md) ----

# kubectl honoring KUBE_CONFIG_PATH (the kubeconfig var the transition suite / storage-e2e read);
# falls back to the ambient kubeconfig when it is unset.
KUBECTL ?= kubectl$(if $(KUBE_CONFIG_PATH), --kubeconfig $(KUBE_CONFIG_PATH),)

# The four snapshot-stack modules the transition suite drives at runtime, and their namespaces.
TRANSITION_MODULES       := snapshot-controller storage-volume-data-manager state-snapshotter storage-foundation
TRANSITION_NS            := d8-snapshot-controller d8-storage-volume-data-manager d8-state-snapshotter d8-storage-foundation
TRANSITION_WORKLOAD_NS   ?= transition-workload
TRANSITION_CLEAN_TIMEOUT ?= 300
# The non-gated (legacy) snapshot-controller tag to un-freeze it back to before cleanup disables it.
# A completed run leaves snapshot-controller on the v0.2.0 handoff build (requires storage-foundation);
# disabling it while frozen there would make the NEXT phase-B enable webhook-denied. Retag it to this
# (main) while it is still enabled so it re-registers non-gated. Match E2E_TRANSITION_SNAPSHOT_CONTROLLER_LEGACY_TAG.
TRANSITION_SNAPC_LEGACY_TAG ?= main
# DataExport/DataImport live under the legacy group (svdm pre-D1) and the unified group (svdm D1);
# the workload may carry either depending on how far a run got.
TRANSITION_DEDI          := dataexports.storage.deckhouse.io dataimports.storage.deckhouse.io dataexports.storage-foundation.deckhouse.io dataimports.storage-foundation.deckhouse.io
TRANSITION_VS            := volumesnapshots.snapshot.storage.k8s.io

.PHONY: test-transition
test-transition: ## Run the transition e2e suite (manual: needs E2E_RUN_TRANSITION=true + the E2E_TRANSITION_* / *_MODULE_PULL_OVERRIDE env — see tests/transition/README.md)
	go test -v -count=1 -timeout 200m ./tests/transition/ -ginkgo.timeout 180m -ginkgo.v

.PHONY: transition-clean
transition-clean: ## Reset the cluster to the transition phase-A start state: FIRST delete the workload (DataExports/DataImports, VolumeSnapshots, pods, PVCs, namespace) while the controllers are still up so finalizers clear and child resources (prime PVC / importer Deployment) are reaped, THEN disable the 4 snapshot modules, drain their namespaces, and sweep leftover domain RBAC
	@echo "==> [1/4] Deleting the workload in '$(TRANSITION_WORKLOAD_NS)' while the controllers are still up (so finalizers clear)..."
	@if $(KUBECTL) get ns $(TRANSITION_WORKLOAD_NS) >/dev/null 2>&1; then \
		echo "  DataExports/DataImports (controllers reap the export/import PVCs + Deployments)..."; \
		for gr in $(TRANSITION_DEDI); do \
			$(KUBECTL) -n $(TRANSITION_WORKLOAD_NS) delete $$gr --all --ignore-not-found --timeout=90s 2>/dev/null || true; \
		done; \
		echo "  VolumeSnapshots..."; \
		$(KUBECTL) -n $(TRANSITION_WORKLOAD_NS) delete $(TRANSITION_VS) --all --ignore-not-found --timeout=90s 2>/dev/null || true; \
		echo "  Pods..."; \
		$(KUBECTL) -n $(TRANSITION_WORKLOAD_NS) delete pods --all --ignore-not-found --timeout=60s 2>/dev/null || true; \
		echo "  PVCs..."; \
		$(KUBECTL) -n $(TRANSITION_WORKLOAD_NS) delete pvc --all --ignore-not-found --timeout=90s 2>/dev/null || true; \
		echo "  Namespace..."; \
		$(KUBECTL) delete ns $(TRANSITION_WORKLOAD_NS) --timeout=120s 2>/dev/null || true; \
	else echo "  namespace absent"; fi
	@echo "==> [2/4] Waiting for '$(TRANSITION_WORKLOAD_NS)' to be FULLY deleted before touching the modules (force-clearing finalizers while it lingers)..."
	@deadline=$$(( $$(date +%s) + $(TRANSITION_CLEAN_TIMEOUT) )); \
	while $(KUBECTL) get ns $(TRANSITION_WORKLOAD_NS) >/dev/null 2>&1; do \
		for res in $$($(KUBECTL) -n $(TRANSITION_WORKLOAD_NS) get $(TRANSITION_VS),pvc,$$(echo '$(TRANSITION_DEDI)' | tr ' ' ',') -o name 2>/dev/null); do \
			$(KUBECTL) -n $(TRANSITION_WORKLOAD_NS) patch $$res --type merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true; \
		done; \
		$(KUBECTL) delete ns $(TRANSITION_WORKLOAD_NS) --wait=false --ignore-not-found >/dev/null 2>&1 || true; \
		if [ $$(date +%s) -ge $$deadline ]; then \
			echo "  TIMEOUT: $(TRANSITION_WORKLOAD_NS) still present — aborting BEFORE disabling modules (disabling now would strand finalizers)"; \
			exit 1; \
		fi; \
		sleep 3; \
	done; \
	echo "  $(TRANSITION_WORKLOAD_NS): gone"
	@echo "==> [3/4] Un-gating snapshot-controller BEFORE disabling it: a completed run leaves it on the v0.2.0 handoff build (requires storage-foundation), and Deckhouse ignores an MPO on a DISABLED module — so disabling it now would freeze it gated and webhook-deny the next phase-B enable. Retag it to '$(TRANSITION_SNAPC_LEGACY_TAG)' while still enabled so it re-registers non-gated..."
	@if [ "$$($(KUBECTL) get moduleconfig snapshot-controller -o jsonpath='{.spec.enabled}' 2>/dev/null)" = "true" ]; then \
		printf 'apiVersion: deckhouse.io/v1alpha2\nkind: ModulePullOverride\nmetadata:\n  name: snapshot-controller\nspec:\n  imageTag: %s\n  scanInterval: 30s\n' "$(TRANSITION_SNAPC_LEGACY_TAG)" | $(KUBECTL) apply -f - >/dev/null 2>&1 || true; \
		deadline=$$(( $$(date +%s) + $(TRANSITION_CLEAN_TIMEOUT) )); \
		while $(KUBECTL) get module snapshot-controller -o jsonpath='{.properties.requirements.modules}' 2>/dev/null | grep -q storage-foundation; do \
			if [ $$(date +%s) -ge $$deadline ]; then echo "  WARN: snapshot-controller still gated on storage-foundation after $(TRANSITION_CLEAN_TIMEOUT)s — may need a manual reset (see README)"; break; fi; \
			sleep 5; \
		done; \
		echo "  snapshot-controller re-registered on '$(TRANSITION_SNAPC_LEGACY_TAG)' (non-gated)"; \
	else echo "  snapshot-controller not enabled — cannot retag a disabled module; if it is frozen gated + disabled see README 'Resetting a reused cluster'"; fi
	@echo "==> [3/4] Disabling the snapshot-stack modules (ModuleConfig enabled=false) + dropping their ModulePullOverrides..."
	@for m in $(TRANSITION_MODULES); do \
		if $(KUBECTL) get moduleconfig $$m >/dev/null 2>&1; then \
			$(KUBECTL) patch moduleconfig $$m --type merge -p '{"spec":{"enabled":false}}' >/dev/null && echo "  moduleconfig/$$m -> enabled=false"; \
		else echo "  moduleconfig/$$m absent (already off)"; fi; \
		$(KUBECTL) delete modulepulloverride $$m --ignore-not-found >/dev/null 2>&1 || true; \
	done
	@echo "     Waiting up to $(TRANSITION_CLEAN_TIMEOUT)s for the snapshot-stack namespaces to drain..."
	@deadline=$$(( $$(date +%s) + $(TRANSITION_CLEAN_TIMEOUT) )); \
	for ns in $(TRANSITION_NS); do \
		while $(KUBECTL) get ns $$ns >/dev/null 2>&1; do \
			if [ $$(date +%s) -ge $$deadline ]; then echo "  TIMEOUT: $$ns still present"; break; fi; \
			sleep 3; \
		done; \
		$(KUBECTL) get ns $$ns >/dev/null 2>&1 || echo "  $$ns: gone"; \
	done
	@echo "==> [4/4] Sweeping leftover hook-created domain RBAC (state-snapshotter ClusterRoles/Bindings survive module disable — see docs/backlog.md #2)..."
	@$(KUBECTL) delete clusterrole,clusterrolebinding -l heritage=deckhouse,module=state-snapshotter --ignore-not-found >/dev/null 2>&1 || true
	@echo "==> Done. Re-check: none of [$(TRANSITION_NS)] and no '$(TRANSITION_WORKLOAD_NS)' should exist before re-running 'make test-transition'."

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
	@echo "E2E_GET_LOAD:                           $${E2E_GET_LOAD}  # true|1|yes -> run the opt-in GET-load measurement (provisions own SC; ~minutes)"
	@echo "E2E_GET_LOAD_ITERATIONS:                $${E2E_GET_LOAD_ITERATIONS}  # GET-load capture waves to run (default 5)"
	@echo "E2E_GET_LOAD_WARMUP:                    $${E2E_GET_LOAD_WARMUP}  # leading GET-load waves excluded from the mean (default 1)"
	@echo "E2E_GET_LOAD_MAX_PER_SEC:               $${E2E_GET_LOAD_MAX_PER_SEC}  # if set, hard-bound the MEAN GET/sec (new run)"
	@echo "E2E_STORAGE_CLASS:                      $${E2E_STORAGE_CLASS}  # thin SC provisioned/used for phase 3 (default e2e-thin)"
	@echo "E2E_PROBE_IMAGE:                        $${E2E_PROBE_IMAGE}  # image with sh+cat for PVC round-trip pods (default busybox:1.36)"
	@echo "E2E_KEEP_CLUSTER_ON_FAILURE:            $${E2E_KEEP_CLUSTER_ON_FAILURE}  # true|1|yes -> keep per-spec resources if a spec failed"
	@echo "E2E_KEEP_CLUSTER:                       $${E2E_KEEP_CLUSTER}  # true|1|yes -> keep per-spec resources always (pass or fail)"
	@echo "E2E_TIMELINE_DIR:                       $${E2E_TIMELINE_DIR}  # capture timeline logs dir (default <tmp>/e2e/timeline)"
	@echo "E2E_LOGS_DIR:                           $${E2E_LOGS_DIR}  # per-pod/container controller logs on failure (default <tmp>/e2e/logs)"

.DEFAULT_GOAL := help
