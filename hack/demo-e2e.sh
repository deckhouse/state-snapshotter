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
#   DEMO_E2E_WAIT_SEC            default: 900 (hard cap; stall abort is earlier — see DEMO_E2E_STALL_SEC)
#   DEMO_E2E_STALL_SEC           fail if SnapshotContent Ready but Snapshot not (default: 120)
#   DEMO_E2E_MANIFEST_WAIT_SEC   fail if MCP name still empty on content (default: 300)
#   DEMO_E2E_POLL_SEC            default: 5
#   DEMO_E2E_WAIT_LOG_EVERY_SEC  progress log interval during waits (default: 30)
#   DEMO_E2E_GC_WAIT_SEC         max wait after forced ObjectKeeper TTL (default: 120)
#   DEMO_E2E_SKIP_CLEANUP        1 = keep namespace
#   DEMO_E2E_SKIP_FORCED_TTL     1 = skip stage 08-forced-ttl-gc (debug only)
#   DEMO_E2E_SKIP_OK_CONTRACT   1 = skip ObjectKeeper contract checks (no objectkeepers.deckhouse.io)
#   DEMO_E2E_REQUIRE_FORCED_TTL 1 = fail if stage 08 cannot patch ObjectKeeper (else skip with clear reason)
#
# Canonical cluster e2e (replaces legacy separate PR-4/PR-8 smoke scripts; use hack/demo-e2e.sh only).
# Stages: 00-preflight .. 09-cleanup under artifacts/<run-id>/.
#
# ObjectKeeper forced TTL (stage 08) is patched by this script, not the controller. If the caller
# cannot patch objectkeepers.deckhouse.io, stage 08 is SKIP by default. The script may install
# temporary ClusterRole/Bindings demo-e2e-objectkeeper-patcher-<run-id> (removed on exit), or use
# cluster-admin. Production admin RBAC does not grant ObjectKeeper patch.
#   DEMO_E2E_ARTIFACT_DIR        artifact root (alias: DEMO_E2E_ARTIFACTS_ROOT)
#   DEMO_E2E_BIND_IMAGE          bind pod image for WaitForFirstConsumer PVCs
#   DEMO_E2E_CONTROLLER_SA       impersonate for graph wiring (default: state-snapshotter controller SA)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN_ID="$(date +%Y%m%d-%H%M%S)"

STORAGE_CLASS="${DEMO_E2E_STORAGE_CLASS:-local-thin}"
WAIT_SEC="${DEMO_E2E_WAIT_SEC:-900}"
STALL_SEC="${DEMO_E2E_STALL_SEC:-120}"
MANIFEST_WAIT_SEC="${DEMO_E2E_MANIFEST_WAIT_SEC:-300}"
GC_WAIT_SEC="${DEMO_E2E_GC_WAIT_SEC:-120}"
POLL_SEC="${DEMO_E2E_POLL_SEC:-5}"
# Per-wait stall tracking (bash 3.2 compatible; one snap/content pair per wait_snapshot_ready call).
STALL_TRACK_SNAP=""
STALL_TRACK_CONTENT=""
SNAPSHOT_STALL_SINCE=0
MANIFEST_STALL_SINCE=0
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
DEMO_E2E_OK_RBAC_ROLE="demo-e2e-objectkeeper-patcher-${RUN_ID}"
DEMO_E2E_OK_RBAC_BINDING="${DEMO_E2E_OK_RBAC_ROLE}"
DEMO_E2E_OK_RBAC_INSTALLED=0
DEMO_E2E_FORCED_TTL_SKIP_REASON=""
CONTROLLER_SA="${DEMO_E2E_CONTROLLER_SA:-system:serviceaccount:d8-state-snapshotter:controller}"

log() { printf '%s\n' "$*" >&2; }

# Parent/child graph refs (childrenSnapshotRefs, ownerRefs) are controller-owned in production.
kubectl_as_controller() { kubectl --as="${CONTROLLER_SA}" "$@"; }

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

cleanup_demo_e2e_objectkeeper_rbac() {
	[[ "${DEMO_E2E_OK_RBAC_INSTALLED}" == "1" ]] || return 0
	kubectl delete clusterrolebinding "${DEMO_E2E_OK_RBAC_BINDING}" --ignore-not-found=true 2>/dev/null || true
	kubectl delete clusterrole "${DEMO_E2E_OK_RBAC_ROLE}" --ignore-not-found=true 2>/dev/null || true
	log "Removed temporary ObjectKeeper test RBAC (${DEMO_E2E_OK_RBAC_ROLE})"
}

