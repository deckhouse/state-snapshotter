#!/usr/bin/env bash
# Canonical cluster demo/e2e: state-snapshotter + storage-foundation in one scenario.
#
# Verifies: manifest capture (MCR/MCP), aggregated manifests, retained lifecycle, ObjectKeeper GC,
# bulk VCR → VSC → SnapshotContent.status.dataRefs[] (local-thin only).
#
# TODO(known-limitation): Rook/Ceph is not supported; do not extend for Ceph without a separate matrix.
#
# Retained manifests read (temporary):
#   GET .../namespaces/{ns}/snapshots/{rootName}/manifests after root Snapshot delete.
# TODO(retained-read-api): migrate to /snapshotcontents/{contentName}/manifests; live route → 404 after delete.
#
# Artifacts: ${DEMO_E2E_ARTIFACT_DIR:-artifacts}/<run-id>/{00-preflight..09-cleanup}/
#
# Usage: ./hack/demo-e2e.sh
#
# Env:
#   DEMO_E2E_NAMESPACE           default: demo-e2e-<timestamp>
#   DEMO_E2E_STORAGE_CLASS       default: local-thin
#   DEMO_E2E_WAIT_SEC            default: 900
#   DEMO_E2E_POLL_SEC            default: 5
#   DEMO_E2E_GC_WAIT_SEC         max wait after forced ObjectKeeper TTL (default: 120)
#   DEMO_E2E_SKIP_CLEANUP        1 = keep namespace
#   DEMO_E2E_SKIP_FORCED_TTL     1 = skip stage 08-forced-ttl-gc
#   DEMO_E2E_SKIP_OK_CONTRACT   1 = skip ObjectKeeper contract checks (no objectkeepers.deckhouse.io)
#   DEMO_E2E_ARTIFACT_DIR        artifact root (alias: DEMO_E2E_ARTIFACTS_ROOT)
#   DEMO_E2E_BIND_IMAGE          bind pod image for WaitForFirstConsumer PVCs

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN_ID="$(date +%Y%m%d-%H%M%S)"

STORAGE_CLASS="${DEMO_E2E_STORAGE_CLASS:-local-thin}"
WAIT_SEC="${DEMO_E2E_WAIT_SEC:-900}"
GC_WAIT_SEC="${DEMO_E2E_GC_WAIT_SEC:-120}"
POLL_SEC="${DEMO_E2E_POLL_SEC:-5}"
NS="${DEMO_E2E_NAMESPACE:-demo-e2e-${RUN_ID}}"
ARTIFACTS_ROOT="${DEMO_E2E_ARTIFACT_DIR:-${DEMO_E2E_ARTIFACTS_ROOT:-artifacts}}"
RUN_ARTIFACT_DIR="${ARTIFACTS_ROOT}/${RUN_ID}"

CHILD_SNAP="demo-child"
ROOT_SNAP="demo-root"
CM_NAME="demo-cm"
PVC_A="pvc-a"
PVC_B="pvc-b"
BIND_IMAGE="${DEMO_E2E_BIND_IMAGE:-registry.k8s.io/pause:3.9}"

SNAP_RES="snapshots.storage.deckhouse.io"
CONTENT_RES="snapshotcontents.storage.deckhouse.io"
VCR_RES="volumecapturerequests.storage.deckhouse.io"
VSC_RES="volumesnapshotcontents.snapshot.storage.k8s.io"
CSI_VS_RES="volumesnapshots.snapshot.storage.k8s.io"
MCR_RES="manifestcapturerequests.state-snapshotter.deckhouse.io"
MCP_RES="manifestcheckpoints.state-snapshotter.deckhouse.io"
OK_RES="objectkeepers.deckhouse.io"
STORAGE_API="storage.deckhouse.io/v1alpha1"
STUB_ANN="state-snapshotter.deckhouse.io/volume-capture-stub-pvcs"
SUBAPI="subresources.state-snapshotter.deckhouse.io"
SUBVER="v1alpha1"

CURRENT_STAGE=""
CHILD_CONTENT=""
ROOT_CONTENT=""
CHILD_MCP=""
ROOT_MCP=""
PVC_A_UID=""
PVC_B_UID=""
ROOT_SNAP_UID=""
OK_NAME="ret-snap-${NS}-${ROOT_SNAP}"
HAS_OBJECTKEEPER=0

log() { printf '%s\n' "$*" >&2; }

need_cmd() {
	command -v "$1" >/dev/null 2>&1 || {
		log "ERROR: missing required command: $1"
		exit 1
	}
}

cleanup_ns() {
	if [[ "${DEMO_E2E_SKIP_CLEANUP:-0}" == "1" ]]; then
		log "SKIP cleanup: DEMO_E2E_SKIP_CLEANUP=1 namespace=${NS}"
		return 0
	fi
	kubectl delete namespace "${NS}" --ignore-not-found=true --wait=false 2>/dev/null || true
}

trap cleanup_ns EXIT

need_cmd kubectl
need_cmd jq
mkdir -p "${RUN_ARTIFACT_DIR}"

