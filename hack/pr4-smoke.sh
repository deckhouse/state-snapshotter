#!/usr/bin/env bash
# PR4 smoke: retained Snapshot lifecycle on namespace default (unified deletion algorithm).
#
# Prerequisites: kubectl, jq; curl optional for gzip.
# Never deletes namespace default.
#
# Controller must expose aggregated subresource. Root ObjectKeeper uses FollowObjectWithTTL on Snapshot;
# spec.ttl comes from STATE_SNAPSHOTTER_SNAPSHOT_ROOT_OK_TTL or STATE_SNAPSHOTTER_NS_ROOT_OK_TTL or built-in default (see pkg/config).
# Physical delete after TTL is driven by Deckhouse ObjectKeeper controller, not this module.
# Optional sleep step checks whether content/MCP were removed by your cluster/ObjectKeeper policy; set PR4_SMOKE_SKIP_TTL=1 to skip.
#
# Mandatory checks (script exits non-zero if failed): discovery, Ready, content/MCP, aggregated manifests,
# post-delete retained snapshot/MCP/aggregated, root ObjectKeeper contract — unless skipped below.
#
# Optional:
#   PR4_SMOKE_NS_SNAP_RESOURCE     default: snapshots.storage.deckhouse.io
#   PR4_SMOKE_SKIP_GZIP            1 = skip kubectl proxy + curl gzip
#   PR4_SMOKE_PROXY_PORT           default 18443
#   PR4_SMOKE_SKIP_TTL             1 = skip TTL wait and post-TTL checks (WARN-only when run)
#   PR4_SMOKE_SKIP_OK_CONTRACT     1 = skip root ObjectKeeper contract (clusters without deckhouse.io ObjectKeeper)
#   PR4_SMOKE_REQUIRE_TTL          1 = after TTL wait, require content (and MCP if set) gone — fail if still present
#   PR4_SMOKE_TTL_LOG_EVERY_SEC    progress log interval during strict TTL wait (default 30)
#   PR4_SMOKE_EXTRA_SNAPSHOT       optional second Snapshot name under default for extra .../snapshots/<name>/manifests probe
#
# Usage: ./hack/pr4-smoke.sh
# Cleanup retained objects after a run: ./hack/pr4-smoke-cleanup.sh

set -euo pipefail

SUBAPI="subresources.state-snapshotter.deckhouse.io"
SUBVER="v1alpha1"
NS_SNAP_RES="${PR4_SMOKE_NS_SNAP_RESOURCE:-snapshots.storage.deckhouse.io}"
WAIT_SEC="${PR4_SMOKE_WAIT_SEC:-300}"
POLL_SEC="${PR4_SMOKE_POLL_SEC:-5}"
PROXY_PORT="${PR4_SMOKE_PROXY_PORT:-18443}"
NS="default"
SNAP_NAME="pr4-smoke"

PROXY_PID=""
TMP=""

log() { printf '%s\n' "$*" >&2; }

# dump_ttl_diagnostics prints YAML and timestamps when strict TTL (PR4_SMOKE_REQUIRE_TTL=1) fails or chain is unclear.
dump_ttl_diagnostics() {
	local bound="${1:-}" ok_name="${2:-}" mcp="${3:-}" agg_path="${4:-}"
	log ""
	log "=== TTL diagnostics: SnapshotContent (name=${bound}) ==="
	if [[ -n "${bound}" ]]; then
		kubectl get snapshotcontent.storage.deckhouse.io "${bound}" -o yaml 2>&1 || log "(get content failed — object may already be gone)"
	else
		log "(no BOUND name)"
	fi
	log ""
	log "=== TTL diagnostics: ObjectKeeper (name=${ok_name}) ==="
	if [[ -n "${ok_name}" ]]; then
		kubectl get objectkeepers.deckhouse.io "${ok_name}" -o yaml 2>&1 || log "(get ObjectKeeper failed — object may be absent)"
	else
		log "(no OK name)"
	fi
	log ""
	if [[ -n "${mcp}" ]]; then
		log "=== TTL diagnostics: ManifestCheckpoint (name=${mcp}) ==="
		kubectl get manifestcheckpoints.state-snapshotter.deckhouse.io "${mcp}" -o yaml 2>&1 || log "(get ManifestCheckpoint failed)"
	fi
	if [[ -n "${agg_path}" ]]; then
		log ""
		log "=== TTL diagnostics: aggregated raw (first 2KiB; may be 404) ==="
		set +e
		kubectl get --raw "${agg_path}" 2>&1 | head -c 2048
		set -e
		log ""
	fi
	log "=== End TTL diagnostics ==="
	log ""
}

