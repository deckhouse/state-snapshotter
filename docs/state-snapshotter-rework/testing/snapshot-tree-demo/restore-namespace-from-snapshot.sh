#!/usr/bin/env bash

# Copyright 2026 Flant JSC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Demo helper: restore a whole namespace from a root Snapshot (happy path).
#
# Manifest restore and volume restore are intentionally split:
#   - non-PVC objects are applied from the aggregated manifests of the Snapshot;
#   - PVCs are NOT applied directly. Each PVC that has volume data is restored via
#     VolumeRestoreRequest (VRR), which is the only supported restore path:
#
#       SnapshotContent.status.dataRefs[]  ->  artifact = VolumeSnapshotContent
#         ->  VolumeRestoreRequest (sourceRef.kind=VolumeSnapshotContent)
#         ->  external-provisioner executor  ->  CSI CreateVolume (snapshotHandle)
#         ->  PV  ->  PVC (spec.volumeName)  ->  storage-foundation sets VRR Ready=True
#
#     Because the executor creates the PV first and creates the PVC with
#     spec.volumeName, the restored PVC is statically pre-bound. It can become
#     Bound without any consumer Pod, even for WaitForFirstConsumer StorageClasses.
#
#   - Pods may be present in the archived manifests, but this demo helper excludes
#     them from restore on purpose. The bind Pod is runtime consumer state, not
#     durable application intent; recreate consumers after the restored PVC is
#     Bound if the demo/application needs them.
#
# Entry point is a user-facing Snapshot (not a cluster-scoped SnapshotContent):
# the aggregated manifests endpoint and the data-ref tree are both reachable from it.
#
# NOT in scope (by design): PVC dataSource/dataSourceRef, temporary VolumeSnapshot,
# generic production restore engine, conflicting restore into a non-empty namespace.
#
# Usage:
#   ./restore-namespace-from-snapshot.sh \
#     --source-namespace snapshot-demo-tree \
#     --snapshot demo-tree \
#     --target-namespace snapshot-demo-restored \
#     --storage-class local-thin
#
# Flags:
#   --source-namespace NS   (required) namespace of the source Snapshot
#   --snapshot NAME         (required) source Snapshot name
#   --target-namespace NS   (required) target namespace to restore into
#   --storage-class NAME    fallback StorageClass when an archived PVC has none
#   --timeout SECONDS       per-PVC wait for Bound + VRR Ready (default 180)
#   --allow-existing-empty  allow target namespace to already exist if it is empty
#   --no-pv-fix             do NOT auto-patch restored PV storageClassName
#                           (current provisioner image creates PV without it; the
#                            fix is applied by default so the demo can complete)
#   --keep-tmp              keep the /tmp working dir for debugging
set -euo pipefail

# --- defaults ---------------------------------------------------------------
SOURCE_NS=""; SNAP=""; TARGET_NS=""; STORAGE_CLASS=""
TIMEOUT=180
ALLOW_EXISTING_EMPTY=false
FIX_PV_SC=true
KEEP_TMP=false
AGG="subresources.state-snapshotter.deckhouse.io/v1alpha1"
SNAP_GROUP="storage.deckhouse.io"
VRR_RES="volumerestorerequests.storage.deckhouse.io"
SC_RES="snapshotcontents.storage.deckhouse.io"

die()  { echo "ERROR: $*" >&2; exit 1; }
info() { echo ">> $*" >&2; }

usage() { sed -n '2,40p' "$0" >&2; exit 1; }

# --- args -------------------------------------------------------------------
while [ $# -gt 0 ]; do
  case "$1" in
    --source-namespace) SOURCE_NS="${2:?}"; shift 2;;
    --snapshot)         SNAP="${2:?}"; shift 2;;
    --target-namespace) TARGET_NS="${2:?}"; shift 2;;
    --storage-class)    STORAGE_CLASS="${2:?}"; shift 2;;
    --timeout)          TIMEOUT="${2:?}"; shift 2;;
    --allow-existing-empty) ALLOW_EXISTING_EMPTY=true; shift;;
    --no-pv-fix)        FIX_PV_SC=false; shift;;
    --keep-tmp)         KEEP_TMP=true; shift;;
    -h|--help)          usage;;
    *) die "unknown argument: $1 (use --help)";;
  esac