stage_dir() { printf '%s/%s' "${RUN_ARTIFACT_DIR}" "$1"; }

write_stage_summary() {
	local stage="$1" status="$2" changed="$3" expected="$4"
	local f
	f="$(stage_dir "${stage}")/summary.txt"
	{
		printf 'stage: %s\n' "${stage}"
		printf 'status: %s\n' "${status}"
		printf 'changed: %s\n' "${changed}"
		printf 'expected: %s\n' "${expected}"
	} >"${f}"
}

begin_stage() {
	CURRENT_STAGE="$1"
	local dir
	dir="$(stage_dir "${CURRENT_STAGE}")"
	mkdir -p "${dir}"
	log ""
	log "======== STAGE ${CURRENT_STAGE} ========"
	printf '%s\n' "${CURRENT_STAGE}" >"${dir}/STAGE.txt"
	date -u +"%Y-%m-%dT%H:%M:%SZ" >"${dir}/timestamp.txt"
}

wait_until() {
	local desc="$1" deadline
	shift
	deadline=$((SECONDS + WAIT_SEC))
	local phase_start=${SECONDS} last_log=${SECONDS}
	local log_every="${DEMO_E2E_WAIT_LOG_EVERY_SEC:-30}"
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
	log "ERROR: timeout waiting for ${desc} (${WAIT_SEC}s) [stage=${CURRENT_STAGE}]"
	return 1
}

wait_until_gc() {
	local desc="$1" deadline
	shift
	deadline=$((SECONDS + GC_WAIT_SEC))
	local phase_start=${SECONDS} last_log=${SECONDS}
	while (( SECONDS < deadline )); do
		if "$@"; then
			log "OK ${desc}"
			return 0
		fi
		if (( SECONDS - last_log >= 15 )); then
			log "WAIT (forced GC): ${desc} ($((SECONDS - phase_start))s / ${GC_WAIT_SEC}s)"
			last_log=${SECONDS}
		fi
		sleep "${POLL_SEC}"
	done
	log "ERROR: timeout forced GC wait for ${desc} (${GC_WAIT_SEC}s)"
	return 1
}

kick_snapshot() {
	kubectl -n "${NS}" annotate "${SNAP_RES}" "$1" \
		"state-snapshotter.deckhouse.io/demo-e2e-kick=$(date +%s)" --overwrite >/dev/null
}

snapshot_bound() {
	kubectl -n "${NS}" get "${SNAP_RES}" "$1" -o json 2>/dev/null | jq -e \
		'.status.boundSnapshotContentName != null and (.status.boundSnapshotContentName | length > 0)' >/dev/null
}

snapshot_ready() {
	kubectl -n "${NS}" get "${SNAP_RES}" "$1" -o json 2>/dev/null | jq -e \
		'(.status.conditions // []) | map(select(.type == "Ready")) | .[0].status == "True"' >/dev/null
}

content_ready() {
	kubectl get "${CONTENT_RES}" "$1" -o json 2>/dev/null | jq -e \
		'(.status.conditions // []) | map(select(.type == "Ready")) | .[0].status == "True"' >/dev/null
}

content_uid() { kubectl get "${CONTENT_RES}" "$1" -o jsonpath='{.metadata.uid}'; }
vcr_name_for_content() { printf 'snap-vcr-%s' "$(content_uid "$1")"; }
pvc_uid() { kubectl -n "${NS}" get pvc "$1" -o jsonpath='{.metadata.uid}'; }
pvc_phase_bound() { [[ "$(kubectl -n "${NS}" get pvc "$1" -o jsonpath='{.status.phase}' 2>/dev/null)" == "Bound" ]]; }

vcr_json() { kubectl -n "${NS}" get "${VCR_RES}" "$1" -o json 2>/dev/null; }
vcr_exists() { kubectl -n "${NS}" get "${VCR_RES}" "$1" >/dev/null 2>&1; }
vcr_absent() { ! vcr_exists "$1"; }
vcr_has_target_uid() { vcr_json "$1" | jq -r '.spec.targets[]?.uid // empty' | grep -qx "$2"; }

content_has_dataref_uid() {
	kubectl get "${CONTENT_RES}" "$1" -o json | jq -e --arg u "$2" \
		'(.status.dataRefs // []) | map(.targetUID) | index($u) != null' >/dev/null
}

volume_capture_published() {
	content_has_dataref_uid "$1" "$2" && return 0
	if vcr_exists "$3"; then
		vcr_json "$3" | jq -e \
			'(.status.conditions // []) | map(select(.type == "Ready")) | .[0] | .status == "True" and .reason == "Completed"' >/dev/null
	fi
	return 1
}

wait_volume_capture_published() {
	wait_until "volume capture published (${1}, uid ${2})" volume_capture_published "$1" "$2" "$3"
}

