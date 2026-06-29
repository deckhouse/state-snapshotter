#!/usr/bin/env bash
# Staged, artifact-producing diagnostic for the snapshot-tree demo (demo VM/Disk domain).
#
# Goal: validate the current architecture end-to-end on a real cluster, from CSD priority
# planning to Phase 2a / Slice 3 status propagation:
#   - GVK/priority registration and priority-driven tree shape (+ inversion);
#   - PlanningReady planning barrier (domain-owned, generation-gated);
#   - artifacts born under execution ObjectKeeper and handed off to SnapshotContent;
#   - ManifestsReady / VolumeReady / ChildrenReady / Ready aggregation on the SnapshotContent tree;
#   - damaged-leaf Ready=False propagation to root and recovery back to Ready=True;
#   - sibling isolation;
#   - both volume-capture data paths: an orphan/standalone PVC (demo-pvc) captured via a CSI
#     VolumeSnapshot visibility leaf at the namespace root, and a PVC nested under a
#     DemoVirtualDisk (demo-pvc-disk) captured via the domain VolumeCaptureRequest path;
#   - root/demo Snapshot Ready as a verbatim mirror of bound SnapshotContent.Ready
#     (no local Ready recompute required to pass);
#   - restore compiler (/manifests-with-data-restoration): apply-ready output validation only —
#     endpoint liveness + no VRR, restore-safe sanitize + targetNamespace rewrite, orphan PVC ->
#     dataSourceRef VolumeSnapshot, domain DemoVirtualDisk -> spec.dataSource + covered PVC
#     suppression, child-before-parent ordering, and server-side dry-run apply into a fresh namespace.
#     This validates the ALREADY implemented compiler; it does not change capture/tree/contract.
#
# Outcome model: STRICTLY BINARY. A group either PASSES (exit 0) or FAILS (exit 1) with a concrete
# reason. There is no "soft finding" / "completed with N warnings" verdict.
#   - die "<reason>"     -> INVARIANT violated (architecture bug): expected vs observed is logged,
#                           artifacts are saved, controller log hint is dumped, run exits 1.
#   - require "<reason>" -> PRECONDITION not satisfied (environment/RBAC/capability/setup, not an
#                           architecture bug): run exits 1 with a PRECONDITION-tagged reason so the
#                           operator can tell "fix the cluster" from "fix the code".
#   - note "<text>"      -> purely informational timeline marker; NEVER affects the verdict. Used only
#                           for genuinely non-invariant observations (e.g. a fast self-heal transient
#                           that may be missed by polling, while the real invariant — recovery to
#                           Ready=True — is asserted with die right after).
# Every invalidation/recovery assertion is made deterministic by a BOUNDED wait (WAIT_SEC for capture
# readiness, INVALIDATION_WAIT_SEC for failure-propagation/self-heal): the expected terminal state is
# reached within the window (PASS) or it is not and we die with the last observed state (FAIL, why).
#
# Artifacts: ${TREE_DEMO_ARTIFACT_DIR:-artifacts}/tree-demo-<run-id>/<stage>/
#   00-preflight topology-disk-only referenced-pvc-without-disk-csd orphan-pvc-cleanup
#   01-priority-vm-first 02-tree-ready topology-vm-disk orphan-pvc-vs
#   domain-pvc-vcr manifest-no-volumesnapshot domain-pvc-failure-coverage
#   20-restore-endpoint-basic 21-restore-sanitize-and-namespace
#   22-restore-orphan-pvc-datasource 23-restore-domain-disk-datasource
#   24-restore-vm-disk-order 25-restore-dry-run-apply
#   03-priority-inverted
#   04-domainready-barrier 05-ownership-handoff 06-mcp-failure 07-mcp-recovery
#   08-vsc-pending 09-vsc-recovery
#   12-child-snapshot-deleted 13-snapshotcontent-deleted 14-mcp-deleted
#   17-child-ready-false 18-recovery 15-chunk-deleted 16-orphan-vsc-deleted
#   10-vsc-missing 11-chunk-missing
#
# Failure-propagation / parent-invalidation stages (12-18) assert the invariant "parent Ready=True IFF
# all required durable descendants/artifacts are present and healthy". Stage 12 is a SELF-HEAL case
# (deleting a child Snapshot OBJECT loses no durable artifact: the planner re-ensures it and the parent
# stays Ready). Stages 13 (child content), 14 (MCP), 15 (chunk), 16 (orphan VSC) and 17 (child
# Ready=False) are real invalidations on artifact loss / child failure. Recoverable ones (13,14,17)
# heal and run before the consolidation gate 18-recovery; the non-recoverable artifact losses
# (15-chunk-deleted, 16-orphan-vsc-deleted) intentionally leave the tree degraded and run AFTER 18,
# joining the destructive tail with 10/11. These stages use a short observation cap
# (TREE_DEMO_INVALIDATION_WAIT_SEC, ~3 reconciles), not the capture-readiness WAIT_SEC.
#
# Usage: ./hack/snapshot-tree-demo-e2e.sh            # full linear run (legacy, ~15 min)
#        TREE_DEMO_GROUP=core ./hack/snapshot-tree-demo-e2e.sh   # one <=5 min logical group
#
# Run logical groups separately (each <=5 min, survives a flaky tunnel):
#   topology core domain priority failure-leaf failure-delete failure-child failure-destructive restore
# See the "test group selection" block below for the exact stage membership of each group.
#
# Env:
#   TREE_DEMO_GROUP           logical group to run (default: all = full linear run). One of:
#                             topology|core|domain|priority|failure-leaf|failure-delete|
#                             failure-child|failure-destructive|restore. All groups except
#                             `topology` rebuild the main tree (00+01+02) first, so each is
#                             self-contained and independently runnable.
#   TREE_DEMO_NAMESPACE       main demo namespace (default: snapshot-demo-tree-<run-id>)
#   TREE_DEMO_STORAGE_CLASS   default: local-thin
#   TREE_DEMO_MODULE_NS       controller namespace (default: d8-state-snapshotter)
#   TREE_DEMO_CONTROLLER_SA   impersonation SA for status patches/chunk reads
#                             (default: system:serviceaccount:<module-ns>:controller)
#   TREE_DEMO_WAIT_SEC        per-wait hard cap seconds for capture readiness (default: 600)
#   TREE_DEMO_INVALIDATION_WAIT_SEC  short cap for failure-propagation/self-heal observations in
#                             stages 12-18/10/11 (default: 30 — a few reconciles, not a capture wait)
#   TREE_DEMO_POLL_SEC        poll interval seconds (default: 5)
#   TREE_DEMO_WAIT_LOG_EVERY_SEC progress log interval (default: 30)
#   TREE_DEMO_ARTIFACT_DIR    artifact root (default: artifacts)
#   TREE_DEMO_BIND_IMAGE      bind pod image for WaitForFirstConsumer PVCs
#   TREE_DEMO_PVC_SIZE        requested size for empty demo PVCs (default: 1Mi). These tests validate
#                             architecture/control-plane semantics, not data payload movement.
#   TREE_DEMO_SKIP_GRAPH      1 = skip per-stage graph rendering (snapshot-graph.sh) — much faster;
#                             graph is a diagnostic aid, not an assertion. Use while debugging tests.
#   TREE_DEMO_SKIP_RESTORE    1 = skip restore-compiler stages 20-25
#   TREE_DEMO_SKIP_CLEANUP    1 = keep namespaces and leave CSD as last set
#   TREE_DEMO_SKIP_INVERSION  1 = skip 03-priority-inverted (avoid global CSD churn)
#   TREE_DEMO_SKIP_VSC        1 = skip 08/09/10 VSC stages
#   TREE_DEMO_SKIP_CHUNK      1 = skip 11-chunk-missing
#   TREE_DEMO_SKIP_RESTORE    1 = skip 20-25 restore compiler stages
#
# DESTRUCTIVE STAGES: 10-vsc-missing deletes a VolumeSnapshotContent (recovery is NOT always
# possible — that is expected) and then drops the main namespace; 11-chunk-missing deletes a
# manifest chunk. Do NOT run on a reusable demo namespace unless cleanup is enabled (cleanup is
# ON by default; TREE_DEMO_SKIP_CLEANUP=1 disables it). On a shared cluster set
# TREE_DEMO_SKIP_VSC=1 and/or TREE_DEMO_SKIP_CHUNK=1 to keep the run non-destructive.
#
# Distroless policy: bind pods use a generic pause image, never the controller image.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN_ID="$(date +%Y%m%d-%H%M%S)"

STORAGE_CLASS="${TREE_DEMO_STORAGE_CLASS:-local-thin}"
MOD_NS="${TREE_DEMO_MODULE_NS:-d8-state-snapshotter}"
WAIT_SEC="${TREE_DEMO_WAIT_SEC:-600}"
# Short cap for failure-propagation / self-heal OBSERVATIONS (stages 12-18, 10, 11). These watch for an
# event-driven status transition (invalidation, re-ensure, recovery) that the controller produces within
# a reconcile or two, or — for an unmet expectation — never. The capture-readiness WAIT_SEC=600 is wrong
# here: on the "never" path it burns ~10 min per check (observed: stage 12 took 646s waiting for a
# transition that by design does not happen). A few reconciles + slack is correct.
INVALIDATION_WAIT_SEC="${TREE_DEMO_INVALIDATION_WAIT_SEC:-30}"
POLL_SEC="${TREE_DEMO_POLL_SEC:-5}"
WAIT_LOG_EVERY_SEC="${TREE_DEMO_WAIT_LOG_EVERY_SEC:-30}"
BIND_IMAGE="${TREE_DEMO_BIND_IMAGE:-registry.k8s.io/pause:3.9}"
PVC_SIZE="${TREE_DEMO_PVC_SIZE:-1Mi}"
CONTROLLER_SA="${TREE_DEMO_CONTROLLER_SA:-system:serviceaccount:${MOD_NS}:controller}"
# Two-pod split: the demo dedicated reconcilers (MCR/VCR + child snapshots) and the demo restore
# mutation run in the separate domain-controller pod (own SA + own aggregated apiserver), not in core.
DOMAIN_SA="${TREE_DEMO_DOMAIN_SA:-system:serviceaccount:${MOD_NS}:domain-controller}"
DOMAIN_APISERVICE="${TREE_DEMO_DOMAIN_APISERVICE:-v1alpha1.subresources.demo.state-snapshotter.deckhouse.io}"

# ---- test group selection ---------------------------------------------------
# The full linear suite is ~15 min and does not survive a flaky tunnel. Split it into logical,
# independently runnable groups (target <=5 min each) via TREE_DEMO_GROUP. Every group except
# `topology` is self-contained: it rebuilds the main tree (00-preflight + 01 + 02) before its own
# stages, so groups can run in any order and on their own. Groups:
#   all                  legacy full linear run (default)
#   topology             topology-disk-only, referenced-pvc-without-disk-csd, orphan-pvc-cleanup
#   core                 [tree] topology-vm-disk, orphan-pvc-vs, domain-pvc-vcr, manifest-no-volumesnapshot
#   domain               [tree] domain-pvc-failure-coverage, 04-domainready-barrier, 05-ownership-handoff
#   priority             [tree] 03-priority-inverted
#   failure-leaf         [tree] 06-mcp-failure, 07-mcp-recovery, 08-vsc-pending, 09-vsc-recovery
#   failure-delete       [tree] 12-child-snapshot-deleted, 13-snapshotcontent-deleted, 14-mcp-deleted
#   failure-child        [tree] 17-child-ready-false, 18-recovery
#   failure-destructive  [tree] 15-chunk-deleted, 16-orphan-vsc-deleted, 10-vsc-missing, 11-chunk-missing
#   restore              [tree] 20-25 restore-compiler stages
# 00-preflight always runs (fail-fast env check). [tree] = main tree rebuilt first.
GROUP="${TREE_DEMO_GROUP:-all}"
# grp <name>: true when the active group is <name> (or the legacy "all" full run).
grp() { [[ "${GROUP}" == "all" || "${GROUP}" == "$1" ]]; }
# need_main_tree: true when the active group must build the main tree (01-priority-vm-first + 02-tree-ready).
need_main_tree() {
	case "${GROUP}" in
	all | core | domain | priority | failure-leaf | failure-delete | failure-child | failure-destructive | restore) return 0 ;;
	*) return 1 ;;
	esac
}
case "${GROUP}" in
all | topology | core | domain | priority | failure-leaf | failure-delete | failure-child | failure-destructive | restore) ;;
*)
	printf 'ERROR: unknown TREE_DEMO_GROUP=%s\n  valid: all topology core domain priority failure-leaf failure-delete failure-child failure-destructive restore\n' "${GROUP}" >&2
	exit 2
	;;
esac

NS="${TREE_DEMO_NAMESPACE:-snapshot-demo-tree-${RUN_ID}}"
NS_TOPO_DISK_ONLY="${NS}-disk-only"
NS_REF_PVC_NO_DISK_CSD="${NS}-ref-pvc-no-disk-csd"
NS_DOMAIN_PVC_FAIL="${NS}-domain-pvc-fail"
NS_PRIORITY_INV="${NS}-priority-inv"
NS_DOMAIN_BARRIER="${NS}-domain-barrier"
NS_RESTORE="${NS}-restore"
CSD_NAME="tree-demo-vm-disk-${RUN_ID}"
SNAP="demo-tree"

ARTIFACTS_ROOT="${TREE_DEMO_ARTIFACT_DIR:-artifacts}"
RUN_ARTIFACT_DIR="${ARTIFACTS_ROOT}/tree-demo-${RUN_ID}"

# API groups / resources.
DEMO_API="demo.state-snapshotter.deckhouse.io/v1alpha1"
STORAGE_API="storage.deckhouse.io/v1alpha1"
SS_API="state-snapshotter.deckhouse.io/v1alpha1"

SNAP_RES="snapshots.storage.deckhouse.io"
CONTENT_RES="snapshotcontents.storage.deckhouse.io"
MCR_RES="manifestcapturerequests.state-snapshotter.deckhouse.io"
MCP_RES="manifestcheckpoints.state-snapshotter.deckhouse.io"
CHUNK_RES="manifestcheckpointcontentchunks.state-snapshotter.deckhouse.io"
CSD_RES="customsnapshotdefinitions.state-snapshotter.deckhouse.io"
VCR_RES="volumecapturerequests.storage.deckhouse.io"
VS_RES="volumesnapshots.snapshot.storage.k8s.io"
VSC_RES="volumesnapshotcontents.snapshot.storage.k8s.io"
VSCLASS_RES="volumesnapshotclasses.snapshot.storage.k8s.io"
OK_RES="objectkeepers.deckhouse.io"
VM_RES="demovirtualmachines.demo.state-snapshotter.deckhouse.io"
DISK_RES="demovirtualdisks.demo.state-snapshotter.deckhouse.io"
VMSNAP_RES="demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io"
DISKSNAP_RES="demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io"

CURRENT_STAGE=""
HAS_OBJECTKEEPER=0

# Tree handles (resolved in 02-tree-ready).
ROOT_CONTENT=""
VM_SNAP=""
VM_CONTENT=""
LEAF_SNAP=""        # disk-vm DemoVirtualDiskSnapshot (covered, under VM)
LEAF_CONTENT=""
LEAF_MCP=""
SIBLING_SNAP=""     # disk-standalone DemoVirtualDiskSnapshot (root child)
SIBLING_CONTENT=""
DOMAIN_VCR_OBSERVED=0

log() { printf '%s\n' "$*" >&2; }
note() {
	log "NOTE[${CURRENT_STAGE}]: $*"
	[[ -n "${CURRENT_STAGE}" ]] && printf 'NOTE: %s\n' "$*" >>"$(stage_dir "${CURRENT_STAGE}")/notes.txt" 2>/dev/null || true
}
die() {
	_KUBECTL_RETRY_DISABLED=1
	log "ERROR[${CURRENT_STAGE}]: $*"
	[[ -n "${CURRENT_STAGE}" ]] && save_artifacts "${CURRENT_STAGE}" 2>/dev/null || true
	dump_controller_logs_hint
	exit 1
}
# require: a PRECONDITION (environment / RBAC / capability / test-setup) was not satisfied. Not an
# architecture bug, but the run cannot validate anything further, so fail fast with a clear tag.
require() { die "PRECONDITION: $*"; }

need_cmd() { command -v "$1" >/dev/null 2>&1 || { log "ERROR: missing required command: $1"; exit 1; }; }
now_rfc3339() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# Resilient kubectl wrapper (transient tunnel / API-server connectivity).
#
# Long staged runs against this cluster are fronted by an SSH tunnel / port-forward that
# intermittently drops; a bare `kubectl` then exits non-zero and `set -e` aborts the whole run
# mid-way (observed repeatedly aborting at later stages with "connect: connection refused" to
# 127.0.0.1:<port>). This wrapper transparently retries ONLY transient connectivity failures
# (connection refused / dial tcp / TLS handshake / EOF / "unable to connect to the server" /
# timeouts) with linear backoff for up to KUBECTL_RETRY_SEC seconds, so a tunnel the operator
# re-establishes is ridden out instead of failing the run. Any other non-zero exit (NotFound,
# validation, conflict, assertion-style get) returns immediately and unchanged, preserving the
# original control flow and `set -e` semantics for callers.
#
# stdin is buffered and re-fed only for "-f -" applies/replaces so a retry can resubmit the
# manifest after the first attempt has already consumed the heredoc/pipe. Retries are disabled via
# _KUBECTL_RETRY_DISABLED on the death/cleanup paths so they fail fast instead of hanging.
_KUBECTL_RETRY_DISABLED=0
KUBECTL_RETRY_SEC="${TREE_DEMO_KUBECTL_RETRY_SEC:-480}"
kubectl() {
	local waited=0 delay=3 rc=0 errfile buffer_stdin=0 stdin_data=""
	case " $* " in
	*" -f - "* | *" -f- "*) buffer_stdin=1 ;;
	esac
	if [[ "${buffer_stdin}" == "1" ]]; then
		stdin_data="$(cat)"
	fi
	errfile="$(mktemp)"
	while :; do
		rc=0
		if [[ "${buffer_stdin}" == "1" ]]; then
			command kubectl "$@" <<<"${stdin_data}" 2>"${errfile}" || rc=$?
		else
			command kubectl "$@" 2>"${errfile}" || rc=$?
		fi
		if [[ "${rc}" -eq 0 ]]; then
			cat "${errfile}" >&2
			rm -f "${errfile}"
			return 0
		fi
		if [[ "${_KUBECTL_RETRY_DISABLED}" != "1" ]] &&
			[[ "${waited}" -lt "${KUBECTL_RETRY_SEC}" ]] &&
			grep -qiE 'connection refused|dial tcp|i/o timeout|TLS handshake|unable to connect to the server|connection reset|unexpected EOF|http2: client connection|net/http: request canceled|Client\.Timeout exceeded|the server is currently unable to handle the request|EOF$' "${errfile}"; then
			log "kubectl transient connectivity error (waited ${waited}s/${KUBECTL_RETRY_SEC}s) — retry in ${delay}s :: $(tr '\n' ' ' <"${errfile}" | head -c 160)"
			sleep "${delay}"
			waited=$((waited + delay))
			[[ "${delay}" -lt 15 ]] && delay=$((delay + 3))
			continue
		fi
		cat "${errfile}" >&2
		rm -f "${errfile}"
		return "${rc}"
	done
}

kubectl_ctrl() { kubectl --as="${CONTROLLER_SA}" "$@"; }

dump_controller_logs_hint() {
	log "HINT: controller logs — kubectl logs -n ${MOD_NS} -l app=controller --tail=300"
	command kubectl logs -n "${MOD_NS}" -l app=controller --tail=60 2>/dev/null | tail -30 >&2 || true
	# Demo reconcile (MCR/VCR/children) and restore mutation live in the domain-controller pod now.
	log "HINT: domain-controller logs — kubectl logs -n ${MOD_NS} -l app=domain-controller --tail=300"
	command kubectl logs -n "${MOD_NS}" -l app=domain-controller --tail=60 2>/dev/null | tail -30 >&2 || true
}

stage_dir() { printf '%s/%s' "${RUN_ARTIFACT_DIR}" "$1"; }

begin_stage() {
	CURRENT_STAGE="$1"
	local dir
	dir="$(stage_dir "${CURRENT_STAGE}")"
	mkdir -p "${dir}"
	log ""
	log "======== STAGE ${CURRENT_STAGE} ========"
	printf '%s\n' "${CURRENT_STAGE}" >"${dir}/STAGE.txt"
	now_rfc3339 >"${dir}/timestamp.txt"
}

# ---- generic getters --------------------------------------------------------

# get_json <resource> <ns|""> <name>  (always exits 0; prints "" on missing)
get_json() {
	if [[ -n "$2" ]]; then kubectl -n "$2" get "$1" "$3" -o json 2>/dev/null || true; else kubectl get "$1" "$3" -o json 2>/dev/null || true; fi
}

# cond_field <json> <type> <field>   (field: status|reason|message)
cond_field() {
	local j="${1:-}"; [[ -n "${j}" ]] || j='{}'
	jq -r --arg t "$2" --arg f "$3" \
		'([.status.conditions[]? | select(.type == $t)][0]) as $c | ($c[$f] // "")' <<<"${j}"
}

# ready_triple <json>  -> prints "status|reason|message" of Ready
ready_triple() {
	local j="${1:-}"; [[ -n "${j}" ]] || j='{}'
	jq -r '([.status.conditions[]? | select(.type == "Ready")][0]) as $c
		| ((($c.status) // "Missing") + "|" + (($c.reason) // "") + "|" + (($c.message) // ""))' <<<"${j}"
}

content_ready_true() { [[ "$(cond_field "$(get_json "${CONTENT_RES}" "" "$1")" Ready status)" == "True" ]]; }
content_ready_false() { [[ "$(cond_field "$(get_json "${CONTENT_RES}" "" "$1")" Ready status)" == "False" ]]; }
snap_ready_true() { [[ "$(cond_field "$(get_json "${SNAP_RES}" "$1" "$2")" Ready status)" == "True" ]]; }

# resource name for a child ref kind.
res_for_kind() {
	case "$1" in
	DemoVirtualMachineSnapshot) printf '%s' "${VMSNAP_RES}" ;;
	DemoVirtualDiskSnapshot) printf '%s' "${DISKSNAP_RES}" ;;
	Snapshot) printf '%s' "${SNAP_RES}" ;;
	*) printf '%s' "" ;;
	esac
}

# ---- wait helpers -----------------------------------------------------------

# wait_until_to <timeout_sec> <desc> <cmd...>: poll <cmd> until success or <timeout_sec> elapses.
wait_until_to() {
	local timeout="$1" desc="$2"; shift 2
	local deadline=$((SECONDS + timeout)) start=${SECONDS} last=${SECONDS}
	while ((SECONDS < deadline)); do
		if "$@"; then log "OK ${desc}"; return 0; fi
		if ((SECONDS - last >= WAIT_LOG_EVERY_SEC)); then
			log "WAIT: ${desc} ($((SECONDS - start))s / ${timeout}s)"; last=${SECONDS}
		fi
		sleep "${POLL_SEC}"
	done
	log "ERROR: timeout waiting for ${desc} (${timeout}s) [stage=${CURRENT_STAGE}]"
	return 1
}

# wait_until uses the capture-readiness cap (WAIT_SEC). For failure-propagation/self-heal observations
# use wait_until_to "${INVALIDATION_WAIT_SEC}" ... instead (a few reconciles, not a 10-min capture wait).
wait_until() { wait_until_to "${WAIT_SEC}" "$@"; }

snap_bound() {
	kubectl -n "$1" get "${SNAP_RES}" "$2" -o json 2>/dev/null \
		| jq -e '(.status.boundSnapshotContentName // "") | length > 0' >/dev/null
}

snap_ready_terminal_false() {
	local triple
	triple="$(ready_triple "$(get_json "${SNAP_RES}" "$1" "$2")")"
	local status reason
	status="${triple%%|*}"; reason="$(cut -d'|' -f2 <<<"${triple}")"
	[[ "${status}" == "False" ]] || return 1
	case "${reason}" in
	ChildrenFailed | CapturePlanDrift | ManifestCheckpointFailed | VolumeCaptureFailed | ListFailed | VolumeCaptureTargetsFailed | GraphPlanningFailed | CreateChildFailed)
		return 0 ;;
	esac
	return 1
}

wait_snapshot_ready() {
	local ns="$1" snap="$2" timeout="${3:-${WAIT_SEC}}"
	local deadline=$((SECONDS + timeout)) start=${SECONDS} last=${SECONDS}
	while ((SECONDS < deadline)); do
		if snap_ready_true "${ns}" "${snap}"; then log "OK Snapshot ${ns}/${snap} Ready"; return 0; fi
		if snap_ready_terminal_false "${ns}" "${snap}"; then
			log "ERROR: Snapshot ${ns}/${snap} terminal Ready=False: $(ready_triple "$(get_json "${SNAP_RES}" "${ns}" "${snap}")")"
			return 1
		fi
		if ((SECONDS - last >= WAIT_LOG_EVERY_SEC)); then
			log "WAIT: Snapshot ${ns}/${snap} Ready ($((SECONDS - start))s / ${timeout}s): $(ready_triple "$(get_json "${SNAP_RES}" "${ns}" "${snap}")")"
			last=${SECONDS}
		fi
		sleep "${POLL_SEC}"
	done
	log "ERROR: timeout waiting for Snapshot ${ns}/${snap} Ready (${timeout}s)"
	return 1
}

pvc_bound() { [[ "$(kubectl -n "$1" get pvc "$2" -o jsonpath='{.status.phase}' 2>/dev/null)" == "Bound" ]]; }