done
[ -n "$SOURCE_NS" ] || usage
[ -n "$SNAP" ]      || usage
[ -n "$TARGET_NS" ] || usage

# --- 1. preflight -----------------------------------------------------------
info "preflight"
command -v kubectl >/dev/null || die "kubectl not found"
command -v jq      >/dev/null || die "jq not found"

kubectl get "$SNAP_GROUP" >/dev/null 2>&1 || true
kubectl get crd "$VRR_RES" >/dev/null 2>&1 || die "CRD $VRR_RES not found (VRR not installed)"

snap_json=$(kubectl -n "$SOURCE_NS" get "snapshots.$SNAP_GROUP" "$SNAP" -o json 2>/dev/null) \
  || die "source Snapshot $SOURCE_NS/$SNAP not found"
ready=$(echo "$snap_json" | jq -r '.status.conditions[]? | select(.type=="Ready") | .status')
[ "$ready" = "True" ] || die "source Snapshot $SOURCE_NS/$SNAP is not Ready=True (got '${ready:-<none>}')"
ROOT_CONTENT=$(echo "$snap_json" | jq -r '.status.boundSnapshotContentName // ""')
[ -n "$ROOT_CONTENT" ] || die "source Snapshot has empty status.boundSnapshotContentName"
info "source Snapshot Ready=True, root content=$ROOT_CONTENT"

if kubectl get ns "$TARGET_NS" >/dev/null 2>&1; then
  $ALLOW_EXISTING_EMPTY || die "target namespace $TARGET_NS already exists (use --allow-existing-empty)"
  cnt=$(kubectl -n "$TARGET_NS" get all,cm,secret,pvc -o name 2>/dev/null | grep -vE 'configmap/kube-root-ca.crt|secret/default-token|serviceaccount/default' | wc -l | tr -d ' ')
  [ "$cnt" = "0" ] || die "target namespace $TARGET_NS exists and is not empty ($cnt objects)"
  info "target namespace $TARGET_NS exists and is empty"
fi

TMP=$(mktemp -d "/tmp/ns-restore-${SNAP}.XXXXXX")
$KEEP_TMP || trap 'rm -rf "$TMP"' EXIT
AGGFILE="$TMP/aggregated.json"

info "fetching aggregated manifests"
kubectl get --raw \
  "/apis/${AGG}/namespaces/${SOURCE_NS}/snapshots/${SNAP}/manifests" > "$AGGFILE" \
  || die "aggregated manifests endpoint did not respond"
jq -e 'type=="array"' "$AGGFILE" >/dev/null || die "aggregated manifests is not a JSON array"

# --- 2. create target namespace --------------------------------------------
kubectl get ns "$TARGET_NS" >/dev/null 2>&1 || { info "creating namespace $TARGET_NS"; kubectl create namespace "$TARGET_NS" >/dev/null; }