# VCR is ephemeral (deleted after handoff). Inspect spec.targets only while VCR is still visible.
verify_vcr_targets_if_present() {
	local vcr="$1" uid="$2" label="$3"
	vcr_exists "${vcr}" || return 0
	wait_until "${label} VCR spec.targets contains pvc uid" vcr_has_target_uid "${vcr}" "${uid}"
	local n
	n="$(vcr_json "${vcr}" | jq '.spec.targets | length')"
	[[ "${n}" == "1" ]] || {
		log "ERROR: ${label} VCR must have exactly one target, got ${n}"
		exit 1
	}
	log "OK ${label} VCR has 1 target before publish completes"
}

dump_ready_blockers() {
	log "DIAG Snapshot $1:"
	kubectl -n "${NS}" get "${SNAP_RES}" "$1" -o json 2>/dev/null | jq -c '.status.conditions // []' >&2 || true
	log "DIAG SnapshotContent $2:"
	kubectl get "${CONTENT_RES}" "$2" -o json 2>/dev/null | jq -c \
		'{conditions: (.status.conditions // []), dataRefs: .status.dataRefs, children: .status.childrenSnapshotContentRefs, mcp: .status.manifestCheckpointName}' \
		>&2 || true
}

wait_snapshot_ready() {
	if ! wait_until "Snapshot $1 Ready" snapshot_ready "$1"; then
		[[ -n "${2:-}" ]] && dump_ready_blockers "$1" "$2"
		return 1
	fi
}

wait_content_ready() {
	if ! wait_until "SnapshotContent $1 Ready" content_ready "$1"; then
		[[ -n "${2:-}" ]] && dump_ready_blockers "${2:-}" "$1"
		return 1
	fi
}

ensure_pvc_bound() {
	local pvc="$1"
	local pod="bind-${pvc}"
	if pvc_phase_bound "${pvc}"; then return 0; fi
	if ! kubectl -n "${NS}" get pod "${pod}" >/dev/null 2>&1; then
		kubectl -n "${NS}" apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${pod}
  labels:
    state-snapshotter.deckhouse.io/demo-e2e: bind
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
	wait_until "PVC ${pvc} Bound" pvc_phase_bound "${pvc}"
}

assert_at_most_one_vcr() {
	local n
	n="$(kubectl -n "${NS}" get "${VCR_RES}" -o json 2>/dev/null | jq '.items | length' 2>/dev/null || echo 0)"
	if [[ "${n}" -gt 1 ]]; then
		log "ERROR: expected at most one VCR in ${NS}, got ${n}"
		exit 1
	fi
}

assert_no_stub_annotation() {
	if kubectl -n "${NS}" get "${SNAP_RES}" "$1" -o json | jq -e --arg k "${STUB_ANN}" '.metadata.annotations[$k] != null' >/dev/null 2>&1; then
		log "ERROR: Snapshot $1 must not use stub ${STUB_ANN}"
		exit 1
	fi
}

assert_vsc_owned_by_content() {
	local cuid
	cuid="$(content_uid "$2")"
	kubectl get "${VSC_RES}" "$1" -o json | jq -e --arg cn "$2" --arg cu "${cuid}" \
		'[.metadata.ownerReferences[]?
			| select(.apiVersion == "storage.deckhouse.io/v1alpha1" and .kind == "SnapshotContent"
				and .name == $cn and .uid == $cu)] | length >= 1' >/dev/null
}

root_has_child_content_ref() {
	kubectl get "${CONTENT_RES}" "${ROOT_CONTENT}" -o json | jq -e --arg c "${CHILD_CONTENT}" \
		'(.status.childrenSnapshotContentRefs // []) | map(.name) | index($c) != null' >/dev/null
}

merge_child_graph_into_root() {
	local root_content root_uid child_uid root_cuid
	root_content="$(kubectl -n "${NS}" get "${SNAP_RES}" "${ROOT_SNAP}" -o jsonpath='{.status.boundSnapshotContentName}')"
	root_uid="$(kubectl -n "${NS}" get "${SNAP_RES}" "${ROOT_SNAP}" -o jsonpath='{.metadata.uid}')"
	child_uid="$(kubectl -n "${NS}" get "${SNAP_RES}" "${CHILD_SNAP}" -o jsonpath='{.metadata.uid}')"
	root_cuid="$(content_uid "${root_content}")"

	kubectl -n "${NS}" patch "${SNAP_RES}" "${CHILD_SNAP}" --type=merge --patch="$(jq -n \
		--arg av "${STORAGE_API}" --arg rn "${ROOT_SNAP}" --arg ru "${root_uid}" \
		'{metadata: {ownerReferences: [{apiVersion: $av, kind: "Snapshot", name: $rn, uid: $ru, controller: true}]}}')" >/dev/null
	kubectl -n "${NS}" patch "${SNAP_RES}" "${ROOT_SNAP}" --subresource=status --type=merge --patch="$(jq -n \
		--arg av "${STORAGE_API}" --arg cn "${CHILD_SNAP}" \
		'{status: {childrenSnapshotRefs: [{apiVersion: $av, kind: "Snapshot", name: $cn}]}}')" >/dev/null
	kubectl patch "${CONTENT_RES}" "${CHILD_CONTENT}" --type=merge --patch="$(jq -n \
		--arg av "${STORAGE_API}" --arg rcn "${root_content}" --arg rcu "${root_cuid}" \
		'{metadata: {ownerReferences: [{apiVersion: $av, kind: "SnapshotContent", name: $rcn, uid: $rcu, controller: true}]}}')" >/dev/null
	kubectl patch "${CONTENT_RES}" "${root_content}" --subresource=status --type=merge --patch="$(jq -n \
		--arg ccn "${CHILD_CONTENT}" '{status: {childrenSnapshotContentRefs: [{name: $ccn}]}}')" >/dev/null
	kick_snapshot "${ROOT_SNAP}"
	kick_snapshot "${CHILD_SNAP}"
}