cleanup_on_exit() {
	cleanup_demo_e2e_objectkeeper_rbac
	cleanup_ns
}

trap cleanup_on_exit EXIT

auth_can_yes() {
	[[ "$(kubectl auth can-i "$1" "${2}" 2>/dev/null || echo no)" == "yes" ]]
}

install_demo_e2e_objectkeeper_rbac() {
	local whoami_json subjects err
	err="$(mktemp)"
	if ! whoami_json="$(kubectl auth whoami -o json 2>/dev/null)"; then
		log "WARN: kubectl auth whoami failed; cannot install temporary ObjectKeeper RBAC"
		rm -f "${err}"
		return 1
	fi
	subjects="$(jq -n \
		--arg user "$(jq -r '.status.userInfo.username // empty' <<<"${whoami_json}")" \
		--argjson groups "$(jq -c '.status.userInfo.groups // []' <<<"${whoami_json}")" \
		'($user | select(length > 0) | [{apiGroup: "rbac.authorization.k8s.io", kind: "User", name: .}])
		 + ($groups | map({apiGroup: "rbac.authorization.k8s.io", kind: "Group", name: .}))')"
	if [[ -z "${subjects}" || "${subjects}" == "[]" ]]; then
		log "WARN: no subjects from kubectl auth whoami"
		rm -f "${err}"
		return 1
	fi
	if ! kubectl apply -f - 2>"${err}" <<EOF; then
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: ${DEMO_E2E_OK_RBAC_ROLE}
  labels:
    demo-e2e.state-snapshotter.deckhouse.io/run-id: "${RUN_ID}"
rules:
- apiGroups: [deckhouse.io]
  resources: [objectkeepers]
  verbs: [get, patch, update]
- apiGroups: [deckhouse.io]
  resources: [objectkeepers/status]
  verbs: [get, patch, update]
EOF
		log "WARN: failed to apply temporary ObjectKeeper ClusterRole (need create clusterroles or cluster-admin)"
		cat "${err}" >&2 2>/dev/null || true
		rm -f "${err}"
		return 1
	fi
	if ! kubectl apply -f - 2>"${err}" <<EOF; then
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: ${DEMO_E2E_OK_RBAC_BINDING}
  labels:
    demo-e2e.state-snapshotter.deckhouse.io/run-id: "${RUN_ID}"
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: ${DEMO_E2E_OK_RBAC_ROLE}
subjects: ${subjects}
EOF
		kubectl delete clusterrole "${DEMO_E2E_OK_RBAC_ROLE}" --ignore-not-found=true 2>/dev/null || true
		log "WARN: failed to apply temporary ObjectKeeper ClusterRoleBinding"
		cat "${err}" >&2 2>/dev/null || true
		rm -f "${err}"
		return 1
	fi
	rm -f "${err}"
	return 0
}

ensure_demo_e2e_objectkeeper_access() {
	[[ "${HAS_OBJECTKEEPER}" == "1" ]] || return 0
	if auth_can_yes patch "${OK_RES}"; then
		log "OK ObjectKeeper patch: caller already authorized"
		return 0
	fi
	if install_demo_e2e_objectkeeper_rbac && auth_can_yes patch "${OK_RES}"; then
		DEMO_E2E_OK_RBAC_INSTALLED=1
		log "OK installed temporary demo-e2e ObjectKeeper RBAC (${DEMO_E2E_OK_RBAC_ROLE})"
		return 0
	fi
	if [[ "${DEMO_E2E_REQUIRE_FORCED_TTL:-0}" == "1" ]]; then
		log "ERROR: forced TTL requires objectkeepers.patch (cluster-admin or successful demo-e2e temp RBAC)"
		exit 1
	fi
	DEMO_E2E_FORCED_TTL_SKIP_REASON="current user cannot patch objectkeepers.deckhouse.io"
	DEMO_E2E_SKIP_FORCED_TTL=1
	if ! auth_can_yes get "${OK_RES}"; then
		log "WARN: cannot get objectkeepers; ObjectKeeper contract checks will be skipped"
		DEMO_E2E_SKIP_OK_CONTRACT=1
	fi
}

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

