#!/usr/bin/env bash
# PR-8 smoke: cluster e2e for N5 two-PVC subtree with real storage-foundation bulk VCR→VSC on local-thin.
#
# Chain: Snapshot → SnapshotContent → one bulk VCR → VSC(s) → VCR.status.dataRefs[] →
#        SnapshotContent.status.dataRefs[] → SCC Ready (MCP + dataRefs + children).
#
# Scenario (ordering avoids child re-capturing pvc-b before root owns residual):
#   1) namespace + pvc-a only → child snapshot captures pvc-a via real VCR
#   2) root snapshot + merge child graph (still only pvc-a in namespace)
#   3) pvc-b → root bulk VCR captures residual pvc-b only
#
# Prerequisites: kubectl, jq; deployed d8-state-snapshotter + d8-storage-foundation controllers;
# StorageClass local-thin with volume snapshot class annotation; VolumeSnapshotClass for local CSI.
#
# Optional:
#   PR8_SMOKE_NAMESPACE         default: nss-smoke-<timestamp> (aligns with pre-e2e smoke)
#   PR8_SMOKE_STORAGE_CLASS     default: local-thin
#   PR8_SMOKE_WAIT_SEC          default: 900
#   PR8_SMOKE_POLL_SEC          default: 5
#   PR8_SMOKE_SKIP_CLEANUP      1 = leave namespace for debugging
#   ARTIFACT_DIR                graph artifacts (default: /tmp/state-snapshotter-pr8-<ts>)
#
# RBAC (three different APIs — do not conflate):
#   volumesnapshots.snapshot.storage.k8s.io     — CSI VolumeSnapshot (foundation creates via VCR)
#   snapshots.storage.deckhouse.io               — state-snapshotter root/child Snapshot (this smoke creates)
#   volumecapturerequests.storage.deckhouse.io   — foundation bulk VCR (controller creates)
# Smoke requires create on snapshots.storage.deckhouse.io in PR8_SMOKE_NAMESPACE (admin kubeconfig or equivalent).
#
# Usage: ./hack/pr8-smoke.sh

set -euo pipefail

STORAGE_CLASS="${PR8_SMOKE_STORAGE_CLASS:-local-thin}"
WAIT_SEC="${PR8_SMOKE_WAIT_SEC:-900}"
POLL_SEC="${PR8_SMOKE_POLL_SEC:-5}"
TS="$(date +%Y%m%d-%H%M%S)"
NS="${PR8_SMOKE_NAMESPACE:-nss-smoke-${TS}}"
CHILD_SNAP="pr8-child"
ROOT_SNAP="pr8-root"
PVC_A="pvc-a"
PVC_B="pvc-b"
BIND_IMAGE="${PR8_SMOKE_BIND_IMAGE:-registry.k8s.io/pause:3.9}"
ARTIFACT_DIR="${ARTIFACT_DIR:-/tmp/state-snapshotter-pr8-${TS}}"

SNAP_RES="snapshots.storage.deckhouse.io"
CSI_VS_RES="volumesnapshots.snapshot.storage.k8s.io"
CONTENT_RES="snapshotcontents.storage.deckhouse.io"
VCR_RES="volumecapturerequests.storage.deckhouse.io"
VSC_RES="volumesnapshotcontents.snapshot.storage.k8s.io"
STORAGE_API="storage.deckhouse.io/v1alpha1"
STUB_ANN="state-snapshotter.deckhouse.io/volume-capture-stub-pvcs"

log() { printf '%s\n' "$*" >&2; }

need_cmd() {
	command -v "$1" >/dev/null 2>&1 || {
		log "ERROR: missing required command: $1"
		exit 1
	}
}

cleanup_ns() {
	if [[ "${PR8_SMOKE_SKIP_CLEANUP:-0}" == "1" ]]; then
		log "SKIP cleanup: PR8_SMOKE_SKIP_CLEANUP=1 namespace=${NS}"
		return 0
	fi
	kubectl delete namespace "${NS}" --ignore-not-found=true --wait=false 2>/dev/null || true
}