dump_list_to_dir() {
	local out="$1" kind="$2" namespaced="${3:-}" resource="$4"
	local -a args=(-o yaml)
	[[ "${namespaced}" == "ns" ]] && args=(-n "${NS}" -o yaml)
	mkdir -p "${out}"
	if [[ "${kind}" == "events" ]]; then
		kubectl get events -n "${NS}" --sort-by=.metadata.creationTimestamp >"${out}/events.txt" 2>"${out}/events.err" || true
		kubectl get events -n "${NS}" -o json >"${out}/events.json" 2>/dev/null || true
		return
	fi
	kubectl get "${resource}" "${args[@]}" >"${out}/${kind}.yaml" 2>"${out}/${kind}.err" || true
	kubectl get "${resource}" -o json "${args[@]}" >"${out}/${kind}.json" 2>/dev/null || true
}

dump_named_resource() {
	local out="$1" file="$2"
	shift 2
	mkdir -p "${out}"
	kubectl "$@" -o yaml >"${out}/${file}.yaml" 2>"${out}/${file}.err" || true
	kubectl "$@" -o json >"${out}/${file}.json" 2>/dev/null || true
}

save_artifacts() {
	local stage="$1" dir res
	dir="$(stage_dir "${stage}")"
	res="${dir}/resources"
	mkdir -p "${res}"
	log "Artifacts: ${dir}"
	dump_list_to_dir "${res}" "configmaps" "ns" "configmap"
	dump_list_to_dir "${res}" "pvcs" "ns" "pvc"
	dump_list_to_dir "${res}" "pods" "ns" "pod"
	dump_list_to_dir "${res}" "events" "ns" ""
	dump_list_to_dir "${res}" "snapshots" "ns" "${SNAP_RES}"
	dump_list_to_dir "${res}" "volumecapturerequests" "ns" "${VCR_RES}"
	dump_list_to_dir "${res}" "volumesnapshots" "ns" "${CSI_VS_RES}"
	dump_list_to_dir "${res}" "snapshotcontents" "" "${CONTENT_RES}"
	dump_list_to_dir "${res}" "volumesnapshotcontents" "" "${VSC_RES}"
	dump_list_to_dir "${res}" "manifestcapturerequests" "ns" "${MCR_RES}"
	dump_list_to_dir "${res}" "manifestcheckpoints" "" "${MCP_RES}"
	dump_list_to_dir "${res}" "objectkeepers" "" "${OK_RES}"
	[[ -n "${CHILD_CONTENT}" ]] && dump_named_resource "${res}" "child-snapshotcontent" get "${CONTENT_RES}" "${CHILD_CONTENT}"
	[[ -n "${ROOT_CONTENT}" ]] && dump_named_resource "${res}" "root-snapshotcontent" get "${CONTENT_RES}" "${ROOT_CONTENT}"
	[[ -n "${CHILD_MCP}" ]] && dump_named_resource "${res}" "child-mcp" get "${MCP_RES}" "${CHILD_MCP}"
	[[ -n "${ROOT_MCP}" ]] && dump_named_resource "${res}" "root-mcp" get "${MCP_RES}" "${ROOT_MCP}"
	kubectl -n "${NS}" get pvc -o json 2>/dev/null | jq -r '.items[]?.spec.volumeName // empty' | while read -r pv; do
		[[ -n "${pv}" ]] && dump_named_resource "${res}" "pv-${pv}" get pv "${pv}"
	done
	printf 'namespace=%s\nrun_id=%s\nstage=%s\n' "${NS}" "${RUN_ID}" "${stage}" >"${dir}/run-context.txt"
}

save_graph() {
	local stage="$1" graph_name="$2" mode="$3" title="$4" desc="$5" graph_snap="${6:-}" graph_content="${7:-}"
	local dir graph_dir
	dir="$(stage_dir "${stage}")"
	graph_dir="${dir}/graph"
	mkdir -p "${graph_dir}"
	[[ -f "${SCRIPT_DIR}/snapshot-graph.sh" ]] || return 0
	if [[ -n "${graph_content}" ]]; then
		bash "${SCRIPT_DIR}/snapshot-graph.sh" --snapshotcontent "${graph_content}" \
			--output-dir "${graph_dir}" --name "${graph_name}" --mode "${mode}" \
			--title "${title}" --description "${desc}" 2>"${dir}/graph.err" || log "WARN: graph ${stage}"
	elif [[ -n "${graph_snap}" ]]; then
		bash "${SCRIPT_DIR}/snapshot-graph.sh" --namespace "${NS}" --snapshot "${graph_snap}" \
			--output-dir "${graph_dir}" --name "${graph_name}" --mode "${mode}" \
			--title "${title}" --description "${desc}" 2>"${dir}/graph.err" || log "WARN: graph ${stage}"
	fi
}