log_forced_ttl_skip() {
	local reason="$1"
	log "08-forced-ttl-gc skipped: ${reason}. Run with cluster-admin or DEMO_E2E_REQUIRE_FORCED_TTL=1 to require this check."
}

# Prints identity, ObjectKeeper patch permission, and planned stage-08 behavior (preflight only).
log_preflight_e2e_plan() {
	local user can_patch stage08
	user="$(kubectl auth whoami -o jsonpath='{.status.userInfo.username}' 2>/dev/null || echo unknown)"
	can_patch="$(kubectl auth can-i patch "${OK_RES}" 2>/dev/null || echo no)"
	log "Preflight user: ${user}"
	log "Preflight can-i patch ${OK_RES}: ${can_patch}"
	if [[ "${DEMO_E2E_SKIP_FORCED_TTL:-0}" == "1" ]]; then
		if [[ -n "${DEMO_E2E_FORCED_TTL_SKIP_REASON}" ]]; then
			stage08="SKIP (${DEMO_E2E_FORCED_TTL_SKIP_REASON})"
		elif [[ "${HAS_OBJECTKEEPER}" != "1" ]]; then
			stage08="SKIP (no ObjectKeeper CRD)"
		else
			stage08="SKIP (DEMO_E2E_SKIP_FORCED_TTL=1 debug)"
		fi
	elif [[ "${DEMO_E2E_REQUIRE_FORCED_TTL:-0}" == "1" ]]; then
		stage08="REQUIRED (fail if cannot patch)"
	elif [[ "${HAS_OBJECTKEEPER}" != "1" ]]; then
		stage08="SKIP (no ObjectKeeper CRD)"
	elif auth_can_yes patch "${OK_RES}"; then
		stage08="RUN (caller can patch objectkeepers)"
	elif [[ "${DEMO_E2E_OK_RBAC_INSTALLED}" == "1" ]]; then
		stage08="RUN (temporary demo-e2e ObjectKeeper RBAC)"
	else
		stage08="SKIP (cannot patch; temp RBAC install failed — escalation policy)"
	fi
	log "Preflight stage 08-forced-ttl-gc plan: ${stage08}"
	PREFLIGHT_USER="${user}"
	PREFLIGHT_OK_PATCH_CAN="${can_patch}"
	PREFLIGHT_STAGE08_PLAN="${stage08}"
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
	local mcr vcr snap_cond content_cond
	log "DIAG Snapshot $1:"
	kubectl -n "${NS}" get "${SNAP_RES}" "$1" -o json 2>/dev/null | jq -c \
		'{conditions: (.status.conditions // []), childrenSnapshotRefs: .status.childrenSnapshotRefs, mcr: .status.manifestCaptureRequestName, vcr: .status.volumeCaptureRequestName}' \
		>&2 || true
	snap_cond="$(snapshot_ready_condition "$1")"
	mcr="$(kubectl -n "${NS}" get "${SNAP_RES}" "$1" -o jsonpath='{.status.manifestCaptureRequestName}' 2>/dev/null || true)"
	if [[ -n "${mcr}" ]]; then
		log "DIAG MCR ${mcr}:"
		kubectl -n "${NS}" get "${MCR_RES}" "${mcr}" -o json 2>/dev/null | jq -c \
			'{conditions: (.status.conditions // []), checkpointName: .status.checkpointName}' >&2 \
			|| log "DIAG MCR ${mcr}: not found"
	fi
	vcr="$(kubectl -n "${NS}" get "${SNAP_RES}" "$1" -o jsonpath='{.status.volumeCaptureRequestName}' 2>/dev/null || true)"
	if [[ -n "${vcr}" ]]; then
		if kubectl -n "${NS}" get "${VCR_RES}" "${vcr}" >/dev/null 2>&1; then
			log "DIAG VCR ${vcr}: exists"
			kubectl -n "${NS}" get "${VCR_RES}" "${vcr}" -o json 2>/dev/null | jq -c '.status.conditions // []' >&2 || true
		else
			log "DIAG VCR ${vcr}: missing (deleted or not created yet)"
		fi
	fi
	log "DIAG SnapshotContent $2:"
	kubectl get "${CONTENT_RES}" "$2" -o json 2>/dev/null | jq -c \
		'{conditions: (.status.conditions // []), dataRefs: .status.dataRefs, children: .status.childrenSnapshotContentRefs, mcp: .status.manifestCheckpointName}' \
		>&2 || true
	content_cond="$(kubectl get "${CONTENT_RES}" "$2" -o json 2>/dev/null | jq -c '([.status.conditions[]? | select(.type == "Ready")][0]) // empty' || true)"
	if content_ready_is_true "$2" && ! snapshot_ready "$1"; then
		log "DIAG mismatch: SnapshotContent Ready=True but Snapshot not Ready (stale mirror / missing enqueue)"
		log "DIAG Snapshot Ready: ${snap_cond:-<none>}"
		log "DIAG SnapshotContent Ready: ${content_cond:-<none>}"
	fi
}