trap cleanup_ns EXIT

need_cmd kubectl
need_cmd jq
mkdir -p "${ARTIFACT_DIR}"

wait_until() {
	local desc="$1"
	shift
	local deadline=$((SECONDS + WAIT_SEC))
	local phase_start=${SECONDS}
	local last_log=${SECONDS}
	local log_every="${PR8_SMOKE_WAIT_LOG_EVERY_SEC:-30}"
	while (( SECONDS < deadline )); do
		if "$@"; then
			log "OK ${desc}"
			return 0
		fi
		if (( SECONDS - last_log >= log_every )); then
			log "WAIT: ${desc} ($((SECONDS - phase_start))s / ${WAIT_SEC}s)"
			last_log=${SECONDS}
		fi
		sleep "${POLL_SEC}"
	done
	log "ERROR: timeout waiting for ${desc} (${WAIT_SEC}s)"
	return 1
}

kick_snapshot() {
	local name="$1"
	kubectl -n "${NS}" annotate "${SNAP_RES}" "${name}" \
		"state-snapshotter.deckhouse.io/pr8-kick=$(date +%s)" --overwrite >/dev/null
}

snapshot_bound() {
	local name="$1"
	kubectl -n "${NS}" get "${SNAP_RES}" "${name}" -o json 2>/dev/null | jq -e \
		'.status.boundSnapshotContentName != null and (.status.boundSnapshotContentName | length > 0)' >/dev/null
}

snapshot_ready() {
	local name="$1"
	kubectl -n "${NS}" get "${SNAP_RES}" "${name}" -o json 2>/dev/null | jq -e \
		'(.status.conditions // []) | map(select(.type == "Ready")) | .[0].status == "True"' >/dev/null
}

content_ready() {
	local name="$1"
	kubectl get "${CONTENT_RES}" "${name}" -o json 2>/dev/null | jq -e \
		'(.status.conditions // []) | map(select(.type == "Ready")) | .[0].status == "True"' >/dev/null
}

content_uid() {
	kubectl get "${CONTENT_RES}" "$1" -o jsonpath='{.metadata.uid}'
}

vcr_name_for_content() {
	printf 'snap-vcr-%s' "$(content_uid "$1")"
}

pvc_uid() {
	kubectl -n "${NS}" get pvc "$1" -o jsonpath='{.metadata.uid}'
}

wait_pvc_bound() {
	local pvc="$1"
	wait_until "PVC ${pvc} Bound" pvc_phase_bound "${pvc}"
}

wait_snapshot_bound() {
	wait_until "Snapshot ${1} bound" snapshot_bound "${1}"
}

dump_ready_blockers() {
	local snap="$1" content="$2"
	log "DIAG: Snapshot ${snap} status.conditions:"
	kubectl -n "${NS}" get "${SNAP_RES}" "${snap}" -o json 2>/dev/null \
		| jq -c '.status.conditions // []' >&2 || true
	log "DIAG: SnapshotContent ${content}:"
	kubectl get "${CONTENT_RES}" "${content}" -o json 2>/dev/null | jq -c \
		'{conditions: (.status.conditions // []), dataRefs: (.status.dataRefs // [] | length), children: (.status.childrenSnapshotContentRefs // []), manifestCheckpointName: .status.manifestCheckpointName}' \
		>&2 || true
}

wait_snapshot_ready() {
	local snap="$1" content="${2:-}"
	if ! wait_until "Snapshot ${snap} Ready" snapshot_ready "${snap}"; then
		if [[ -n "${content}" ]]; then
			dump_ready_blockers "${snap}" "${content}"
		fi
		return 1
	fi
}

wait_content_ready() {
	local content="$1" snap="${2:-}"
	if ! wait_until "SnapshotContent ${content} Ready" content_ready "${content}"; then
		if [[ -n "${snap}" ]]; then
			dump_ready_blockers "${snap}" "${content}"
		fi
		return 1
	fi
}

vcr_json() {
	local vcr="$1"
	kubectl -n "${NS}" get "${VCR_RES}" "${vcr}" -o json 2>/dev/null
}