cleanup() {
	if [[ -n "${TMP}" ]]; then
		rm -f "${TMP}"
		TMP=""
	fi
	if [[ -n "${PROXY_PID}" ]]; then
		kill "${PROXY_PID}" 2>/dev/null || true
		wait "${PROXY_PID}" 2>/dev/null || true
		PROXY_PID=""
	fi
}

trap cleanup EXIT

need_cmd() {
	command -v "$1" >/dev/null 2>&1 || {
		log "ERROR: missing required command: $1"
		exit 1
	}
}

need_cmd kubectl
need_cmd jq

log "== PR4 smoke: namespace=${NS} snapshot=${SNAP_NAME}"

log "== 1. Discovery"
DISC="/apis/${SUBAPI}/${SUBVER}"
disc_json=$(kubectl get --raw "${DISC}")
echo "${disc_json}" | jq -e --arg n "snapshots/manifests" \
	'.resources[] | select(.name == $n) | .namespaced == true' >/dev/null
log "OK discovery"

log "== 2. ConfigMap cm1"
# Label only when we create it so hack/pr4-smoke-cleanup.sh can remove cm1 without touching a pre-existing cm1.
PR4_CM_LABEL_KEY="state-snapshotter.deckhouse.io/pr4-smoke"
PR4_CM_LABEL_VAL="managed"
if ! kubectl -n "${NS}" get configmap cm1 >/dev/null 2>&1; then
	kubectl -n "${NS}" create configmap cm1 --from-literal=k=v
	kubectl -n "${NS}" label configmap cm1 "${PR4_CM_LABEL_KEY}=${PR4_CM_LABEL_VAL}" --overwrite
	log "created cm1 (labeled for cleanup)"
else
	log "cm1 already exists"
fi

log "== 3. Remove stale Snapshot if present"
kubectl -n "${NS}" delete "${NS_SNAP_RES}" "${SNAP_NAME}" --ignore-not-found=true --wait=true 2>/dev/null || true
sleep 2

log "== 4. Create Snapshot ${SNAP_NAME}"
kubectl -n "${NS}" apply -f - <<EOF
apiVersion: storage.deckhouse.io/v1alpha1
kind: Snapshot
metadata:
  name: ${SNAP_NAME}
spec: {}
EOF

log "== 5. Wait Ready + bound content"
deadline=$((SECONDS + WAIT_SEC))
ok=0
while (( SECONDS < deadline )); do
	if kubectl -n "${NS}" get "${NS_SNAP_RES}" "${SNAP_NAME}" -o json 2>/dev/null | jq -e \
		'.status.boundSnapshotContentName != null and (.status.boundSnapshotContentName | length > 0)' >/dev/null 2>&1; then
		if kubectl -n "${NS}" get "${NS_SNAP_RES}" "${SNAP_NAME}" -o json 2>/dev/null | jq -e \
			'.status.conditions // [] | map(select(.type == "Ready")) | .[0].status == "True"' >/dev/null 2>&1; then
			ok=1
			break
		fi
	fi
	sleep "${POLL_SEC}"
done
if [[ "${ok}" != "1" ]]; then
	log "ERROR: snapshot not Ready in time"
	kubectl -n "${NS}" get "${NS_SNAP_RES}" "${SNAP_NAME}" -o yaml >&2 || true
	exit 1
fi

BOUND=$(kubectl -n "${NS}" get "${NS_SNAP_RES}" "${SNAP_NAME}" -o json | jq -r '.status.boundSnapshotContentName')
MCP=$(kubectl get snapshotcontent.storage.deckhouse.io "${BOUND}" -o jsonpath='{.status.manifestCheckpointName}' 2>/dev/null || true)
log "OK Ready; content=${BOUND} MCP=${MCP}"

