#!/usr/bin/env bash
# PR4 smoke: retained NamespaceSnapshot lifecycle on namespace default (unified deletion algorithm).
#
# Prerequisites: kubectl, jq; curl optional for gzip.
# Never deletes namespace default.
#
# Controller must expose aggregated subresource. For the TTL phase, the running controller should have:
#   STATE_SNAPSHOTTER_NS_ROOT_OK_TTL=30s
#   STATE_SNAPSHOTTER_ORPHAN_NSC_DELETE_AFTER=35s
# (or similar). If not set, set PR4_SMOKE_SKIP_TTL=1 to skip steps 10–11.
#
# Optional:
#   PR4_SMOKE_NS_SNAP_RESOURCE   default: namespacesnapshots.storage.deckhouse.io
#   PR4_SMOKE_SKIP_GZIP          1 = skip kubectl proxy + curl gzip
#   PR4_SMOKE_PROXY_PORT         default 18443
#   PR4_SMOKE_SKIP_TTL           1 = skip TTL wait and post-TTL checks
#   PR4_SMOKE_LEGACY_SNAPSHOT    name for legacy .../snapshots/<name>/manifests in default
#
# Usage: ./hack/pr4-smoke.sh

set -euo pipefail

SUBAPI="subresources.state-snapshotter.deckhouse.io"
SUBVER="v1alpha1"
NS_SNAP_RES="${PR4_SMOKE_NS_SNAP_RESOURCE:-namespacesnapshots.storage.deckhouse.io}"
WAIT_SEC="${PR4_SMOKE_WAIT_SEC:-300}"
POLL_SEC="${PR4_SMOKE_POLL_SEC:-5}"
PROXY_PORT="${PR4_SMOKE_PROXY_PORT:-18443}"
NS="default"
SNAP_NAME="pr4-smoke"

PROXY_PID=""
TMP=""

log() { printf '%s\n' "$*" >&2; }

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
echo "${disc_json}" | jq -e --arg n "namespacesnapshots/manifests" \
	'.resources[] | select(.name == $n) | .namespaced == true' >/dev/null
log "OK discovery"

log "== 2. ConfigMap cm1"
if ! kubectl -n "${NS}" get configmap cm1 >/dev/null 2>&1; then
	kubectl -n "${NS}" create configmap cm1 --from-literal=k=v
	log "created cm1"
else
	log "cm1 already exists"
fi

log "== 3. Remove stale NamespaceSnapshot if present"
kubectl -n "${NS}" delete "${NS_SNAP_RES}" "${SNAP_NAME}" --ignore-not-found=true --wait=true 2>/dev/null || true
sleep 2

log "== 4. Create NamespaceSnapshot ${SNAP_NAME}"
kubectl -n "${NS}" apply -f - <<EOF
apiVersion: storage.deckhouse.io/v1alpha1
kind: NamespaceSnapshot
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
MCP=$(kubectl get namespacesnapshotcontent.storage.deckhouse.io "${BOUND}" -o jsonpath='{.status.manifestCheckpointName}' 2>/dev/null || true)
log "OK Ready; NSC=${BOUND} MCP=${MCP}"

AGG_PATH="/apis/${SUBAPI}/${SUBVER}/namespaces/${NS}/namespacesnapshots/${SNAP_NAME}/manifests"
TMP=$(mktemp)

log "== 6. Aggregated GET (before root delete)"
kubectl get --raw "${AGG_PATH}" >"${TMP}"
jq -e 'type == "array" and length >= 1' "${TMP}" >/dev/null
jq -e '[.[] | select(.kind == "ConfigMap" and .metadata.name == "cm1")] | length >= 1' "${TMP}" >/dev/null
log "OK aggregated contains ConfigMap cm1"

log "== 7. Delete NamespaceSnapshot"
kubectl -n "${NS}" delete "${NS_SNAP_RES}" "${SNAP_NAME}" --wait=true

log "== 8. Post-delete: snapshot gone, retained tree present"
! kubectl -n "${NS}" get "${NS_SNAP_RES}" "${SNAP_NAME}" >/dev/null 2>&1 || {
	log "ERROR: NamespaceSnapshot still exists"
	exit 1
}
kubectl get namespacesnapshotcontent.storage.deckhouse.io "${BOUND}" >/dev/null
[[ -n "${MCP}" ]] && kubectl get manifestcheckpoints.state-snapshotter.deckhouse.io "${MCP}" >/dev/null
log "OK NSC + MCP present"

log "== 9. Aggregated GET after root delete (retained)"
kubectl get --raw "${AGG_PATH}" >"${TMP}"
jq -e '[.[] | select(.kind == "ConfigMap" and .metadata.name == "cm1")] | length >= 1' "${TMP}" >/dev/null
log "OK retained aggregated"

log "== 10. TTL wait (controller env: NS_ROOT_OK_TTL + ORPHAN_NSC_DELETE_AFTER)"
if [[ "${PR4_SMOKE_SKIP_TTL:-0}" == "1" ]]; then
	log "SKIP: PR4_SMOKE_SKIP_TTL=1 (delete NamespaceSnapshotContent ${BOUND} manually if needed)"
else
	log "sleep 45s (expect NSC/MCP purged if controller has TTL env)..."
	sleep 45
	log "== 11. Post-TTL expectations"
	if kubectl get namespacesnapshotcontent.storage.deckhouse.io "${BOUND}" >/dev/null 2>&1; then
		log "WARN: NSC still exists (check STATE_SNAPSHOTTER_ORPHAN_NSC_DELETE_AFTER on controller)"
	else
		log "OK NamespaceSnapshotContent removed"
	fi
	if [[ -n "${MCP}" ]] && kubectl get manifestcheckpoints.state-snapshotter.deckhouse.io "${MCP}" >/dev/null 2>&1; then
		log "WARN: MCP still exists"
	else
		log "OK ManifestCheckpoint removed (or none)"
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
neg_out=$(kubectl get --raw "/apis/${SUBAPI}/${SUBVER}/namespaces/${NS}/namespacesnapshots/${NEG_NAME}/manifests" 2>&1)
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

log "== 14. Legacy Snapshot manifests (optional)"
if [[ -z "${PR4_SMOKE_LEGACY_SNAPSHOT:-}" ]]; then
	log "SKIP: PR4_SMOKE_LEGACY_SNAPSHOT"
else
	LEG_PATH="/apis/${SUBAPI}/${SUBVER}/namespaces/${NS}/snapshots/${PR4_SMOKE_LEGACY_SNAPSHOT}/manifests"
	kubectl get --raw "${LEG_PATH}" | jq -e 'type == "array"' >/dev/null
	log "OK legacy manifests array"
fi

log "== PR4 smoke PASSED"