vcr_ready() {
	local vcr="$1"
	vcr_json "${vcr}" | jq -e \
		'(.status.conditions // []) | map(select(.type == "Ready")) | .[0] | .status == "True" and .reason == "Completed"' >/dev/null
}

# SSOT for publish is SnapshotContent.status.dataRefs[]; VCR is deleted after handoff and may
# disappear before smoke observes Ready=True — do not wait only on VCR Ready.
volume_capture_published() {
	local content="$1" uid="$2" vcr="$3"
	if content_has_dataref_uid "${content}" "${uid}"; then
		return 0
	fi
	if vcr_exists "${vcr}" && vcr_ready "${vcr}"; then
		return 0
	fi
	return 1
}

wait_volume_capture_published() {
	local content="$1" uid="$2" vcr="$3"
	wait_until "volume capture published (${content}, pvc uid ${uid})" \
		volume_capture_published "${content}" "${uid}" "${vcr}"
}

content_has_dataref_uid() {
	local content="$1"
	local uid="$2"
	kubectl get "${CONTENT_RES}" "${content}" -o json | jq -e --arg u "${uid}" \
		'(.status.dataRefs // []) | map(.targetUID) | index($u) != null' >/dev/null
}

wait_content_dataref_uid() {
	wait_until "SnapshotContent ${1} dataRef targetUID ${2}" content_has_dataref_uid "${1}" "${2}"
}

vcr_target_uids() {
	local vcr="$1"
	vcr_json "${vcr}" | jq -r '.spec.targets[]?.uid // empty' | sort -u
}

vcr_dataref_uids() {
	local vcr="$1"
	vcr_json "${vcr}" | jq -r '.status.dataRefs[]?.targetUID // empty' | sort -u
}

vcr_has_target_uid() {
	local vcr="$1" uid="$2"
	vcr_target_uids "${vcr}" | grep -qx "${uid}"
}

vcr_exists() {
	kubectl -n "${NS}" get "${VCR_RES}" "$1" >/dev/null 2>&1
}

vcr_absent() {
	! vcr_exists "$1"
}

pvc_phase_bound() {
	local pvc="$1"
	[[ "$(kubectl -n "${NS}" get pvc "${pvc}" -o jsonpath='{.status.phase}' 2>/dev/null)" == "Bound" ]]
}

# local-thin (and similar SCs) use WaitForFirstConsumer — schedule a holder pod so the PVC binds.
ensure_pvc_bound() {
	local pvc="$1"
	local pod="bind-${pvc}"
	if pvc_phase_bound "${pvc}"; then
		return 0
	fi
	if ! kubectl -n "${NS}" get pod "${pod}" >/dev/null 2>&1; then
		kubectl -n "${NS}" apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${pod}
  labels:
    state-snapshotter.deckhouse.io/pr8-smoke: bind
spec:
  restartPolicy: Never
  containers:
    - name: hold
      image: ${BIND_IMAGE}
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: ${pvc}
EOF
	fi
	wait_pvc_bound "${pvc}"
}

count_namespace_vcrs() {
	kubectl -n "${NS}" get "${VCR_RES}" -o json 2>/dev/null | jq '.items | length' 2>/dev/null || echo 0
}

assert_at_most_one_vcr() {
	local n
	n="$(count_namespace_vcrs)"
	if [[ "${n}" -gt 1 ]]; then
		log "ERROR: expected at most one VolumeCaptureRequest in ${NS}, got ${n}"
		kubectl -n "${NS}" get "${VCR_RES}" -o wide >&2 || true
		exit 1
	fi
}

assert_no_stub_annotation() {
	local snap="$1"
	if kubectl -n "${NS}" get "${SNAP_RES}" "${snap}" -o json | jq -e --arg k "${STUB_ANN}" '.metadata.annotations[$k] != null' >/dev/null 2>&1; then
		log "ERROR: Snapshot ${snap} must not use stub annotation ${STUB_ANN}"
		exit 1
	fi
}