finish_stage() {
	local stage="$1" status="$2" changed="$3" expected="$4"
	local gname="${5:-${stage}}" gmode="${6:-logical}" gsnap="${7:-}" gcontent="${8:-}"
	write_stage_summary "${stage}" "${status}" "${changed}" "${expected}"
	save_artifacts "${stage}"
	save_graph "${stage}" "${gname}" "${gmode}" "${stage}" "${expected}" "${gsnap}" "${gcontent}"
}

aggregated_path() { printf '/apis/%s/%s/namespaces/%s/snapshots/%s/manifests' "${SUBAPI}" "${SUBVER}" "${NS}" "$1"; }

verify_aggregated_present() {
	local snap="$1" out="$2"
	# TODO(retained-read-api): temporary snapshot-name route; see script header.
	if ! kubectl get --raw "$(aggregated_path "${snap}")" >"${out}" 2>"${out}.err"; then
		log "ERROR: aggregated GET failed for ${snap} (see ${out}.err)"
		log "Hint: admin kubeconfig needs get snapshots/manifests (subresources.state-snapshotter.deckhouse.io); see templates/rbac-for-us.yaml"
		cat "${out}.err" >&2 2>/dev/null || true
		exit 1
	fi
	jq -e 'type == "array" and length >= 1' "${out}" >/dev/null
	jq -e --arg n "${CM_NAME}" '[.[] | select(.kind == "ConfigMap" and .metadata.name == $n)] | length >= 1' "${out}" >/dev/null
	log "OK aggregated manifests contain ${CM_NAME}"
}

verify_aggregated_absent() {
	local snap="$1"
	set +e
	kubectl get --raw "$(aggregated_path "${snap}")" >/dev/null 2>&1
	local rc=$?
	set -e
	[[ "${rc}" -ne 0 ]] || {
		log "ERROR: aggregated read still works for ${snap} after forced GC (expected failure)"
		return 1
	}
	log "OK aggregated read path no longer succeeds (rc=${rc})"
}

assert_root_objectkeeper_contract() {
	[[ "${HAS_OBJECTKEEPER}" == "1" ]] || return 0
	[[ "${DEMO_E2E_SKIP_OK_CONTRACT:-0}" != "1" ]] || return 0
	local ok_json ok_uid nsc_json
	ok_json="$(kubectl get "${OK_RES}" "${OK_NAME}" -o json)"
	ok_uid="$(echo "${ok_json}" | jq -r '.metadata.uid')"
	echo "${ok_json}" | jq -e --arg suid "${ROOT_SNAP_UID}" \
		'(.spec.mode == "FollowObjectWithTTL") and (.spec.ttl != null)
			and (.spec.followObjectRef.kind == "Snapshot") and (.spec.followObjectRef.uid == $suid)' >/dev/null
	nsc_json="$(kubectl get "${CONTENT_RES}" "${ROOT_CONTENT}" -o json)"
	echo "${nsc_json}" | jq -e --arg on "${OK_NAME}" --arg ouid "${ok_uid}" \
		'[.metadata.ownerReferences[]? | select(.kind == "ObjectKeeper" and .name == $on and .uid == $ouid and .controller == true)] | length >= 1' >/dev/null
	log "OK ObjectKeeper ${OK_NAME} contract"
}

# Force ObjectKeeper GC without waiting real TTL: shorten spec.ttl (metav1.Duration JSON string).
# Deckhouse ObjectKeeper controller reconciles FollowObjectWithTTL after followed Snapshot is gone.
force_objectkeeper_ttl_expired() {
	local ok_name="$1" dir patch_file before
	dir="$(stage_dir "08-forced-ttl-gc")"
	mkdir -p "${dir}"
	before="${dir}/objectkeeper-before-patch.yaml"
	kubectl get "${OK_RES}" "${ok_name}" -o yaml >"${before}" 2>/dev/null || true
	patch_file="${dir}/objectkeeper-ttl-patch.json"
	# spec.ttl is *metav1.Duration; "0s" makes retention immediate on next OK reconcile.
	printf '%s\n' '{"spec":{"ttl":"0s"}}' >"${patch_file}"
	log "Patching ${ok_name} spec.ttl=0s (forced expiry; no real TTL wait)"
	kubectl patch "${OK_RES}" "${ok_name}" --type=merge --patch-file="${patch_file}"
	kubectl get "${OK_RES}" "${ok_name}" -o yaml >"${dir}/objectkeeper-after-patch.yaml" 2>/dev/null || true
	# Optional status fields vary by Deckhouse version; best-effort only.
	local past
	past="$(date -u -v-2H +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '2 hours ago' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)"
	kubectl patch "${OK_RES}" "${ok_name}" --type=merge --subresource=status \
		--patch "{\"status\":{\"expiresAt\":\"${past}\"}}" 2>"${dir}/objectkeeper-status-patch.err" || true
}