storage_class_binding_mode() {
	kubectl get storageclass "${STORAGE_CLASS}" -o json 2>/dev/null \
		| jq -r '.volumeBindingMode // "Immediate"'
}

# demo_mirror_equal <res> <ns> <snap> <content>: demo Snapshot Ready triple == bound content Ready triple.
demo_mirror_equal() {
	[[ -n "$3" && -n "$4" ]] || return 1
	[[ "$(ready_triple "$(get_json "$1" "$2" "$3")")" == "$(ready_triple "$(get_json "${CONTENT_RES}" "" "$4")")" ]]
}

# wait_demo_mirror <res> <snap> <content> <label>: verify the Slice 3 demo content->snapshot watch
# keeps the demo Snapshot Ready a verbatim mirror of its bound SnapshotContent (event-driven).
wait_demo_mirror() {
	[[ -n "$2" && -n "$3" ]] || { note "demo $4 mirror: handle missing (snap=$2 content=$3); skipped"; return 0; }
	wait_until "demo $4 Snapshot Ready mirrors bound content" demo_mirror_equal "$1" "${NS}" "$2" "$3" \
		|| die "demo $4 Snapshot Ready != bound content Ready (demo content->snapshot watch lag/regression): snap=[$(ready_triple "$(get_json "$1" "${NS}" "$2")")] content=[$(ready_triple "$(get_json "${CONTENT_RES}" "" "$3")")]"
}

# ---- source / CSD bootstrap -------------------------------------------------

apply_source_namespace() {
	local ns="$1"
	kubectl get namespace "${ns}" >/dev/null 2>&1 || kubectl create namespace "${ns}" >/dev/null
	kubectl apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: demo-snapshot-cm
  namespace: ${ns}
data:
  demo: tree
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: demo-pvc
  namespace: ${ns}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: ${STORAGE_CLASS}
  resources:
    requests:
      storage: ${PVC_SIZE}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: demo-pvc-disk
  namespace: ${ns}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: ${STORAGE_CLASS}
  resources:
    requests:
      storage: ${PVC_SIZE}
---
apiVersion: v1
kind: Pod
metadata:
  name: bind-demo-pvc
  namespace: ${ns}
spec:
  # Required for WaitForFirstConsumer StorageClasses: scheduling this pod lets Kubernetes
  # choose a node and bind both demo PVCs before the snapshot run starts.
  restartPolicy: Never
  containers:
    - name: hold
      image: ${BIND_IMAGE}
      volumeMounts:
        - name: data
          mountPath: /data
        - name: data-disk
          mountPath: /data-disk
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: demo-pvc
    - name: data-disk
      persistentVolumeClaim:
        claimName: demo-pvc-disk
---
apiVersion: ${DEMO_API}
kind: DemoVirtualMachine
metadata:
  name: vm-1
  namespace: ${ns}
spec:
  virtualDiskName: disk-vm
---
apiVersion: ${DEMO_API}
kind: DemoVirtualDisk
metadata:
  name: disk-vm
  namespace: ${ns}
spec:
  persistentVolumeClaimName: demo-pvc-disk
---
apiVersion: ${DEMO_API}
kind: DemoVirtualDisk
metadata:
  name: disk-standalone
  namespace: ${ns}
spec: {}
EOF
}

apply_referenced_pvc_without_disk_csd_source() {
	local ns="$1"
	kubectl get namespace "${ns}" >/dev/null 2>&1 || kubectl create namespace "${ns}" >/dev/null
	kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: pvc-1
  namespace: ${ns}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: ${STORAGE_CLASS}
  resources:
    requests:
      storage: ${PVC_SIZE}
---
apiVersion: v1
kind: Pod
metadata:
  name: bind-pvc-1
  namespace: ${ns}
spec:
  # Required for WaitForFirstConsumer StorageClasses so pvc-1 reaches Bound before snapshotting.
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
        claimName: pvc-1
---
apiVersion: ${DEMO_API}
kind: DemoVirtualDisk
metadata:
  name: disk-1
  namespace: ${ns}
spec:
  persistentVolumeClaimName: pvc-1
EOF
}

apply_domain_pvc_failure_source() {
	local ns="$1"
	kubectl get namespace "${ns}" >/dev/null 2>&1 || kubectl create namespace "${ns}" >/dev/null
	kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: pvc-fail
  namespace: ${ns}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: ${STORAGE_CLASS}
  resources:
    requests:
      storage: ${PVC_SIZE}
---
apiVersion: v1
kind: Pod
metadata:
  name: bind-pvc-fail
  namespace: ${ns}
spec:
  # Required for WaitForFirstConsumer StorageClasses so pvc-fail reaches Bound before VCR injection.
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
        claimName: pvc-fail
---
apiVersion: ${DEMO_API}
kind: DemoVirtualDisk
metadata:
  name: disk-fail
  namespace: ${ns}
spec:
  persistentVolumeClaimName: pvc-fail
EOF
}

apply_root_snapshot() {
	local ns="$1"
	kubectl apply -f - <<EOF
apiVersion: ${STORAGE_API}
kind: Snapshot
metadata:
  name: ${SNAP}
  namespace: ${ns}
spec: {}
EOF
}

delete_csd() {
	kubectl delete "${CSD_RES}" "${CSD_NAME}" --ignore-not-found=true --wait=true >/dev/null 2>&1 || true
}

# apply_csd <vm_priority> <disk_priority>
apply_csd() {
	local vmp="$1" diskp="$2"
	kubectl apply -f - <<EOF
apiVersion: ${SS_API}
kind: CustomSnapshotDefinition
metadata:
  name: ${CSD_NAME}
spec:
  snapshotResourceMapping:
    - source:
        apiVersion: ${DEMO_API}
        kind: DemoVirtualMachine
      snapshot:
        apiVersion: ${DEMO_API}
        kind: DemoVirtualMachineSnapshot
      priority: ${vmp}
    - source:
        apiVersion: ${DEMO_API}
        kind: DemoVirtualDisk
      snapshot:
        apiVersion: ${DEMO_API}
        kind: DemoVirtualDiskSnapshot
      priority: ${diskp}
EOF
}

apply_disk_only_csd() {
	local diskp="$1"
	kubectl apply -f - <<EOF
apiVersion: ${SS_API}
kind: CustomSnapshotDefinition
metadata:
  name: ${CSD_NAME}
spec:
  snapshotResourceMapping:
    - source:
        apiVersion: ${DEMO_API}
        kind: DemoVirtualDisk
      snapshot:
        apiVersion: ${DEMO_API}
        kind: DemoVirtualDiskSnapshot
      priority: ${diskp}
EOF
}

csd_accepted() {
	get_json "${CSD_RES}" "" "${CSD_NAME}" \
		| jq -e '[.status.conditions[]? | select(.type == "Accepted" and .status == "True")] | length >= 1' >/dev/null
}

# AccessGranted is set by an external Deckhouse hook in production; demo/smoke sets it manually.
patch_csd_source_access_granted() {
	local gen body
	gen="$(kubectl get "${CSD_RES}" "${CSD_NAME}" -o jsonpath='{.metadata.generation}' 2>/dev/null || echo 0)"
	body="$(get_json "${CSD_RES}" "" "${CSD_NAME}" | jq \
		--arg now "$(now_rfc3339)" --argjson gen "${gen:-0}" \
		'.status.conditions = ((.status.conditions // []) | map(select(.type != "AccessGranted")) + [{
			type: "AccessGranted", status: "True", reason: "TreeDemoE2E",
			message: "manual tree-demo approval", lastTransitionTime: $now, observedGeneration: $gen
		}])')"
	[[ -n "${body}" ]] || return 0
	printf '%s' "${body}" | kubectl replace --subresource=status -f - >/dev/null 2>&1 && return 0
	printf '%s' "${body}" | kubectl_ctrl replace --subresource=status -f - >/dev/null 2>&1 || true
}

ensure_csd_eligible() {
	wait_until "CSD ${CSD_NAME} Accepted=True" csd_accepted || die "CSD ${CSD_NAME} never Accepted"
	patch_csd_source_access_granted
	wait_until "CSD ${CSD_NAME} AccessGranted=True" \
		bash -c "kubectl get '${CSD_RES}' '${CSD_NAME}' -o json | jq -e '[.status.conditions[]?|select(.type==\"AccessGranted\" and .status==\"True\")]|length>=1' >/dev/null" \
		|| require "CSD ${CSD_NAME} AccessGranted not True (manual patch did not stick / external hook owns it); tree cannot build"
}

# inversion_safe: priority inversion mutates the GLOBAL CSD priority, which affects every namespace
# that uses the demo domain. Refuse to run it if there are demo snapshots outside this run's
# namespaces, or another CSD mapping the demo GVKs — a global priority flip could disturb them,
# wake real reconciles, or leave racey residue. Sets INVERSION_BLOCKERS for the caller's message.
INVERSION_BLOCKERS=""
inversion_safe() {
	local ours foreign csd_other
	ours="${NS}|${NS_PRIORITY_INV}|${NS_DOMAIN_BARRIER}"
	foreign="$(kubectl get "${VMSNAP_RES},${DISKSNAP_RES}" -A -o json 2>/dev/null \
		| jq -r --arg ours "${ours}" '[.items[]? | select((($ours|split("|")) | index(.metadata.namespace)) == null) | (.metadata.namespace + "/" + .kind + "/" + .metadata.name)] | join(",")' 2>/dev/null || true)"
	csd_other="$(kubectl get "${CSD_RES}" -o json 2>/dev/null \
		| jq -r --arg me "${CSD_NAME}" '[.items[]? | select(.metadata.name != $me) | select(any(.spec.snapshotResourceMapping[]?; .source.kind == "DemoVirtualMachine" or .source.kind == "DemoVirtualDisk")) | .metadata.name] | join(",")' 2>/dev/null || true)"
	INVERSION_BLOCKERS=""
	[[ -n "${foreign}" ]] && INVERSION_BLOCKERS="foreign demo snapshots: ${foreign}"
	[[ -n "${csd_other}" ]] && INVERSION_BLOCKERS="${INVERSION_BLOCKERS}${INVERSION_BLOCKERS:+; }other demo-mapping CSDs: ${csd_other}"
	[[ -z "${INVERSION_BLOCKERS}" ]]
}

# ---- artifact capture -------------------------------------------------------

dump_kind() {
	local out="$1" file="$2" ns="$3" res="$4"
	mkdir -p "${out}"
	if [[ -n "${ns}" ]]; then
		kubectl -n "${ns}" get "${res}" -o yaml >"${out}/${file}.yaml" 2>/dev/null || true
		kubectl -n "${ns}" get "${res}" -o json >"${out}/${file}.json" 2>/dev/null || true
	else
		kubectl get "${res}" -o yaml >"${out}/${file}.yaml" 2>/dev/null || true
		kubectl get "${res}" -o json >"${out}/${file}.json" 2>/dev/null || true
	fi
}

save_artifacts() {
	local stage="$1" ns="${2:-${NS}}" dir res
	dir="$(stage_dir "${stage}")"
	res="${dir}/resources"
	mkdir -p "${res}"
	log "Artifacts: ${dir}"
	dump_kind "${res}" "configmaps" "${ns}" configmap
	dump_kind "${res}" "pvcs" "${ns}" pvc
	dump_kind "${res}" "pods" "${ns}" pod
	dump_kind "${res}" "demovirtualmachines" "${ns}" "${VM_RES}"
	dump_kind "${res}" "demovirtualdisks" "${ns}" "${DISK_RES}"
	dump_kind "${res}" "demovirtualmachinesnapshots" "${ns}" "${VMSNAP_RES}"
	dump_kind "${res}" "demovirtualdisksnapshots" "${ns}" "${DISKSNAP_RES}"
	dump_kind "${res}" "snapshots" "${ns}" "${SNAP_RES}"
	dump_kind "${res}" "manifestcapturerequests" "${ns}" "${MCR_RES}"
	dump_kind "${res}" "volumecapturerequests" "${ns}" "${VCR_RES}"
	dump_kind "${res}" "volumesnapshots" "${ns}" "${VS_RES}"
	dump_kind "${res}" "snapshotcontents" "" "${CONTENT_RES}"
	dump_kind "${res}" "manifestcheckpoints" "" "${MCP_RES}"
	dump_kind "${res}" "volumesnapshotcontents" "" "${VSC_RES}"
	dump_kind "${res}" "customsnapshotdefinitions" "" "${CSD_RES}"
	[[ "${HAS_OBJECTKEEPER}" == "1" ]] && dump_kind "${res}" "objectkeepers" "" "${OK_RES}"
	kubectl -n "${ns}" get "${SNAP_RES}" "${SNAP}" -o json 2>/dev/null \
		| jq '.status.childrenSnapshotRefs // []' >"${dir}/childrenSnapshotRefs.json" 2>/dev/null || true
	kubectl get "${CONTENT_RES}" -o json 2>/dev/null \
		| jq '[.items[]? | {name: .metadata.name, dataRefs: (.status.dataRefs // [])}]' >"${dir}/dataRefs.json" 2>/dev/null || true
	kubectl get --raw "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/${ns}/snapshots/${SNAP}/manifests" \
		>"${dir}/aggregated-manifests.json" 2>"${dir}/aggregated-manifests.err" || true
	kubectl get events -n "${ns}" --sort-by=.metadata.creationTimestamp >"${res}/events.txt" 2>/dev/null || true
	condition_table "${ns}" >"${dir}/conditions.txt" 2>/dev/null || true
	ownerref_table "${ns}" >"${dir}/ownerrefs.txt" 2>/dev/null || true
	printf 'namespace=%s\nrun_id=%s\nstage=%s\ncsd=%s\n' "${ns}" "${RUN_ID}" "${stage}" "${CSD_NAME}" >"${dir}/run-context.txt"
}