dump_controller_logs_hint() {
	log "HINT: controller logs — kubectl logs -n d8-state-snapshotter deploy/controller --tail=300"
	kubectl logs -n d8-state-snapshotter -l app=controller --tail=60 2>/dev/null | tail -30 >&2 || true
}

snapshot_ready_condition() {
	kubectl -n "${NS}" get "${SNAP_RES}" "$1" -o json 2>/dev/null \
		| jq -c '([.status.conditions[]? | select(.type == "Ready")][0]) // empty'
}

content_ready_is_true() {
	content_ready "$1"
}

snapshot_terminal_ready_failure() {
	local cond reason
	cond="$(snapshot_ready_condition "$1")"
	[[ -n "${cond}" && "${cond}" != "null" ]] || return 1
	reason="$(jq -r '.reason // ""' <<<"${cond}")"
	case "${reason}" in
	ChildSnapshotFailed|CapturePlanDrift|ManifestCheckpointFailed|VolumeCaptureFailed|ListFailed|VolumeCaptureTargetsFailed)
		log "ERROR: Snapshot $1 terminal Ready=False reason=${reason} msg=$(jq -r '.message // ""' <<<"${cond}")"
		return 0
		;;
	esac
	return 1
}

# Abort when progress is obviously stuck (do not burn full DEMO_E2E_WAIT_SEC).
snapshot_ready_stall_abort() {
	local snap="$1" content="$2" cond reason content_mcp
	cond="$(snapshot_ready_condition "${snap}")"
	[[ -n "${cond}" && "${cond}" != "null" ]] || return 1
	reason="$(jq -r '.reason // ""' <<<"${cond}")"

	if content_ready_is_true "${content}" && ! snapshot_ready "${snap}"; then
		if [[ "${STALL_TRACK_SNAP}" != "${snap}" || "${STALL_TRACK_CONTENT}" != "${content}" || "${SNAPSHOT_STALL_SINCE}" -eq 0 ]]; then
			STALL_TRACK_SNAP="${snap}"
			STALL_TRACK_CONTENT="${content}"
			SNAPSHOT_STALL_SINCE=${SECONDS}
		fi
		if (( SECONDS - SNAPSHOT_STALL_SINCE >= STALL_SEC )); then
			log "ERROR: SnapshotContent ${content} Ready=True but Snapshot ${snap} not Ready for ${STALL_SEC}s (likely stale status / controller not reconciling)"
			dump_ready_blockers "${snap}" "${content}"
			dump_controller_logs_hint
			return 0
		fi
	else
		STALL_TRACK_SNAP=""
		STALL_TRACK_CONTENT=""
		SNAPSHOT_STALL_SINCE=0
	fi

	content_mcp="$(kubectl get "${CONTENT_RES}" "${content}" -o jsonpath='{.status.manifestCheckpointName}' 2>/dev/null || true)"
	if [[ "${reason}" == "ManifestCapturePending" && -z "${content_mcp}" ]]; then
		if [[ "${STALL_TRACK_SNAP}" != "${snap}" || "${STALL_TRACK_CONTENT}" != "${content}" || "${MANIFEST_STALL_SINCE}" -eq 0 ]]; then
			STALL_TRACK_SNAP="${snap}"
			STALL_TRACK_CONTENT="${content}"
			MANIFEST_STALL_SINCE=${SECONDS}
		fi
		if (( SECONDS - MANIFEST_STALL_SINCE >= MANIFEST_WAIT_SEC )); then
			log "ERROR: Snapshot ${snap} ManifestCapturePending and SnapshotContent ${content} still has no manifestCheckpointName (${MANIFEST_WAIT_SEC}s)"
			dump_ready_blockers "${snap}" "${content}"
			dump_controller_logs_hint
			return 0
		fi
	else
		MANIFEST_STALL_SINCE=0
	fi
	return 1
}