content_absent() { ! kubectl get "${CONTENT_RES}" "${ROOT_CONTENT}" >/dev/null 2>&1; }
mcp_absent() { [[ -z "${ROOT_MCP}" ]] || ! kubectl get "${MCP_RES}" "${ROOT_MCP}" >/dev/null 2>&1; }

# =============================================================================
log "== demo-e2e run_id=${RUN_ID} namespace=${NS} storageClass=${STORAGE_CLASS}"
log "Artifacts: ${RUN_ARTIFACT_DIR}"

# --- 00-preflight ---
begin_stage "00-preflight"
kubectl get storageclass "${STORAGE_CLASS}" >/dev/null
kubectl get crd snapshots.storage.deckhouse.io snapshotcontents.storage.deckhouse.io >/dev/null
kubectl get crd volumecapturerequests.storage.deckhouse.io >/dev/null
kubectl get crd volumesnapshots.snapshot.storage.k8s.io volumesnapshotcontents.snapshot.storage.k8s.io >/dev/null
if kubectl get crd objectkeepers.deckhouse.io >/dev/null 2>&1; then
	HAS_OBJECTKEEPER=1
else
	log "WARN: objectkeepers.deckhouse.io CRD missing; ObjectKeeper checks and forced TTL skipped"
fi
kubectl get crd manifestcheckpoints.state-snapshotter.deckhouse.io manifestcapturerequests.state-snapshotter.deckhouse.io >/dev/null
DECK_SNAP_CAN="$(kubectl auth can-i create "${SNAP_RES}" -n "${NS}" 2>&1 || true)"
[[ "${DECK_SNAP_CAN}" == "yes" ]] || { log "ERROR: need create ${SNAP_RES} in ${NS}"; exit 1; }
# can-i is unreliable for APIService subresources; probe --raw (404 = RBAC ok, Forbidden = missing rule).
AGG_RBAC_PROBE="$(stage_dir "00-preflight")/aggregated-rbac-probe.err"
set +e
kubectl get --raw "/apis/${SUBAPI}/${SUBVER}/namespaces/default/snapshots/__demo_e2e_rbac_probe__/manifests" >/dev/null 2>"${AGG_RBAC_PROBE}"
AGG_RBAC_RC=$?
set -e
if grep -q Forbidden "${AGG_RBAC_PROBE}" 2>/dev/null; then
	log "ERROR: kubernetes-admin cannot GET snapshots/manifests (${SUBAPI})"
	log "Redeploy state-snapshotter module (templates/rbac-for-us.yaml) or have cluster-admin patch ClusterRole d8:state-snapshotter:admin-kubeconfig"
	cat "${AGG_RBAC_PROBE}" >&2
	exit 1
fi
[[ "${AGG_RBAC_RC}" -eq 0 || "${AGG_RBAC_RC}" -eq 1 ]] || {
	log "ERROR: aggregated RBAC probe failed (rc=${AGG_RBAC_RC})"
	cat "${AGG_RBAC_PROBE}" >&2
	exit 1
}
{
	echo "storage_class=${STORAGE_CLASS}"
	echo "snap_create_can=${DECK_SNAP_CAN}"
	echo "agg_rbac_probe_rc=${AGG_RBAC_RC}"
	echo "has_objectkeeper=${HAS_OBJECTKEEPER}"
	kubectl get storageclass "${STORAGE_CLASS}" -o json | jq '{name: .metadata.name, annotations: .metadata.annotations}' || true
	kubectl get volumesnapshotclass -o name 2>/dev/null || true
	kubectl api-resources 2>/dev/null | grep -E 'snapshot|volumecapture|manifest|objectkeeper' || true
} >"$(stage_dir "00-preflight")/preflight.txt"
finish_stage "00-preflight" "PASS" "API/RBAC/SC checks" "Cluster ready for demo-e2e" "preflight" "logical"

# --- 01-source-created ---
begin_stage "01-source-created"
kubectl create namespace "${NS}" >/dev/null
kubectl -n "${NS}" create configmap "${CM_NAME}" --from-literal=demo=e2e >/dev/null
kubectl -n "${NS}" apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${PVC_A}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: ${STORAGE_CLASS}
  resources:
    requests:
      storage: 1Gi
EOF
ensure_pvc_bound "${PVC_A}"
PVC_A_UID="$(pvc_uid "${PVC_A}")"
log "OK ${CM_NAME}, ${PVC_A} uid=${PVC_A_UID}"
finish_stage "01-source-created" "PASS" "ns, ConfigMap, PVC A, bind pod" "Sources for manifest + volume capture" "source" "logical"

# --- 02-child-created ---
begin_stage "02-child-created"
kubectl -n "${NS}" apply -f - <<EOF
apiVersion: ${STORAGE_API}
kind: Snapshot
metadata:
  name: ${CHILD_SNAP}