# --- 3+4. prepare and apply non-PVC manifests -------------------------------
# Aggregated API (capture stores raw manifests as-is) strips only metadata.namespace
# and drops cluster-scoped objects; uid/resourceVersion/creationTimestamp/managedFields/
# ownerReferences/status may remain. Clean them here until the API guarantees clean output.
read -r -d '' EXCLUDE_DEF <<'JQ' || true
def is_excluded:
  (.kind == "PersistentVolumeClaim")
  or (.kind == "Pod")
  or (.kind == "Endpoints") or (.kind == "EndpointSlice") or (.kind == "Event")
  or (.kind == "ServiceAccount" and (.metadata.name == "default"))
  or (.kind == "ConfigMap" and (.metadata.name == "kube-root-ca.crt"))
  or (.kind == "Secret" and (((.type // "") | tostring) | test("service-account-token")));
JQ

MANIFESTS="$TMP/restore-manifests.json"
jq "${EXCLUDE_DEF}"'
  { apiVersion: "v1", kind: "List",
    items: [ .[]
      | select(is_excluded | not)
      | .metadata.namespace = $ns
      | del(.metadata.uid, .metadata.resourceVersion, .metadata.generation,
            .metadata.creationTimestamp, .metadata.managedFields,
            .metadata.ownerReferences, .metadata.finalizers, .metadata.selfLink,
            .metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"],
            .status) ] }
' --arg ns "$TARGET_NS" "$AGGFILE" > "$MANIFESTS"

APPLIED_SUMMARY=$(jq -r '[.items[].kind] | group_by(.) | map("\(.[0])=\(length)") | join(", ")' "$MANIFESTS")
EXCLUDED_SUMMARY=$(jq -r "${EXCLUDE_DEF}"' [ .[] | select(is_excluded) | .kind ] | group_by(.) | map("\(.[0])=\(length)") | join(", ")' "$AGGFILE")
info "applying non-PVC manifests ($APPLIED_SUMMARY)"
kubectl apply -f "$MANIFESTS" >&2

# --- 5. walk the SnapshotContent tree, build PVC -> VSC map -----------------
# BFS over status.childrenSnapshotContentRefs starting from the root content.
# No associative arrays (keep bash 3.2 / zsh compatible): use TSV temp files.
PVC_VSC="$TMP/pvc-vsc.tsv"   # original-PVC-name \t VolumeSnapshotContent-name
SEEN="$TMP/seen.txt"
: > "$PVC_VSC"; : > "$SEEN"
QUEUE="$ROOT_CONTENT"
while [ -n "$QUEUE" ]; do
  c="${QUEUE%%$'\n'*}"
  if [ "$c" = "$QUEUE" ]; then QUEUE=""; else QUEUE="${QUEUE#*$'\n'}"; fi
  [ -n "$c" ] || continue
  grep -qxF "$c" "$SEEN" && continue
  echo "$c" >> "$SEEN"
  cjson=$(kubectl get "$SC_RES" "$c" -o json 2>/dev/null) || { info "WARN: content $c not found, skipping"; continue; }
  echo "$cjson" | jq -r '.status.dataRefs[]? | select(.artifact.kind=="VolumeSnapshotContent") | "\(.target.name)\t\(.artifact.name)"' >> "$PVC_VSC"
  children=$(echo "$cjson" | jq -r '.status.childrenSnapshotContentRefs[]?.name')
  [ -n "$children" ] && QUEUE="${QUEUE:+$QUEUE$'\n'}$children"
done
# dedup pvc->vsc keeping first occurrence per PVC
awk -F'\t' '!seen[$1]++' "$PVC_VSC" > "$PVC_VSC.dedup" && mv "$PVC_VSC.dedup" "$PVC_VSC"

# Only restore PVCs that are part of the namespace snapshot (present in manifests).
# storageClassName: from the archived PVC manifest, else --storage-class fallback.
PVC_SC="$TMP/pvc-sc.tsv"     # PVC-name \t storageClassName(from manifest)
jq -r '.[] | select(.kind=="PersistentVolumeClaim") | "\(.metadata.name)\t\(.spec.storageClassName // "")"' "$AGGFILE" > "$PVC_SC"

lookup_vsc() { awk -F'\t' -v k="$1" '$1==k{print $2; exit}' "$PVC_VSC"; }

# --- 6. create one VRR per snapshot'd PVC -----------------------------------
RESULTS="$TMP/results.tsv"; : > "$RESULTS"
RESTORE_COUNT=0
while IFS=$'\t' read -r pvc mfsc; do
  [ -n "$pvc" ] || continue
  vsc=$(lookup_vsc "$pvc")
  if [ -z "$vsc" ]; then
    info "WARN: PVC '$pvc' is in manifests but has no VolumeSnapshotContent in dataRefs — skipping volume restore"
    continue
  fi
  sc="$mfsc"; [ -n "$sc" ] || sc="$STORAGE_CLASS"
  [ -n "$sc" ] || die "PVC '$pvc' has no storageClassName and no --storage-class fallback"
  info "creating VRR restore-$pvc (VSC=$vsc, sc=$sc)"
  cat <<EOF | kubectl apply -f - >&2
apiVersion: storage.deckhouse.io/v1alpha1
kind: VolumeRestoreRequest
metadata:
  name: restore-${pvc}
  namespace: ${TARGET_NS}
spec:
  sourceRef:
    kind: VolumeSnapshotContent
    name: ${vsc}
  targetNamespace: ${TARGET_NS}
  targetPVCName: ${pvc}
  storageClassName: ${sc}
EOF
  echo "$pvc"$'\t'"$vsc"$'\t'"$sc" >> "$RESULTS"
  RESTORE_COUNT=$((RESTORE_COUNT+1))
done < "$PVC_SC"

# --- 7. wait for PVC Bound + VRR Ready --------------------------------------
FINAL="$TMP/final.tsv"; : > "$FINAL"
while IFS=$'\t' read -r pvc vsc sc; do
  [ -n "$pvc" ] || continue
  info "waiting for PVC $pvc (Bound) + VRR restore-$pvc (Ready), timeout=${TIMEOUT}s"
  deadline=$(( $(date +%s) + TIMEOUT ))
  patched=false
  phase=""; vrr=""
  while [ "$(date +%s)" -lt "$deadline" ]; do
    phase=$(kubectl -n "$TARGET_NS" get pvc "$pvc" -o jsonpath='{.status.phase}' 2>/dev/null || true)
    # workaround for current provisioner image: PV created without storageClassName
    if $FIX_PV_SC && [ "$phase" != "Bound" ] && [ "$patched" = false ]; then
      pv=$(kubectl -n "$TARGET_NS" get pvc "$pvc" -o jsonpath='{.spec.volumeName}' 2>/dev/null || true)
      if [ -n "$pv" ]; then
        pvsc=$(kubectl get pv "$pv" -o jsonpath='{.spec.storageClassName}' 2>/dev/null || true)
        if [ -z "$pvsc" ]; then
          info "  PV $pv has empty storageClassName -> patching to $sc (provisioner-image workaround)"
          kubectl patch pv "$pv" --type merge -p '{"spec":{"storageClassName":"'"$sc"'"}}' >&2 || true
          patched=true
        fi
      fi
    fi
    vrr=$(kubectl -n "$TARGET_NS" get "$VRR_RES" "restore-$pvc" -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.status}{end}' 2>/dev/null || true)
    if [ "$phase" = "Bound" ] && [ "$vrr" = "True" ]; then break; fi
    sleep 5
  done
  if [ "$phase" = "Bound" ] && [ "$vrr" = "True" ]; then
    echo "$pvc"$'\t'"OK pvc=Bound vrr=Ready vsc=$vsc" >> "$FINAL"
  else
    echo "$pvc"$'\t'"FAIL pvc=${phase:-<none>} vrr=${vrr:-<none>} vsc=$vsc" >> "$FINAL"
  fi
done < "$RESULTS"

# --- 8. summary -------------------------------------------------------------
echo ""
echo "============================================================"
echo "Namespace restore summary"
echo "  source:  $SOURCE_NS/$SNAP  (content $ROOT_CONTENT)"
echo "  target:  $TARGET_NS"
echo "  applied manifests:  ${APPLIED_SUMMARY:-<none>}"
echo "  excluded (handled via VRR / generated / runtime): ${EXCLUDED_SUMMARY:-<none>}"
echo "  PVC restored via VRR:"
ok=true
if [ "$RESTORE_COUNT" -eq 0 ]; then
  echo "    (none)"
fi
while IFS=$'\t' read -r pvc res; do
  [ -n "$pvc" ] || continue
  echo "    - $pvc: $res"
  case "$res" in FAIL*) ok=false;; esac
done < "$FINAL"
echo "============================================================"
$ok || { echo "RESULT: FAILURE" ; exit 1; }
echo "RESULT: SUCCESS"