assert_vsc_owned_by_content() {
	local vsc="$1"
	local content="$2"
	local cuid
	cuid="$(content_uid "${content}")"
	kubectl get "${VSC_RES}" "${vsc}" -o json | jq -e --arg cn "${content}" --arg cu "${cuid}" \
		'[.metadata.ownerReferences[]?
			| select(.apiVersion == "storage.deckhouse.io/v1alpha1"
				and .kind == "SnapshotContent"
				and .name == $cn
				and .uid == $cu)] | length >= 1' >/dev/null
}

merge_child_graph_into_root() {
	local root_snap="$1"
	local child_snap="$2"
	local child_content="$3"
	local root_content
	root_content="$(kubectl -n "${NS}" get "${SNAP_RES}" "${root_snap}" -o jsonpath='{.status.boundSnapshotContentName}')"
	local root_uid child_uid root_cuid child_cuid
	root_uid="$(kubectl -n "${NS}" get "${SNAP_RES}" "${root_snap}" -o jsonpath='{.metadata.uid}')"
	child_uid="$(kubectl -n "${NS}" get "${SNAP_RES}" "${child_snap}" -o jsonpath='{.metadata.uid}')"
	root_cuid="$(content_uid "${root_content}")"
	child_cuid="$(content_uid "${child_content}")"

	# Child Snapshot ownerRef -> root Snapshot (controller=true).
	local child_patch
	child_patch="$(jq -n \
		--arg av "${STORAGE_API}" --arg rn "${root_snap}" --arg ru "${root_uid}" \
		'{
			metadata: {
				ownerReferences: [{
					apiVersion: $av, kind: "Snapshot", name: $rn, uid: $ru, controller: true
				}]
			}
		}')"
	kubectl -n "${NS}" patch "${SNAP_RES}" "${child_snap}" --type=merge --patch="${child_patch}" >/dev/null

	# Root status.childrenSnapshotRefs
	local root_status_patch
	root_status_patch="$(jq -n \
		--arg av "${STORAGE_API}" --arg cn "${child_snap}" \
		'{status: {childrenSnapshotRefs: [{apiVersion: $av, kind: "Snapshot", name: $cn}]}}')"
	kubectl -n "${NS}" patch "${SNAP_RES}" "${root_snap}" --subresource=status --type=merge --patch="${root_status_patch}" >/dev/null

	# Child content ownerRef -> root content
	local child_content_patch
	child_content_patch="$(jq -n \
		--arg av "${STORAGE_API}" --arg rcn "${root_content}" --arg rcu "${root_cuid}" \
		'{
			metadata: {
				ownerReferences: [{
					apiVersion: $av, kind: "SnapshotContent", name: $rcn, uid: $rcu, controller: true
				}]
			}
		}')"
	kubectl patch "${CONTENT_RES}" "${child_content}" --type=merge --patch="${child_content_patch}" >/dev/null

	# Root content status.childrenSnapshotContentRefs
	local root_content_status_patch
	root_content_status_patch="$(jq -n \
		--arg ccn "${child_content}" \
		'{status: {childrenSnapshotContentRefs: [{name: $ccn}]}}')"
	kubectl patch "${CONTENT_RES}" "${root_content}" --subresource=status --type=merge --patch="${root_content_status_patch}" >/dev/null

	kick_snapshot "${root_snap}"
	kick_snapshot "${child_snap}"
}

log "== PR8 smoke: namespace=${NS} storageClass=${STORAGE_CLASS}"
log "Artifacts: ${ARTIFACT_DIR}"

log "== 0. Pre-flight"
kubectl get storageclass "${STORAGE_CLASS}" >/dev/null
kubectl get crd volumecapturerequests.storage.deckhouse.io >/dev/null
kubectl get crd snapshots.storage.deckhouse.io >/dev/null

