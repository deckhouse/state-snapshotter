#!/usr/bin/env bash
# PR4 smoke test: NamespaceSnapshot aggregated manifests on a real cluster.
#
# Prerequisites: kubectl (configured), jq, curl (optional, for gzip check).
# Does not change controller code or CRDs; creates a throwaway namespace.
#
# Optional environment variables:
#   PR4_SMOKE_NS_SNAP_RESOURCE  kubectl resource plural (default: namespacesnapshots.storage.deckhouse.io)
#   PR4_SMOKE_SKIP_GZIP         if set to 1, skip gzip check (no kubectl proxy / curl)
#   PR4_SMOKE_PROXY_PORT        port for kubectl proxy (default: 18443)
#   PR4_SMOKE_LEGACY_SNAPSHOT   if set, name of an existing Snapshot in the smoke namespace to hit
#                                 GET .../snapshots/<name>/manifests (same NS as smoke test)
#   PR4_SMOKE_SKIP_CLEANUP      if set to 1, do not delete the namespace on exit (debug)
#
# Usage: ./hack/pr4-smoke.sh

set -euo pipefail

SUBAPI="subresources.state-snapshotter.deckhouse.io"
SUBVER="v1alpha1"
NS_SNAP_RES="${PR4_SMOKE_NS_SNAP_RESOURCE:-namespacesnapshots.storage.deckhouse.io}"
WAIT_SEC="${PR4_SMOKE_WAIT_SEC:-300}"
POLL_SEC="${PR4_SMOKE_POLL_SEC:-5}"
PROXY_PORT="${PR4_SMOKE_PROXY_PORT:-18443}"

NS="pr4-smoke-$(date +%s)"
PROXY_PID=""

log() { printf '%s\n' "$*" >&2; }

