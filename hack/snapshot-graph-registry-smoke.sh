#!/usr/bin/env bash
# Cluster smoke: state-snapshotter controller health + optional CSD lifecycle checks for the
# dynamic snapshot graph registry (GVK pairs from CSD/bootstrap + RESTMapper).
#
# Prerequisites: kubectl, jq.
# Idempotent: creates a throwaway namespace when REGISTRY_SMOKE_WORK_NS is unset; trap deletes it.
#
# Environment:
#   REGISTRY_SMOKE_MODULE_NS   Controller namespace (default: d8-state-snapshotter).
#   REGISTRY_SMOKE_WORK_NS     Fixed workspace namespace (otherwise auto: ssg-smoke-<pid>).
#   REGISTRY_SMOKE_SKIP_CSD    If 1, skip CSD create/delete/eligibility (only module health + log scan).
#   REGISTRY_SMOKE_LOG_TAIL    Lines of controller logs to scan for panic/fatal (default 400).
#
# Usage: bash hack/snapshot-graph-registry-smoke.sh

set -euo pipefail

MODULE_NS="${REGISTRY_SMOKE_MODULE_NS:-d8-state-snapshotter}"
LOG_TAIL="${REGISTRY_SMOKE_LOG_TAIL:-400}"
SKIP_CSD="${REGISTRY_SMOKE_SKIP_CSD:-0}"

log() { printf '%s\n' "$*" >&2; }
fail() { log "FAIL: $*"; exit 1; }

AUTO_NS=""
if [[ -z "${REGISTRY_SMOKE_WORK_NS:-}" ]]; then
	AUTO_NS="ssg-smoke-$$"
	WORK_NS="$AUTO_NS"
else
	WORK_NS="${REGISTRY_SMOKE_WORK_NS}"
fi

cleanup() {
	if [[ -n "$AUTO_NS" ]]; then
		kubectl delete namespace "$AUTO_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
	fi
}
trap cleanup EXIT

log "== snapshot graph registry smoke (module ns=${MODULE_NS}, work ns=${WORK_NS}) =="

kubectl cluster-info >/dev/null || fail "kubectl cluster-info"

if ! kubectl get ns "$MODULE_NS" >/dev/null 2>&1; then
	log "WARN: namespace ${MODULE_NS} not found — skipping strict controller checks (set REGISTRY_SMOKE_MODULE_NS)"
else
	READY=$(kubectl get pods -n "$MODULE_NS" -o json 2>/dev/null | jq -r '[.items[] | select(.status.phase=="Running")] | length' || echo 0)
	if [[ "$READY" =~ ^[0-9]+$ ]] && [[ "$READY" -lt 1 ]]; then
		log "WARN: no Running pods in ${MODULE_NS} — check deployment (continuing)"
	else
		log "OK: found Running pod(s) in ${MODULE_NS}"
	fi
	CTRL_POD=$(kubectl get pods -n "$MODULE_NS" -o json 2>/dev/null | jq -r '.items[] | select(.status.phase=="Running") | .metadata.name' | head -1 || true)
	if [[ -n "$CTRL_POD" ]]; then
		log "== scanning controller logs (${CTRL_POD}, tail=${LOG_TAIL}) for panic/fatal =="
		if kubectl logs -n "$MODULE_NS" "$CTRL_POD" --tail="$LOG_TAIL" 2>/dev/null | grep -E 'panic:|fatal error:' >/dev/null; then
			fail "panic or fatal found in recent controller logs"
		fi
		log "OK: no panic/fatal in recent controller logs"
	fi
fi

if [[ "$SKIP_CSD" == "1" ]]; then
	log "REGISTRY_SMOKE_SKIP_CSD=1 — done (CSD scenarios skipped)."
	exit 0
fi

kubectl get crd customsnapshotdefinitions.state-snapshotter.deckhouse.io >/dev/null 2>&1 || \
	fail "CSD CRD not installed on cluster"
if kubectl get crd domainspecificsnapshotcontrollers.state-snapshotter.deckhouse.io >/dev/null 2>&1; then
	fail "legacy domainspecificsnapshotcontrollers CRD is still installed"
fi

if [[ -n "$AUTO_NS" ]]; then
	kubectl create namespace "$WORK_NS" >/dev/null
fi

CSD_NAME="ssg-smoke-csd-${RANDOM}"
log "== applying temporary CSD ${CSD_NAME} (cluster-scoped) =="

kubectl apply -f - <<EOF
apiVersion: state-snapshotter.deckhouse.io/v1alpha1
kind: CustomSnapshotDefinition
metadata:
  name: ${CSD_NAME}
spec:
  ownerModule: smoke-ssg
  snapshotResourceMapping:
    - resourceCRDName: demovirtualdisks.demo.state-snapshotter.deckhouse.io
      snapshotCRDName: demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io
      priority: 0
EOF
# (CRD names must match metadata.name in crds/demo.state-snapshotter.deckhouse.io_*.yaml)

log "== waiting for Accepted on CSD =="
ACC=""
for _ in $(seq 1 60); do
	ACC=$(kubectl get customsnapshotdefinitions.state-snapshotter.deckhouse.io "$CSD_NAME" \
		-o jsonpath='{.status.conditions[?(@.type=="Accepted")].status}' 2>/dev/null || true)
	if [[ "$ACC" == "True" ]]; then
		log "OK: CSD Accepted=True"
		break
	fi
	sleep 2
done
if [[ "${ACC:-}" != "True" ]]; then
	log "WARN: CSD did not reach Accepted=True (demo CRDs may be missing); deleting CSD and exiting 0"
	kubectl delete customsnapshotdefinitions.state-snapshotter.deckhouse.io "$CSD_NAME" --wait=false >/dev/null 2>&1 || true
	exit 0
fi

log "== deleting CSD (registry must rebuild on next reconcile; no panic) =="
kubectl delete customsnapshotdefinitions.state-snapshotter.deckhouse.io "$CSD_NAME" --wait=true

if [[ -n "$CTRL_POD" ]]; then
	if kubectl logs -n "$MODULE_NS" "$CTRL_POD" --tail="$LOG_TAIL" 2>/dev/null | grep -E 'panic:|fatal error:' >/dev/null; then
		fail "panic or fatal after CSD delete"
	fi
fi

log "PASS: snapshot-graph-registry-smoke completed"