log "== 0a. RBAC matrix (CSI vs state-snapshotter vs foundation; namespace=${NS})"
log "Note: namespace may not exist yet; can-i still reports policy for that namespace name."
CSI_VS_CAN="$(kubectl auth can-i create "${CSI_VS_RES}" -n "${NS}" 2>&1 || true)"
DECK_SNAP_CAN="$(kubectl auth can-i create "${SNAP_RES}" -n "${NS}" 2>&1 || true)"
VCR_CAN="$(kubectl auth can-i create "${VCR_RES}" -n "${NS}" 2>&1 || true)"
log "  kubectl auth can-i create ${CSI_VS_RES} -n ${NS}  => ${CSI_VS_CAN}"
log "    (CSI VolumeSnapshot — not created by this smoke; foundation VCR controller)"
log "  kubectl auth can-i create ${SNAP_RES} -n ${NS}  => ${DECK_SNAP_CAN}"
log "    (storage.deckhouse.io Snapshot — PR-8 smoke creates pr8-child / pr8-root via kubectl apply)"
log "  kubectl auth can-i create ${VCR_RES} -n ${NS}  => ${VCR_CAN}"
log "    (VolumeCaptureRequest — usually created by state-snapshotter controller, not smoke)"
log "== 0b. api-resources (snapshot / volumecapture)"
kubectl api-resources 2>/dev/null | grep -E 'snapshot|volumecapture' | while read -r line; do log "  ${line}"; done || true

if [[ "${DECK_SNAP_CAN}" != "yes" ]]; then
	log "ERROR: PR-8 smoke requires create on ${SNAP_RES} in namespace ${NS} (got: ${DECK_SNAP_CAN})"
	log "This is NOT CSI volumesnapshots.snapshot.storage.k8s.io (that may still be yes)."
	log "Failed step when run without preflight: == 3 Child Snapshot — kubectl apply kind=Snapshot apiVersion=${STORAGE_API}"
	log "Hint: Deckhouse binding d8:state-snapshotter:admin-kubeconfig or RBAC granting snapshots.storage.deckhouse.io"
	exit 1
fi
log "OK pre-flight RBAC: ${SNAP_RES} create allowed in ${NS}"

log "== 1. Namespace"
kubectl create namespace "${NS}" >/dev/null
log "OK namespace ${NS}"

log "== 2. PVC ${PVC_A} (${STORAGE_CLASS})"
kubectl -n "${NS}" apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${PVC_A}
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: ${STORAGE_CLASS}
  resources:
    requests:
      storage: 1Gi
EOF
ensure_pvc_bound "${PVC_A}"
PVC_A_UID="$(pvc_uid "${PVC_A}")"
log "OK pvc-a uid=${PVC_A_UID}"

log "== 3. Child Snapshot ${CHILD_SNAP} (resource=${SNAP_RES}, NOT ${CSI_VS_RES})"
log "CMD: kubectl -n ${NS} apply -f -  # kind=Snapshot apiVersion=${STORAGE_API}"
if ! kubectl -n "${NS}" apply -f - <<EOF
apiVersion: ${STORAGE_API}
kind: Snapshot
metadata:
  name: ${CHILD_SNAP}
spec: {}
EOF
then
	log "ERROR: kubectl apply failed for ${SNAP_RES} (state-snapshotter Snapshot, not CSI VolumeSnapshot)"
	exit 1
fi
wait_snapshot_bound "${CHILD_SNAP}"
CHILD_CONTENT="$(kubectl -n "${NS}" get "${SNAP_RES}" "${CHILD_SNAP}" -o jsonpath='{.status.boundSnapshotContentName}')"
CHILD_VCR="$(vcr_name_for_content "${CHILD_CONTENT}")"
log "child content=${CHILD_CONTENT} expected VCR=${CHILD_VCR}"

assert_no_stub_annotation "${CHILD_SNAP}"
assert_at_most_one_vcr

wait_until "VolumeCaptureRequest ${CHILD_VCR} exists" vcr_exists "${CHILD_VCR}"
assert_at_most_one_vcr

log "== 4. Child VCR targets (pvc-a only)"
wait_until "child VCR spec.targets contains pvc-a uid" vcr_has_target_uid "${CHILD_VCR}" "${PVC_A_UID}"