wait_snapshot_ready() {
	local snap="$1" content="$2"
	local deadline=$((SECONDS + WAIT_SEC)) phase_start=${SECONDS} last_log=${SECONDS}
	local log_every="${DEMO_E2E_WAIT_LOG_EVERY_SEC:-30}"
	STALL_TRACK_SNAP=""
	STALL_TRACK_CONTENT=""
	SNAPSHOT_STALL_SINCE=0
	MANIFEST_STALL_SINCE=0
	while (( SECONDS < deadline )); do
		if snapshot_ready "${snap}"; then
			log "OK Snapshot ${snap} Ready"
			return 0
		fi
		if snapshot_terminal_ready_failure "${snap}"; then
			dump_ready_blockers "${snap}" "${content}"
			dump_controller_logs_hint
			save_artifacts "${CURRENT_STAGE}" 2>/dev/null || true
			return 1
		fi
		if snapshot_ready_stall_abort "${snap}" "${content}"; then
			save_artifacts "${CURRENT_STAGE}" 2>/dev/null || true
			return 1
		fi
		if (( SECONDS - last_log >= log_every )); then
			log "WAIT: Snapshot ${snap} Ready ($((SECONDS - phase_start))s / ${WAIT_SEC}s; stall=${STALL_SEC}s)"
			dump_ready_blockers "${snap}" "${content}"
			last_log=${SECONDS}
		fi
		sleep "${POLL_SEC}"
	done
	log "ERROR: timeout waiting for Snapshot ${snap} Ready (${WAIT_SEC}s) [stage=${CURRENT_STAGE}]"
	dump_ready_blockers "${snap}" "${content}"
	dump_controller_logs_hint
	save_artifacts "${CURRENT_STAGE}" 2>/dev/null || true
	return 1
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
	local root_content root_uid root_cuid
	root_content="$(kubectl -n "${NS}" get "${SNAP_RES}" "${ROOT_SNAP}" -o jsonpath='{.status.boundSnapshotContentName}')"
	root_uid="$(kubectl -n "${NS}" get "${SNAP_RES}" "${ROOT_SNAP}" -o jsonpath='{.metadata.uid}')"
	root_cuid="$(content_uid "${root_content}")"

	kubectl -n "${NS}" patch "${SNAP_RES}" "${CHILD_SNAP}" --type=merge --patch="$(jq -n \
		--arg av "${STORAGE_API}" --arg rn "${ROOT_SNAP}" --arg ru "${root_uid}" \
		'{metadata: {ownerReferences: [{apiVersion: $av, kind: "Snapshot", name: $rn, uid: $ru, controller: true}]}}')" >/dev/null
	kubectl_as_controller -n "${NS}" patch "${SNAP_RES}" "${ROOT_SNAP}" --subresource=status --type=merge --patch="$(jq -n \
		--arg av "${STORAGE_API}" --arg cn "${CHILD_SNAP}" \
		'{status: {childrenSnapshotRefs: [{apiVersion: $av, kind: "Snapshot", name: $cn}]}}')" >/dev/null
	kubectl patch "${CONTENT_RES}" "${CHILD_CONTENT}" --type=merge --patch="$(jq -n \
		--arg av "${STORAGE_API}" --arg rcn "${root_content}" --arg rcu "${root_cuid}" \
		'{metadata: {ownerReferences: [{apiVersion: $av, kind: "SnapshotContent", name: $rcn, uid: $rcu, controller: true}]}}')" >/dev/null
	kubectl patch "${CONTENT_RES}" "${root_content}" --subresource=status --type=merge --patch="$(jq -n \
		--arg ccn "${CHILD_CONTENT}" '{status: {childrenSnapshotContentRefs: [{name: $ccn}]}}')" >/dev/null
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
			--title "${title}" --description "${desc}" 2>"${dir}/graph.err" || {
			log "ERROR: snapshot graph failed for stage ${stage} (see ${dir}/graph.err)"
			exit 1
		}
	elif [[ -n "${graph_snap}" ]]; then
		bash "${SCRIPT_DIR}/snapshot-graph.sh" --namespace "${NS}" --snapshot "${graph_snap}" \
			--output-dir "${graph_dir}" --name "${graph_name}" --mode "${mode}" \
			--title "${title}" --description "${desc}" 2>"${dir}/graph.err" || {
			log "ERROR: snapshot graph failed for stage ${stage} (see ${dir}/graph.err)"
			exit 1
		}
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
	local snap="$1" out="$2" require_cm="${3:-1}"
	# TODO(retained-read-api): temporary snapshot-name route; see script header.
	if ! kubectl get --raw "$(aggregated_path "${snap}")" >"${out}" 2>"${out}.err"; then
		log "ERROR: aggregated GET failed for ${snap} (see ${out}.err)"
		if grep -q Forbidden "${out}.err" 2>/dev/null; then
			log "Hint: grant get snapshots/manifests (subresources.state-snapshotter.deckhouse.io); see templates/rbac-for-us.yaml"
		elif grep -q 'duplicate object detected' "${out}.err" 2>/dev/null; then
			log "Hint: merged child+root subtree returns 409 by contract; use child snapshot or MCP route for single-archive read"
		fi
		cat "${out}.err" >&2 2>/dev/null || true
		exit 1
	fi
	jq -e 'type == "array" and length >= 1' "${out}" >/dev/null
	if [[ "${require_cm}" == "1" ]]; then
		jq -e --arg n "${CM_NAME}" '[.[] | select(.kind == "ConfigMap" and .metadata.name == $n)] | length >= 1' "${out}" >/dev/null
		log "OK aggregated manifests contain ${CM_NAME}"
	else
		log "OK aggregated manifests non-empty for ${snap}"
	fi
}