cleanup() {
	if [[ -n "${PROXY_PID}" ]]; then
		kill "${PROXY_PID}" 2>/dev/null || true
		wait "${PROXY_PID}" 2>/dev/null || true
		PROXY_PID=""
	fi
	if [[ "${PR4_SMOKE_SKIP_CLEANUP:-0}" != "1" ]]; then
		kubectl delete namespace "${NS}" --ignore-not-found=true --wait=false 2>/dev/null || true
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

log "== PR4 smoke: namespace ${NS}"

log "== 1. Discovery: namespacesnapshots/manifests (namespaced)"
DISC="/apis/${SUBAPI}/${SUBVER}"
if ! disc_json="$(kubectl get --raw "${DISC}")"; then
	log "ERROR: kubectl get --raw ${DISC} failed"
	exit 1
fi
if ! echo "${disc_json}" | jq -e --arg n "namespacesnapshots/manifests" \
	'.resources[] | select(.name == $n) | .namespaced == true' >/dev/null; then
	log "ERROR: discovery missing namespaced resource namespacesnapshots/manifests"
	echo "${disc_json}" | jq '.resources[]?.name' >&2 || true
	exit 1
fi
log "OK: discovery lists namespacesnapshots/manifests (namespaced)"

log "== 2. Create namespace and ConfigMap"
kubectl create namespace "${NS}"
kubectl -n "${NS}" create configmap cm1 --from-literal=k=v

log "== 3. Create NamespaceSnapshot snap"
kubectl -n "${NS}" apply -f - <<EOF
apiVersion: storage.deckhouse.io/v1alpha1
kind: NamespaceSnapshot
metadata:
  name: snap
spec: {}
EOF

log "== 4. Wait for bound content + Ready (up to ${WAIT_SEC}s)"
deadline=$((SECONDS + WAIT_SEC))
ok=0
while (( SECONDS < deadline )); do
	if kubectl -n "${NS}" get "${NS_SNAP_RES}" snap -o json 2>/dev/null | jq -e \
		'.status.boundSnapshotContentName != null and (.status.boundSnapshotContentName | length > 0)' \
		>/dev/null 2>&1; then
		if kubectl -n "${NS}" get "${NS_SNAP_RES}" snap -o json 2>/dev/null | jq -e \
			'.status.conditions // [] | map(select(.type == "Ready")) | .[0].status == "True"' \
			>/dev/null 2>&1; then
			ok=1
			break
		fi
	fi
	sleep "${POLL_SEC}"
done
if [[ "${ok}" != "1" ]]; then
	log "ERROR: NamespaceSnapshot snap not Ready or bound in time"
	kubectl -n "${NS}" get "${NS_SNAP_RES}" snap -o yaml >&2 || true
	exit 1
fi
	log "OK: NamespaceSnapshot bound and Ready"

BOUND="$(kubectl -n "${NS}" get "${NS_SNAP_RES}" snap -o json | jq -r '.status.boundSnapshotContentName')"
log "Root NSC: ${BOUND}"

AGG_PATH="/apis/${SUBAPI}/${SUBVER}/namespaces/${NS}/namespacesnapshots/snap/manifests"

log "== 5. Aggregated manifests (kubectl get --raw)"
if ! body="$(kubectl get --raw "${AGG_PATH}")"; then
	log "ERROR: aggregated GET failed (debug dump below)"
	kubectl -n "${NS}" get "${NS_SNAP_RES}" snap -o yaml >&2 || true
	if [[ -n "${BOUND}" && "${BOUND}" != "null" ]]; then
		kubectl get namespacesnapshotcontent "${BOUND}" -o yaml >&2 || true
	fi
	exit 1
fi
if ! echo "${body}" | jq -e 'type == "array"' >/dev/null; then
	log "ERROR: response is not a JSON array"
	echo "${body}" | head -c 500 >&2
	exit 1
fi
COUNT="$(echo "${body}" | jq -r 'length')"
if [[ -z "${COUNT}" || ! "${COUNT}" =~ ^[0-9]+$ || "${COUNT}" -lt 1 ]]; then
	log "ERROR: empty or invalid aggregated result (count=${COUNT})"
	echo "${body}" | jq '.' >&2 || echo "${body}" >&2
	exit 1
fi
log "OK: aggregated object count = ${COUNT}"
if ! echo "${body}" | jq -e '[.[] | select(.kind == "ConfigMap" and .metadata.name == "cm1")] | length >= 1' >/dev/null; then
	log "ERROR: expected ConfigMap cm1 in aggregated array"
	echo "${body}" | jq '.' >&2
	exit 1
fi
log "OK: aggregated JSON array contains ConfigMap cm1"

log "== 6. Gzip (kubectl proxy + curl)"
if [[ "${PR4_SMOKE_SKIP_GZIP:-0}" == "1" ]]; then
	log "SKIP: PR4_SMOKE_SKIP_GZIP=1"
elif ! command -v curl >/dev/null 2>&1; then
	log "SKIP: curl not installed (install curl to test gzip)"
else
	kubectl proxy --port="${PROXY_PORT}" >/tmp/pr4-smoke-kubectl-proxy.log 2>&1 &
	PROXY_PID=$!
	if [[ -n "${PROXY_PID}" ]]; then
		log "kubectl proxy pid=${PROXY_PID} (port=${PROXY_PORT})"
	fi
	sleep 2
	hdrf="$(mktemp)"
	bodyf="$(mktemp)"
	trap 'rm -f "${hdrf}" "${bodyf}"; cleanup' EXIT
	if ! curl -sS -D "${hdrf}" -H "Accept-Encoding: gzip" -o "${bodyf}" \
		"http://127.0.0.1:${PROXY_PORT}${AGG_PATH}"; then
		log "ERROR: curl via kubectl proxy failed"
		exit 1
	fi
	if ! grep -qi '^content-encoding:[[:space:]]*gzip' "${hdrf}"; then
		log "ERROR: expected Content-Encoding: gzip"
		cat "${hdrf}" >&2
		exit 1
	fi
	if ! gunzip -c "${bodyf}" | jq -e 'type == "array"' >/dev/null; then
		log "ERROR: gunzip body is not a JSON array"
		exit 1
	fi
	rm -f "${hdrf}" "${bodyf}"
	trap cleanup EXIT
	kill "${PROXY_PID}" 2>/dev/null || true
	wait "${PROXY_PID}" 2>/dev/null || true
	PROXY_PID=""
	log "OK: gzip response decodes to JSON array"
fi

log "== 7. Negative: missing NamespaceSnapshot -> expect failure + Status JSON"
NEG_NAME="does-not-exist-$RANDOM"
set +e
neg_out="$(kubectl get --raw "/apis/${SUBAPI}/${SUBVER}/namespaces/default/namespacesnapshots/${NEG_NAME}/manifests" 2>&1)"
neg_rc=$?
set -e
if [[ "${neg_rc}" -eq 0 ]]; then
	log "ERROR: expected kubectl failure for missing NamespaceSnapshot"
	exit 1
fi
if echo "${neg_out}" | jq -e '(.kind == "Status" and (.code == 404))' >/dev/null 2>&1; then
	log "OK: negative case returned Status with code 404"
elif echo "${neg_out}" | grep -qiE '404|NotFound'; then
	log "OK: negative case output contains 404 / NotFound"
else
	log "ERROR: expected 404 or NotFound in negative case (neg_rc=${neg_rc})"
	echo "${neg_out}" >&2
	exit 1
fi

log "== 8. Legacy Snapshot /manifests (optional)"
if [[ -z "${PR4_SMOKE_LEGACY_SNAPSHOT:-}" ]]; then
	log "SKIP: set PR4_SMOKE_LEGACY_SNAPSHOT=<name> to verify .../snapshots/<name>/manifests in namespace ${NS}"
else
	LEG_PATH="/apis/${SUBAPI}/${SUBVER}/namespaces/${NS}/snapshots/${PR4_SMOKE_LEGACY_SNAPSHOT}/manifests"
	if ! kubectl get --raw "${LEG_PATH}" | jq -e 'type == "array"' >/dev/null; then
		log "ERROR: legacy snapshots manifests did not return a JSON array"
		exit 1
	fi
	log "OK: legacy snapshot manifests returned JSON array"
fi

log "== PR4 smoke PASSED"