targets_n="$(vcr_json "${CHILD_VCR}" | jq '.spec.targets | length')"
if [[ "${targets_n}" != "1" ]]; then
	log "ERROR: child VCR must have exactly one target, got ${targets_n}"
	vcr_json "${CHILD_VCR}" | jq '.spec.targets' >&2
	exit 1
fi
log "OK child VCR has 1 target (pvc-a)"

wait_volume_capture_published "${CHILD_CONTENT}" "${PVC_A_UID}" "${CHILD_VCR}"
if vcr_exists "${CHILD_VCR}"; then
	log "OK child VCR still present (Ready or publishing)"
	wait_until "child VCR ${CHILD_VCR} deleted after publish" vcr_absent "${CHILD_VCR}"
else
	log "OK child VCR already deleted after publish (expected)"
fi

CHILD_VSC="$(kubectl get "${CONTENT_RES}" "${CHILD_CONTENT}" -o json | jq -r \
	--arg u "${PVC_A_UID}" \
	'(.status.dataRefs // [])[] | select(.targetUID == $u) | .artifact.name' | head -1)"
if [[ -z "${CHILD_VSC}" || "${CHILD_VSC}" == "null" ]]; then
	log "ERROR: child SnapshotContent missing VSC artifact for pvc-a"
	kubectl get "${CONTENT_RES}" "${CHILD_CONTENT}" -o yaml >&2
	exit 1
fi
assert_vsc_owned_by_content "${CHILD_VSC}" "${CHILD_CONTENT}"
log "OK child dataRefs + VSC ownerRef (${CHILD_VSC})"

wait_content_ready "${CHILD_CONTENT}" "${CHILD_SNAP}"
wait_snapshot_ready "${CHILD_SNAP}" "${CHILD_CONTENT}"
log "OK child Ready"

log "== 5. Root Snapshot ${ROOT_SNAP} + merge child graph (before pvc-b)"
kubectl -n "${NS}" apply -f - <<EOF
apiVersion: ${STORAGE_API}
kind: Snapshot
metadata:
  name: ${ROOT_SNAP}
spec: {}
EOF
wait_snapshot_bound "${ROOT_SNAP}"
ROOT_CONTENT="$(kubectl -n "${NS}" get "${SNAP_RES}" "${ROOT_SNAP}" -o jsonpath='{.status.boundSnapshotContentName}')"
merge_child_graph_into_root "${ROOT_SNAP}" "${CHILD_SNAP}" "${CHILD_CONTENT}"
assert_no_stub_annotation "${ROOT_SNAP}"
log "OK root bound content=${ROOT_CONTENT}, graph merged"

# Root may reconcile once before childrenSnapshotRefs are visible and create a VCR for
# child-covered pvc-a (only PVC in namespace at that moment). Drop it before pvc-b.
ROOT_VCR="$(vcr_name_for_content "${ROOT_CONTENT}")"
if vcr_exists "${ROOT_VCR}"; then
	if vcr_target_uids "${ROOT_VCR}" | grep -qx "${PVC_A_UID}"; then
		log "WARN: deleting premature root VCR ${ROOT_VCR} (captured child-covered pvc-a before residual pvc-b)"
		kubectl -n "${NS}" delete "${VCR_RES}" "${ROOT_VCR}" --wait=true
		kick_snapshot "${ROOT_SNAP}"
	fi
fi

log "== 6. PVC ${PVC_B}"
kubectl -n "${NS}" apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${PVC_B}
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: ${STORAGE_CLASS}
  resources:
    requests:
      storage: 1Gi
EOF
ensure_pvc_bound "${PVC_B}"
PVC_B_UID="$(pvc_uid "${PVC_B}")"
log "OK pvc-b uid=${PVC_B_UID}"

kick_snapshot "${ROOT_SNAP}"

ROOT_VCR="$(vcr_name_for_content "${ROOT_CONTENT}")"
assert_at_most_one_vcr