SNAP_UID=$(kubectl -n "${NS}" get "${NS_SNAP_RES}" "${SNAP_NAME}" -o json | jq -r '.metadata.uid')
MCR_RES="${PR4_SMOKE_MCR_RESOURCE:-manifestcapturerequests.state-snapshotter.deckhouse.io}"
MCR_NAME="snap-${SNAP_UID}"
log "== 5c. ManifestCaptureRequest absent after capture (temporary request)"
if kubectl -n "${NS}" get "${MCR_RES}" "${MCR_NAME}" >/dev/null 2>&1; then
	log "ERROR: ManifestCaptureRequest ${MCR_NAME} still exists after Ready (expected deleted)"
	exit 1
fi
log "OK no ManifestCaptureRequest ${MCR_NAME}"
OK_NAME="ret-snap-${NS}-${SNAP_NAME}"
log "== 5b. Root ObjectKeeper contract"
if [[ "${PR4_SMOKE_SKIP_OK_CONTRACT:-0}" == "1" ]]; then
	log "SKIP: PR4_SMOKE_SKIP_OK_CONTRACT=1"
else
	ok_json=$(kubectl get objectkeepers.deckhouse.io "${OK_NAME}" -o json) || {
		log "ERROR: root ObjectKeeper ${OK_NAME} not found (required when PR4_SMOKE_SKIP_OK_CONTRACT unset)"
		exit 1
	}
	OK_UID=$(echo "${ok_json}" | jq -r '.metadata.uid')
	if ! echo "${ok_json}" | jq -e --arg suid "${SNAP_UID}" \
		'(.spec.mode == "FollowObjectWithTTL")
			and (.spec.ttl != null)
			and (.spec.followObjectRef.kind == "Snapshot")
			and (.spec.followObjectRef.uid == $suid)
			and ([ .metadata.ownerReferences[]? | select(.kind == "SnapshotContent") ] | length == 0)' >/dev/null; then
		log "ERROR: ObjectKeeper ${OK_NAME} contract mismatch (expect FollowObjectWithTTL on Snapshot, no ownerRef to SnapshotContent)"
		exit 1
	fi
	if ! kubectl -n "${NS}" get "${NS_SNAP_RES}" "${SNAP_NAME}" -o json \
		| jq -e --arg ok "${OK_NAME}" 'all(.metadata.ownerReferences[]?; .kind != "ObjectKeeper" or .name != $ok)' >/dev/null; then
		log "ERROR: Snapshot ${SNAP_NAME} must not be owned by ObjectKeeper ${OK_NAME}"
		exit 1
	fi
	nsc_json=$(kubectl get snapshotcontent.storage.deckhouse.io "${BOUND}" -o json) || {
		log "ERROR: SnapshotContent ${BOUND} not found"
		exit 1
	}
	if ! echo "${nsc_json}" | jq -e --arg on "${OK_NAME}" --arg ouid "${OK_UID}" \
		'[ .metadata.ownerReferences[]? | select(.apiVersion == "deckhouse.io/v1alpha1" and .kind == "ObjectKeeper" and .name == $on and .uid == $ouid and .controller == true) ] | length >= 1' >/dev/null; then
		log "ERROR: SnapshotContent ${BOUND} must have controller ownerRef -> ObjectKeeper ${OK_NAME}"
		exit 1
	fi
	log "OK retained root chain: OK follow Snapshot; content ownerRef -> OK"
fi

AGG_PATH="/apis/${SUBAPI}/${SUBVER}/namespaces/${NS}/snapshots/${SNAP_NAME}/manifests"
TMP=$(mktemp)

log "== 6. Aggregated GET (before root delete)"
kubectl get --raw "${AGG_PATH}" >"${TMP}"
jq -e 'type == "array" and length >= 1' "${TMP}" >/dev/null
jq -e '[.[] | select(.kind == "ConfigMap" and .metadata.name == "cm1")] | length >= 1' "${TMP}" >/dev/null
log "OK aggregated contains ConfigMap cm1"

log "== 7. Delete Snapshot"
kubectl -n "${NS}" delete "${NS_SNAP_RES}" "${SNAP_NAME}" --wait=true