verify_aggregated_expect_conflict() {
	local snap="$1" out="$2"
	set +e
	kubectl get --raw "$(aggregated_path "${snap}")" >"${out}" 2>"${out}.err"
	local rc=$?
	set -e
	grep -q 'duplicate object detected' "${out}.err" 2>/dev/null || {
		log "ERROR: expected 409 duplicate-object Conflict for ${snap} (rc=${rc})"
		cat "${out}.err" >&2 2>/dev/null || true
		exit 1
	}
	log "OK aggregated read on ${snap} returns 409 (duplicate in child+root MCP tree; normative)"
}

mcp_manifests_path() { printf '/apis/%s/%s/manifestcheckpoints/%s/manifests' "${SUBAPI}" "${SUBVER}" "$1"; }

mcp_manifests_rbac_ok() {
	local probe_err="$1"
	set +e
	kubectl get --raw "$(mcp_manifests_path "__demo_e2e_mcp_rbac_probe__")" >/dev/null 2>"${probe_err}"
	local rc=$?
	set -e
	! grep -q Forbidden "${probe_err}" 2>/dev/null && [[ "${rc}" -ne 0 ]]
}

verify_mcp_manifests_present() {
	local mcp="$1" out="$2"
	if ! kubectl get --raw "$(mcp_manifests_path "${mcp}")" >"${out}" 2>"${out}.err"; then
		if grep -q Forbidden "${out}.err" 2>/dev/null; then
			log "WARN: skip MCP manifests read (need get manifestcheckpoints/manifests on admin-kubeconfig; module RBAC sync)"
			return 0
		fi
		log "ERROR: MCP manifests GET failed for ${mcp}"
		cat "${out}.err" >&2 2>/dev/null || true
		exit 1
	fi
	jq -e --arg n "${CM_NAME}" '[.[] | select(.kind == "ConfigMap" and .metadata.name == $n)] | length >= 1' "${out}" >/dev/null
	log "OK root MCP ${mcp} archive contains ${CM_NAME}"
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
	ensure_demo_e2e_objectkeeper_access
else
	log "WARN: objectkeepers.deckhouse.io CRD missing; ObjectKeeper checks and forced TTL skipped"
fi
log_preflight_e2e_plan
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
MCP_RBAC_PROBE="$(stage_dir "00-preflight")/mcp-rbac-probe.err"
if ! mcp_manifests_rbac_ok "${MCP_RBAC_PROBE}"; then
	log "WARN: cannot GET manifestcheckpoints/manifests (${SUBAPI}); stage 07 MCP archive check will be skipped until module RBAC sync"
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
	echo "ok_rbac_installed=${DEMO_E2E_OK_RBAC_INSTALLED}"
	echo "skip_forced_ttl=${DEMO_E2E_SKIP_FORCED_TTL:-0}"
	echo "require_forced_ttl=${DEMO_E2E_REQUIRE_FORCED_TTL:-0}"
	echo "e2e_user=${PREFLIGHT_USER:-unknown}"
	echo "ok_patch_can=${PREFLIGHT_OK_PATCH_CAN:-unknown}"
	echo "stage08_plan=${PREFLIGHT_STAGE08_PLAN:-unknown}"
	echo "skip_ok_contract=${DEMO_E2E_SKIP_OK_CONTRACT:-0}"
	kubectl get storageclass "${STORAGE_CLASS}" -o json | jq '{name: .metadata.name, annotations: .metadata.annotations}' || true
	kubectl get volumesnapshotclass -o name 2>/dev/null || true
	kubectl api-resources 2>/dev/null | grep -E 'snapshot|volumecapture|manifest|objectkeeper' || true
} >"$(stage_dir "00-preflight")/preflight.txt"
finish_stage "00-preflight" "PASS" "API/RBAC/SC checks" "Cluster ready for demo-e2e" "preflight" "logical"