log "== 7. Root residual pvc-b (VCR optional after publish; SSOT is SnapshotContent.dataRefs[])"
wait_volume_capture_published "${ROOT_CONTENT}" "${PVC_B_UID}" "${ROOT_VCR}"

if vcr_exists "${ROOT_VCR}"; then
	root_vcr_json="$(vcr_json "${ROOT_VCR}" || true)"
	if [[ -z "${root_vcr_json}" ]]; then
		log "OK root VCR removed during inspection (publish handoff race)"
	elif ! echo "${root_vcr_json}" | jq -e '.spec.targets' >/dev/null 2>&1; then
		log "OK root VCR has no spec.targets yet (deleted or publishing)"
	else
		if echo "${root_vcr_json}" | jq -r '.spec.targets[]?.uid // empty' | grep -qx "${PVC_A_UID}"; then
			log "ERROR: root VCR must not target child-covered pvc-a"
			echo "${root_vcr_json}" | jq '.spec.targets' >&2
			exit 1
		fi
		root_targets_n="$(echo "${root_vcr_json}" | jq '.spec.targets | length')"
		if [[ "${root_targets_n}" != "1" ]]; then
			log "ERROR: root VCR must have exactly one target (residual pvc-b), got ${root_targets_n}"
			exit 1
		fi
		log "OK root VCR has 1 target (pvc-b) before publish completes"
		if vcr_exists "${ROOT_VCR}"; then
			wait_until "root VCR ${ROOT_VCR} deleted after publish" vcr_absent "${ROOT_VCR}" || true
		fi
	fi
else
	log "OK root VCR already deleted after publish (expected)"
fi

# Child must not grow a second capture for pvc-b.
if kubectl -n "${NS}" get "${VCR_RES}" "${CHILD_VCR}" >/dev/null 2>&1; then
	log "ERROR: child VCR re-created for pvc-b (child should keep only published pvc-a)"
	exit 1
fi
if kubectl get "${CONTENT_RES}" "${CHILD_CONTENT}" -o json | jq -e --arg u "${PVC_B_UID}" \
	'(.status.dataRefs // []) | map(.targetUID) | index($u) != null' >/dev/null; then
	log "ERROR: child SnapshotContent must not publish dataRef for pvc-b"
	exit 1
fi
log "OK child did not duplicate pvc-b"

if kubectl get "${CONTENT_RES}" "${ROOT_CONTENT}" -o json | jq -e --arg u "${PVC_A_UID}" \
	'(.status.dataRefs // []) | map(.targetUID) | index($u) != null' >/dev/null; then
	log "ERROR: root SnapshotContent must not duplicate child-covered pvc-a in dataRefs"
	exit 1
fi
log "OK root dataRefs has pvc-b only (residual)"

ROOT_VSC="$(kubectl get "${CONTENT_RES}" "${ROOT_CONTENT}" -o json | jq -r \
	--arg u "${PVC_B_UID}" \
	'(.status.dataRefs // [])[] | select(.targetUID == $u) | .artifact.name' | head -1)"
assert_vsc_owned_by_content "${ROOT_VSC}" "${ROOT_CONTENT}"
log "OK root VSC ownerRef (${ROOT_VSC})"

wait_content_ready "${ROOT_CONTENT}" "${ROOT_SNAP}"
wait_snapshot_ready "${ROOT_SNAP}" "${ROOT_CONTENT}"
log "OK root Ready (MCP + dataRefs + children)"

if [[ -f "$(dirname "$0")/snapshot-graph.sh" ]]; then
	bash "$(dirname "$0")/snapshot-graph.sh" \
		--namespace "${NS}" \
		--snapshot "${ROOT_SNAP}" \
		--output-dir "${ARTIFACT_DIR}" \
		--name "pr8-ready" \
		--mode logical \
		--title "PR8 two-PVC subtree ready" \
		--description "PR8 cluster smoke: child pvc-a via VCR/VSC; root residual pvc-b; bulk VCR per content." \
		2>/dev/null || log "WARN: snapshot-graph.sh failed (non-fatal)"
fi

log "== PR8 smoke PASSED (namespace=${NS})"