spec: {}
EOF
wait_until "child Snapshot bound" snapshot_bound "${CHILD_SNAP}"
CHILD_CONTENT="$(kubectl -n "${NS}" get "${SNAP_RES}" "${CHILD_SNAP}" -o jsonpath='{.status.boundSnapshotContentName}')"
assert_no_stub_annotation "${CHILD_SNAP}"
log "child content=${CHILD_CONTENT}"
finish_stage "02-child-created" "PASS" "child Snapshot applied and bound" "SnapshotContent exists" "child-created" "lifecycle" "${CHILD_SNAP}"

# --- 03-child-ready ---
begin_stage "03-child-ready"
CHILD_VCR="$(vcr_name_for_content "${CHILD_CONTENT}")"
assert_at_most_one_vcr
if content_has_dataref_uid "${CHILD_CONTENT}" "${PVC_A_UID}"; then
	log "OK child volume already published in dataRefs[] (VCR handoff done)"
else
	if vcr_exists "${CHILD_VCR}"; then
		verify_vcr_targets_if_present "${CHILD_VCR}" "${PVC_A_UID}" "child"
	fi
	wait_volume_capture_published "${CHILD_CONTENT}" "${PVC_A_UID}" "${CHILD_VCR}"
fi
vcr_exists "${CHILD_VCR}" && wait_until "child VCR deleted after publish" vcr_absent "${CHILD_VCR}" || true
CHILD_VSC="$(kubectl get "${CONTENT_RES}" "${CHILD_CONTENT}" -o json | jq -r --arg u "${PVC_A_UID}" \
	'(.status.dataRefs // [])[] | select(.targetUID == $u) | .artifact.name' | head -1)"
[[ -n "${CHILD_VSC}" ]] || { log "ERROR: child dataRefs missing VSC"; exit 1; }
assert_vsc_owned_by_content "${CHILD_VSC}" "${CHILD_CONTENT}"
CHILD_MCP="$(kubectl get "${CONTENT_RES}" "${CHILD_CONTENT}" -o jsonpath='{.status.manifestCheckpointName}')"
[[ -n "${CHILD_MCP}" ]] && kubectl get "${MCP_RES}" "${CHILD_MCP}" >/dev/null
wait_content_ready "${CHILD_CONTENT}" "${CHILD_SNAP}"
wait_snapshot_ready "${CHILD_SNAP}" "${CHILD_CONTENT}"
finish_stage "03-child-ready" "PASS" "VCR publish, VSC ownerRef, MCP, Ready" "Child volume+manifest ready" "child-ready" "logical" "${CHILD_SNAP}"

# --- 04-root-source-delta ---
begin_stage "04-root-source-delta"
kubectl -n "${NS}" apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${PVC_B}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: ${STORAGE_CLASS}
  resources:
    requests:
      storage: 1Gi
EOF
ensure_pvc_bound "${PVC_B}"
PVC_B_UID="$(pvc_uid "${PVC_B}")"
log "OK pvc-b uid=${PVC_B_UID}"
finish_stage "04-root-source-delta" "PASS" "PVC B + bind pod" "Residual volume source ready" "root-delta" "logical"

# --- 05-root-created ---
begin_stage "05-root-created"
kubectl -n "${NS}" apply -f - <<EOF
apiVersion: ${STORAGE_API}
kind: Snapshot
metadata:
  name: ${ROOT_SNAP}
spec: {}
EOF
wait_until "root Snapshot bound" snapshot_bound "${ROOT_SNAP}"
ROOT_CONTENT="$(kubectl -n "${NS}" get "${SNAP_RES}" "${ROOT_SNAP}" -o jsonpath='{.status.boundSnapshotContentName}')"
ROOT_SNAP_UID="$(kubectl -n "${NS}" get "${SNAP_RES}" "${ROOT_SNAP}" -o jsonpath='{.metadata.uid}')"
merge_child_graph_into_root "${ROOT_SNAP}" "${CHILD_SNAP}" "${CHILD_CONTENT}"
assert_no_stub_annotation "${ROOT_SNAP}"
ROOT_VCR="$(vcr_name_for_content "${ROOT_CONTENT}")"
if vcr_exists "${ROOT_VCR}" && vcr_json "${ROOT_VCR}" | jq -r '.spec.targets[]?.uid // empty' | grep -qx "${PVC_A_UID}"; then
	log "WARN: deleting premature root VCR ${ROOT_VCR} (child-covered pvc-a before residual pvc-b)"
	kubectl -n "${NS}" delete "${VCR_RES}" "${ROOT_VCR}" --wait=true
	kick_snapshot "${ROOT_SNAP}"
fi
kick_snapshot "${ROOT_SNAP}"
finish_stage "05-root-created" "PASS" "root Snapshot, subtree merge" "Root content + child refs wired" "root-created" "lifecycle" "${ROOT_SNAP}"

# --- 06-root-ready ---
begin_stage "06-root-ready"
ROOT_VCR="$(vcr_name_for_content "${ROOT_CONTENT}")"
assert_at_most_one_vcr
if content_has_dataref_uid "${ROOT_CONTENT}" "${PVC_B_UID}"; then
	log "OK root residual already in dataRefs[]"