log "== 8. Post-delete: snapshot gone, retained tree present"
! kubectl -n "${NS}" get "${NS_SNAP_RES}" "${SNAP_NAME}" >/dev/null 2>&1 || {
	log "ERROR: Snapshot still exists"
	exit 1
}
kubectl get snapshotcontent.storage.deckhouse.io "${BOUND}" >/dev/null
[[ -n "${MCP}" ]] && kubectl get manifestcheckpoints.state-snapshotter.deckhouse.io "${MCP}" >/dev/null
if kubectl -n "${NS}" get "${MCR_RES}" "${MCR_NAME}" >/dev/null 2>&1; then
	log "ERROR: ManifestCaptureRequest ${MCR_NAME} present after root delete (unexpected)"
	exit 1
fi
log "OK content + MCP present; no MCR"

log "== 9. Aggregated GET after root delete (retained)"
kubectl get --raw "${AGG_PATH}" >"${TMP}"
jq -e '[.[] | select(.kind == "ConfigMap" and .metadata.name == "cm1")] | length >= 1' "${TMP}" >/dev/null
log "OK retained aggregated"

log "== 10. Optional wait (Deckhouse ObjectKeeper TTL / GC; observational unless PR4_SMOKE_REQUIRE_TTL=1)"
if [[ "${PR4_SMOKE_SKIP_TTL:-0}" == "1" ]]; then
	log "SKIP: PR4_SMOKE_SKIP_TTL=1 (no wait; use real-cluster TTL smoke separately if needed)"
elif [[ "${PR4_SMOKE_REQUIRE_TTL:-0}" == "1" ]]; then
	TTL_LOG_EVERY="${PR4_SMOKE_TTL_LOG_EVERY_SEC:-30}"
	log "PR4_SMOKE_REQUIRE_TTL=1: waiting up to ${WAIT_SEC}s for content ${BOUND} to disappear (poll ${POLL_SEC}s; progress log every ${TTL_LOG_EVERY}s)..."
	log "Hint: controller always creates root OK with FollowObjectWithTTL; if content never disappears, check ObjectKeeper controller and whether WAIT_SEC exceeds spec.ttl (override via STATE_SNAPSHOTTER_SNAPSHOT_ROOT_OK_TTL for shorter test TTL)."
	deadline=$((SECONDS + WAIT_SEC))
	ttl_ok=0
	ttl_phase_start=${SECONDS}
	last_log=${SECONDS}
	while (( SECONDS < deadline )); do
		if ! kubectl get snapshotcontent.storage.deckhouse.io "${BOUND}" >/dev/null 2>&1; then
			ttl_ok=1
			log "OK: SnapshotContent ${BOUND} disappeared after $((SECONDS - ttl_phase_start))s (strict TTL wait)"
			break
		fi
		if (( SECONDS - last_log >= TTL_LOG_EVERY )); then
			log "TTL wait: content ${BOUND} still present, elapsed $((SECONDS - ttl_phase_start))s / limit ${WAIT_SEC}s"
			kubectl get snapshotcontent.storage.deckhouse.io "${BOUND}" -o jsonpath='{.metadata.name} rv={.metadata.resourceVersion} deletionTimestamp={.metadata.deletionTimestamp}{"\n"}' 2>/dev/null || true
			last_log=${SECONDS}
		fi
		sleep "${POLL_SEC}"
	done
	if [[ "${ttl_ok}" != "1" ]]; then
		log "ERROR: SnapshotContent ${BOUND} still exists after ${WAIT_SEC}s — check ObjectKeeper TTL / module config / OK controller reconcile."
		dump_ttl_diagnostics "${BOUND}" "${OK_NAME}" "${MCP}" "${AGG_PATH}"
		exit 1
	fi
	log "OK SnapshotContent removed (strict TTL phase)"
	if [[ -n "${MCP}" ]] && kubectl get manifestcheckpoints.state-snapshotter.deckhouse.io "${MCP}" >/dev/null 2>&1; then
		log "ERROR: ManifestCheckpoint ${MCP} still exists after content gone (PR4_SMOKE_REQUIRE_TTL=1) — expect GC after ownerRef broken"
		dump_ttl_diagnostics "" "${OK_NAME}" "${MCP}" "${AGG_PATH}"
		exit 1
	fi
	log "OK ManifestCheckpoint absent after TTL phase"
	log "== 10c. Strict TTL: aggregated subresource should fail (snapshot gone; no retained read)"
	set +e
	agg_after_ttl=$(kubectl get --raw "${AGG_PATH}" 2>&1)
	agg_rc=$?
	set -e
	if [[ "${agg_rc}" -eq 0 ]]; then
		log "ERROR: aggregated GET still succeeded after strict TTL (expected 404 / failure — retained path should stop)"
		log "${agg_after_ttl}" | head -c 4096 >&2 || true
		dump_ttl_diagnostics "" "${OK_NAME}" "" "${AGG_PATH}"
		exit 1
	fi
	log "OK aggregated path no longer readable (rc=${agg_rc})"