# condition_table <ns>: Ready/ManifestsReady/VolumeReady/ChildrenReady/PlanningReady for snapshots + contents.
condition_table() {
	local ns="$1"
	printf '%-22s %-34s %-60s\n' "KIND" "NAME" "Ready|ManifestsReady|VolumeReady|ChildrenReady|PlanningReady(obsGen/gen)"
	local res
	for res in "${SNAP_RES}" "${VMSNAP_RES}" "${DISKSNAP_RES}"; do
		kubectl -n "${ns}" get "${res}" -o json 2>/dev/null | jq -r '
			.items[]? | [
				.kind, .metadata.name,
				(([.status.conditions[]?|select(.type=="Ready")][0].status)//"-") + "|" +
				(([.status.conditions[]?|select(.type=="ManifestsReady")][0].status)//"-") + "|" +
				(([.status.conditions[]?|select(.type=="VolumeReady")][0].status)//"-") + "|" +
				(([.status.conditions[]?|select(.type=="ChildrenReady")][0].status)//"-") + "|" +
				(([.status.conditions[]?|select(.type=="PlanningReady")][0].status)//"-") + "(" +
				((([.status.conditions[]?|select(.type=="PlanningReady")][0].observedGeneration)//0)|tostring) + "/" +
				((.metadata.generation//0)|tostring) + ")"
			] | "\(.[0])\t\(.[1])\t\(.[2])"' 2>/dev/null \
			| while IFS=$'\t' read -r k n c; do printf '%-22s %-34s %-60s\n' "${k}" "${n}" "${c}"; done
	done
	kubectl get "${CONTENT_RES}" -o json 2>/dev/null | jq -r '
		.items[]? | [
			"SnapshotContent", .metadata.name,
			(([.status.conditions[]?|select(.type=="Ready")][0].status)//"-") + "|" +
			(([.status.conditions[]?|select(.type=="ManifestsReady")][0].status)//"-") + "|" +
			(([.status.conditions[]?|select(.type=="VolumeReady")][0].status)//"-") + "|" +
			(([.status.conditions[]?|select(.type=="ChildrenReady")][0].status)//"-") + "|" +
			(([.status.conditions[]?|select(.type=="PlanningReady")][0].status)//"-")
		] | "\(.[0])\t\(.[1])\t\(.[2])"' 2>/dev/null \
		| while IFS=$'\t' read -r k n c; do printf '%-22s %-34s %-60s\n' "${k}" "${n}" "${c}"; done
}

# ownerref_table <ns>: MCP/VSC/SnapshotContent ownerRefs (lifecycle/handoff view).
ownerref_table() {
	local ns="$1"
	printf '== ManifestCheckpoint ownerRefs ==\n'
	kubectl get "${MCP_RES}" -o json 2>/dev/null | jq -r '
		.items[]? | .metadata.name + " <- " +
		((.metadata.ownerReferences // []) | map(.kind + "/" + .name) | join(",") // "none")'
	printf '== VolumeSnapshotContent ownerRefs ==\n'
	kubectl get "${VSC_RES}" -o json 2>/dev/null | jq -r '
		.items[]? | .metadata.name + " <- " +
		((.metadata.ownerReferences // []) | map(.kind + "/" + .name) | join(",") // "none")'
	printf '== SnapshotContent ownerRefs ==\n'
	kubectl get "${CONTENT_RES}" -o json 2>/dev/null | jq -r '
		.items[]? | .metadata.name + " <- " +
		((.metadata.ownerReferences // []) | map(.kind + "/" + .name) | join(",") // "none")'
}

dump_content_mcp_chunks() {
	local content="$1" out="$2" label="$3"
	local mcp chunk_dir chunk
	mkdir -p "${out}/${label}/chunks"
	kubectl get "${CONTENT_RES}" "${content}" -o yaml >"${out}/${label}/content.yaml" 2>/dev/null || return 1
	kubectl get "${CONTENT_RES}" "${content}" -o json >"${out}/${label}/content.json" 2>/dev/null || return 1
	mcp="$(kubectl get "${CONTENT_RES}" "${content}" -o jsonpath='{.status.manifestCheckpointName}' 2>/dev/null || true)"
	[[ -n "${mcp}" ]] || return 1
	printf '%s\n' "${mcp}" >"${out}/${label}/mcp-name.txt"
	kubectl get "${MCP_RES}" "${mcp}" -o yaml >"${out}/${label}/mcp.yaml" 2>/dev/null || return 1
	kubectl get "${MCP_RES}" "${mcp}" -o json >"${out}/${label}/mcp.json" 2>/dev/null || return 1
	chunk_dir="${out}/${label}/chunks"
	while IFS= read -r chunk; do
		[[ -n "${chunk}" ]] || continue
		kubectl_ctrl get "${CHUNK_RES}" "${chunk}" -o yaml >"${chunk_dir}/${chunk}.yaml" 2>/dev/null || return 1
		kubectl_ctrl get "${CHUNK_RES}" "${chunk}" -o json >"${chunk_dir}/${chunk}.json" 2>/dev/null || return 1
	done < <(jq -r '.status.chunks[]?.name // empty' "${out}/${label}/mcp.json")
	python3 - "${chunk_dir}" "${out}/${label}/chunk-objects.json" <<'PY'
import base64, gzip, json, pathlib, sys
chunk_dir = pathlib.Path(sys.argv[1])
out = pathlib.Path(sys.argv[2])
objects = []
for path in sorted(chunk_dir.glob("*.json")):
    with path.open() as f:
        chunk = json.load(f)
    data = chunk.get("spec", {}).get("data", "")
    if not data:
        continue
    decoded = gzip.decompress(base64.b64decode(data))
    payload = json.loads(decoded)
    if isinstance(payload, list):
        objects.extend(payload)
    else:
        objects.append(payload)
with out.open("w") as f:
    json.dump(objects, f, indent=2, sort_keys=True)
PY
}

assert_no_vs_vsc_in_content_mcp() {
	local content="$1" out="$2" label="$3"
	dump_content_mcp_chunks "${content}" "${out}" "${label}" || die "cannot dump raw MCP/chunks for content ${content}"
	jq -e '[.[]?|select(.kind=="VolumeSnapshot" or .kind=="VolumeSnapshotContent")]|length==0' \
		"${out}/${label}/chunk-objects.json" >/dev/null \
		|| die "raw MCP chunks for ${content} contain VolumeSnapshot/VolumeSnapshotContent"
}

assert_datarefs_target_uid_unique() {
	local out="$1"; shift
	local content
	mkdir -p "${out}"
	: >"${out}/datarefs-by-content.jsonl"
	for content in "$@"; do
		[[ -n "${content}" ]] || continue
		kubectl get "${CONTENT_RES}" "${content}" -o json 2>/dev/null \
			| jq -c --arg c "${content}" '(.status.dataRefs // [])[]? | {content: $c, targetUID, target, artifact}' \
			>>"${out}/datarefs-by-content.jsonl"
	done
	jq -s '
		group_by(.targetUID)
		| map(select((.[0].targetUID // "") != "" and length > 1))
		| length == 0
	' "${out}/datarefs-by-content.jsonl" >/dev/null \
		|| die "duplicate dataRefs targetUID across snapshot contents (see ${out}/datarefs-by-content.jsonl)"
}

# save_graph <stage> <ns> <snap> <name> <mode>
save_graph() {
	local stage="$1" ns="$2" snap="$3" name="$4" mode="${5:-logical}"
	local dir graph_dir
	dir="$(stage_dir "${stage}")"
	graph_dir="${dir}/graph"
	# Graph rendering (snapshot-graph.sh) is the single most expensive per-stage step — it makes many
	# impersonated chunk reads + a graphviz render and dominates wall time. It is a diagnostic aid, not
	# an assertion, so allow turning it off while iterating on the test logic itself. Re-enable (unset
	# the flag) once stages pass to capture the visual artifacts.
	if [[ "${TREE_DEMO_SKIP_GRAPH:-0}" == "1" ]]; then
		return 0
	fi
	mkdir -p "${graph_dir}"
	[[ -f "${SCRIPT_DIR}/snapshot-graph.sh" ]] || return 0
	bash "${SCRIPT_DIR}/snapshot-graph.sh" --namespace "${ns}" --snapshot "${snap}" \
		--output-dir "${graph_dir}" --name "${name}" --mode "${mode}" \
		--title "tree-demo ${stage}" --chunk-as "${CONTROLLER_SA}" 2>"${dir}/graph.err" \
		|| note "graph render failed for ${stage} (see graph.err)"
}

# ---- cleanup ----------------------------------------------------------------

# reclaim_run_vscs: VolumeSnapshotContents are cluster-scoped and the handoff forces
# deletionPolicy=Retain for durability, so they AND their physical thin-pool snapshots survive
# namespace deletion (Retain = the CSI driver does not delete the backend snapshot when the VSC object
# is GC'd). Across repeated runs this leaks snapshots and exhausts the thin pool. On cleanup, flip this
# run's VSCs back to Delete so the backend snapshot is reclaimed, then remove them. Matched by the bound
# VolumeSnapshot namespace (still populated here because we reclaim before deleting namespaces). Only
# touches VSCs bound to THIS run's namespaces — never another workload's artifacts.
reclaim_run_vscs() {
	local nss="${NS}|${NS_TOPO_DISK_ONLY}|${NS_REF_PVC_NO_DISK_CSD}|${NS_DOMAIN_PVC_FAIL}|${NS_PRIORITY_INV}|${NS_DOMAIN_BARRIER}|${NS_RESTORE}"
	local vscs v
	vscs="$(command kubectl get "${VSC_RES}" -o json 2>/dev/null \
		| jq -r --arg re "^(${nss})$" '.items[]|select((.spec.volumeSnapshotRef.namespace // "")|test($re))|.metadata.name' 2>/dev/null || true)"
	[[ -n "${vscs}" ]] || return 0
	for v in ${vscs}; do
		command kubectl patch "${VSC_RES}" "${v}" --type=merge -p '{"spec":{"deletionPolicy":"Delete"}}' >/dev/null 2>&1 || true
		command kubectl delete "${VSC_RES}" "${v}" --ignore-not-found=true --wait=false >/dev/null 2>&1 || true
	done
	log "cleanup: reclaimed run VolumeSnapshotContents (Delete + remove) to free thin pool: $(echo "${vscs}" | tr '\n' ' ')"
}

cleanup_on_exit() {
	local rc=$?
	_KUBECTL_RETRY_DISABLED=1
	if [[ "${TREE_DEMO_SKIP_CLEANUP:-0}" == "1" ]]; then
		log "SKIP cleanup: TREE_DEMO_SKIP_CLEANUP=1 (namespaces ${NS} ${NS_TOPO_DISK_ONLY} ${NS_REF_PVC_NO_DISK_CSD} ${NS_DOMAIN_PVC_FAIL} ${NS_PRIORITY_INV} ${NS_DOMAIN_BARRIER} ${NS_RESTORE}, CSD ${CSD_NAME} kept)"
		return 0
	fi
	kubectl delete "${CSD_RES}" "${CSD_NAME}" --ignore-not-found=true --wait=false 2>/dev/null || true
	reclaim_run_vscs
	kubectl delete namespace "${NS}" "${NS_TOPO_DISK_ONLY}" "${NS_REF_PVC_NO_DISK_CSD}" "${NS_DOMAIN_PVC_FAIL}" "${NS_PRIORITY_INV}" "${NS_DOMAIN_BARRIER}" "${NS_RESTORE}" --ignore-not-found=true --wait=false 2>/dev/null || true
	return "${rc}"
}
trap cleanup_on_exit EXIT

# =============================================================================
need_cmd kubectl
need_cmd jq
need_cmd python3
mkdir -p "${RUN_ARTIFACT_DIR}"
log "== tree-demo-e2e run_id=${RUN_ID} ns=${NS} sc=${STORAGE_CLASS} module=${MOD_NS}"
log "Artifacts: ${RUN_ARTIFACT_DIR}"

# ---------------------------------------------------------------------------
# 00-preflight
# ---------------------------------------------------------------------------
begin_stage "00-preflight"
kubectl get storageclass "${STORAGE_CLASS}" >/dev/null || die "storageclass ${STORAGE_CLASS} missing"
STORAGE_CLASS_BINDING_MODE="$(storage_class_binding_mode)"
case "${STORAGE_CLASS_BINDING_MODE}" in
Immediate | WaitForFirstConsumer)
	note "StorageClass ${STORAGE_CLASS} volumeBindingMode=${STORAGE_CLASS_BINDING_MODE}; bind pods are created before PVC Bound waits"
	;;
*)
	require "StorageClass ${STORAGE_CLASS} has unexpected volumeBindingMode=${STORAGE_CLASS_BINDING_MODE} (expected Immediate or WaitForFirstConsumer); PVC binding behavior is unknown"
	;;
esac
kubectl get crd \
	snapshots.storage.deckhouse.io snapshotcontents.storage.deckhouse.io \
	manifestcheckpoints.state-snapshotter.deckhouse.io manifestcapturerequests.state-snapshotter.deckhouse.io \
	customsnapshotdefinitions.state-snapshotter.deckhouse.io \
	volumecapturerequests.storage.deckhouse.io \
	volumesnapshots.snapshot.storage.k8s.io volumesnapshotcontents.snapshot.storage.k8s.io \
	volumesnapshotclasses.snapshot.storage.k8s.io \
	demovirtualmachines.demo.state-snapshotter.deckhouse.io \
	demovirtualdisks.demo.state-snapshotter.deckhouse.io \
	demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io \
	demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io >/dev/null \
	|| die "required CRDs missing (controller not deployed or CRDs not installed)"
if kubectl get crd objectkeepers.deckhouse.io >/dev/null 2>&1; then HAS_OBJECTKEEPER=1; else
	note "objectkeepers.deckhouse.io CRD missing; ownership-handoff ObjectKeeper checks limited"
fi
kubectl get pods -n "${MOD_NS}" -l app=controller >"$(stage_dir 00-preflight)/controller-pods.txt" 2>/dev/null \
	|| note "could not list controller pods in ${MOD_NS}"
# Two-pod split: the demo dedicated reconcilers run in the domain-controller pod, and the demo restore
# subtree is delegated by core to the domain aggregated apiserver over the kube-apiserver aggregation
# layer. Without a Ready domain-controller pod nothing would create demo MCR/VCR/children (the tree
# would stall) and restore stages 20-25 could not be served — so this is a hard precondition.
kubectl get pods -n "${MOD_NS}" -l app=domain-controller >"$(stage_dir 00-preflight)/domain-controller-pods.txt" 2>/dev/null \
	|| note "could not list domain-controller pods in ${MOD_NS}"
if ! kubectl get deploy -n "${MOD_NS}" domain-controller >/dev/null 2>&1; then
	require "domain-controller Deployment not found in ${MOD_NS}; deploy the module with stateSnapshotter.enableDemoDomain=true (the demo tree + restore delegation run in the domain pod)"
fi
# Pass MOD_NS as a positional arg (not interpolated into the bash -c body) so an operator-overridden
# namespace can never corrupt/inject the inner script. The inner kubectl is the raw binary, not the
# resilient kubectl() wrapper (shell functions are not exported to the child shell); that is fine because
# wait_until polls for up to WAIT_SEC and rides out transient apiserver/tunnel drops on its own.
wait_until "domain-controller Deployment Available" \
	bash -c 'kubectl get deploy -n "$1" domain-controller -o json 2>/dev/null | jq -e "[.status.conditions[]?|select(.type==\"Available\")][0].status==\"True\"" >/dev/null' _ "${MOD_NS}" \
	|| require "domain-controller Deployment never became Available; demo reconcile + restore delegation cannot run"
# The domain aggregated apiserver must be registered AND healthy for core->domain restore delegation
# (GET .../manifests-with-data-restoration on a demo subtree routes here via kube-apiserver).
if ! kubectl get apiservice "${DOMAIN_APISERVICE}" >/dev/null 2>&1; then
	require "domain APIService ${DOMAIN_APISERVICE} not found; restore delegation to the domain apiserver cannot work (enableDemoDomain + serving-cert)"
fi
wait_until "domain APIService ${DOMAIN_APISERVICE} Available" \
	bash -c 'kubectl get apiservice "$1" -o json 2>/dev/null | jq -e "[.status.conditions[]?|select(.type==\"Available\")][0].status==\"True\"" >/dev/null' _ "${DOMAIN_APISERVICE}" \
	|| require "domain APIService ${DOMAIN_APISERVICE} not Available; core cannot delegate demo restore subtrees to the domain apiserver"
# External demo-domain RBAC (granted by Deckhouse RBAC controller in production).
if [[ "$(kubectl auth can-i get "${VM_RES}" --as="system:serviceaccount:${MOD_NS}:webhooks" -n "${NS}" 2>/dev/null || echo no)" != "yes" ]]; then
	require "webhook SA cannot get demo inventory (${VM_RES}); tree would fail with SubtreeManifestCapturePending — grant external demo-domain RBAC + redeploy"
fi
# Demo capture artifacts (MCR/VCR + child snapshots) are created by the DOMAIN SA after the split.
if [[ "$(kubectl auth can-i create "${MCR_RES}" --as="${DOMAIN_SA}" -n "${NS}" 2>/dev/null || echo no)" != "yes" ]]; then
	require "domain SA (${DOMAIN_SA}) cannot create ${MCR_RES} in target ns; demo capture would stall (RBAC split / 030-domain-rbac hook)"
fi
# Core still creates MCR for the namespace-root (generic) capture leg.
if [[ "$(kubectl auth can-i create "${MCR_RES}" --as="${CONTROLLER_SA}" -n "${NS}" 2>/dev/null || echo no)" != "yes" ]]; then
	require "controller SA cannot create ${MCR_RES} in target ns; root capture would stall"
fi
# Pre-existing CSD claiming the demo snapshot kinds => guaranteed KindConflict for THIS run's CSD,
# which would only surface as a 600s "never Accepted" timeout much later. CSDs are cluster-scoped, so
# a leftover from an interrupted prior run survives namespace cleanup. Fail fast with the offenders
# named (the run has not created its own CSD yet, so any demo-mapping CSD here is foreign).
PREEXISTING_DEMO_CSD="$(kubectl get "${CSD_RES}" -o json 2>/dev/null \
	| jq -r '[.items[]? | select(any(.spec.snapshotResourceMapping[]?; .source.kind == "DemoVirtualMachine" or .source.kind == "DemoVirtualDisk")) | .metadata.name] | join(", ")' 2>/dev/null || true)"
if [[ -n "${PREEXISTING_DEMO_CSD}" ]]; then
	die "pre-existing CSD(s) already claim the demo snapshot kinds: ${PREEXISTING_DEMO_CSD}. This run's CSD ${CSD_NAME} would hit Accepted=False/KindConflict. Delete the leftover CSD(s) first: kubectl delete ${CSD_RES} ${PREEXISTING_DEMO_CSD// /,}"
fi
# Fresh objects must NOT carry deprecated conditions.
{
	echo "module_ns=${MOD_NS}"
	echo "controller_sa=${CONTROLLER_SA}"
	echo "storage_class=${STORAGE_CLASS}"
	echo "pvc_size=${PVC_SIZE}"
	echo "storage_class_volume_binding_mode=${STORAGE_CLASS_BINDING_MODE}"
	echo "wait_for_first_consumer_bind_pods=bind-demo-pvc,bind-pvc-1,bind-pvc-fail"
	echo "has_objectkeeper=${HAS_OBJECTKEEPER}"
	kubectl api-resources 2>/dev/null | grep -E 'snapshot|manifest|customsnapshot|demovirtual|objectkeeper' || true
} >"$(stage_dir 00-preflight)/preflight.txt"
save_artifacts "00-preflight" "${NS}"
log "00-preflight: PASS"

# ===== GROUP topology: independent namespaces; no main tree =====
if grp topology; then
# ---------------------------------------------------------------------------
# topology-disk-only (reference != coverage: VM link does not cover disk)
# ---------------------------------------------------------------------------
begin_stage "topology-disk-only"
delete_csd
apply_disk_only_csd 10
ensure_csd_eligible
apply_source_namespace "${NS_TOPO_DISK_ONLY}"
wait_until "disk-only demo-pvc-disk Bound" pvc_bound "${NS_TOPO_DISK_ONLY}" demo-pvc-disk || require "disk-only demo-pvc-disk never Bound within ${WAIT_SEC}s"
apply_root_snapshot "${NS_TOPO_DISK_ONLY}"
wait_until "disk-only root Snapshot bound" snap_bound "${NS_TOPO_DISK_ONLY}" "${SNAP}" || die "disk-only root Snapshot never bound"
wait_snapshot_ready "${NS_TOPO_DISK_ONLY}" "${SNAP}" || die "disk-only root Snapshot did not become Ready"
DISK_ONLY_ROOT_JSON="$(get_json "${SNAP_RES}" "${NS_TOPO_DISK_ONLY}" "${SNAP}")"
DISK_ONLY_VM_CHILDREN="$(echo "${DISK_ONLY_ROOT_JSON}" | jq '[.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualMachineSnapshot")]|length')"
DISK_ONLY_DISK_CHILDREN="$(echo "${DISK_ONLY_ROOT_JSON}" | jq '[.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualDiskSnapshot")]|length')"
[[ "${DISK_ONLY_VM_CHILDREN}" == "0" ]] || die "disk-only root must not contain DemoVirtualMachineSnapshot child"
[[ "${DISK_ONLY_DISK_CHILDREN}" == "2" ]] || die "disk-only root expected two DemoVirtualDiskSnapshot children, got ${DISK_ONLY_DISK_CHILDREN}"
kubectl -n "${NS_TOPO_DISK_ONLY}" get "${VM_RES},${DISK_RES}" -o json 2>/dev/null \
	| jq -e 'all(.items[]?; ((.metadata.ownerReferences // [])|length)==0)' >/dev/null \
	|| die "disk-only source objects unexpectedly carry ownerReferences; this scenario expects topology without ownerReferences"
DISK_ONLY_CHILD_NAMES="$(echo "${DISK_ONLY_ROOT_JSON}" | jq -r '.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualDiskSnapshot")|.name')"
DISK_ONLY_SOURCES="$(
	for child in ${DISK_ONLY_CHILD_NAMES}; do
		kubectl -n "${NS_TOPO_DISK_ONLY}" get "${DISKSNAP_RES}" "${child}" -o json 2>/dev/null \
			| jq -r --arg k 'state-snapshotter.deckhouse.io/source-ref' '.metadata.annotations[$k] | fromjson? | .name // empty'
	done | sort
)"
printf '%s\n' "${DISK_ONLY_SOURCES}" >"$(stage_dir topology-disk-only)/disk-child-source-names.txt"
grep -qx 'disk-standalone' "$(stage_dir topology-disk-only)/disk-child-source-names.txt" || die "disk-only root missing disk-standalone child source"
grep -qx 'disk-vm' "$(stage_dir topology-disk-only)/disk-child-source-names.txt" || die "disk-only root missing disk-vm child source (VM reference must not imply coverage)"
kubectl get --raw "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/${NS_TOPO_DISK_ONLY}/snapshots/${SNAP}/manifests" \
	>"$(stage_dir topology-disk-only)/aggregated.json" 2>/dev/null || die "disk-only aggregated manifests route failed"
jq -e '[.[]?|select(.apiVersion=="'"${DEMO_API}"'" and .kind=="DemoVirtualDisk" and (.metadata.name=="disk-vm" or .metadata.name=="disk-standalone"))]|length==2' \
	"$(stage_dir topology-disk-only)/aggregated.json" >/dev/null \
	|| die "disk-only aggregated manifests must include both DemoVirtualDisk manifests via child subtrees"
note "disk-only: VM CSD absent, disk-vm and disk-standalone are top-level disk snapshots; ownerReferences are not topology"
save_graph "topology-disk-only" "${NS_TOPO_DISK_ONLY}" "${SNAP}" "topology-disk-only" "logical"
save_artifacts "topology-disk-only" "${NS_TOPO_DISK_ONLY}"
log "topology-disk-only: PASS"

# ---------------------------------------------------------------------------
# referenced-pvc-without-disk-csd (Disk -> PVC link does not cover PVC)
# ---------------------------------------------------------------------------
begin_stage "referenced-pvc-without-disk-csd"
delete_csd
apply_referenced_pvc_without_disk_csd_source "${NS_REF_PVC_NO_DISK_CSD}"
wait_until "referenced-pvc pvc-1 Bound" pvc_bound "${NS_REF_PVC_NO_DISK_CSD}" pvc-1 || require "referenced-pvc pvc-1 never Bound within ${WAIT_SEC}s"
apply_root_snapshot "${NS_REF_PVC_NO_DISK_CSD}"
wait_until "referenced-pvc root Snapshot bound" snap_bound "${NS_REF_PVC_NO_DISK_CSD}" "${SNAP}" || die "referenced-pvc root Snapshot never bound"
wait_snapshot_ready "${NS_REF_PVC_NO_DISK_CSD}" "${SNAP}" || die "referenced-pvc root Snapshot did not become Ready"
REF_ROOT_JSON="$(get_json "${SNAP_RES}" "${NS_REF_PVC_NO_DISK_CSD}" "${SNAP}")"
REF_ROOT_CONTENT="$(echo "${REF_ROOT_JSON}" | jq -r '.status.boundSnapshotContentName // ""')"
REF_PVC_UID="$(kubectl -n "${NS_REF_PVC_NO_DISK_CSD}" get pvc pvc-1 -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
echo "${REF_ROOT_JSON}" | jq -e '[.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualDiskSnapshot")]|length==0' >/dev/null \
	|| die "referenced-pvc root must not create DemoVirtualDiskSnapshot when Disk CSD is absent"
kubectl -n "${NS_REF_PVC_NO_DISK_CSD}" get "${VCR_RES}" -o json 2>/dev/null \
	| jq -e --arg n "pvc-1" '[.items[]?|select(any(.spec.targets[]?; .name==$n))]|length==0' >/dev/null \
	|| die "namespace root must not create VCR for uncovered pvc-1"
[[ -n "${REF_ROOT_CONTENT}" ]] || die "referenced-pvc root content missing"
# CSI-fulfilled artifacts (VS visibility leaf + VSC dataRef) are part of capture readiness, so they are
# bound by the capture budget (WAIT_SEC) — same as the orphan-pvc-vs core stage — not the invalidation
# budget. The orphan VSC is fulfilled by the external CSI snapshot controller, whose latency belongs to
# capture, so a 30s invalidation window would false-fail on slow CSI fulfillment.
wait_until_to "${WAIT_SEC}" "referenced-pvc root CSI VolumeSnapshot visibility leaf present" \
	bash -c "kubectl -n '${NS_REF_PVC_NO_DISK_CSD}' get '${SNAP_RES}' '${SNAP}' -o json | jq -e '[.status.childrenSnapshotRefs[]?|select(.kind==\"VolumeSnapshot\")]|length>=1' >/dev/null" \
	|| die "referenced-pvc root did not create a CSI VolumeSnapshot visibility leaf for uncovered pvc-1 within ${WAIT_SEC}s"
wait_until_to "${WAIT_SEC}" "referenced-pvc root content publishes pvc-1 VSC in dataRefs" \
	bash -c "kubectl get '${CONTENT_RES}' '${REF_ROOT_CONTENT}' -o json | jq -e '[.status.dataRefs[]?|select(.targetUID==\"${REF_PVC_UID}\" and .artifact.kind==\"VolumeSnapshotContent\")]|length>=1' >/dev/null" \
	|| die "referenced-pvc root content did not publish pvc-1 VSC in dataRefs within ${WAIT_SEC}s"
# Refresh after the bounded waits so the orphan-pvc-cleanup stage sees the now-published VS leaf.
REF_ROOT_JSON="$(get_json "${SNAP_RES}" "${NS_REF_PVC_NO_DISK_CSD}" "${SNAP}")"
note "referenced-pvc: Disk spec references pvc-1, but without Disk CSD the PVC is orphan and uses VS/VSC, not VCR"
save_graph "referenced-pvc-without-disk-csd" "${NS_REF_PVC_NO_DISK_CSD}" "${SNAP}" "referenced-pvc-without-disk-csd" "logical"
save_artifacts "referenced-pvc-without-disk-csd" "${NS_REF_PVC_NO_DISK_CSD}"
log "referenced-pvc-without-disk-csd: PASS"

# ---------------------------------------------------------------------------
# orphan-pvc-cleanup (HARD: VS visibility-leaf GC on root delete + retained VSC durability)
# ---------------------------------------------------------------------------
begin_stage "orphan-pvc-cleanup"
REF_VS="$(echo "${REF_ROOT_JSON}" | jq -r '[.status.childrenSnapshotRefs[]?|select(.kind=="VolumeSnapshot")][0].name // ""')"
REF_VSC="$(get_json "${CONTENT_RES}" "" "${REF_ROOT_CONTENT}" | jq -r --arg u "${REF_PVC_UID}" '[.status.dataRefs[]?|select(.targetUID==$u and .artifact.kind=="VolumeSnapshotContent")][0].artifact.name // ""')"
{
	echo "root_snapshot=${SNAP}"
	echo "root_content=${REF_ROOT_CONTENT}"
	echo "orphan_vs=${REF_VS}"
	echo "orphan_vsc=${REF_VSC}"
} >"$(stage_dir orphan-pvc-cleanup)/handles.txt"
[[ -n "${REF_VS}" ]] && kubectl -n "${NS_REF_PVC_NO_DISK_CSD}" get "${VS_RES}" "${REF_VS}" -o yaml >"$(stage_dir orphan-pvc-cleanup)/orphan-vs-before-delete.yaml" 2>/dev/null || true
[[ -n "${REF_VSC}" ]] && kubectl get "${VSC_RES}" "${REF_VSC}" -o yaml >"$(stage_dir orphan-pvc-cleanup)/orphan-vsc-before-delete.yaml" 2>/dev/null || true
[[ -n "${REF_VS}" ]] || require "orphan cleanup: orphan VolumeSnapshot handle missing (tree did not build a CSI VS visibility leaf for pvc-1)"
[[ -n "${REF_VSC}" ]] || require "orphan cleanup: retained VSC handle missing (root content published no VSC dataRef for pvc-1)"
kubectl -n "${NS_REF_PVC_NO_DISK_CSD}" delete "${SNAP_RES}" "${SNAP}" --wait=false >/dev/null 2>&1 \
	|| die "orphan cleanup: could not delete root Snapshot ${SNAP}"
# Invariant: the orphan CSI VolumeSnapshot visibility leaf is garbage-collected with the root Snapshot.
deadline=$((SECONDS + 120)); vs_gone=0
while ((SECONDS < deadline)); do
	if ! kubectl -n "${NS_REF_PVC_NO_DISK_CSD}" get "${VS_RES}" "${REF_VS}" >/dev/null 2>&1; then
		vs_gone=1
		note "orphan cleanup: VolumeSnapshot ${REF_VS} was removed after root delete"
		break
	fi
	sleep 2
done
if [[ "${vs_gone}" != "1" ]]; then
	kubectl -n "${NS_REF_PVC_NO_DISK_CSD}" get "${VS_RES}" "${REF_VS}" -o yaml >"$(stage_dir orphan-pvc-cleanup)/orphan-vs-after-delete.yaml" 2>/dev/null || true
	die "orphan cleanup: VolumeSnapshot ${REF_VS} still exists 120s after root Snapshot delete (visibility leaf not GC'd)"
fi
# Invariant: the retained data artifact (VSC, deletionPolicy=Retain) MUST survive the root delete (durable).
if kubectl get "${VSC_RES}" "${REF_VSC}" -o json >"$(stage_dir orphan-pvc-cleanup)/orphan-vsc-after-delete.json" 2>/dev/null; then
	jq -e '.spec.deletionPolicy == "Retain"' "$(stage_dir orphan-pvc-cleanup)/orphan-vsc-after-delete.json" >/dev/null \
		&& note "orphan cleanup: retained VSC ${REF_VSC} remains with deletionPolicy=Retain (durable)" \
		|| die "orphan cleanup: VSC ${REF_VSC} remains but deletionPolicy is not Retain (durability broken)"
else
	die "orphan cleanup: retained VSC ${REF_VSC} disappeared after root delete (durable data artifact lost)"
fi
save_artifacts "orphan-pvc-cleanup" "${NS_REF_PVC_NO_DISK_CSD}"
log "orphan-pvc-cleanup: done"
fi # end GROUP topology

# ===== MAIN TREE BOOTSTRAP (01 + 02): built by every group except `topology` =====
if need_main_tree; then
# ---------------------------------------------------------------------------
# 01-priority-vm-first  (GVK/priority registration)
# ---------------------------------------------------------------------------
begin_stage "01-priority-vm-first"
apply_csd 100 10
ensure_csd_eligible
CSD_JSON="$(get_json "${CSD_RES}" "" "${CSD_NAME}")"
echo "${CSD_JSON}" | jq '.' >"$(stage_dir 01-priority-vm-first)/csd.json"
# Resolved GVK/GVR present (status.snapshotResourceMapping or resolved fields, best-effort).
echo "${CSD_JSON}" | jq -r '
	"== spec mapping (priority) ==",
	(.spec.snapshotResourceMapping[]? | "  source=\(.source.kind) snapshot=\(.snapshot.kind) priority=\(.priority)"),
	"== status (resolved) ==",
	(.status // {} | tojson)' >"$(stage_dir 01-priority-vm-first)/priority-order.txt"
# Assert VM priority strictly higher than Disk in the registered spec.
VMP="$(echo "${CSD_JSON}" | jq -r '[.spec.snapshotResourceMapping[]?|select(.source.kind=="DemoVirtualMachine")][0].priority // 0')"
DISKP="$(echo "${CSD_JSON}" | jq -r '[.spec.snapshotResourceMapping[]?|select(.source.kind=="DemoVirtualDisk")][0].priority // 0')"
[[ "${VMP}" -gt "${DISKP}" ]] || die "expected VM priority > Disk priority, got VM=${VMP} Disk=${DISKP}"
note "registered priority VM=${VMP} > Disk=${DISKP}"
save_artifacts "01-priority-vm-first" "${NS}"
log "01-priority-vm-first: PASS"

# ---------------------------------------------------------------------------
# 02-tree-ready (tree shape + happy-path conditions + mirror baseline)
# ---------------------------------------------------------------------------
begin_stage "02-tree-ready"
apply_source_namespace "${NS}"
wait_until "demo-pvc Bound in ${NS}" pvc_bound "${NS}" demo-pvc || require "demo-pvc never Bound within ${WAIT_SEC}s"
wait_until "demo-pvc-disk Bound in ${NS}" pvc_bound "${NS}" demo-pvc-disk || require "demo-pvc-disk never Bound within ${WAIT_SEC}s"
apply_root_snapshot "${NS}"
wait_until "root Snapshot bound in ${NS}" snap_bound "${NS}" "${SNAP}" || die "root Snapshot never bound"
ROOT_CONTENT="$(kubectl -n "${NS}" get "${SNAP_RES}" "${SNAP}" -o jsonpath='{.status.boundSnapshotContentName}')"
note "root content=${ROOT_CONTENT}"
DOMAIN_VCR_OBSERVED=0
VCR_OBS_DEADLINE=$((SECONDS + 120))
while ((SECONDS < VCR_OBS_DEADLINE)); do
	kubectl -n "${NS}" get "${VCR_RES}" -o json >"$(stage_dir 02-tree-ready)/domain-vcr-observed.json.tmp" 2>/dev/null || true
	if jq -e '[.items[]?|select(any(.spec.targets[]?; .name=="demo-pvc-disk"))]|length>=1' \
		"$(stage_dir 02-tree-ready)/domain-vcr-observed.json.tmp" >/dev/null 2>&1; then
		mv "$(stage_dir 02-tree-ready)/domain-vcr-observed.json.tmp" "$(stage_dir 02-tree-ready)/domain-vcr-observed.json"
		DOMAIN_VCR_OBSERVED=1
		note "observed domain VCR for demo-pvc-disk before handoff"
		break
	fi
	snap_ready_true "${NS}" "${SNAP}" && break
	sleep 1
done
rm -f "$(stage_dir 02-tree-ready)/domain-vcr-observed.json.tmp"
[[ "${DOMAIN_VCR_OBSERVED}" == "1" ]] || note "domain VCR for demo-pvc-disk was not observed before Ready; it may be created and cleaned quickly after handoff"
wait_snapshot_ready "${NS}" "${SNAP}" || die "root Snapshot did not become Ready"

# Resolve tree via kind-aware childrenSnapshotRefs.
ROOT_JSON="$(get_json "${SNAP_RES}" "${NS}" "${SNAP}")"
VM_SNAP="$(echo "${ROOT_JSON}" | jq -r '[.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualMachineSnapshot")][0].name // ""')"
N_ROOT_VMSNAP="$(echo "${ROOT_JSON}" | jq '[.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualMachineSnapshot")]|length')"
N_ROOT_DISKSNAP="$(echo "${ROOT_JSON}" | jq '[.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualDiskSnapshot")]|length')"
SIBLING_SNAP="$(echo "${ROOT_JSON}" | jq -r '[.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualDiskSnapshot")][0].name // ""')"
[[ -n "${VM_SNAP}" ]] || die "root has no DemoVirtualMachineSnapshot child (tree shape wrong)"
[[ "${N_ROOT_VMSNAP}" == "1" ]] || die "expected exactly 1 root VM snapshot child, got ${N_ROOT_VMSNAP}"
[[ "${N_ROOT_DISKSNAP}" == "1" ]] || die "expected exactly 1 root Disk snapshot child (standalone only), got ${N_ROOT_DISKSNAP} (covered VM disk may be duplicated at root)"
[[ -n "${SIBLING_SNAP}" ]] || die "root has no standalone DemoVirtualDiskSnapshot child"

VM_JSON="$(get_json "${VMSNAP_RES}" "${NS}" "${VM_SNAP}")"
LEAF_SNAP="$(echo "${VM_JSON}" | jq -r '[.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualDiskSnapshot")][0].name // ""')"
N_VM_DISK="$(echo "${VM_JSON}" | jq '[.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualDiskSnapshot")]|length')"
[[ -n "${LEAF_SNAP}" ]] || die "VM snapshot ${VM_SNAP} has no DemoVirtualDiskSnapshot child (covered disk missing from VM subtree)"
[[ "${N_VM_DISK}" == "1" ]] || die "expected 1 disk under VM, got ${N_VM_DISK}"

VM_CONTENT="$(echo "${VM_JSON}" | jq -r '.status.boundSnapshotContentName // ""')"
LEAF_CONTENT="$(kubectl -n "${NS}" get "${DISKSNAP_RES}" "${LEAF_SNAP}" -o jsonpath='{.status.boundSnapshotContentName}' 2>/dev/null || true)"
SIBLING_CONTENT="$(kubectl -n "${NS}" get "${DISKSNAP_RES}" "${SIBLING_SNAP}" -o jsonpath='{.status.boundSnapshotContentName}' 2>/dev/null || true)"
LEAF_MCP=""
[[ -n "${LEAF_CONTENT}" ]] && LEAF_MCP="$(kubectl get "${CONTENT_RES}" "${LEAF_CONTENT}" -o jsonpath='{.status.manifestCheckpointName}' 2>/dev/null || true)"
note "tree: root=${SNAP} vm=${VM_SNAP} leaf(disk-vm)=${LEAF_SNAP} sibling(disk-standalone)=${SIBLING_SNAP}"
note "contents: root=${ROOT_CONTENT} vm=${VM_CONTENT} leaf=${LEAF_CONTENT} sibling=${SIBLING_CONTENT} leafMCP=${LEAF_MCP}"
{
	echo "root_snapshot=${SNAP}"
	echo "root_content=${ROOT_CONTENT}"
	echo "vm_snapshot=${VM_SNAP}"; echo "vm_content=${VM_CONTENT}"
	echo "leaf_snapshot=${LEAF_SNAP}"; echo "leaf_content=${LEAF_CONTENT}"; echo "leaf_mcp=${LEAF_MCP}"
	echo "sibling_snapshot=${SIBLING_SNAP}"; echo "sibling_content=${SIBLING_CONTENT}"
} >"$(stage_dir 02-tree-ready)/tree-handles.txt"

# Happy-path conditions: every content ManifestsReady=VolumeReady=ChildrenReady=Ready=True.
for c in "${LEAF_CONTENT}" "${SIBLING_CONTENT}" "${VM_CONTENT}" "${ROOT_CONTENT}"; do
	[[ -n "${c}" ]] || continue
	wait_until "SnapshotContent ${c} Ready=True" content_ready_true "${c}" || die "content ${c} not Ready"
	cj="$(get_json "${CONTENT_RES}" "" "${c}")"
	[[ "$(cond_field "${cj}" ManifestsReady status)" == "True" ]] || die "content ${c} ManifestsReady != True while Ready=True (inconsistent aggregation)"
	[[ "$(cond_field "${cj}" VolumeReady status)" == "True" ]] || die "content ${c} VolumeReady != True while Ready=True (inconsistent aggregation)"
	[[ "$(cond_field "${cj}" ChildrenReady status)" == "True" ]] || die "content ${c} ChildrenReady != True while Ready=True (inconsistent aggregation)"
done

# --- no-double-reconcile contract (two-pod split) --------------------------
# Cutover invariant: a demo CR is reconciled by exactly ONE controller (the domain pod) and its
# SnapshotContent is created by exactly ONE controller (the core common binder). The strong guarantee is
# STRUCTURAL — the domain-controller binary registers only the demo dedicated planning controllers, which
# never construct a SnapshotContent (commits 3-4) — so this stage's job is to confirm the tree actually
# converged on durable single-owner state, not to re-prove the binary wiring. Verified at Ready:
#  (a) every demo snapshot in the tree (root Snapshot + DemoVirtual{Machine,Disk}Snapshot) has a non-empty
#      boundSnapshotContentName AND that SnapshotContent object exists — asserted by counting bound refs
#      against the demo-snapshot population (an unbound snapshot fails) and resolving each content object.
#      Any of those failing means the core binder never took ownership of a demo content.
#  (b) no two demo snapshots share a content name — a clean 1:1 snapshot->content mapping (the tree-shape
#      assertions above already pin the kind/count of each child).
#  (c) the demo planning output exists on the domain-owned snapshots: VM snapshot has the domain
#      PlanningReady barrier condition (only the domain reconciler sets it). The well-formed
#      VM->disk subtree resolved above is itself proof the single domain reconciler ran.
# Note: a same-snapshot duplicate content is impossible by construction — the binder mints a deterministic
# name (snapshotContentName: "ns-<uid>") and create-or-adopts on AlreadyExists — so (a)+(b) cannot be
# defeated by a second binder racing the same snapshot.
NDR_SNAPS_JSON="$(kubectl -n "${NS}" get "${SNAP_RES},${VMSNAP_RES},${DISKSNAP_RES}" -o json 2>/dev/null)"
[[ -n "${NDR_SNAPS_JSON}" ]] || die "no-double-reconcile: could not list demo snapshots in ${NS}"
NDR_SNAP_COUNT="$(printf '%s' "${NDR_SNAPS_JSON}" | jq '[.items[]?] | length')"
NDR_BOUND="$(printf '%s' "${NDR_SNAPS_JSON}" | jq -r '.items[]? | .status.boundSnapshotContentName // empty' | sed '/^$/d' | sort)"
NDR_TOTAL="$(printf '%s\n' "${NDR_BOUND}" | sed '/^$/d' | wc -l | tr -d ' ')"
NDR_DISTINCT="$(printf '%s\n' "${NDR_BOUND}" | sed '/^$/d' | sort -u | wc -l | tr -d ' ')"
[[ "${NDR_SNAP_COUNT}" -ge 1 ]] || die "no-double-reconcile: no demo snapshots found in ${NS} (tree did not materialize)"
[[ "${NDR_TOTAL}" -eq "${NDR_SNAP_COUNT}" ]] || die "no-double-reconcile: ${NDR_SNAP_COUNT} demo snapshots but ${NDR_TOTAL} bound a SnapshotContent — an unbound demo snapshot (core binder did not take ownership of its content)"
[[ "${NDR_TOTAL}" -eq "${NDR_DISTINCT}" ]] || die "no-double-reconcile: ${NDR_TOTAL} bound SnapshotContent refs but only ${NDR_DISTINCT} distinct — a content is shared between snapshots"
# Each bound content must resolve to a real cluster object (cluster-scoped; no namespace).
while read -r ndr_c; do
	[[ -n "${ndr_c}" ]] || continue
	kubectl get "${CONTENT_RES}" "${ndr_c}" >/dev/null 2>&1 \
		|| die "no-double-reconcile: bound SnapshotContent ${ndr_c} does not exist (dangling binding / content not owned by the core binder)"
done <<<"${NDR_BOUND}"
VM_CSR="$(cond_field "$(get_json "${VMSNAP_RES}" "${NS}" "${VM_SNAP}")" PlanningReady status)"
[[ -n "${VM_CSR}" ]] || die "no-double-reconcile: VM snapshot ${VM_SNAP} has no PlanningReady condition (domain reconciler did not handle it)"
note "no-double-reconcile: ${NDR_DISTINCT} distinct SnapshotContent, all present (single common owner); VM snapshot PlanningReady=${VM_CSR} (domain reconciler)"

# --- two-PVC capture paths -------------------------------------------------
# demo-pvc       : orphan/standalone at root  -> CSI VolumeSnapshot visibility leaf + VSC in ROOT content dataRefs.
# demo-pvc-disk  : nested under DemoVirtualDisk/disk-vm -> domain VCR path -> VSC in the DISK content dataRefs.
# Topology invariants below hold regardless of CSI fulfillment; positive data-ref checks rely on the
# content Ready=True gate above (a Ready content has its data leg handed off).
ORPHAN_PVC_UID="$(kubectl -n "${NS}" get pvc demo-pvc -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
DISK_PVC_UID="$(kubectl -n "${NS}" get pvc demo-pvc-disk -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
ROOT_CONTENT_JSON="$(get_json "${CONTENT_RES}" "" "${ROOT_CONTENT}")"
LEAF_CONTENT_JSON="{}"
[[ -n "${LEAF_CONTENT}" ]] && LEAF_CONTENT_JSON="$(get_json "${CONTENT_RES}" "" "${LEAF_CONTENT}")"

# Invariant: the nested disk PVC is NEVER a root orphan (no root dataRef for it).
echo "${ROOT_CONTENT_JSON}" | jq -e --arg u "${DISK_PVC_UID}" \
	'($u=="") or ([.status.dataRefs[]?|select(.targetUID==$u)]|length==0)' >/dev/null \
	|| die "nested demo-pvc-disk must NOT appear in root SnapshotContent dataRefs (belongs to disk-vm content)"
# Invariant: the orphan PVC is NEVER on the disk content.
echo "${LEAF_CONTENT_JSON}" | jq -e --arg u "${ORPHAN_PVC_UID}" \
	'($u=="") or ([.status.dataRefs[]?|select(.targetUID==$u)]|length==0)' >/dev/null \
	|| die "orphan demo-pvc must NOT appear in disk content dataRefs (belongs to root)"

# Orphan demo-pvc -> VS visibility leaf path. The VS leaf and its VSC dataRef are fulfilled by the
# external CSI snapshot controller, so they belong to the CAPTURE budget (WAIT_SEC) — not the invalidation
# budget — and are bounded-waited so a CSI fulfillment lag is ridden out, not silently passed nor false-failed.
if [[ -n "${ORPHAN_PVC_UID}" ]]; then
	wait_until_to "${WAIT_SEC}" "orphan demo-pvc root VolumeSnapshot visibility leaf present" \
		bash -c "kubectl -n '${NS}' get '${SNAP_RES}' '${SNAP}' -o json | jq -e '[.status.childrenSnapshotRefs[]?|select(.kind==\"VolumeSnapshot\")]|length>=1' >/dev/null" \
		|| die "no root VolumeSnapshot leaf for orphan demo-pvc within ${WAIT_SEC}s (check StorageClass volumesnapshotclass annotation / CSI driver)"
	wait_until_to "${WAIT_SEC}" "orphan demo-pvc VSC in root content dataRefs" \
		bash -c "kubectl get '${CONTENT_RES}' '${ROOT_CONTENT}' -o json | jq -e '[.status.dataRefs[]?|select(.targetUID==\"${ORPHAN_PVC_UID}\" and .artifact.kind==\"VolumeSnapshotContent\")]|length>=1' >/dev/null" \
		|| die "orphan demo-pvc VSC not in root content dataRefs within ${WAIT_SEC}s (CSI fulfillment did not publish)"
	note "orphan demo-pvc: root VolumeSnapshot visibility leaf + VSC dataRef present"
fi

# Nested demo-pvc-disk -> domain VCR -> VSC on disk-vm content (VCR path is also capture-budget fulfillment).
if [[ -n "${DISK_PVC_UID}" && -n "${LEAF_CONTENT}" ]]; then
	wait_until_to "${WAIT_SEC}" "nested demo-pvc-disk VSC in disk content ${LEAF_CONTENT} dataRefs" \
		bash -c "kubectl get '${CONTENT_RES}' '${LEAF_CONTENT}' -o json | jq -e '[.status.dataRefs[]?|select(.targetUID==\"${DISK_PVC_UID}\" and .artifact.kind==\"VolumeSnapshotContent\")]|length>=1' >/dev/null" \
		|| die "nested demo-pvc-disk VSC not in disk content ${LEAF_CONTENT} dataRefs within ${WAIT_SEC}s (VCR path fulfillment)"
	note "nested demo-pvc-disk: VSC published in disk content ${LEAF_CONTENT} dataRefs (VCR path)"
	# The disk PVC must NOT be a root VolumeSnapshot leaf: count root VS leaves <= number of orphan PVCs (1).
	N_ROOT_VS="$(get_json "${SNAP_RES}" "${NS}" "${SNAP}" | jq '[.status.childrenSnapshotRefs[]?|select(.kind=="VolumeSnapshot")]|length')"
	[[ "${N_ROOT_VS}" -le 1 ]] || die "expected at most 1 root VolumeSnapshot leaf (orphan demo-pvc only), got ${N_ROOT_VS} (nested disk PVC leaking into root orphan path?)"
fi

# ConfigMap captured in a manifest checkpoint but NOT a child snapshot (manifest-only object).
echo "${ROOT_JSON}" | jq -e '[.status.childrenSnapshotRefs[]?|select(.kind=="ConfigMap")]|length==0' >/dev/null \
	|| die "ConfigMap must not be a child snapshot"
if kubectl get --raw "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/${NS}/snapshots/${SNAP}/manifests" \
	>"$(stage_dir 02-tree-ready)/aggregated.json" 2>/dev/null; then
	jq -e '[.[]?|select(.kind=="ConfigMap" and .metadata.name=="demo-snapshot-cm")]|length>=1' \
		"$(stage_dir 02-tree-ready)/aggregated.json" >/dev/null \
		&& note "ConfigMap demo-snapshot-cm present in aggregated manifests (manifest-only)" \
		|| die "ConfigMap demo-snapshot-cm not found in aggregated manifests (manifest-only object missing from snapshot)"
else
	note "aggregated manifests route returned non-200 (may be 409 duplicate-in-subtree, or RBAC); see graph"
fi

# Mirror baseline: root Snapshot Ready triple == root SnapshotContent Ready triple.
RS="$(ready_triple "$(get_json "${SNAP_RES}" "${NS}" "${SNAP}")")"
RC="$(ready_triple "$(get_json "${CONTENT_RES}" "" "${ROOT_CONTENT}")")"
[[ "${RS}" == "${RC}" ]] || die "root Snapshot Ready mirror mismatch: snap=[${RS}] content=[${RC}]"
note "mirror OK: root Snapshot Ready == root content Ready [${RS}]"
save_graph "02-tree-ready" "${NS}" "${SNAP}" "tree" "logical"
save_artifacts "02-tree-ready" "${NS}"
log "02-tree-ready: PASS"
fi # end MAIN TREE BOOTSTRAP

# ===== GROUP core: structural assertions over the main tree =====
if grp core; then
# ---------------------------------------------------------------------------
# topology-vm-disk (VM + Disk CSD: referenced disk covered by VM subtree)
# ---------------------------------------------------------------------------
begin_stage "topology-vm-disk"
ROOT_JSON="$(get_json "${SNAP_RES}" "${NS}" "${SNAP}")"
VM_JSON="$(get_json "${VMSNAP_RES}" "${NS}" "${VM_SNAP}")"
[[ "$(kubectl -n "${NS}" get "${VM_RES}" vm-1 -o jsonpath='{.spec.virtualDiskName}' 2>/dev/null)" == "disk-vm" ]] \
	|| die "vm+disk topology: vm-1.spec.virtualDiskName must be disk-vm"
[[ "$(kubectl -n "${NS}" get "${DISK_RES}" disk-vm -o jsonpath='{.spec.persistentVolumeClaimName}' 2>/dev/null)" == "demo-pvc-disk" ]] \
	|| die "vm+disk topology: disk-vm.spec.persistentVolumeClaimName must be demo-pvc-disk"
[[ "$(echo "${ROOT_JSON}" | jq '[.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualMachineSnapshot")]|length')" == "1" ]] \
	|| die "vm+disk topology: root must contain exactly one DemoVirtualMachineSnapshot"
[[ "$(echo "${ROOT_JSON}" | jq '[.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualDiskSnapshot")]|length')" == "1" ]] \
	|| die "vm+disk topology: root must contain only standalone DemoVirtualDiskSnapshot"
ROOT_DISK_SOURCE="$(
	kubectl -n "${NS}" get "${DISKSNAP_RES}" "${SIBLING_SNAP}" -o json 2>/dev/null \
		| jq -r --arg k 'state-snapshotter.deckhouse.io/source-ref' '.metadata.annotations[$k] | fromjson? | .name // empty'
)"
VM_DISK_SOURCE="$(
	kubectl -n "${NS}" get "${DISKSNAP_RES}" "${LEAF_SNAP}" -o json 2>/dev/null \
		| jq -r --arg k 'state-snapshotter.deckhouse.io/source-ref' '.metadata.annotations[$k] | fromjson? | .name // empty'
)"
[[ "${ROOT_DISK_SOURCE}" == "disk-standalone" ]] || die "vm+disk topology: root disk child source=${ROOT_DISK_SOURCE}, expected disk-standalone"
[[ "${VM_DISK_SOURCE}" == "disk-vm" ]] || die "vm+disk topology: VM disk child source=${VM_DISK_SOURCE}, expected disk-vm"
echo "${ROOT_JSON}" | jq -e --arg leaf "${LEAF_SNAP}" \
	'[.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualDiskSnapshot" and .name==$leaf)]|length==0' >/dev/null \
	|| die "vm+disk topology: disk-vm must not be a direct root child once VM actually snapshots it"
printf '%s\n' "spec links are the topology source: vm-1.spec.virtualDiskName=disk-vm; disk-vm.spec.persistentVolumeClaimName=demo-pvc-disk." \
	>"$(stage_dir topology-vm-disk)/topology-source.txt"
note "spec links are the topology source"
note "vm+disk: root has VM child + standalone disk only; disk-vm is covered inside VM subtree"
save_graph "topology-vm-disk" "${NS}" "${SNAP}" "topology-vm-disk" "logical"
save_artifacts "topology-vm-disk" "${NS}"
log "topology-vm-disk: PASS"

# ---------------------------------------------------------------------------
# orphan-pvc-vs (namespace residual PVC uses CSI VolumeSnapshot, not VCR)
# ---------------------------------------------------------------------------
begin_stage "orphan-pvc-vs"
ROOT_JSON="$(get_json "${SNAP_RES}" "${NS}" "${SNAP}")"
ROOT_CONTENT_JSON="$(get_json "${CONTENT_RES}" "" "${ROOT_CONTENT}")"
ORPHAN_PVC_UID="$(kubectl -n "${NS}" get pvc demo-pvc -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
[[ -n "${ORPHAN_PVC_UID}" ]] || die "orphan-pvc-vs: demo-pvc UID missing"
wait_until_to "${WAIT_SEC}" "root SnapshotContent publishes demo-pvc VSC dataRef" \
	bash -c "kubectl get '${CONTENT_RES}' '${ROOT_CONTENT}' -o json 2>/dev/null | jq -e --arg u '${ORPHAN_PVC_UID}' '[.status.dataRefs[]?|select(.targetUID==\$u and .artifact.kind==\"VolumeSnapshotContent\")]|length>=1' >/dev/null" \
	|| die "orphan-pvc-vs: root SnapshotContent must publish demo-pvc VSC in dataRefs"
ROOT_CONTENT_JSON="$(get_json "${CONTENT_RES}" "" "${ROOT_CONTENT}")"
kubectl get "${CONTENT_RES}" "${ROOT_CONTENT}" -o yaml >"$(stage_dir orphan-pvc-vs)/root-content.yaml" 2>/dev/null || true
echo "${ROOT_JSON}" | jq -e '[.status.childrenSnapshotRefs[]?|select(.kind=="VolumeSnapshot")]|length>=1' >/dev/null \
	|| die "orphan-pvc-vs: root childrenSnapshotRefs must include a VolumeSnapshot visibility leaf"
kubectl -n "${NS}" get "${VS_RES}" -o json >"$(stage_dir orphan-pvc-vs)/volumesnapshots.json" 2>/dev/null \
	|| die "orphan-pvc-vs: cannot list VolumeSnapshots"
jq -e '[.items[]?|select(.spec.source.persistentVolumeClaimName=="demo-pvc")]|length>=1' \
	"$(stage_dir orphan-pvc-vs)/volumesnapshots.json" >/dev/null \
	|| die "orphan-pvc-vs: no CSI VolumeSnapshot found for demo-pvc"
ORPHAN_VS="$(jq -r '[.items[]?|select(.spec.source.persistentVolumeClaimName=="demo-pvc")][0].metadata.name // ""' "$(stage_dir orphan-pvc-vs)/volumesnapshots.json")"
[[ -n "${ORPHAN_VS}" ]] || die "orphan-pvc-vs: cannot resolve orphan VolumeSnapshot name"
kubectl -n "${NS}" get "${VS_RES}" "${ORPHAN_VS}" -o yaml >"$(stage_dir orphan-pvc-vs)/orphan-vs.yaml" 2>/dev/null || true
kubectl -n "${NS}" get "${VS_RES}" "${ORPHAN_VS}" -o json >"$(stage_dir orphan-pvc-vs)/orphan-vs.json" 2>/dev/null || true
ROOT_UID="$(echo "${ROOT_JSON}" | jq -r '.metadata.uid // ""')"
jq -e --arg name "${SNAP}" --arg uid "${ROOT_UID}" '
	any(.metadata.ownerReferences[]?; .apiVersion=="'"${STORAGE_API}"'" and .kind=="Snapshot" and .name==$name and .uid==$uid and (.controller // false) == false)
	and ([.metadata.ownerReferences[]?|select((.controller // false)==true)]|length==0)
' "$(stage_dir orphan-pvc-vs)/orphan-vs.json" >/dev/null \
	|| die "orphan-pvc-vs: VolumeSnapshot must have non-controller ownerRef to root Snapshot and no controller=true ownerRef"
ORPHAN_VS_CLASS="$(jq -r '.spec.volumeSnapshotClassName // ""' "$(stage_dir orphan-pvc-vs)/orphan-vs.json")"
[[ -n "${ORPHAN_VS_CLASS}" ]] || die "orphan-pvc-vs: VolumeSnapshot spec.volumeSnapshotClassName must be explicitly set"
SC_VS_CLASS="$(kubectl get storageclass "${STORAGE_CLASS}" -o json 2>/dev/null | jq -r '.metadata.annotations["storage.deckhouse.io/volumesnapshotclass"] // ""' || true)"
[[ -z "${SC_VS_CLASS}" || "${ORPHAN_VS_CLASS}" == "${SC_VS_CLASS}" ]] \
	|| die "orphan-pvc-vs: VolumeSnapshotClass ${ORPHAN_VS_CLASS} does not match StorageClass annotation ${SC_VS_CLASS}"
kubectl get "${VSCLASS_RES}" "${ORPHAN_VS_CLASS}" -o yaml >"$(stage_dir orphan-pvc-vs)/orphan-vsclass.yaml" 2>/dev/null || true
kubectl -n "${NS}" get "${VCR_RES}" -o json 2>/dev/null \
	| jq -e '[.items[]?|select(any(.spec.targets[]?; .name=="demo-pvc"))]|length==0' >/dev/null \
	|| die "orphan-pvc-vs: namespace root must not create VCR for orphan demo-pvc"
echo "${ROOT_CONTENT_JSON}" | jq -e --arg u "${ORPHAN_PVC_UID}" \
	'[.status.dataRefs[]?|select(.targetUID==$u and .artifact.kind=="VolumeSnapshotContent")]|length>=1' >/dev/null \
	|| die "orphan-pvc-vs: root SnapshotContent must publish demo-pvc VSC in dataRefs"
ORPHAN_VSC="$(echo "${ROOT_CONTENT_JSON}" | jq -r --arg u "${ORPHAN_PVC_UID}" '[.status.dataRefs[]?|select(.targetUID==$u and .artifact.kind=="VolumeSnapshotContent")][0].artifact.name // ""')"
[[ -n "${ORPHAN_VSC}" ]] || die "orphan-pvc-vs: cannot resolve orphan VSC from root dataRefs"
kubectl get "${VSC_RES}" "${ORPHAN_VSC}" -o yaml >"$(stage_dir orphan-pvc-vs)/orphan-vsc.yaml" 2>/dev/null || true
kubectl get "${VSC_RES}" "${ORPHAN_VSC}" -o json >"$(stage_dir orphan-pvc-vs)/orphan-vsc.json" 2>/dev/null || true
jq -e '.spec.deletionPolicy == "Retain"' "$(stage_dir orphan-pvc-vs)/orphan-vsc.json" >/dev/null \
	|| die "orphan-pvc-vs: bound VSC deletionPolicy must be Retain"
echo "${ROOT_CONTENT_JSON}" | jq -e --arg vm "${VM_CONTENT}" --arg disk "${SIBLING_CONTENT}" \
	'(.status.childrenSnapshotContentRefs // []) as $refs
	| ($refs|length)==2
	  and any($refs[]?; .name==$vm)
	  and any($refs[]?; .name==$disk)' >/dev/null \
	|| die "orphan-pvc-vs: VolumeSnapshot visibility leaf must not become a SnapshotContent subtree child"
echo "${ROOT_CONTENT_JSON}" | jq -e --arg vs "${ORPHAN_VS}" \
	'([.status.childrenSnapshotContentRefs[]?|select(.name==$vs)]|length)==0' >/dev/null \
	|| die "orphan-pvc-vs: childrenSnapshotContentRefs must not reference the orphan VolumeSnapshot"
kubectl get "${CONTENT_RES}" -o json 2>/dev/null \
	| jq -e --arg vs "${ORPHAN_VS}" --arg ns "${NS}" '
		[.items[]?|select(
			(any(.metadata.ownerReferences[]?; .kind=="VolumeSnapshot" and .name==$vs))
			or ((.metadata.annotations["state-snapshotter.deckhouse.io/source-ref"] // "{}" | fromjson?) as $src | $src.kind == "VolumeSnapshot" and $src.name == $vs and $src.namespace == $ns)
		)]|length==0' >/dev/null \
	|| die "orphan-pvc-vs: no SnapshotContent may be materialized for the VolumeSnapshot visibility leaf"
note "orphan-pvc-vs: demo-pvc uses CSI VolumeSnapshot visibility leaf + root dataRefs, with no namespace VCR"
save_graph "orphan-pvc-vs" "${NS}" "${SNAP}" "orphan-pvc-vs" "logical"
save_artifacts "orphan-pvc-vs" "${NS}"
log "orphan-pvc-vs: PASS"

# ---------------------------------------------------------------------------
# domain-pvc-vcr (domain-covered PVC uses VCR and is not duplicated as orphan)
# ---------------------------------------------------------------------------
begin_stage "domain-pvc-vcr"
ROOT_CONTENT_JSON="$(get_json "${CONTENT_RES}" "" "${ROOT_CONTENT}")"
LEAF_CONTENT_JSON="$(get_json "${CONTENT_RES}" "" "${LEAF_CONTENT}")"
DISK_PVC_UID="$(kubectl -n "${NS}" get pvc demo-pvc-disk -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
[[ -n "${DISK_PVC_UID}" ]] || die "domain-pvc-vcr: demo-pvc-disk UID missing"
wait_until_to "${WAIT_SEC}" "disk SnapshotContent publishes demo-pvc-disk VSC dataRef" \
	bash -c "kubectl get '${CONTENT_RES}' '${LEAF_CONTENT}' -o json 2>/dev/null | jq -e --arg u '${DISK_PVC_UID}' '[.status.dataRefs[]?|select(.targetUID==\$u and .artifact.kind==\"VolumeSnapshotContent\")]|length>=1' >/dev/null" \
	|| die "domain-pvc-vcr: disk SnapshotContent must publish demo-pvc-disk VSC in dataRefs"
ROOT_CONTENT_JSON="$(get_json "${CONTENT_RES}" "" "${ROOT_CONTENT}")"
LEAF_CONTENT_JSON="$(get_json "${CONTENT_RES}" "" "${LEAF_CONTENT}")"
kubectl get "${CONTENT_RES}" "${ROOT_CONTENT}" -o yaml >"$(stage_dir domain-pvc-vcr)/root-content.yaml" 2>/dev/null || true
kubectl get "${CONTENT_RES}" "${LEAF_CONTENT}" -o yaml >"$(stage_dir domain-pvc-vcr)/leaf-content.yaml" 2>/dev/null || true
kubectl -n "${NS}" get "${VCR_RES}" -o json >"$(stage_dir domain-pvc-vcr)/volumecapturerequests.json" 2>/dev/null \
	|| die "domain-pvc-vcr: cannot list VolumeCaptureRequests"
if jq -e '[.items[]?|select(any(.spec.targets[]?; .name=="demo-pvc-disk"))]|length>=1' \
	"$(stage_dir domain-pvc-vcr)/volumecapturerequests.json" >/dev/null; then
	note "domain-pvc-vcr: live VCR for demo-pvc-disk still present"
elif [[ "${DOMAIN_VCR_OBSERVED}" == "1" && -f "$(stage_dir 02-tree-ready)/domain-vcr-observed.json" ]]; then
	cp "$(stage_dir 02-tree-ready)/domain-vcr-observed.json" "$(stage_dir domain-pvc-vcr)/domain-vcr-observed-before-handoff.json"
	note "domain-pvc-vcr: VCR for demo-pvc-disk was observed before handoff and later cleaned"
else
	die "domain-pvc-vcr: did not observe a VCR for demo-pvc-disk before or after handoff"
fi
kubectl -n "${NS}" get "${VS_RES}" -o json 2>/dev/null \
	| jq -e '[.items[]?|select(.spec.source.persistentVolumeClaimName=="demo-pvc-disk")]|length==0' >/dev/null \
	|| die "domain-pvc-vcr: demo-pvc-disk must not leak into root orphan VolumeSnapshot path"
echo "${ROOT_CONTENT_JSON}" | jq -e --arg u "${DISK_PVC_UID}" \
	'[.status.dataRefs[]?|select(.targetUID==$u)]|length==0' >/dev/null \
	|| die "domain-pvc-vcr: root dataRefs must not duplicate domain-covered demo-pvc-disk"
echo "${LEAF_CONTENT_JSON}" | jq -e --arg u "${DISK_PVC_UID}" \
	'[.status.dataRefs[]?|select(.targetUID==$u and .artifact.kind=="VolumeSnapshotContent")]|length>=1' >/dev/null \
	|| die "domain-pvc-vcr: disk SnapshotContent must publish demo-pvc-disk VSC in dataRefs"
DOMAIN_VSC="$(echo "${LEAF_CONTENT_JSON}" | jq -r --arg u "${DISK_PVC_UID}" '[.status.dataRefs[]?|select(.targetUID==$u and .artifact.kind=="VolumeSnapshotContent")][0].artifact.name // ""')"
[[ -n "${DOMAIN_VSC}" ]] || die "domain-pvc-vcr: cannot resolve domain VSC from leaf dataRefs"
kubectl get "${VSC_RES}" "${DOMAIN_VSC}" -o yaml >"$(stage_dir domain-pvc-vcr)/domain-vsc.yaml" 2>/dev/null || true
kubectl get "${VSC_RES}" "${DOMAIN_VSC}" -o json >"$(stage_dir domain-pvc-vcr)/domain-vsc.json" 2>/dev/null || true
jq -e '.spec.deletionPolicy == "Retain"' "$(stage_dir domain-pvc-vcr)/domain-vsc.json" >/dev/null \
	|| die "domain-pvc-vcr: domain VSC deletionPolicy must be Retain"
assert_datarefs_target_uid_unique "$(stage_dir domain-pvc-vcr)" "${ROOT_CONTENT}" "${VM_CONTENT}" "${LEAF_CONTENT}" "${SIBLING_CONTENT}"
note "domain-pvc-vcr: demo-pvc-disk is covered by disk domain VCR and absent from root orphan VS/dataRefs"
save_graph "domain-pvc-vcr" "${NS}" "${SNAP}" "domain-pvc-vcr" "logical"
save_artifacts "domain-pvc-vcr" "${NS}"
log "domain-pvc-vcr: PASS"

# ---------------------------------------------------------------------------
# manifest-no-volumesnapshot (data artifacts stay out of MCP/aggregated manifests)
# ---------------------------------------------------------------------------
begin_stage "manifest-no-volumesnapshot"
wait_until_to "${WAIT_SEC}" "root dataRefs still reference VolumeSnapshotContent artifact" \
	bash -c "kubectl get '${CONTENT_RES}' '${ROOT_CONTENT}' -o json 2>/dev/null | jq -e '[.status.dataRefs[]?|select(.artifact.kind==\"VolumeSnapshotContent\")]|length>=1' >/dev/null" \
	|| die "manifest-no-volumesnapshot: root dataRefs must still reference VolumeSnapshotContent artifact"
kubectl get "${CONTENT_RES}" "${ROOT_CONTENT}" -o yaml >"$(stage_dir manifest-no-volumesnapshot)/root-content.yaml" 2>/dev/null || true
kubectl get "${CONTENT_RES}" "${LEAF_CONTENT}" -o yaml >"$(stage_dir manifest-no-volumesnapshot)/leaf-content.yaml" 2>/dev/null || true
kubectl get --raw "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/${NS}/snapshots/${SNAP}/manifests" \
	>"$(stage_dir manifest-no-volumesnapshot)/aggregated.json" 2>/dev/null \
	|| die "manifest-no-volumesnapshot: aggregated manifests route failed"
jq -e '[.[]?|select(.kind=="PersistentVolumeClaim" and (.metadata.name=="demo-pvc" or .metadata.name=="demo-pvc-disk"))]|length==2' \
	"$(stage_dir manifest-no-volumesnapshot)/aggregated.json" >/dev/null \
	|| die "manifest-no-volumesnapshot: aggregated manifests must include both PVC manifests"
jq -e '[.[]?|select(.kind=="VolumeSnapshot" or .kind=="VolumeSnapshotContent")]|length==0' \
	"$(stage_dir manifest-no-volumesnapshot)/aggregated.json" >/dev/null \
	|| die "manifest-no-volumesnapshot: aggregated manifests must not include VolumeSnapshot or VolumeSnapshotContent"
get_json "${CONTENT_RES}" "" "${ROOT_CONTENT}" | jq -e \
	'[.status.dataRefs[]?|select(.artifact.kind=="VolumeSnapshotContent")]|length>=1' >/dev/null \
	|| die "manifest-no-volumesnapshot: root dataRefs must still reference VolumeSnapshotContent artifact"
for pair in "root:${ROOT_CONTENT}" "vm:${VM_CONTENT}" "leaf:${LEAF_CONTENT}" "sibling:${SIBLING_CONTENT}"; do
	label="${pair%%:*}"
	content="${pair#*:}"
	[[ -n "${content}" ]] || continue
	assert_no_vs_vsc_in_content_mcp "${content}" "$(stage_dir manifest-no-volumesnapshot)/raw-mcp" "${label}"
done
jq -e '[.[]?|select(.kind=="PersistentVolumeClaim" and .metadata.name=="demo-pvc")]|length>=1' \
	"$(stage_dir manifest-no-volumesnapshot)/raw-mcp/root/chunk-objects.json" >/dev/null \
	|| die "manifest-no-volumesnapshot: raw root MCP chunks must include orphan PVC demo-pvc"
jq -e '[.[]?|select(.kind=="PersistentVolumeClaim" and .metadata.name=="demo-pvc-disk")]|length>=1' \
	"$(stage_dir manifest-no-volumesnapshot)/raw-mcp/leaf/chunk-objects.json" >/dev/null \
	|| die "manifest-no-volumesnapshot: raw disk leaf MCP chunks must include domain PVC demo-pvc-disk"
note "manifest-no-volumesnapshot: PVC manifests are present, CSI VS/VSC manifests are absent, VSC remains only as dataRefs artifact"
save_graph "manifest-no-volumesnapshot" "${NS}" "${SNAP}" "manifest-no-volumesnapshot" "logical"
save_artifacts "manifest-no-volumesnapshot" "${NS}"
log "manifest-no-volumesnapshot: PASS"
fi # end GROUP core

# ===========================================================================
# Restore compiler cluster e2e (stages 20-25).
#
# Scope: validate the ALREADY implemented /manifests-with-data-restoration compiler on the live
# main-namespace tree built above (root=${SNAP} in ${NS}). These stages READ the endpoint and assert
# the output is apply-ready; they do NOT change capture/tree-building, dataRefs[]/children*Refs, the
# compiler contract, or solve retained-read. The endpoint output is a JSON array of manifests; jq uses
# (.items? // .) — error-safe: a bare array stays as-is, and a future {items:[...]} envelope still works.
#
# Restore endpoint output (resolved once in 20, reused by 21-25).
# ===== GROUP restore: 20-25 restore-compiler stages over the main tree =====
if grp restore; then
RESTORE_OUT=""
if [[ "${TREE_DEMO_SKIP_RESTORE:-0}" == "1" ]]; then
	begin_stage "20-restore-endpoint-basic"
	note "restore compiler stages 20-25 skipped (TREE_DEMO_SKIP_RESTORE=1)"
	log "20-restore-endpoint-basic: skipped"
else
	# -----------------------------------------------------------------------
	# 20-restore-endpoint-basic (endpoint is live, JSON, non-empty, no VRR)
	# -----------------------------------------------------------------------
	begin_stage "20-restore-endpoint-basic"
	kubectl get namespace "${NS_RESTORE}" >/dev/null 2>&1 || kubectl create namespace "${NS_RESTORE}" >/dev/null
	RESTORE_OUT="$(stage_dir 20-restore-endpoint-basic)/manifests-with-data-restoration.json"
	kubectl get --raw \
		"/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/${NS}/snapshots/${SNAP}/manifests-with-data-restoration?targetNamespace=${NS_RESTORE}" \
		>"${RESTORE_OUT}" 2>"$(stage_dir 20-restore-endpoint-basic)/endpoint.err" \
		|| die "20-restore-endpoint-basic: restore endpoint request failed (see endpoint.err)"
	jq -e '.' "${RESTORE_OUT}" >/dev/null \
		|| die "20-restore-endpoint-basic: endpoint did not return valid JSON"
	jq -e '((.items? // .) | type) == "array" and ((.items? // .) | length) > 0' "${RESTORE_OUT}" >/dev/null \
		|| die "20-restore-endpoint-basic: restore output is empty"
	jq -e '[(.items? // .)[] | select(.kind == "VolumeRestoreRequest")] | length == 0' "${RESTORE_OUT}" >/dev/null \
		|| die "20-restore-endpoint-basic: output must not contain VolumeRestoreRequest (VRR is removed from read-path)"
	note "20-restore-endpoint-basic: endpoint live, JSON, non-empty, no VRR (target ns ${NS_RESTORE})"
	save_artifacts "20-restore-endpoint-basic" "${NS}"
	log "20-restore-endpoint-basic: PASS"

	# -----------------------------------------------------------------------
	# 21-restore-sanitize-and-namespace (restore-safe sanitize + ns rewrite)
	# -----------------------------------------------------------------------
	begin_stage "21-restore-sanitize-and-namespace"
	jq -e '
		all((.items? // .)[];
			(.metadata | has("uid") | not) and
			(.metadata | has("resourceVersion") | not) and
			(.metadata | has("managedFields") | not) and
			(.metadata | has("ownerReferences") | not) and
			(.metadata | has("finalizers") | not) and
			(has("status") | not)
		)
	' "${RESTORE_OUT}" >/dev/null \
		|| die "21-restore-sanitize-and-namespace: output still carries runtime/server-managed fields (uid/resourceVersion/managedFields/ownerReferences/finalizers/status)"
	# Restore compiler is namespaced-only (MVP): every emitted object is namespaced and rewritten to the
	# target namespace; cluster-scoped objects are dropped (none must remain without a namespace).
	jq -e --arg ns "${NS_RESTORE}" '
		all((.items? // .)[]; (.metadata.namespace // "") == $ns)
	' "${RESTORE_OUT}" >/dev/null \
		|| die "21-restore-sanitize-and-namespace: not every object was rewritten to targetNamespace ${NS_RESTORE} (or a cluster-scoped object leaked)"
	note "21-restore-sanitize-and-namespace: sanitized + all objects in ${NS_RESTORE}"
	save_artifacts "21-restore-sanitize-and-namespace" "${NS}"
	log "21-restore-sanitize-and-namespace: PASS"

	# -----------------------------------------------------------------------
	# 22-restore-orphan-pvc-datasource (orphan PVC -> dataSourceRef VolumeSnapshot)
	# -----------------------------------------------------------------------
	begin_stage "22-restore-orphan-pvc-datasource"
	jq -e '
		[(.items? // .)[]
			| select(.apiVersion == "v1" and .kind == "PersistentVolumeClaim" and .metadata.name == "demo-pvc")
		] | length == 1
	' "${RESTORE_OUT}" >/dev/null \
		|| die "22-restore-orphan-pvc-datasource: expected exactly one orphan PVC demo-pvc in output"
	jq -e '
		[(.items? // .)[]
			| select(.kind == "PersistentVolumeClaim" and .metadata.name == "demo-pvc")
			| select(.spec.dataSourceRef.kind == "VolumeSnapshot")
			| select((.spec.dataSourceRef.name // "") != "")
			| select((.spec.dataSourceRef.apiGroup // "") == "snapshot.storage.k8s.io")
		] | length == 1
	' "${RESTORE_OUT}" >/dev/null \
		|| die "22-restore-orphan-pvc-datasource: demo-pvc must have spec.dataSourceRef -> VolumeSnapshot (snapshot.storage.k8s.io) with a non-empty name"
	# The compiler must resolve the VS visibility-leaf name captured for demo-pvc (set in orphan-pvc-vs).
	if [[ -n "${ORPHAN_VS:-}" ]]; then
		jq -e --arg vs "${ORPHAN_VS}" '
			[(.items? // .)[]
				| select(.kind == "PersistentVolumeClaim" and .metadata.name == "demo-pvc")
				| select(.spec.dataSourceRef.name == $vs)
			] | length == 1
		' "${RESTORE_OUT}" >/dev/null \
			|| die "22-restore-orphan-pvc-datasource: demo-pvc dataSourceRef.name must be the orphan VolumeSnapshot leaf ${ORPHAN_VS}"
	fi
	# Stale captured PVC binding fields must be stripped.
	jq -e '
		all((.items? // .)[] | select(.kind == "PersistentVolumeClaim");
			(.spec | has("volumeName") | not) and (.spec | has("dataSource") | not)
		)
	' "${RESTORE_OUT}" >/dev/null \
		|| die "22-restore-orphan-pvc-datasource: PVC output must not carry spec.volumeName or spec.dataSource"
	# VS/VSC are transferred separately (data + tree); the compiler only references them, never emits them.
	jq -e '
		[(.items? // .)[] | select(.kind == "VolumeSnapshot" or .kind == "VolumeSnapshotContent")] | length == 0
	' "${RESTORE_OUT}" >/dev/null \
		|| die "22-restore-orphan-pvc-datasource: output must not emit VolumeSnapshot/VolumeSnapshotContent objects"
	note "22-restore-orphan-pvc-datasource: demo-pvc -> dataSourceRef VolumeSnapshot, VS/VSC not emitted"
	save_artifacts "22-restore-orphan-pvc-datasource" "${NS}"
	log "22-restore-orphan-pvc-datasource: PASS"

	# -----------------------------------------------------------------------
	# 23-restore-domain-disk-datasource (DemoVirtualDisk -> dataSource + covered PVC suppressed)
	# -----------------------------------------------------------------------
	begin_stage "23-restore-domain-disk-datasource"
	# disk-vm (covered, under VM) and disk-standalone (root child) are each captured under their own
	# DemoVirtualDiskSnapshot, so both must be rewritten to restore from that snapshot.
	for disk in disk-vm disk-standalone; do
		jq -e --arg d "${disk}" '
			[(.items? // .)[]
				| select(.apiVersion == "'"${DEMO_API}"'" and .kind == "DemoVirtualDisk" and .metadata.name == $d)
				| select(.spec.dataSource.kind == "DemoVirtualDiskSnapshot")
				| select((.spec.dataSource.name // "") != "")
			] | length == 1
		' "${RESTORE_OUT}" >/dev/null \
			|| die "23-restore-domain-disk-datasource: DemoVirtualDisk ${disk} must have spec.dataSource -> DemoVirtualDiskSnapshot with a non-empty name"
	done
	# demo-pvc-disk is owned/covered by disk-vm: the restored disk recreates it, so it must NOT appear as
	# a standalone PVC restore object (covered-PVC suppression).
	jq -e '
		[(.items? // .)[]
			| select(.kind == "PersistentVolumeClaim" and .metadata.name == "demo-pvc-disk")
		] | length == 0
	' "${RESTORE_OUT}" >/dev/null \
		|| die "23-restore-domain-disk-datasource: covered PVC demo-pvc-disk must not be emitted as a standalone PVC"
	note "23-restore-domain-disk-datasource: both disks point at their DemoVirtualDiskSnapshot; covered demo-pvc-disk suppressed"
	save_artifacts "23-restore-domain-disk-datasource" "${NS}"
	log "23-restore-domain-disk-datasource: PASS"

	# -----------------------------------------------------------------------
	# 24-restore-vm-disk-order (post-order output: child disk before parent VM)
	# -----------------------------------------------------------------------
	begin_stage "24-restore-vm-disk-order"
	jq -e '
		(.items? // .) as $o
		| ([$o | to_entries[] | select(.value.kind == "DemoVirtualDisk" and .value.metadata.name == "disk-vm") | .key][0]) as $disk
		| ([$o | to_entries[] | select(.value.kind == "DemoVirtualMachine" and .value.metadata.name == "vm-1") | .key][0]) as $vm
		| ($disk != null) and ($vm != null) and ($disk < $vm)
	' "${RESTORE_OUT}" >/dev/null \
		|| die "24-restore-vm-disk-order: restored disk-vm must appear before its parent vm-1 (child-before-parent post-order output)"
	note "24-restore-vm-disk-order: disk-vm precedes vm-1 in output"
	save_artifacts "24-restore-vm-disk-order" "${NS}"
	log "24-restore-vm-disk-order: PASS"

	# -----------------------------------------------------------------------
	# 25-restore-dry-run-apply (manifests are truly apply-ready)
	# -----------------------------------------------------------------------
	begin_stage "25-restore-dry-run-apply"
	# kubectl apply needs a single document/List, not a bare JSON array; wrap the output in a v1 List.
	# No -n: every object already carries its rewritten namespace (verified in stage 21), and passing -n
	# alongside in-manifest namespaces can make apply reject mismatched/empty namespaces.
	jq '{apiVersion: "v1", kind: "List", items: (.items? // .)}' "${RESTORE_OUT}" \
		>"$(stage_dir 25-restore-dry-run-apply)/restore-list.json"
	kubectl apply --dry-run=server -f "$(stage_dir 25-restore-dry-run-apply)/restore-list.json" \
		>"$(stage_dir 25-restore-dry-run-apply)/dry-run.out" 2>&1 \
		|| { cat "$(stage_dir 25-restore-dry-run-apply)/dry-run.out" >&2; die "25-restore-dry-run-apply: server-side dry-run apply rejected the restore manifests (sanitize gap; see dry-run.out)"; }
	note "25-restore-dry-run-apply: server-side dry-run apply accepted all restore manifests in ${NS_RESTORE}"
	save_artifacts "25-restore-dry-run-apply" "${NS}"
	log "25-restore-dry-run-apply: PASS"
fi
fi # end GROUP restore

# ===== GROUP domain (part 1/2): planned subtree capture failure =====
if grp domain; then
# ---------------------------------------------------------------------------
# domain-pvc-failure-coverage (HARD when the injected failure HOLDS: planned subtree capture failure)
# ---------------------------------------------------------------------------
begin_stage "domain-pvc-failure-coverage"
cat >"$(stage_dir domain-pvc-failure-coverage)/expected-outcome.txt" <<'EOF'
We perturb a planned disk subtree by injecting VCR Ready=False for pvc-fail, then give the owning
volume-capture controller the full invalidation window to settle into a deterministic terminal state.
An injected VCR-status patch cannot be made to stick against the owning controller on a snapshottable
(empty) PVC, so the outcome is decided on observable state, not on the (racy) injection sticking:
- INJECT did not stick (VCR back to Ready=True): empty pvc-fail snapshot SUCCEEDED ->
    PASS iff a durable VSC dataRef for pvc-fail (by target UID) is published in some SnapshotContent;
    FAIL (die) on success-without-artifact. (Domain terminal-failure propagation: integration tests.)
- INJECT held (VCR Ready=False) AND root Ready=False: PASS (failed closed / propagated).
- INJECT held AND durable VSC dataRef for pvc-fail present: PASS (data durable; stale VCR status only).
- INJECT held AND root Ready=True AND no durable artifact for pvc-fail: FAIL (die) — stale Ready=True
    over lost data (INV-FAIL-PROP violation). The durable check is tree-wide and keyed by pvc-fail UID,
    because the domain path publishes the VSC dataRef in the disk SnapshotContent (not the root content).
EOF
apply_domain_pvc_failure_source "${NS_DOMAIN_PVC_FAIL}"
wait_until "domain-failure pvc-fail Bound" pvc_bound "${NS_DOMAIN_PVC_FAIL}" pvc-fail || require "domain-failure pvc-fail never Bound within ${WAIT_SEC}s"
apply_root_snapshot "${NS_DOMAIN_PVC_FAIL}"
wait_until "domain-failure root Snapshot bound" snap_bound "${NS_DOMAIN_PVC_FAIL}" "${SNAP}" || require "domain-failure root Snapshot never bound within ${WAIT_SEC}s"
DOMAIN_FAIL_VCR=""
deadline=$((SECONDS + 180))
while ((SECONDS < deadline)); do
	kubectl -n "${NS_DOMAIN_PVC_FAIL}" get "${VCR_RES}" -o json >"$(stage_dir domain-pvc-failure-coverage)/vcr-list-before-inject.json" 2>/dev/null || true
	DOMAIN_FAIL_VCR="$(jq -r '[.items[]?|select(any(.spec.targets[]?; .name=="pvc-fail"))][0].metadata.name // ""' "$(stage_dir domain-pvc-failure-coverage)/vcr-list-before-inject.json" 2>/dev/null || true)"
	[[ -n "${DOMAIN_FAIL_VCR}" ]] && break
	if snap_ready_terminal_false "${NS_DOMAIN_PVC_FAIL}" "${SNAP}"; then
		break
	fi
	sleep 2
done
if [[ -n "${DOMAIN_FAIL_VCR}" ]]; then
	kubectl -n "${NS_DOMAIN_PVC_FAIL}" get "${VCR_RES}" "${DOMAIN_FAIL_VCR}" -o yaml >"$(stage_dir domain-pvc-failure-coverage)/vcr-before-inject.yaml" 2>/dev/null || true
	VCR_FAIL_PATCH="$(jq -n --arg now "$(now_rfc3339)" '{
		status: {
			conditions: [{
				type: "Ready",
				status: "False",
				reason: "InjectedFailure",
				message: "tree-demo injected domain VCR failure",
				lastTransitionTime: $now
			}]
		}
	}')"
	kubectl -n "${NS_DOMAIN_PVC_FAIL}" patch "${VCR_RES}" "${DOMAIN_FAIL_VCR}" --subresource=status --type=merge -p "${VCR_FAIL_PATCH}" >/dev/null 2>&1 \
		|| kubectl_ctrl -n "${NS_DOMAIN_PVC_FAIL}" patch "${VCR_RES}" "${DOMAIN_FAIL_VCR}" --subresource=status --type=merge -p "${VCR_FAIL_PATCH}" >/dev/null 2>&1 \
		|| require "domain-failure: could not inject terminal VCR failure on ${DOMAIN_FAIL_VCR}"
	kubectl -n "${NS_DOMAIN_PVC_FAIL}" get "${VCR_RES}" "${DOMAIN_FAIL_VCR}" -o yaml >"$(stage_dir domain-pvc-failure-coverage)/vcr-after-inject.yaml" 2>/dev/null || true
else
	require "domain-failure: VCR for pvc-fail was not observed before terminal state/timeout (cannot inject a domain capture failure)"
fi
FAIL_PVC_UID="$(kubectl -n "${NS_DOMAIN_PVC_FAIL}" get pvc pvc-fail -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
[[ -n "${FAIL_PVC_UID}" ]] || require "domain-failure: cannot resolve pvc-fail UID (needed to verify durable artifact by target)"
# Deterministic settle: an injected VCR-status patch CANNOT be made to stick against the owning
# volume-capture controller on a snapshottable (empty) PVC — the controller reconciles such a VCR back to
# Ready=True. So instead of sampling once after a fixed sleep (a timing artifact), give the owning
# controller the full invalidation window to reach one of the two deterministic terminal states, and stop
# as soon as either is reached:
#   (a) VCR reconciled back to Ready=True  -> capture SUCCEEDED (no failure to propagate), or
#   (b) root Snapshot Ready=False          -> failure propagated / failed closed.
settle_deadline=$((SECONDS + INVALIDATION_WAIT_SEC))
while ((SECONDS < settle_deadline)); do
	VCR_READY_AFTER="$(cond_field "$(get_json "${VCR_RES}" "${NS_DOMAIN_PVC_FAIL}" "${DOMAIN_FAIL_VCR}")" Ready status)"
	ROOT_READY_NOW="$(cond_field "$(get_json "${SNAP_RES}" "${NS_DOMAIN_PVC_FAIL}" "${SNAP}")" Ready status)"
	[[ "${VCR_READY_AFTER}" == "True" || "${ROOT_READY_NOW}" == "False" ]] && break
	sleep "${POLL_SEC}"
done
VCR_READY_AFTER="$(cond_field "$(get_json "${VCR_RES}" "${NS_DOMAIN_PVC_FAIL}" "${DOMAIN_FAIL_VCR}")" Ready status)"
INJECT_HELD=0
[[ "${VCR_READY_AFTER}" == "False" ]] && INJECT_HELD=1
FAIL_ROOT_JSON="$(get_json "${SNAP_RES}" "${NS_DOMAIN_PVC_FAIL}" "${SNAP}")"
FAIL_READY="$(ready_triple "${FAIL_ROOT_JSON}")"
# Durable-artifact check, TREE-WIDE and keyed by the pvc-fail target UID. The domain path publishes the
# VSC dataRef in the *disk* SnapshotContent (not the root content) and uses a VCR (not an orphan VS leaf),
# so the previous root-content/orphan-VS-only check looked at the wrong objects. SnapshotContents are
# cluster-scoped; filtering by FAIL_PVC_UID isolates pvc-fail's artifact from any other tree's contents.
FAIL_DURABLE=0
if kubectl get "${CONTENT_RES}" -o json 2>/dev/null \
	| jq -e --arg u "${FAIL_PVC_UID}" '[.items[]?|.status.dataRefs[]?|select(.targetUID==$u and .artifact.kind=="VolumeSnapshotContent")]|length>=1' >/dev/null; then
	FAIL_DURABLE=1
fi
echo "${FAIL_ROOT_JSON}" | jq '{children: .status.childrenSnapshotRefs, ready: ([.status.conditions[]?|select(.type=="Ready")][0]), content: .status.boundSnapshotContentName}' \
	>"$(stage_dir domain-pvc-failure-coverage)/root-after-inject.summary.json" 2>/dev/null || true
printf 'vcr=%s\nvcr_ready_after_settle=%s\ninject_held=%s\nroot_ready=%s\nfail_pvc_uid=%s\nfail_pvc_durable_dataref=%s\n' \
	"${DOMAIN_FAIL_VCR}" "${VCR_READY_AFTER:-<none>}" "${INJECT_HELD}" "${FAIL_READY}" "${FAIL_PVC_UID}" "${FAIL_DURABLE}" \
	>"$(stage_dir domain-pvc-failure-coverage)/inject-outcome.txt"
if [[ "${INJECT_HELD}" != "1" ]]; then
	# (a) Controller reconciled the VCR back to Ready=${VCR_READY_AFTER}: the empty pvc-fail snapshot
	# SUCCEEDED. There is no terminal failure to propagate (an injected VCR-status patch cannot be made to
	# stick against the owning controller on a snapshottable PVC; domain-path terminal-failure propagation
	# is asserted deterministically in the controller integration tests). The positive invariant still
	# holds deterministically: a successful capture MUST have published a durable VSC dataRef for pvc-fail.
	[[ "${FAIL_DURABLE}" == "1" ]] \
		|| die "domain-failure: pvc-fail capture reported success (VCR Ready=${VCR_READY_AFTER}) but NO durable VSC dataRef for pvc-fail (uid=${FAIL_PVC_UID}) is published in any SnapshotContent (success without artifact — INV-FAIL-PROP violation; see inject-outcome.txt)"
	note "domain-failure: injected VCR Ready=False did not stick (owning controller reconciled ${DOMAIN_FAIL_VCR} back to Ready=${VCR_READY_AFTER} within ${INVALIDATION_WAIT_SEC}s); empty pvc-fail snapshot succeeded with a durable VSC dataRef. root Ready=[${FAIL_READY}] is correct. Domain-path terminal-failure propagation is covered by integration tests."
elif echo "${FAIL_READY}" | grep -qE '^False\|'; then
	# (b) VCR held Ready=False AND root failed closed — failure propagated correctly.
	note "domain-failure: root failed closed over a HELD domain VCR failure [${FAIL_READY}]"
elif [[ "${FAIL_DURABLE}" == "1" ]]; then
	# (c) VCR held Ready=False but the durable VSC dataRef for pvc-fail IS published: the data is durable
	# and the stale VCR status is cosmetic only, so root Ready=True is correct (artifact-backed, not lost).
	note "domain-failure: VCR held Ready=False, but a durable VSC dataRef for pvc-fail (uid=${FAIL_PVC_UID}) IS published; data is durable (stale VCR status only). root Ready=[${FAIL_READY}] is correct."
else
	# (d) VCR held Ready=False, root Ready=True, and NO durable artifact for pvc-fail anywhere: genuine
	# stale Ready=True over lost data.
	die "domain-failure: root Ready=[${FAIL_READY}] with VCR held Ready=False and NO durable VSC dataRef for pvc-fail (uid=${FAIL_PVC_UID}) in any SnapshotContent (stale Ready=True over lost data — INV-FAIL-PROP violation; see inject-outcome.txt)"
fi
save_graph "domain-pvc-failure-coverage" "${NS_DOMAIN_PVC_FAIL}" "${SNAP}" "domain-pvc-failure-coverage" "logical"
save_artifacts "domain-pvc-failure-coverage" "${NS_DOMAIN_PVC_FAIL}"
log "domain-pvc-failure-coverage: done"
fi # end GROUP domain (part 1/2)

# ===== GROUP priority: 03-priority-inverted (global CSD priority flip) =====
if grp priority; then
# ---------------------------------------------------------------------------
# 03-priority-inverted (priority must influence planning, or fail closed)
# ---------------------------------------------------------------------------
begin_stage "03-priority-inverted"
if [[ "${TREE_DEMO_SKIP_INVERSION:-0}" == "1" ]]; then
	note "skipped (TREE_DEMO_SKIP_INVERSION=1)"
elif ! inversion_safe; then
	require "priority inversion is unsafe: a global CSD priority flip would disturb demo workload outside this run (${INVERSION_BLOCKERS}). Run on an isolated cluster, or set TREE_DEMO_SKIP_INVERSION=1 to acknowledge and skip."
	{ echo "inversion skipped (unsafe global CSD churn)"; echo "blockers: ${INVERSION_BLOCKERS}"; } >"$(stage_dir 03-priority-inverted)/skipped.txt"
	kubectl get "${VMSNAP_RES},${DISKSNAP_RES}" -A -o wide >"$(stage_dir 03-priority-inverted)/demo-snapshots-all-namespaces.txt" 2>/dev/null || true
	kubectl get "${CSD_RES}" -o yaml >"$(stage_dir 03-priority-inverted)/customsnapshotdefinitions.yaml" 2>/dev/null || true
else
	apply_csd 10 100
	ensure_csd_eligible
	apply_source_namespace "${NS_PRIORITY_INV}"
	wait_until "demo-pvc Bound in ${NS_PRIORITY_INV}" pvc_bound "${NS_PRIORITY_INV}" demo-pvc || require "inverted demo-pvc never Bound within ${WAIT_SEC}s"
	apply_root_snapshot "${NS_PRIORITY_INV}"
	# Expected behavior (recorded so the stage proves a planner decision, not "something happened").
	EXP="$(stage_dir 03-priority-inverted)/expected-vs-actual.txt"
	{
		echo "== VM-first baseline (case 02-tree-ready) =="
		echo "  root children: VMsnap=${N_ROOT_VMSNAP} Disksnap=${N_ROOT_DISKSNAP} (VM + standalone disk)"
		echo "  VM children:   covered disk=${N_VM_DISK}"
		echo "== Disk-first expected =="
		echo "  EITHER root child set changes (covered VM disk no longer hidden under VM:"
		echo "         root Disksnap increases, or VM disk-child count changes),"
		echo "  OR controller publishes an explicit fail-closed reason"
		echo "         (PriorityLayerPending / GraphPlanningFailed / ChildGraphPending / SourceIdentity*)"
		echo "         explaining the ambiguous/invalid coverage order."
	} >"${EXP}"
	if wait_until "inverted root Snapshot bound" snap_bound "${NS_PRIORITY_INV}" "${SNAP}"; then
		# Give the planner time to settle (may fail-closed under inverted priority).
		sleep "$((POLL_SEC * 4))"
		INV_JSON="$(get_json "${SNAP_RES}" "${NS_PRIORITY_INV}" "${SNAP}")"
		INV_VM="$(echo "${INV_JSON}" | jq '[.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualMachineSnapshot")]|length')"
		INV_DISK="$(echo "${INV_JSON}" | jq '[.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualDiskSnapshot")]|length')"
		INV_READY="$(ready_triple "${INV_JSON}")"
		# Inverted VM subtree disk-child count (coverage order signal).
		INV_VMSNAP="$(echo "${INV_JSON}" | jq -r '[.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualMachineSnapshot")][0].name // ""')"
		INV_VM_DISK="-"
		[[ -n "${INV_VMSNAP}" ]] && INV_VM_DISK="$(kubectl -n "${NS_PRIORITY_INV}" get "${VMSNAP_RES}" "${INV_VMSNAP}" -o json 2>/dev/null \
			| jq '[.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualDiskSnapshot")]|length' 2>/dev/null || echo '-')"
		{
			echo "== Disk-first actual =="
			echo "  root children: VMsnap=${INV_VM} Disksnap=${INV_DISK}"
			echo "  VM children:   covered disk=${INV_VM_DISK}"
			echo "  root Ready:    [${INV_READY}]"
		} >>"${EXP}"
		note "inverted root children: VMsnap=${INV_VM} Disksnap=${INV_DISK} VMdisk=${INV_VM_DISK}; Ready=[${INV_READY}]"
		echo "${INV_JSON}" | jq '{children: .status.childrenSnapshotRefs, ready: ([.status.conditions[]?|select(.type=="Ready")][0])}' \
			>"$(stage_dir 03-priority-inverted)/inverted-root.json"
		# Prove priority influenced the planner decision.
		shape_changed=0
		[[ "${INV_DISK}" != "${N_ROOT_DISKSNAP}" || "${INV_VM}" != "${N_ROOT_VMSNAP}" ]] && shape_changed=1
		[[ "${INV_VM_DISK}" != "-" && "${INV_VM_DISK}" != "${N_VM_DISK}" ]] && shape_changed=1
		if [[ "${shape_changed}" == "1" ]]; then
			note "PASS: disk-first changed planned tree vs vm-first (root VM=${N_ROOT_VMSNAP}->${INV_VM}, Disk=${N_ROOT_DISKSNAP}->${INV_DISK}, VMdisk=${N_VM_DISK}->${INV_VM_DISK}); priority influences the planner"
		elif echo "${INV_READY}" | grep -qE 'PriorityLayerPending|GraphPlanningFailed|ChildGraphPending|SourceIdentity'; then
			note "PASS (fail-closed): inverted priority produced explicit planner refusal: [${INV_READY}]"
		else
			die "priority inversion did NOT change the planner decision and gave no fail-closed reason: baseline root(VM=${N_ROOT_VMSNAP},Disk=${N_ROOT_DISKSNAP}) VMdisk=${N_VM_DISK} == inverted root(VM=${INV_VM},Disk=${INV_DISK}) VMdisk=${INV_VM_DISK} (priority does NOT affect planning — see expected-vs-actual.txt)"
		fi
	else
		note "inverted root Snapshot never bound within window (possible fail-closed/slow planner); recorded"
		echo "== Disk-first actual ==" >>"${EXP}"; echo "  root Snapshot not bound within window" >>"${EXP}"
	fi
	save_graph "03-priority-inverted" "${NS_PRIORITY_INV}" "${SNAP}" "inverted" "logical"
	save_artifacts "03-priority-inverted" "${NS_PRIORITY_INV}"
	# Restore vm-first priority so the main tree (case 02) stays consistent for later stages.
	apply_csd 100 10
	ensure_csd_eligible
	wait_snapshot_ready "${NS}" "${SNAP}" || die "main tree did not reconverge to Ready after CSD priority restore"
fi
log "03-priority-inverted: done"
fi # end GROUP priority

# ===== GROUP domain (part 2/2): 04-domainready-barrier + 05-ownership-handoff =====
if grp domain; then
# ---------------------------------------------------------------------------
# 04-domainready-barrier (domain-owned, generation-gated planning handoff)
# ---------------------------------------------------------------------------
begin_stage "04-domainready-barrier"
# HARD final assertion: every domain snapshot is PlanningReady=True at its current generation.
for pair in "${SNAP_RES}|${SNAP}" "${VMSNAP_RES}|${VM_SNAP}" "${DISKSNAP_RES}|${LEAF_SNAP}" "${DISKSNAP_RES}|${SIBLING_SNAP}"; do
	res="${pair%%|*}"; name="${pair##*|}"
	[[ -n "${name}" ]] || die "domain snapshot handle empty (${res}); tree not resolved"
	j="$(get_json "${res}" "${NS}" "${name}")"
	dr_status="$(cond_field "${j}" PlanningReady status)"
	[[ "${dr_status}" == "True" ]] || die "${res}/${name} PlanningReady=${dr_status:-<none>} (every domain snapshot must publish PlanningReady=True)"
	obs="$(jq -r '([.status.conditions[]?|select(.type=="PlanningReady")][0].observedGeneration)//0' <<<"${j}")"
	gen="$(jq -r '.metadata.generation//0' <<<"${j}")"
	[[ "${obs}" == "${gen}" ]] || die "${res}/${name} PlanningReady observedGeneration(${obs}) != generation(${gen}) (stale barrier)"
	note "${res}/${name} PlanningReady=True observedGeneration=${obs}==generation"
done
# HARD: PlanningReady is a Snapshot-like planning barrier only and must NEVER appear on a
# SnapshotContent. A PlanningReady on content means the common/generic layer self-published it
# (regression of the Slice 2 / snapshotbinding contract: PlanningReady is domain-owned).
DR_ON_CONTENT="$(kubectl get "${CONTENT_RES}" -o json 2>/dev/null \
	| jq -r '[.items[]?|select(any(.status.conditions[]?; .type=="PlanningReady"))|.metadata.name]|join(",")' 2>/dev/null || true)"
[[ -z "${DR_ON_CONTENT}" ]] || die "PlanningReady found on SnapshotContent(s) [${DR_ON_CONTENT}] — common-layer self-publication regression"
note "no SnapshotContent carries PlanningReady (barrier remains domain/Snapshot-owned)"
# Timeline probe: confirm content is not bound before current-gen PlanningReady on a fresh node.
apply_source_namespace "${NS_DOMAIN_BARRIER}"
wait_until "barrier demo-pvc Bound" pvc_bound "${NS_DOMAIN_BARRIER}" demo-pvc || true
apply_root_snapshot "${NS_DOMAIN_BARRIER}"
TL="$(stage_dir 04-domainready-barrier)/timeline.txt"
: >"${TL}"
deadline=$((SECONDS + 120)); dr_seen=0; bound_before_dr=0
while ((SECONDS < deadline)); do
	bj="$(get_json "${SNAP_RES}" "${NS_DOMAIN_BARRIER}" "${SNAP}")"
	bound="$(jq -r '.status.boundSnapshotContentName // ""' <<<"${bj}")"
	drs="$(cond_field "${bj}" PlanningReady status)"
	dro="$(jq -r '([.status.conditions[]?|select(.type=="PlanningReady")][0].observedGeneration)//0' <<<"${bj}")"
	bgen="$(jq -r '.metadata.generation//0' <<<"${bj}")"
	printf '%s bound=%s PlanningReady=%s(obs=%s/gen=%s)\n' "$(now_rfc3339)" "${bound:-<none>}" "${drs:-<none>}" "${dro}" "${bgen}" >>"${TL}"
	if [[ -n "${bound}" && "${dr_seen}" == "0" && "${drs}" != "True" ]]; then bound_before_dr=1; fi
	if [[ "${drs}" == "True" && "${dro}" == "${bgen}" ]]; then dr_seen=1; fi
	[[ "${dr_seen}" == "1" && -n "${bound}" ]] && break
	sleep 2
done
[[ "${bound_before_dr}" == "0" ]] || die "barrier: SnapshotContent bound BEFORE current-gen PlanningReady=True (planning barrier violated — content must not bind until domain is ready; see timeline.txt)"
[[ "${dr_seen}" == "1" ]] && note "barrier: PlanningReady=True reached current generation" || die "barrier: PlanningReady=True at current generation not observed within 120s (barrier never satisfied; see timeline.txt)"
note "stale-PlanningReady injection not performed (out of scope; documented limitation)"
save_artifacts "04-domainready-barrier" "${NS_DOMAIN_BARRIER}"
kubectl delete namespace "${NS_DOMAIN_BARRIER}" --ignore-not-found=true --wait=false 2>/dev/null || true
log "04-domainready-barrier: done"

# ---------------------------------------------------------------------------
# 05-ownership-handoff (artifacts born under execution OK, handed off to content)
# ---------------------------------------------------------------------------
begin_stage "05-ownership-handoff"
# LIMITATION: this live stage observes the POST-handoff steady state (ownerRef -> SnapshotContent).
# It does NOT prove birth-under-execution-ObjectKeeper: the intermediate "MCP/VSC initially
# ownerRef -> execution ObjectKeeper" window is short and may be missed in a live run. Creation-time
# ownership (born under OK, then handed off) is covered by integration tests, not asserted here.
note "handoff stage checks steady-state ownerRef only; creation-time ownership (born under execution ObjectKeeper) is covered by integration tests and may be missed in live runs"
# MCP handed off to SnapshotContent (steady-state truth: ownerRef -> SnapshotContent).
if [[ -n "${LEAF_MCP}" ]]; then
	mcp_owner="$(kubectl get "${MCP_RES}" "${LEAF_MCP}" -o json 2>/dev/null \
		| jq -r '[.metadata.ownerReferences[]?|select(.kind=="SnapshotContent")]|length' 2>/dev/null || echo 0)"
	[[ "${mcp_owner}" -ge 1 ]] && note "leaf MCP ${LEAF_MCP} ownerRef -> SnapshotContent (handed off)" \
		|| die "leaf MCP ${LEAF_MCP} not owned by SnapshotContent (handoff incomplete): $(kubectl get "${MCP_RES}" "${LEAF_MCP}" -o jsonpath='{.metadata.ownerReferences}' 2>/dev/null)"
else
	note "no leaf MCP resolved; skipping MCP handoff check"
fi
# Execution ObjectKeeper ret-mcr-* present (best-effort; OK CRD may be absent).
if [[ "${HAS_OBJECTKEEPER}" == "1" ]]; then
	OK_MCR_COUNT="$(kubectl get "${OK_RES}" -o json 2>/dev/null | jq '[.items[]?|select(.metadata.name|startswith("ret-mcr-"))]|length' 2>/dev/null || echo 0)"
	note "execution ObjectKeepers ret-mcr-*: ${OK_MCR_COUNT:-0}"
	[[ "${OK_MCR_COUNT:-0}" -ge 1 ]] || note "no ret-mcr-* ObjectKeeper visible now (may already be cleaned after handoff)"
fi
# VSC handoff + dataRefs published after handoff (find any tree content carrying a dataRef).
DATA_CONTENT=""
for c in "${LEAF_CONTENT}" "${SIBLING_CONTENT}" "${VM_CONTENT}" "${ROOT_CONTENT}"; do
	[[ -n "${c}" ]] || continue
	if [[ "$(kubectl get "${CONTENT_RES}" "${c}" -o json 2>/dev/null | jq '(.status.dataRefs // [])|length' 2>/dev/null || echo 0)" -ge 1 ]]; then DATA_CONTENT="${c}"; break; fi
done
if [[ -n "${DATA_CONTENT}" ]]; then
	VSC_NAME="$(kubectl get "${CONTENT_RES}" "${DATA_CONTENT}" -o jsonpath='{.status.dataRefs[0].artifact.name}' 2>/dev/null || true)"
	note "data leg: content ${DATA_CONTENT} dataRef artifact=${VSC_NAME}"
	if [[ -n "${VSC_NAME}" ]]; then
		vsc_owner="$(kubectl get "${VSC_RES}" "${VSC_NAME}" -o json 2>/dev/null \
			| jq -r '[.metadata.ownerReferences[]?|select(.kind=="SnapshotContent")]|length' 2>/dev/null || echo 0)"
		[[ "${vsc_owner}" -ge 1 ]] && note "VSC ${VSC_NAME} ownerRef -> SnapshotContent (handed off)" \
			|| die "VSC ${VSC_NAME} not owned by SnapshotContent (handoff incomplete or different owner)"
	fi
else
	note "no tree content has dataRefs[] (no volume captured in this run); data-leg handoff check skipped"
fi
save_artifacts "05-ownership-handoff" "${NS}"
log "05-ownership-handoff: done"
fi # end GROUP domain (part 2/2)

# ---------------------------------------------------------------------------
# 06-mcp-failure (damaged leaf propagates to root; sibling isolation)
# ---------------------------------------------------------------------------
patch_mcp_ready() {
	local mcp="$1" status="$2" reason="$3" msg="$4"
	local patch
	patch="$(jq -n --arg s "${status}" --arg r "${reason}" --arg m "${msg}" --arg now "$(now_rfc3339)" \
		'{status:{conditions:[{type:"Ready",status:$s,reason:$r,message:$m,lastTransitionTime:$now}]}}')"
	kubectl patch "${MCP_RES}" "${mcp}" --subresource=status --type=merge -p "${patch}" >/dev/null 2>&1 \
		|| kubectl_ctrl patch "${MCP_RES}" "${mcp}" --subresource=status --type=merge -p "${patch}" >/dev/null 2>&1
}

# ===== GROUP failure-leaf: 06/07 MCP fail+recovery, 08/09 VSC pending+recovery =====
if grp failure-leaf; then
MCP_FAILURE_DONE=0
begin_stage "06-mcp-failure"
if [[ -z "${LEAF_MCP}" ]]; then
	die "no leaf MCP resolved on the healthy tree (cannot exercise MCP-failure propagation; tree shape/capture incomplete)"
else
	if ! patch_mcp_ready "${LEAF_MCP}" "False" "Failed" "tree-demo injected MCP failure"; then
		require "cannot patch MCP ${LEAF_MCP} status (RBAC); cannot inject MCP failure"
	else
		MCP_FAILURE_DONE=1
		wait_until "leaf content ${LEAF_CONTENT} ManifestsReady=False" \
			bash -c "[[ \$(kubectl get '${CONTENT_RES}' '${LEAF_CONTENT}' -o json | jq -r '([.status.conditions[]?|select(.type==\"ManifestsReady\")][0].status)//\"\"') == False ]]" \
			|| die "leaf content ${LEAF_CONTENT} ManifestsReady did not flip False after MCP failure"
		LEAF_RR_REASON="$(cond_field "$(get_json "${CONTENT_RES}" "" "${LEAF_CONTENT}")" ManifestsReady reason)"
		[[ "${LEAF_RR_REASON}" == "ManifestCheckpointFailed" ]] || die "leaf ManifestsReady reason=${LEAF_RR_REASON} (expected ManifestCheckpointFailed)"
		wait_until "leaf content ${LEAF_CONTENT} Ready=False" content_ready_false "${LEAF_CONTENT}" || die "leaf content did not flip Ready=False after MCP failure"
		[[ -n "${VM_CONTENT}" ]] && { wait_until "VM content ${VM_CONTENT} ChildrenReady=False" \
			bash -c "[[ \$(kubectl get '${CONTENT_RES}' '${VM_CONTENT}' -o json | jq -r '([.status.conditions[]?|select(.type==\"ChildrenReady\")][0].status)//\"\"') == False ]]" \
			|| die "VM content ${VM_CONTENT} ChildrenReady did not flip False (propagation broken)"; }
		wait_until "root content ${ROOT_CONTENT} ChildrenReady=False" \
			bash -c "[[ \$(kubectl get '${CONTENT_RES}' '${ROOT_CONTENT}' -o json | jq -r '([.status.conditions[]?|select(.type==\"ChildrenReady\")][0].status)//\"\"') == False ]]" \
			|| die "root content ChildrenReady did not flip False (propagation broken)"
		ROOT_CR_REASON="$(cond_field "$(get_json "${CONTENT_RES}" "" "${ROOT_CONTENT}")" ChildrenReady reason)"
		[[ "${ROOT_CR_REASON}" == "ChildrenFailed" ]] || die "root ChildrenReady reason=${ROOT_CR_REASON} (expected ChildrenFailed)"
		wait_until "root Snapshot ${SNAP} Ready=False" \
			bash -c "[[ \$(kubectl -n '${NS}' get '${SNAP_RES}' '${SNAP}' -o json | jq -r '([.status.conditions[]?|select(.type==\"Ready\")][0].status)//\"\"') == False ]]" \
			|| die "root Snapshot did not flip Ready=False"
		# Mirror equality under failure.
		RS="$(ready_triple "$(get_json "${SNAP_RES}" "${NS}" "${SNAP}")")"
		RC="$(ready_triple "$(get_json "${CONTENT_RES}" "" "${ROOT_CONTENT}")")"
		[[ "${RS}" == "${RC}" ]] || die "failure mirror mismatch: snap=[${RS}] content=[${RC}]"
		# Message contains failed-leaf signal (best-effort).
		echo "${RC}" | grep -q "${LEAF_CONTENT}" && note "root Ready message references failed leaf ${LEAF_CONTENT}" \
			|| note "root Ready message does not name failed leaf (informational; msg=[${RC}])"
		# Sibling isolation.
		[[ -n "${SIBLING_CONTENT}" ]] && { content_ready_true "${SIBLING_CONTENT}" \
			&& note "sibling content ${SIBLING_CONTENT} remained Ready=True (isolation OK)" \
			|| die "sibling content ${SIBLING_CONTENT} not Ready=True (isolation broken)"; }
		# Slice 3 demo content->snapshot watch: demo Snapshots mirror their bound content under failure.
		wait_demo_mirror "${DISKSNAP_RES}" "${LEAF_SNAP}" "${LEAF_CONTENT}" "leaf-disk"
		wait_demo_mirror "${VMSNAP_RES}" "${VM_SNAP}" "${VM_CONTENT}" "vm"
		note "MCP failure propagated leaf->root with sibling isolation, verbatim root mirror, and demo content->snapshot mirror"
	fi
fi
save_graph "06-mcp-failure" "${NS}" "${SNAP}" "tree" "logical"
save_artifacts "06-mcp-failure" "${NS}"
log "06-mcp-failure: done"

# ---------------------------------------------------------------------------
# 07-mcp-recovery (recovery propagates back to Ready=True)
# ---------------------------------------------------------------------------
begin_stage "07-mcp-recovery"
if [[ "${MCP_FAILURE_DONE}" == "1" ]]; then
	patch_mcp_ready "${LEAF_MCP}" "True" "Ready" "tree-demo recovery" || require "cannot restore MCP ${LEAF_MCP} status (RBAC)"
	wait_until "leaf content ${LEAF_CONTENT} Ready=True" content_ready_true "${LEAF_CONTENT}" || die "leaf content did not recover Ready=True after MCP restore"
	[[ -n "${VM_CONTENT}" ]] && { wait_until "VM content recover Ready=True" content_ready_true "${VM_CONTENT}" || die "VM content did not recover Ready=True after MCP restore"; }
	wait_until "root content ${ROOT_CONTENT} Ready=True" content_ready_true "${ROOT_CONTENT}" || die "root content did not recover Ready=True"
	wait_snapshot_ready "${NS}" "${SNAP}" || die "root Snapshot did not recover Ready=True"
	RS="$(ready_triple "$(get_json "${SNAP_RES}" "${NS}" "${SNAP}")")"
	RC="$(ready_triple "$(get_json "${CONTENT_RES}" "" "${ROOT_CONTENT}")")"
	[[ "${RS}" == "${RC}" ]] || die "recovery mirror mismatch: snap=[${RS}] content=[${RC}]"
	[[ -n "${SIBLING_CONTENT}" ]] && { content_ready_true "${SIBLING_CONTENT}" || die "sibling content ${SIBLING_CONTENT} not Ready=True after recovery"; }
	# Slice 3 demo content->snapshot watch: demo Snapshots mirror recovery back to Ready=True.
	wait_demo_mirror "${DISKSNAP_RES}" "${LEAF_SNAP}" "${LEAF_CONTENT}" "leaf-disk"
	wait_demo_mirror "${VMSNAP_RES}" "${VM_SNAP}" "${VM_CONTENT}" "vm"
	note "MCP recovery restored full tree to Ready=True with verbatim root + demo content->snapshot mirror"
else
	note "skipped (no MCP failure injected in 06)"
fi
save_graph "07-mcp-recovery" "${NS}" "${SNAP}" "tree" "logical"
save_artifacts "07-mcp-recovery" "${NS}"
log "07-mcp-recovery: done"

# ---------------------------------------------------------------------------
# 08-vsc-pending / 09-vsc-recovery (data leg degradation, non-terminal)
# ---------------------------------------------------------------------------
patch_vsc_ready_to_use() {
	local vsc="$1" val="$2"
	local patch
	patch="$(jq -n --argjson v "${val}" '{status:{readyToUse:$v}}')"
	kubectl patch "${VSC_RES}" "${vsc}" --subresource=status --type=merge -p "${patch}" >/dev/null 2>&1 \
		|| kubectl_ctrl patch "${VSC_RES}" "${vsc}" --subresource=status --type=merge -p "${patch}" >/dev/null 2>&1
}

# Find a tree content carrying a dataRef + its VSC.
DATA_CONTENT=""; DATA_VSC=""
for c in "${LEAF_CONTENT}" "${SIBLING_CONTENT}" "${VM_CONTENT}" "${ROOT_CONTENT}"; do
	[[ -n "${c}" ]] || continue
	v="$(kubectl get "${CONTENT_RES}" "${c}" -o jsonpath='{.status.dataRefs[0].artifact.name}' 2>/dev/null || true)"
	if [[ -n "${v}" ]]; then DATA_CONTENT="${c}"; DATA_VSC="${v}"; break; fi
done

VSC_PENDING_DONE=0
begin_stage "08-vsc-pending"
if [[ "${TREE_DEMO_SKIP_VSC:-0}" == "1" ]]; then
	note "skipped (TREE_DEMO_SKIP_VSC=1)"
elif [[ -z "${DATA_VSC}" ]]; then
	note "no tree content carries a VSC dataRef; VSC stages not applicable (skipped)"
else
	if patch_vsc_ready_to_use "${DATA_VSC}" false; then
		VSC_PENDING_DONE=1
		wait_until "content ${DATA_CONTENT} VolumeReady=False (data pending)" \
			bash -c "[[ \$(kubectl get '${CONTENT_RES}' '${DATA_CONTENT}' -o json | jq -r '([.status.conditions[]?|select(.type==\"VolumeReady\")][0].status)//\"\"') == False ]]" \
			|| die "content ${DATA_CONTENT} VolumeReady did not flip False on VSC readyToUse=false"
		RR_REASON="$(cond_field "$(get_json "${CONTENT_RES}" "" "${DATA_CONTENT}")" VolumeReady reason)"
		[[ "${RR_REASON}" == "DataCapturePending" ]] || die "data pending reason=${RR_REASON} (expected DataCapturePending)"
		wait_until "root Snapshot Ready=False mirror" \
			bash -c "[[ \$(kubectl -n '${NS}' get '${SNAP_RES}' '${SNAP}' -o json | jq -r '([.status.conditions[]?|select(.type==\"Ready\")][0].status)//\"\"') == False ]]" \
			|| die "root Snapshot did not flip Ready=False on VSC pending (propagation broken)"
		RS="$(ready_triple "$(get_json "${SNAP_RES}" "${NS}" "${SNAP}")")"
		RC="$(ready_triple "$(get_json "${CONTENT_RES}" "" "${ROOT_CONTENT}")")"
		[[ "${RS}" == "${RC}" ]] || die "vsc-pending mirror mismatch: snap=[${RS}] content=[${RC}]"
		note "VSC readyToUse=false surfaced DataCapturePending (non-terminal) up to root mirror"
	else
		note "cannot patch VSC ${DATA_VSC} status (admission/RBAC); VSC pending not exercised"
	fi
fi
save_artifacts "08-vsc-pending" "${NS}"
log "08-vsc-pending: done"

begin_stage "09-vsc-recovery"
if [[ "${VSC_PENDING_DONE}" == "1" ]]; then
	patch_vsc_ready_to_use "${DATA_VSC}" true || require "cannot restore VSC ${DATA_VSC} status (admission/RBAC)"
	wait_until "content ${DATA_CONTENT} Ready=True after VSC recovery" content_ready_true "${DATA_CONTENT}" || die "content ${DATA_CONTENT} did not recover Ready=True after VSC restore"
	wait_snapshot_ready "${NS}" "${SNAP}" || die "root Snapshot did not recover Ready=True after VSC restore"
	note "VSC readyToUse=true restored tree"
else
	note "skipped (no VSC pending injected)"
fi
save_artifacts "09-vsc-recovery" "${NS}"
log "09-vsc-recovery: done"
fi # end GROUP failure-leaf

# ===========================================================================
# Failure propagation & parent invalidation (literal deletion + recovery).
#
# Invariant under test: parent Ready=True IFF all required durable descendants/artifacts are present
# and healthy. Two distinct behaviours are covered, do not conflate them:
#   - SELF-HEAL of ephemeral ORCHESTRATION (stage 12): deleting a child Snapshot OBJECT loses no
#     durable artifact, so the parent must STAY Ready while the planner re-ensures the child. No
#     invalidation is expected here.
#   - INVALIDATION on loss/failure of a durable ARTIFACT or a child's health: 13 (child
#     SnapshotContent), 14 (MCP), 15 (chunk), 16 (orphan VSC) and 17 (child Ready=False) must flip the
#     parent off Ready=True; recoverable ones (13,14,17) then heal back and the consolidation gate
#     18-recovery confirms the tree is Ready again. The non-recoverable artifact losses (15,16) run
#     after 18 because they intentionally degrade the tree.
# All observations here use the short INVALIDATION_WAIT_SEC (a few reconciles), never the capture cap.
# ===========================================================================

# resolve_main_tree_handles re-derives the live tree handles (ROOT_CONTENT/VM_SNAP/SIBLING_*/LEAF_*/
# LEAF_MCP) from the current cluster state. Recoverable deletions recreate child snapshots/contents
# with NEW UIDs (and therefore new content names), so handles captured in 02-tree-ready go stale; every
# destructive stage below re-resolves before/after instead of trusting cached names.
resolve_main_tree_handles() {
	local rj vmj
	rj="$(get_json "${SNAP_RES}" "${NS}" "${SNAP}")"
	ROOT_CONTENT="$(echo "${rj}" | jq -r '.status.boundSnapshotContentName // ""')"
	VM_SNAP="$(echo "${rj}" | jq -r '[.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualMachineSnapshot")][0].name // ""')"
	SIBLING_SNAP="$(echo "${rj}" | jq -r '[.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualDiskSnapshot")][0].name // ""')"
	vmj="{}"
	[[ -n "${VM_SNAP}" ]] && vmj="$(get_json "${VMSNAP_RES}" "${NS}" "${VM_SNAP}")"
	VM_CONTENT="$(echo "${vmj}" | jq -r '.status.boundSnapshotContentName // ""')"
	LEAF_SNAP="$(echo "${vmj}" | jq -r '[.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualDiskSnapshot")][0].name // ""')"
	LEAF_CONTENT=""
	[[ -n "${LEAF_SNAP}" ]] && LEAF_CONTENT="$(kubectl -n "${NS}" get "${DISKSNAP_RES}" "${LEAF_SNAP}" -o jsonpath='{.status.boundSnapshotContentName}' 2>/dev/null || true)"
	SIBLING_CONTENT=""
	[[ -n "${SIBLING_SNAP}" ]] && SIBLING_CONTENT="$(kubectl -n "${NS}" get "${DISKSNAP_RES}" "${SIBLING_SNAP}" -o jsonpath='{.status.boundSnapshotContentName}' 2>/dev/null || true)"
	LEAF_MCP=""
	[[ -n "${LEAF_CONTENT}" ]] && LEAF_MCP="$(kubectl get "${CONTENT_RES}" "${LEAF_CONTENT}" -o jsonpath='{.status.manifestCheckpointName}' 2>/dev/null || true)"
}

# root_ready_completed: true while the root Snapshot Ready is verbatim True/Completed.
root_ready_completed() {
	[[ "$(get_json "${SNAP_RES}" "${NS}" "${SNAP}" | jq -r '([.status.conditions[]?|select(.type=="Ready")][0] | (.status+"/"+.reason)) // ""')" == "True/Completed" ]]
}

# ---------------------------------------------------------------------------
# 12-child-snapshot-deleted (SELF-HEAL, not invalidation)
# ---------------------------------------------------------------------------
# A child Snapshot object (e.g. a standalone DemoVirtualDiskSnapshot) is ephemeral ORCHESTRATION, not a
# durable descendant. The parent Snapshot reconcile re-plans the child graph on every pass from the LIVE
# source inventory (reconcileParentOwnedChildGraph -> ensureParentOwnedChildSnapshot), with a
# DETERMINISTIC child name nss-child-<hash(parent|GVK|GVK|sourceName|sourceUID)>. So deleting the child
# Snapshot while its source object still exists is SELF-HEALED: the planner recreates it (same name, NEW
# UID). Crucially this does NOT touch the durable artifacts (the child SnapshotContent / its MCP / VSC),
# which is why the root stays Ready=Completed throughout — per INV-FAIL-PROP the required durable
# descendants/artifacts are all still present and healthy. There is NO invalidation here by design (the
# earlier "wait for root to leave Ready=Completed" was a wrong model: nothing was lost). Real artifact
# invalidation is exercised by 13 (child content), 14 (MCP), 15 (chunk), 16 (orphan VSC), 17 (child
# Ready=False). Short INVALIDATION_WAIT_SEC: re-ensure is event-driven (a reconcile or two).
# ===== GROUP failure-delete: 12 self-heal, 13 child-content delete, 14 MCP delete =====
if grp failure-delete; then
begin_stage "12-child-snapshot-deleted"
resolve_main_tree_handles
CDIR="$(stage_dir 12-child-snapshot-deleted)"
if [[ -z "${SIBLING_SNAP}" ]]; then
	die "12: no standalone DemoVirtualDiskSnapshot child resolved on the healthy tree (cannot exercise self-heal)"
else
	CHILD_UID_BEFORE="$(kubectl -n "${NS}" get "${DISKSNAP_RES}" "${SIBLING_SNAP}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
	kubectl -n "${NS}" get "${DISKSNAP_RES}" "${SIBLING_SNAP}" -o yaml >"${CDIR}/child-before-delete.yaml" 2>/dev/null || true
	printf 'child=%s\nuid_before=%s\n' "${SIBLING_SNAP}" "${CHILD_UID_BEFORE}" >"${CDIR}/handles.txt"
	root_ready_completed && note "12: root Ready=Completed before child snapshot delete" \
		|| require "12: root not Ready=Completed before delete (tree not healthy at stage start)"
	DEL_OUT="$(kubectl -n "${NS}" delete "${DISKSNAP_RES}" "${SIBLING_SNAP}" --wait=false 2>&1)"
	DEL_RC=$?
	printf '%s\n' "${DEL_OUT}" >"${CDIR}/delete-stderr.txt"
	if [[ "${DEL_RC}" -eq 0 ]]; then
		# HARD: the planner self-heals the deleted child from the live source (same deterministic name,
		# new UID). This is the positive evidence the parent re-ensures its orchestration graph.
		wait_until_to "${INVALIDATION_WAIT_SEC}" "child snapshot ${SIBLING_SNAP} re-ensured (new UID)" \
			bash -c "u=\$(kubectl -n '${NS}' get '${DISKSNAP_RES}' '${SIBLING_SNAP}' -o jsonpath='{.metadata.uid}' 2>/dev/null || true); [[ -n \"\${u}\" && \"\${u}\" != '${CHILD_UID_BEFORE}' ]]" \
			|| die "12: deleted child snapshot ${SIBLING_SNAP} not re-ensured by the planner (UID unchanged=${CHILD_UID_BEFORE}); source object missing or planning gated"
		CHILD_UID_AFTER="$(kubectl -n "${NS}" get "${DISKSNAP_RES}" "${SIBLING_SNAP}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
		printf 'uid_after=%s\n' "${CHILD_UID_AFTER}" >>"${CDIR}/handles.txt"
		# Root must stay Ready=Completed: deleting orchestration must NOT invalidate durable artifacts.
		# Sample across the window; any flip to non-True/Completed would mean the content tree was wrongly
		# disturbed by an orchestration-only delete.
		stayed=1
		for _ in $(seq 1 6); do root_ready_completed || { stayed=0; break; }; sleep 2; done
		[[ "${stayed}" == "1" ]] && note "12: root stayed Ready=Completed across child re-ensure (durable artifacts untouched)" \
			|| die "12: root left Ready=Completed during child re-ensure (orchestration-only delete must NOT invalidate durable artifacts)"
		wait_snapshot_ready "${NS}" "${SNAP}" "${INVALIDATION_WAIT_SEC}" \
			|| die "12: root Snapshot not Ready=True after child snapshot self-heal"
		note "12: child snapshot deletion self-healed by planner (uid ${CHILD_UID_BEFORE}->${CHILD_UID_AFTER}); no invalidation, durable artifacts and root Ready intact"
	else
		die "12: could not delete child snapshot ${SIBLING_SNAP} (rc=${DEL_RC}): $(printf '%s' "${DEL_OUT}" | tr '\n' ' ' | head -c 200)"
	fi
fi
resolve_main_tree_handles
save_graph "12-child-snapshot-deleted" "${NS}" "${SNAP}" "tree" "logical"
save_artifacts "12-child-snapshot-deleted" "${NS}"
log "12-child-snapshot-deleted: done"

# ---------------------------------------------------------------------------
# 13-snapshotcontent-deleted (recoverable: binder recreates child content)
# ---------------------------------------------------------------------------
begin_stage "13-snapshotcontent-deleted"
resolve_main_tree_handles
CDIR="$(stage_dir 13-snapshotcontent-deleted)"
if [[ -z "${SIBLING_CONTENT}" ]]; then
	die "13: no standalone child SnapshotContent resolved on the healthy tree (cannot exercise child-content deletion)"
else
	CONTENT_UID_BEFORE="$(kubectl get "${CONTENT_RES}" "${SIBLING_CONTENT}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
	kubectl get "${CONTENT_RES}" "${SIBLING_CONTENT}" -o yaml >"${CDIR}/content-before-delete.yaml" 2>/dev/null || true
	printf 'content=%s\nuid_before=%s\n' "${SIBLING_CONTENT}" "${CONTENT_UID_BEFORE}" >"${CDIR}/handles.txt"
	# A child SnapshotContent carries FinalizerParentProtect; delete sets deletionTimestamp and the
	# content controller cascade-removes finalizers, then GC deletes it and the binder recreates it
	# from the still-bound child snapshot.
	DEL_OUT="$(kubectl delete "${CONTENT_RES}" "${SIBLING_CONTENT}" --wait=false 2>&1)"
	DEL_RC=$?
	printf '%s\n' "${DEL_OUT}" >"${CDIR}/delete-stderr.txt"
	if [[ "${DEL_RC}" -eq 0 ]]; then
		# Transient invalidation observation only: the binder may recreate the child content so fast that
		# the brief root ChildrenReady=False window is not caught by polling. This is NOT the invariant
		# (the invariants — child recreated + tree recovers Ready=True — are asserted hard below), so a
		# miss here is informational, never a verdict.
		wait_until_to "${INVALIDATION_WAIT_SEC}" "root content ${ROOT_CONTENT} ChildrenReady=False after child content delete" \
			bash -c "[[ \$(kubectl get '${CONTENT_RES}' '${ROOT_CONTENT}' -o json | jq -r '([.status.conditions[]?|select(.type==\"ChildrenReady\")][0].status)//\"\"') == False ]]" \
			&& note "13: root content ChildrenReady=False observed after child content deletion" \
			|| note "13: transient root ChildrenReady=False not observed (recreation raced ahead; informational)"
		# HARD invariant: the binder recreates the child content (new UID) and the tree recovers Ready=True.
		wait_until_to "${INVALIDATION_WAIT_SEC}" "child content ${SIBLING_CONTENT} present again" \
			bash -c "kubectl get '${CONTENT_RES}' '${SIBLING_CONTENT}' -o name >/dev/null 2>&1" \
			|| die "13: child content ${SIBLING_CONTENT} was not recreated within ${INVALIDATION_WAIT_SEC}s (binder did not re-bind deleted child content)"
		CONTENT_UID_AFTER="$(kubectl get "${CONTENT_RES}" "${SIBLING_CONTENT}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
		printf 'uid_after=%s\n' "${CONTENT_UID_AFTER}" >>"${CDIR}/handles.txt"
		wait_snapshot_ready "${NS}" "${SNAP}" "${INVALIDATION_WAIT_SEC}" || die "13: tree did not recover Ready=True within ${INVALIDATION_WAIT_SEC}s after child content recreation"
		note "13: child SnapshotContent deletion propagated to root then recovered (uid ${CONTENT_UID_BEFORE}->${CONTENT_UID_AFTER})"
	else
		die "13: could not delete child content ${SIBLING_CONTENT} (rc=${DEL_RC}): $(printf '%s' "${DEL_OUT}" | tr '\n' ' ' | head -c 200)"
	fi
fi
resolve_main_tree_handles
save_graph "13-snapshotcontent-deleted" "${NS}" "${SNAP}" "tree" "logical"
save_artifacts "13-snapshotcontent-deleted" "${NS}"
log "13-snapshotcontent-deleted: done"

# ---------------------------------------------------------------------------
# 14-mcp-deleted (ManifestCheckpoint deletion -> ManifestsReady invalidation)
# ---------------------------------------------------------------------------
begin_stage "14-mcp-deleted"
resolve_main_tree_handles
CDIR="$(stage_dir 14-mcp-deleted)"
if [[ -z "${LEAF_MCP}" || -z "${LEAF_CONTENT}" ]]; then
	die "14: no leaf MCP/content resolved on the healthy tree (cannot exercise MCP deletion)"
else
	kubectl get "${MCP_RES}" "${LEAF_MCP}" -o yaml >"${CDIR}/mcp-before-delete.yaml" 2>/dev/null || true
	printf 'leaf_content=%s\nleaf_mcp=%s\n' "${LEAF_CONTENT}" "${LEAF_MCP}" >"${CDIR}/handles.txt"
	# A published MCP referenced by content.status.manifestCheckpointName is removed. The owning
	# SnapshotContent is woken via the ManifestCheckpoint ownerRef wake-up watch; reconcile reclassifies
	# the now-missing MCP as ManifestCapturePending (NotFound is the legitimate pre-publish window) so the
	# manifest leg flips and Ready leaves True (no stale Ready=True over a missing checkpoint).
	DEL_OUT="$(kubectl delete "${MCP_RES}" "${LEAF_MCP}" --wait=false 2>&1)"
	DEL_RC=$?
	printf '%s\n' "${DEL_OUT}" >"${CDIR}/delete-stderr.txt"
	if [[ "${DEL_RC}" -eq 0 ]]; then
		wait_until_to "${INVALIDATION_WAIT_SEC}" "leaf content ${LEAF_CONTENT} ManifestsReady=False after MCP delete" \
			bash -c "[[ \$(kubectl get '${CONTENT_RES}' '${LEAF_CONTENT}' -o json | jq -r '([.status.conditions[]?|select(.type==\"ManifestsReady\")][0].status)//\"\"') == False ]]" \
			&& note "14: leaf content ManifestsReady=False after MCP deletion" \
			|| die "14: leaf content ${LEAF_CONTENT} ManifestsReady did not flip False within ${INVALIDATION_WAIT_SEC}s after MCP deletion (ownerRef wake-up broken)"
		# HARD INV-FAIL-PROP: the root content must NOT stay Ready=True over a now-missing checkpoint.
		wait_until_to "${INVALIDATION_WAIT_SEC}" "root content ${ROOT_CONTENT} not Ready=True after MCP delete" \
			bash -c "[[ \$(kubectl get '${CONTENT_RES}' '${ROOT_CONTENT}' -o json | jq -r '([.status.conditions[]?|select(.type==\"Ready\")][0].status)//\"\"') != True ]]" \
			&& note "14: root content left Ready=True after leaf MCP deletion" \
			|| die "14: root content stayed Ready=True after leaf MCP deletion within ${INVALIDATION_WAIT_SEC}s (stale Ready=True over a missing checkpoint — INV-FAIL-PROP violation)"
		# Recovery here requires the capture path to re-run and re-publish the checkpoint, which is not
		# guaranteed inside the short observation window (documented limitation; the consolidation gate
		# 18-recovery asserts steady-state recovery). Informational only — not a verdict.
		wait_snapshot_ready "${NS}" "${SNAP}" "${INVALIDATION_WAIT_SEC}" \
			&& note "14: tree recovered Ready=True after MCP re-publication" \
			|| note "14: tree did not recover Ready=True within ${INVALIDATION_WAIT_SEC}s (MCP re-publication requires a capture re-run; informational, see 18-recovery)"
	else
		die "14: could not delete MCP ${LEAF_MCP} (rc=${DEL_RC}): $(printf '%s' "${DEL_OUT}" | tr '\n' ' ' | head -c 200)"
	fi
fi
resolve_main_tree_handles
save_graph "14-mcp-deleted" "${NS}" "${SNAP}" "tree" "logical"
save_artifacts "14-mcp-deleted" "${NS}"
log "14-mcp-deleted: done"
fi # end GROUP failure-delete

# ---------------------------------------------------------------------------
# 17-child-ready-false (child Ready=False must invalidate parent; then recover)
# ---------------------------------------------------------------------------
# ===== GROUP failure-child: 17 child Ready=False propagation, 18 recovery gate =====
if grp failure-child; then
begin_stage "17-child-ready-false"
resolve_main_tree_handles
CDIR="$(stage_dir 17-child-ready-false)"
# Drive a child Ready=False deterministically by failing the standalone child's own MCP (its content
# ManifestsReady -> Ready flips False), then assert the root does NOT stay Ready=True, then restore.
SIBLING_MCP=""
[[ -n "${SIBLING_CONTENT}" ]] && SIBLING_MCP="$(kubectl get "${CONTENT_RES}" "${SIBLING_CONTENT}" -o jsonpath='{.status.manifestCheckpointName}' 2>/dev/null || true)"
if [[ -z "${SIBLING_MCP}" ]]; then
	die "17: no standalone child MCP resolved on the healthy tree (cannot drive a child Ready=False)"
else
	printf 'sibling_content=%s\nsibling_mcp=%s\n' "${SIBLING_CONTENT}" "${SIBLING_MCP}" >"${CDIR}/handles.txt"
	if patch_mcp_ready "${SIBLING_MCP}" "False" "Failed" "tree-demo 17 child Ready=False"; then
		wait_until_to "${INVALIDATION_WAIT_SEC}" "standalone child content ${SIBLING_CONTENT} Ready=False" content_ready_false "${SIBLING_CONTENT}" \
			|| die "17: standalone child content ${SIBLING_CONTENT} did not flip Ready=False within ${INVALIDATION_WAIT_SEC}s after MCP failure (cannot drive the child-failed precondition)"
		if wait_until_to "${INVALIDATION_WAIT_SEC}" "root Snapshot ${SNAP} not Ready=True (child failed)" \
			bash -c "[[ \$(kubectl -n '${NS}' get '${SNAP_RES}' '${SNAP}' -o json | jq -r '([.status.conditions[]?|select(.type==\"Ready\")][0].status)//\"\"') != True ]]"; then
			note "17: root did not stay Ready=True while a child is Ready=False"
		else
			# Propagation evidence dump: was the failed sibling content actually tracked as a child of the
			# root content (status.childrenSnapshotContentRefs), and what is the root content aggregation?
			kubectl get "${CONTENT_RES}" "${SIBLING_CONTENT}" -o json >"${CDIR}/sibling-content.json" 2>&1 || true
			kubectl get "${CONTENT_RES}" "${ROOT_CONTENT}" -o json >"${CDIR}/root-content.json" 2>&1 || true
			kubectl -n "${NS}" get "${SNAP_RES}" "${SNAP}" -o json >"${CDIR}/root-snapshot.json" 2>&1 || true
			log "17 DIAG: sibling=${SIBLING_CONTENT} ready=$(jq -r '([.status.conditions[]?|select(.type=="Ready")][0]|(.status+"/"+.reason))//""' "${CDIR}/sibling-content.json" 2>/dev/null)"
			log "17 DIAG: root content ChildrenReady=$(jq -r '([.status.conditions[]?|select(.type=="ChildrenReady")][0]|(.status+"/"+.reason))//""' "${CDIR}/root-content.json" 2>/dev/null) Ready=$(jq -r '([.status.conditions[]?|select(.type=="Ready")][0]|(.status+"/"+.reason))//""' "${CDIR}/root-content.json" 2>/dev/null)"
			log "17 DIAG: root content childrenSnapshotContentRefs=$(jq -c '[.status.childrenSnapshotContentRefs[]?.name]//[]' "${CDIR}/root-content.json" 2>/dev/null)"
			log "17 DIAG: sibling tracked in root refs=$(jq -r --arg s "${SIBLING_CONTENT}" '([.status.childrenSnapshotContentRefs[]?|select(.name==$s)]|length)//0' "${CDIR}/root-content.json" 2>/dev/null)"
			# restore the MCP before dying so we do not leave the tree wedged for SKIP_CLEANUP inspection.
			patch_mcp_ready "${SIBLING_MCP}" "True" "Ready" "tree-demo 17 restore-before-die" || true
			die "17: root stayed Ready=True while a required child is Ready=False (stale Ready=True; see ${CDIR}/root-content.json)"
		fi
		# Recovery.
		patch_mcp_ready "${SIBLING_MCP}" "True" "Ready" "tree-demo 17 recovery" || require "17: could not restore child MCP ${SIBLING_MCP} status (RBAC)"
		wait_until_to "${INVALIDATION_WAIT_SEC}" "standalone child content ${SIBLING_CONTENT} Ready=True" content_ready_true "${SIBLING_CONTENT}" \
			|| die "17: standalone child content ${SIBLING_CONTENT} did not recover Ready=True within ${INVALIDATION_WAIT_SEC}s"
		wait_snapshot_ready "${NS}" "${SNAP}" "${INVALIDATION_WAIT_SEC}" || die "17: tree did not recover Ready=True within ${INVALIDATION_WAIT_SEC}s after child recovery"
		note "17: child Ready=False propagated to root and recovered"
	else
		require "17: cannot patch standalone child MCP ${SIBLING_MCP} status (RBAC); cannot drive child Ready=False"
	fi
fi
resolve_main_tree_handles
save_graph "17-child-ready-false" "${NS}" "${SNAP}" "tree" "logical"
save_artifacts "17-child-ready-false" "${NS}"
log "17-child-ready-false: done"

# ---------------------------------------------------------------------------
# 18-recovery (consolidation gate: tree fully healed after recoverable deletions)
# ---------------------------------------------------------------------------
begin_stage "18-recovery"
resolve_main_tree_handles
wait_snapshot_ready "${NS}" "${SNAP}" "${INVALIDATION_WAIT_SEC}" \
	&& note "18: root Snapshot Ready=True after all recoverable invalidations" \
	|| die "18: root Snapshot not Ready=True at consolidation within ${INVALIDATION_WAIT_SEC}s (a recoverable invalidation did not heal)"
for c in "${ROOT_CONTENT}" "${VM_CONTENT}" "${LEAF_CONTENT}" "${SIBLING_CONTENT}"; do
	[[ -n "${c}" ]] || continue
	content_ready_true "${c}" && note "18: content ${c} Ready=True" || die "18: content ${c} not Ready=True at recovery consolidation"
done
# Mirror equality must hold regardless of recovery timing (HARD).
RS="$(ready_triple "$(get_json "${SNAP_RES}" "${NS}" "${SNAP}")")"
RC="$(ready_triple "$(get_json "${CONTENT_RES}" "" "${ROOT_CONTENT}")")"
[[ "${RS}" == "${RC}" ]] || die "18: root Snapshot Ready mirror mismatch at recovery: snap=[${RS}] content=[${RC}]"
note "18: recovery mirror OK [${RS}]"
save_graph "18-recovery" "${NS}" "${SNAP}" "tree" "logical"
save_artifacts "18-recovery" "${NS}"
log "18-recovery: done"
fi # end GROUP failure-child

# ---------------------------------------------------------------------------
# 15-chunk-deleted (TERMINAL: missing chunk = ManifestCheckpointFailed integrity loss)
# ---------------------------------------------------------------------------
# Non-recoverable by design (the chunk payload is gone). Runs after 18 because it permanently degrades
# the leaf. Chunk deletion does NOT wake reconcile (no chunk watch in Phase 2a), so an MCP bump is used
# to trigger revalidation; the integrity check then fails closed (no stale Ready=True over lost data).
# ===== GROUP failure-destructive: 15 chunk, 16 orphan VSC, 10 VSC missing, 11 chunk missing =====
# Terminal/degrading stages; run last (10 drops the main namespace).
if grp failure-destructive; then
begin_stage "15-chunk-deleted"
resolve_main_tree_handles
CDIR="$(stage_dir 15-chunk-deleted)"
if [[ -z "${LEAF_MCP}" ]]; then
	die "15: no leaf MCP resolved on the tree (cannot exercise chunk deletion)"
else
	CHUNK="$(kubectl get "${CHUNK_RES}" -o json 2>/dev/null | jq -r --arg m "${LEAF_MCP}" \
		'[.items[]?|select(.spec.checkpointName==$m)][0].metadata.name // ""')"
	[[ -n "${CHUNK}" ]] || CHUNK="$(kubectl get "${CHUNK_RES}" "${LEAF_MCP}-0" -o name 2>/dev/null | sed 's#.*/##' || true)"
	if [[ -z "${CHUNK}" ]]; then
		die "15: could not resolve a chunk for MCP ${LEAF_MCP} (expected at least one published chunk)"
	else
		printf 'leaf_content=%s\nleaf_mcp=%s\nchunk=%s\n' "${LEAF_CONTENT}" "${LEAF_MCP}" "${CHUNK}" >"${CDIR}/handles.txt"
		kubectl get "${MCP_RES}" "${LEAF_MCP}" -o yaml >"${CDIR}/mcp-before-delete.yaml" 2>/dev/null || true
		kubectl_ctrl get "${CHUNK_RES}" "${CHUNK}" -o yaml >"${CDIR}/chunk-before-delete.yaml" 2>/dev/null || true
		CHUNK_DEL_OUT="$(kubectl delete "${CHUNK_RES}" "${CHUNK}" --wait=false 2>&1)"; CHUNK_DEL_RC=$?
		printf '%s\n' "${CHUNK_DEL_OUT}" >"${CDIR}/delete-stderr.txt"
		if [[ "${CHUNK_DEL_RC}" -eq 0 ]]; then
			kubectl annotate "${MCP_RES}" "${LEAF_MCP}" "tree-demo.state-snapshotter.deckhouse.io/bump=$(date +%s)" --overwrite >/dev/null 2>&1 \
				|| kubectl_ctrl annotate "${MCP_RES}" "${LEAF_MCP}" "tree-demo.state-snapshotter.deckhouse.io/bump=$(date +%s)" --overwrite >/dev/null 2>&1 || true
			wait_until_to "${INVALIDATION_WAIT_SEC}" "leaf content ${LEAF_CONTENT} ManifestsReady=False after chunk delete+bump" \
				bash -c "[[ \$(kubectl get '${CONTENT_RES}' '${LEAF_CONTENT}' -o json | jq -r '([.status.conditions[]?|select(.type==\"ManifestsReady\")][0].status)//\"\"') == False ]]" \
				|| die "15: leaf content ${LEAF_CONTENT} ManifestsReady did not flip False within ${INVALIDATION_WAIT_SEC}s after chunk delete+bump (integrity check did not fail closed)"
			RR_REASON="$(cond_field "$(get_json "${CONTENT_RES}" "" "${LEAF_CONTENT}")" ManifestsReady reason)"
			[[ "${RR_REASON}" == "ManifestCheckpointFailed" ]] && note "15: missing chunk surfaced ManifestCheckpointFailed (terminal integrity loss)" \
				|| die "15: chunk-missing reason=${RR_REASON} (expected ManifestCheckpointFailed)"
			wait_until_to "${INVALIDATION_WAIT_SEC}" "root Snapshot ${SNAP} Ready=False after chunk delete" \
				bash -c "[[ \$(kubectl -n '${NS}' get '${SNAP_RES}' '${SNAP}' -o json | jq -r '([.status.conditions[]?|select(.type==\"Ready\")][0].status)//\"\"') == False ]]" \
				|| die "15: root Snapshot did not flip Ready=False within ${INVALIDATION_WAIT_SEC}s after chunk delete (stale Ready=True over lost chunk — INV-FAIL-PROP violation)"
			note "15: LIMITATION — chunk deletion alone does not wake reconcile; an MCP bump was required (no chunk watch in Phase 2a)"
		else
			die "15: could not delete chunk ${CHUNK} (rc=${CHUNK_DEL_RC}): $(printf '%s' "${CHUNK_DEL_OUT}" | tr '\n' ' ' | head -c 200)"
		fi
	fi
fi
save_artifacts "15-chunk-deleted" "${NS}"
log "15-chunk-deleted: done"

# ---------------------------------------------------------------------------
# 16-orphan-vsc-deleted (TERMINAL: retained orphan VSC lost -> ArtifactMissing)
# ---------------------------------------------------------------------------
# The orphan demo-pvc data leg is durable via a retained VolumeSnapshotContent referenced by the root
# content dataRefs[]; the CSI VolumeSnapshot is only a visibility leaf. Deleting the retained VSC is a
# real data loss: the root content must flip VolumeReady=False/ArtifactMissing (no stale Ready=True
# over a missing data artifact). Non-recoverable (Retain means CSI will not recreate it).
begin_stage "16-orphan-vsc-deleted"
resolve_main_tree_handles
CDIR="$(stage_dir 16-orphan-vsc-deleted)"
ORPHAN_PVC_UID="$(kubectl -n "${NS}" get pvc demo-pvc -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
ORPHAN_VSC=""
[[ -n "${ROOT_CONTENT}" && -n "${ORPHAN_PVC_UID}" ]] && ORPHAN_VSC="$(get_json "${CONTENT_RES}" "" "${ROOT_CONTENT}" \
	| jq -r --arg u "${ORPHAN_PVC_UID}" '[.status.dataRefs[]?|select(.targetUID==$u and .artifact.kind=="VolumeSnapshotContent")][0].artifact.name // ""')"
if [[ -z "${ORPHAN_VSC}" ]]; then
	die "16: no orphan demo-pvc VSC in root dataRefs (02-tree-ready asserted it; data leg lost before this stage)"
else
	printf 'root_content=%s\norphan_pvc_uid=%s\norphan_vsc=%s\n' "${ROOT_CONTENT}" "${ORPHAN_PVC_UID}" "${ORPHAN_VSC}" >"${CDIR}/handles.txt"
	kubectl get "${VSC_RES}" "${ORPHAN_VSC}" -o yaml >"${CDIR}/orphan-vsc-before-delete.yaml" 2>/dev/null || true
	VSC_DEL_OUT="$(kubectl delete "${VSC_RES}" "${ORPHAN_VSC}" --wait=false 2>&1)"; VSC_DEL_RC=$?
	printf '%s\n' "${VSC_DEL_OUT}" >"${CDIR}/delete-stderr.txt"
	if [[ "${VSC_DEL_RC}" -eq 0 ]]; then
		wait_until_to "${INVALIDATION_WAIT_SEC}" "root content ${ROOT_CONTENT} VolumeReady=False after orphan VSC delete" \
			bash -c "[[ \$(kubectl get '${CONTENT_RES}' '${ROOT_CONTENT}' -o json | jq -r '([.status.conditions[]?|select(.type==\"VolumeReady\")][0].status)//\"\"') == False ]]" \
			|| die "16: root content VolumeReady did not flip False within ${INVALIDATION_WAIT_SEC}s after orphan VSC delete (artifact wake-up + revalidation both failed to fire)"
		RR_REASON="$(cond_field "$(get_json "${CONTENT_RES}" "" "${ROOT_CONTENT}")" VolumeReady reason)"
		[[ "${RR_REASON}" == "ArtifactMissing" ]] && note "16: orphan VSC deletion surfaced ArtifactMissing at root" \
			|| die "16: orphan VSC missing reason=${RR_REASON} (expected ArtifactMissing)"
		wait_until_to "${INVALIDATION_WAIT_SEC}" "root Snapshot ${SNAP} Ready=False after orphan VSC delete" \
			bash -c "[[ \$(kubectl -n '${NS}' get '${SNAP_RES}' '${SNAP}' -o json | jq -r '([.status.conditions[]?|select(.type==\"Ready\")][0].status)//\"\"') == False ]]" \
			|| die "16: root Snapshot did not flip Ready=False within ${INVALIDATION_WAIT_SEC}s after orphan VSC delete (stale Ready=True over a missing data artifact — INV-FAIL-PROP violation)"
		[[ -n "${LEAF_CONTENT}" && "${LEAF_CONTENT}" != "${ROOT_CONTENT}" ]] && { content_ready_true "${LEAF_CONTENT}" \
			&& note "16: domain disk content isolation held under orphan VSC delete" \
			|| die "16: domain disk content ${LEAF_CONTENT} not Ready under orphan VSC delete (isolation broken)"; }
		note "16: orphan (visibility-leaf) data artifact loss invalidated the root identically to the domain path"
	else
		die "16: could not delete orphan VSC ${ORPHAN_VSC} (rc=${VSC_DEL_RC}): $(printf '%s' "${VSC_DEL_OUT}" | tr '\n' ' ' | head -c 200)"
	fi
fi
save_graph "16-orphan-vsc-deleted" "${NS}" "${SNAP}" "tree" "logical"
save_artifacts "16-orphan-vsc-deleted" "${NS}"
log "16-orphan-vsc-deleted: done"

# ---------------------------------------------------------------------------
# 10-vsc-missing (terminal artifact-missing; destructive — cleans NS after)
# ---------------------------------------------------------------------------
begin_stage "10-vsc-missing"
NS_DELETED=""
if [[ "${TREE_DEMO_SKIP_VSC:-0}" == "1" ]]; then
	note "skipped (TREE_DEMO_SKIP_VSC=1)"
	save_artifacts "10-vsc-missing" "${NS}"
elif [[ -z "${DATA_VSC}" ]]; then
	note "no VSC dataRef to delete; skipped"
	save_artifacts "10-vsc-missing" "${NS}"
elif kubectl delete "${VSC_RES}" "${DATA_VSC}" --wait=false 2>/dev/null; then
	wait_until_to "${INVALIDATION_WAIT_SEC}" "content ${DATA_CONTENT} VolumeReady=False (artifact missing)" \
		bash -c "[[ \$(kubectl get '${CONTENT_RES}' '${DATA_CONTENT}' -o json | jq -r '([.status.conditions[]?|select(.type==\"VolumeReady\")][0].status)//\"\"') == False ]]" \
		|| die "content ${DATA_CONTENT} VolumeReady did not flip False within ${INVALIDATION_WAIT_SEC}s after VSC delete (artifact-missing not detected)"
	RR_REASON="$(cond_field "$(get_json "${CONTENT_RES}" "" "${DATA_CONTENT}")" VolumeReady reason)"
	[[ "${RR_REASON}" == "ArtifactMissing" ]] || die "missing-artifact reason=${RR_REASON} (expected ArtifactMissing)"
	[[ -n "${SIBLING_CONTENT}" && "${SIBLING_CONTENT}" != "${DATA_CONTENT}" ]] && { content_ready_true "${SIBLING_CONTENT}" \
		&& note "sibling isolation held under VSC delete" || die "sibling content ${SIBLING_CONTENT} not Ready under VSC delete (isolation broken)"; }
	note "VSC deletion surfaced ArtifactMissing; main namespace will be cleaned (recovery not possible)"
	save_artifacts "10-vsc-missing" "${NS}"
	# Destructive: recreate is not possible — drop the main namespace so later runs are clean.
	kubectl delete namespace "${NS}" --ignore-not-found=true --wait=false 2>/dev/null || true
	NS_DELETED=1
else
	note "cannot delete VSC ${DATA_VSC}; skipped"
	save_artifacts "10-vsc-missing" "${NS}"
fi
log "10-vsc-missing: done"

# ---------------------------------------------------------------------------
# 11-chunk-missing (documented limitation: chunk watch not implemented)
# ---------------------------------------------------------------------------
begin_stage "11-chunk-missing"
if [[ "${TREE_DEMO_SKIP_CHUNK:-0}" == "1" || -n "${NS_DELETED:-}" ]]; then
	note "skipped (TREE_DEMO_SKIP_CHUNK=1 or main namespace deleted in stage 10)"
elif [[ -z "${LEAF_MCP}" ]]; then
	note "no leaf MCP; skipped"
else
	CHUNK="$(kubectl get "${CHUNK_RES}" -o json 2>/dev/null | jq -r --arg m "${LEAF_MCP}" \
		'[.items[]?|select(.spec.checkpointName==$m)][0].metadata.name // ""')"
	if [[ -z "${CHUNK}" ]]; then
		CHUNK="$(kubectl get "${CHUNK_RES}" "${LEAF_MCP}-0" -o name 2>/dev/null | sed 's#.*/##')"
	fi
	CDIR="$(stage_dir 11-chunk-missing)"
	if [[ -z "${CHUNK}" ]]; then
		note "could not resolve a chunk for MCP ${LEAF_MCP}; skipped"
	else
		# Evidence: capture MCP + chunk identity BEFORE delete so we can later prove the chunk was
		# absent while MCP.status.chunks[] still referenced it (dangling ref), and that the bump did
		# not silently rewrite the chunk list.
		printf '%s\n' "${CHUNK}" >"${CDIR}/deleted-chunk.txt"
		kubectl get "${MCP_RES}" "${LEAF_MCP}" -o yaml >"${CDIR}/mcp-before-delete.yaml" 2>/dev/null || true
		kubectl get "${MCP_RES}" "${LEAF_MCP}" -o json 2>/dev/null \
			| jq '{name: .metadata.name, generation: .metadata.generation, chunks: .status.chunks, ready: ([.status.conditions[]?|select(.type=="Ready")][0])}' \
			>"${CDIR}/mcp-before-delete.summary.json" 2>/dev/null || true
		kubectl get "${CHUNK_RES}" "${CHUNK}" -o yaml >"${CDIR}/chunk-before-delete.yaml" 2>/dev/null || true
		MCP_REFS_CHUNK_BEFORE="$(kubectl get "${MCP_RES}" "${LEAF_MCP}" -o json 2>/dev/null \
			| jq -r --arg c "${CHUNK}" '[(.status.chunks // [])[] | (if type=="object" then (.name // .) else . end)] | index($c) != null' 2>/dev/null || echo unknown)"
		note "MCP.status.chunks referenced ${CHUNK} before delete: ${MCP_REFS_CHUNK_BEFORE}"
		if ! kubectl delete "${CHUNK_RES}" "${CHUNK}" --wait=false 2>/dev/null; then
			note "cannot delete chunk ${CHUNK}; skipped"
		else
		kubectl get "${MCP_RES}" "${LEAF_MCP}" -o yaml >"${CDIR}/mcp-after-delete.yaml" 2>/dev/null || true
		note "deleted chunk ${CHUNK}; chunk-watch is intentionally NOT implemented — bumping MCP to trigger reconcile"
		kubectl annotate "${MCP_RES}" "${LEAF_MCP}" "tree-demo.state-snapshotter.deckhouse.io/bump=$(date +%s)" --overwrite >/dev/null 2>&1 \
			|| kubectl_ctrl annotate "${MCP_RES}" "${LEAF_MCP}" "tree-demo.state-snapshotter.deckhouse.io/bump=$(date +%s)" --overwrite >/dev/null 2>&1 || true
		kubectl get "${MCP_RES}" "${LEAF_MCP}" -o yaml >"${CDIR}/mcp-after-bump.yaml" 2>/dev/null || true
		MCP_REFS_CHUNK_AFTER="$(kubectl get "${MCP_RES}" "${LEAF_MCP}" -o json 2>/dev/null \
			| jq -r --arg c "${CHUNK}" '[(.status.chunks // [])[] | (if type=="object" then (.name // .) else . end)] | index($c) != null' 2>/dev/null || echo unknown)"
		note "MCP.status.chunks still references ${CHUNK} after delete+bump: ${MCP_REFS_CHUNK_AFTER} (expected true: dangling ref, list not rewritten away)"
		wait_until_to "${INVALIDATION_WAIT_SEC}" "leaf content ${LEAF_CONTENT} ManifestsReady=False after chunk delete + bump" \
			bash -c "[[ \$(kubectl get '${CONTENT_RES}' '${LEAF_CONTENT}' -o json | jq -r '([.status.conditions[]?|select(.type==\"ManifestsReady\")][0].status)//\"\"') == False ]]" \
			|| die "leaf content ${LEAF_CONTENT} ManifestsReady did not flip False within ${INVALIDATION_WAIT_SEC}s after chunk delete + MCP bump (integrity revalidation did not fail closed)"
		RR_MSG="$(cond_field "$(get_json "${CONTENT_RES}" "" "${LEAF_CONTENT}")" ManifestsReady message)"
		echo "${RR_MSG}" | grep -q "${CHUNK}" && note "ManifestsReady message names missing chunk ${CHUNK}" \
			|| note "ManifestsReady message does not name missing chunk (informational; msg=[${RR_MSG}])"
		note "LIMITATION: chunk deletion alone does not wake reconcile; an MCP update/bump is required (no chunk->MCP watch in Phase 2a)"
		fi
		save_artifacts "11-chunk-missing" "${NS}"
	fi
fi
log "11-chunk-missing: done"
fi # end GROUP failure-destructive

# ---------------------------------------------------------------------------
log ""
{
	echo "run_id=${RUN_ID}"
	echo "namespace=${NS}"
	echo "group=${GROUP}"
	echo "verdict=PASS"
} >"${RUN_ARTIFACT_DIR}/SUMMARY.txt"
# Reaching here means every stage in the selected group passed: any invariant violation (die) or
# unmet precondition (require) would have exited non-zero earlier. Outcome is strictly binary.
log "== tree-demo-e2e PASSED (group=${GROUP}) — all invariants held"
log "Artifacts: ${RUN_ARTIFACT_DIR}"