# --- 01-source-created ---
begin_stage "01-source-created"
kubectl create namespace "${NS}" >/dev/null
# ConfigMap after child Ready (stage 04) so child/root MCP trees do not both archive demo-cm (409 on aggregated read).
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
log "OK ${PVC_A} uid=${PVC_A_UID}"
finish_stage "01-source-created" "PASS" "ns, PVC A, bind pod" "Volume source for child capture" "source" "logical"

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
verify_aggregated_present "${CHILD_SNAP}" "$(stage_dir "03-child-ready")/aggregated-child.json" 0
finish_stage "03-child-ready" "PASS" "VCR publish, VSC ownerRef, MCP, Ready, child aggregated" "Child volume+manifest ready" "child-ready" "logical" "${CHILD_SNAP}"

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
kubectl -n "${NS}" create configmap "${CM_NAME}" --from-literal=demo=e2e >/dev/null
log "OK ${CM_NAME}, pvc-b uid=${PVC_B_UID}"
finish_stage "04-root-source-delta" "PASS" "PVC B, ConfigMap, bind pod" "Residual volume + manifest source for root" "root-delta" "logical"

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
fi
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
verify_aggregated_expect_conflict "${ROOT_SNAP}" "$(stage_dir "06-root-ready")/aggregated-root-conflict.err"
finish_stage "06-root-ready" "PASS" "residual pvc-b, MCP, root aggregated 409, OK" "Root Ready + subtree duplicate contract" "root-ready" "logical" "${ROOT_SNAP}"

# --- 07-root-deleted-retained ---
begin_stage "07-root-deleted-retained"
kubectl -n "${NS}" delete "${SNAP_RES}" "${ROOT_SNAP}" --wait=true
kubectl -n "${NS}" get "${SNAP_RES}" "${ROOT_SNAP}" >/dev/null 2>&1 && { log "ERROR: root Snapshot still exists"; exit 1; }
kubectl get "${CONTENT_RES}" "${ROOT_CONTENT}" >/dev/null
kubectl get "${MCP_RES}" "${ROOT_MCP}" >/dev/null
verify_mcp_manifests_present "${ROOT_MCP}" "$(stage_dir "07-root-deleted-retained")/root-mcp-manifests.json"
verify_aggregated_expect_conflict "${ROOT_SNAP}" "$(stage_dir "07-root-deleted-retained")/aggregated-retained-conflict.err"
finish_stage "07-root-deleted-retained" "PASS" "root Snapshot deleted" "Content+MCP retained; MCP read + retained snapshot route" "retained" "lifecycle" "" "${ROOT_CONTENT}"

# --- 08-forced-ttl-gc ---
begin_stage "08-forced-ttl-gc"
if [[ "${DEMO_E2E_SKIP_FORCED_TTL:-0}" == "1" || "${HAS_OBJECTKEEPER}" != "1" ]]; then
	if [[ "${HAS_OBJECTKEEPER}" != "1" ]]; then
		log_forced_ttl_skip "objectkeepers.deckhouse.io CRD not installed"
	elif [[ -n "${DEMO_E2E_FORCED_TTL_SKIP_REASON}" ]]; then
		log_forced_ttl_skip "${DEMO_E2E_FORCED_TTL_SKIP_REASON}"
	elif [[ "${DEMO_E2E_SKIP_FORCED_TTL:-0}" == "1" ]]; then
		log_forced_ttl_skip "DEMO_E2E_SKIP_FORCED_TTL=1 (debug)"
	else
		log_forced_ttl_skip "forced TTL phase not run"
	fi
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