else
	log "sleep 45s (content/MCP may remain until ObjectKeeper TTL; WARN-only below)"
	sleep 45
	log "== 11. Post-wait (informational)"
	if kubectl get snapshotcontent.storage.deckhouse.io "${BOUND}" >/dev/null 2>&1; then
		log "INFO: content still exists (normal without TTL or slow ObjectKeeper reconcile)"
	else
		log "INFO: SnapshotContent removed"
	fi
	if [[ -n "${MCP}" ]] && kubectl get manifestcheckpoints.state-snapshotter.deckhouse.io "${MCP}" >/dev/null 2>&1; then
		log "INFO: MCP still exists"
	else
		log "INFO: ManifestCheckpoint removed or absent"
	fi
fi

log "== 12. Gzip (optional)"
if [[ "${PR4_SMOKE_SKIP_GZIP:-0}" == "1" ]]; then
	log "SKIP: PR4_SMOKE_SKIP_GZIP=1"
elif ! command -v curl >/dev/null 2>&1; then
	log "SKIP: no curl"
else
	kubectl proxy --port="${PROXY_PORT}" >/tmp/pr4-smoke-kubectl-proxy.log 2>&1 &
	PROXY_PID=$!
	sleep 2
	hdrf=$(mktemp)
	bodyf=$(mktemp)
	curl -sS -D "${hdrf}" -H "Accept-Encoding: gzip" -o "${bodyf}" \
		"http://127.0.0.1:${PROXY_PORT}${AGG_PATH}" || true
	if grep -qi '^content-encoding:[[:space:]]*gzip' "${hdrf}"; then
		gunzip -c "${bodyf}" | jq -e 'type == "array"' >/dev/null
		log "OK gzip"
	else
		log "SKIP or WARN: no gzip (aggregated path may 404 after TTL)"
	fi
	rm -f "${hdrf}" "${bodyf}"
	kill "${PROXY_PID}" 2>/dev/null || true
	wait "${PROXY_PID}" 2>/dev/null || true
	PROXY_PID=""
fi

log "== 13. Negative 404"
NEG_NAME="does-not-exist-$RANDOM"
set +e
neg_out=$(kubectl get --raw "/apis/${SUBAPI}/${SUBVER}/namespaces/${NS}/snapshots/${NEG_NAME}/manifests" 2>&1)
neg_rc=$?
set -e
if [[ "${neg_rc}" -eq 0 ]]; then
	log "ERROR: expected failure for missing snapshot"
	exit 1
fi
if echo "${neg_out}" | jq -e '(.kind == "Status" and (.code == 404))' >/dev/null 2>&1; then
	log "OK negative 404"
elif echo "${neg_out}" | grep -qiE '404|NotFound'; then
	log "OK negative NotFound"
else
	log "WARN: unexpected negative output (rc=${neg_rc})"
fi

log "== 14. Extra snapshot manifests (optional)"
if [[ -z "${PR4_SMOKE_EXTRA_SNAPSHOT:-}" ]]; then
	log "SKIP: PR4_SMOKE_EXTRA_SNAPSHOT"
else
	EXTRA_PATH="/apis/${SUBAPI}/${SUBVER}/namespaces/${NS}/snapshots/${PR4_SMOKE_EXTRA_SNAPSHOT}/manifests"
	kubectl get --raw "${EXTRA_PATH}" | jq -e 'type == "array"' >/dev/null
	log "OK extra snapshot manifests array"
fi

log "== PR4 smoke PASSED"