else
	if vcr_exists "${ROOT_VCR}"; then
		verify_vcr_targets_if_present "${ROOT_VCR}" "${PVC_B_UID}" "root"
	fi
	wait_volume_capture_published "${ROOT_CONTENT}" "${PVC_B_UID}" "${ROOT_VCR}"
fi
vcr_exists "${ROOT_VCR}" && wait_until "root VCR deleted after publish" vcr_absent "${ROOT_VCR}" || true
content_has_dataref_uid "${CHILD_CONTENT}" "${PVC_B_UID}" && { log "ERROR: child has pvc-b dataRef"; exit 1; }
content_has_dataref_uid "${ROOT_CONTENT}" "${PVC_A_UID}" && { log "ERROR: root duplicates pvc-a"; exit 1; }
content_has_dataref_uid "${ROOT_CONTENT}" "${PVC_B_UID}" || { log "ERROR: root missing pvc-b dataRef"; exit 1; }
ROOT_VSC="$(kubectl get "${CONTENT_RES}" "${ROOT_CONTENT}" -o json | jq -r --arg u "${PVC_B_UID}" \
	'(.status.dataRefs // [])[] | select(.targetUID == $u) | .artifact.name' | head -1)"
assert_vsc_owned_by_content "${ROOT_VSC}" "${ROOT_CONTENT}"
ROOT_MCP="$(kubectl get "${CONTENT_RES}" "${ROOT_CONTENT}" -o jsonpath='{.status.manifestCheckpointName}')"
kubectl get "${MCP_RES}" "${ROOT_MCP}" >/dev/null
wait_until "root has child content ref" root_has_child_content_ref
wait_content_ready "${ROOT_CONTENT}" "${ROOT_SNAP}"
wait_snapshot_ready "${ROOT_SNAP}" "${ROOT_CONTENT}"
assert_root_objectkeeper_contract
verify_aggregated_present "${ROOT_SNAP}" "$(stage_dir "06-root-ready")/aggregated-before-delete.json"
finish_stage "06-root-ready" "PASS" "residual pvc-b, MCP, aggregated, OK" "Root Ready + read-path" "root-ready" "logical" "${ROOT_SNAP}"

# --- 07-root-deleted-retained ---
begin_stage "07-root-deleted-retained"
kubectl -n "${NS}" delete "${SNAP_RES}" "${ROOT_SNAP}" --wait=true
kubectl -n "${NS}" get "${SNAP_RES}" "${ROOT_SNAP}" >/dev/null 2>&1 && { log "ERROR: root Snapshot still exists"; exit 1; }
kubectl get "${CONTENT_RES}" "${ROOT_CONTENT}" >/dev/null
kubectl get "${MCP_RES}" "${ROOT_MCP}" >/dev/null
verify_aggregated_present "${ROOT_SNAP}" "$(stage_dir "07-root-deleted-retained")/aggregated-after-delete.json"
finish_stage "07-root-deleted-retained" "PASS" "root Snapshot deleted" "Content+MCP retained; temporary aggregated read" "retained" "lifecycle" "" "${ROOT_CONTENT}"

# --- 08-forced-ttl-gc ---
begin_stage "08-forced-ttl-gc"
if [[ "${DEMO_E2E_SKIP_FORCED_TTL:-0}" == "1" || "${HAS_OBJECTKEEPER}" != "1" ]]; then
	log "SKIP forced TTL phase (SKIP_FORCED_TTL or no ObjectKeeper CRD)"
	write_stage_summary "08-forced-ttl-gc" "SKIP" "forced TTL skipped" "N/A"
	save_artifacts "08-forced-ttl-gc"
else
	kubectl get "${OK_RES}" "${OK_NAME}" >/dev/null || { log "ERROR: ${OK_NAME} missing for forced TTL"; exit 1; }
	force_objectkeeper_ttl_expired "${OK_NAME}"
	wait_until_gc "root SnapshotContent ${ROOT_CONTENT} deleted" content_absent
	wait_until_gc "root MCP ${ROOT_MCP} deleted" mcp_absent
	verify_aggregated_absent "${ROOT_SNAP}"
	kubectl get "${OK_RES}" "${OK_NAME}" >/dev/null 2>&1 && log "INFO: ObjectKeeper ${OK_NAME} still present (may be terminal/cleanup pending)" || log "OK ObjectKeeper removed"
	finish_stage "08-forced-ttl-gc" "PASS" "ObjectKeeper spec.ttl=0s" "Retained content/MCP GC; aggregated read fails" "forced-gc" "lifecycle" "" "${ROOT_CONTENT}"
fi

# --- 09-cleanup ---
begin_stage "09-cleanup"
finish_stage "09-cleanup" "PASS" "final capture" "Namespace cleanup via trap unless SKIP_CLEANUP" "cleanup" "logical"

log ""
log "== demo-e2e PASSED run_id=${RUN_ID} namespace=${NS}"
log "Artifacts: ${RUN_ARTIFACT_DIR}"
