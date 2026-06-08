#!/usr/bin/env bash
# Staged, artifact-producing diagnostic for the snapshot-tree demo (demo VM/Disk domain).
#
# Goal: validate the current architecture end-to-end on a real cluster, from CSD priority
# planning to Phase 2a / Slice 3 status propagation:
#   - GVK/priority registration and priority-driven tree shape (+ inversion);
#   - DomainReady planning barrier (domain-owned, generation-gated);
#   - artifacts born under execution ObjectKeeper and handed off to SnapshotContent;
#   - RequestsReady / ChildrenReady / Ready aggregation on the SnapshotContent tree;
#   - damaged-leaf Ready=False propagation to root and recovery back to Ready=True;
#   - sibling isolation;
#   - root/demo Snapshot Ready as a verbatim mirror of bound SnapshotContent.Ready
#     (no local Ready recompute required to pass).
#
# This is a DIAGNOSTIC scenario, not a fragile one-shot e2e:
#   - core invariants hard-fail (preflight, tree shape, happy-path Ready, MCP-failure
#     propagation + sibling isolation, MCP recovery, mirror equality);
#   - racey / environment-dependent checks (priority inversion shape, DomainReady timeline,
#     ownership-handoff intermediates, VSC pending/recovery/missing, chunk-missing) are
#     best-effort: they emit WARN + SOFT and store artifacts instead of aborting the run.
#
# Artifacts: ${TREE_DEMO_ARTIFACT_DIR:-artifacts}/tree-demo-<run-id>/<stage>/
#   00-preflight 01-priority-vm-first 02-tree-ready 03-priority-inverted
#   04-domainready-barrier 05-ownership-handoff 06-mcp-failure 07-mcp-recovery
#   08-vsc-pending 09-vsc-recovery 10-vsc-missing 11-chunk-missing
#
# Usage: ./hack/snapshot-tree-demo-e2e.sh
#
# Env:
#   TREE_DEMO_NAMESPACE       main demo namespace (default: snapshot-demo-tree-<run-id>)
#   TREE_DEMO_STORAGE_CLASS   default: local-thin
#   TREE_DEMO_MODULE_NS       controller namespace (default: d8-state-snapshotter)
#   TREE_DEMO_CONTROLLER_SA   impersonation SA for status patches/chunk reads
#                             (default: system:serviceaccount:<module-ns>:controller)
#   TREE_DEMO_WAIT_SEC        per-wait hard cap seconds (default: 600)
#   TREE_DEMO_POLL_SEC        poll interval seconds (default: 5)
#   TREE_DEMO_WAIT_LOG_EVERY_SEC progress log interval (default: 30)
#   TREE_DEMO_ARTIFACT_DIR    artifact root (default: artifacts)
#   TREE_DEMO_BIND_IMAGE      bind pod image for WaitForFirstConsumer PVCs
#   TREE_DEMO_SKIP_CLEANUP    1 = keep namespaces and leave CSD as last set
#   TREE_DEMO_SKIP_INVERSION  1 = skip 03-priority-inverted (avoid global CSD churn)
#   TREE_DEMO_SKIP_VSC        1 = skip 08/09/10 VSC stages
#   TREE_DEMO_SKIP_CHUNK      1 = skip 11-chunk-missing
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
POLL_SEC="${TREE_DEMO_POLL_SEC:-5}"
WAIT_LOG_EVERY_SEC="${TREE_DEMO_WAIT_LOG_EVERY_SEC:-30}"
BIND_IMAGE="${TREE_DEMO_BIND_IMAGE:-registry.k8s.io/pause:3.9}"
CONTROLLER_SA="${TREE_DEMO_CONTROLLER_SA:-system:serviceaccount:${MOD_NS}:controller}"

NS="${TREE_DEMO_NAMESPACE:-snapshot-demo-tree-${RUN_ID}}"
NS_PRIORITY_INV="${NS}-priority-inv"
NS_DOMAIN_BARRIER="${NS}-domain-barrier"
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
VSC_RES="volumesnapshotcontents.snapshot.storage.k8s.io"
OK_RES="objectkeepers.deckhouse.io"
VM_RES="demovirtualmachines.demo.state-snapshotter.deckhouse.io"
DISK_RES="demovirtualdisks.demo.state-snapshotter.deckhouse.io"
VMSNAP_RES="demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io"
DISKSNAP_RES="demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io"

CURRENT_STAGE=""
SOFT_FAILURES=0
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

log() { printf '%s\n' "$*" >&2; }
soft() {
	SOFT_FAILURES=$((SOFT_FAILURES + 1))
	log "SOFT[${CURRENT_STAGE}]: $*"
	[[ -n "${CURRENT_STAGE}" ]] && printf 'SOFT: %s\n' "$*" >>"$(stage_dir "${CURRENT_STAGE}")/notes.txt" 2>/dev/null || true
}
note() {
	log "NOTE[${CURRENT_STAGE}]: $*"
	[[ -n "${CURRENT_STAGE}" ]] && printf 'NOTE: %s\n' "$*" >>"$(stage_dir "${CURRENT_STAGE}")/notes.txt" 2>/dev/null || true
}
die() {
	log "ERROR[${CURRENT_STAGE}]: $*"
	[[ -n "${CURRENT_STAGE}" ]] && save_artifacts "${CURRENT_STAGE}" 2>/dev/null || true
	dump_controller_logs_hint
	exit 1
}

need_cmd() { command -v "$1" >/dev/null 2>&1 || { log "ERROR: missing required command: $1"; exit 1; }; }
now_rfc3339() { date -u +%Y-%m-%dT%H:%M:%SZ; }

kubectl_ctrl() { kubectl --as="${CONTROLLER_SA}" "$@"; }

dump_controller_logs_hint() {
	log "HINT: controller logs — kubectl logs -n ${MOD_NS} -l app=controller --tail=300"
	kubectl logs -n "${MOD_NS}" -l app=controller --tail=60 2>/dev/null | tail -30 >&2 || true
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

wait_until() {
	local desc="$1"; shift
	local deadline=$((SECONDS + WAIT_SEC)) start=${SECONDS} last=${SECONDS}
	while ((SECONDS < deadline)); do
		if "$@"; then log "OK ${desc}"; return 0; fi
		if ((SECONDS - last >= WAIT_LOG_EVERY_SEC)); then
			log "WAIT: ${desc} ($((SECONDS - start))s / ${WAIT_SEC}s)"; last=${SECONDS}
		fi
		sleep "${POLL_SEC}"
	done
	log "ERROR: timeout waiting for ${desc} (${WAIT_SEC}s) [stage=${CURRENT_STAGE}]"
	return 1
}

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
	local ns="$1" snap="$2"
	local deadline=$((SECONDS + WAIT_SEC)) start=${SECONDS} last=${SECONDS}
	while ((SECONDS < deadline)); do
		if snap_ready_true "${ns}" "${snap}"; then log "OK Snapshot ${ns}/${snap} Ready"; return 0; fi
		if snap_ready_terminal_false "${ns}" "${snap}"; then
			log "ERROR: Snapshot ${ns}/${snap} terminal Ready=False: $(ready_triple "$(get_json "${SNAP_RES}" "${ns}" "${snap}")")"
			return 1
		fi
		if ((SECONDS - last >= WAIT_LOG_EVERY_SEC)); then
			log "WAIT: Snapshot ${ns}/${snap} Ready ($((SECONDS - start))s / ${WAIT_SEC}s): $(ready_triple "$(get_json "${SNAP_RES}" "${ns}" "${snap}")")"
			last=${SECONDS}
		fi
		sleep "${POLL_SEC}"
	done
	log "ERROR: timeout waiting for Snapshot ${ns}/${snap} Ready (${WAIT_SEC}s)"
	return 1
}

pvc_bound() { [[ "$(kubectl -n "$1" get pvc "$2" -o jsonpath='{.status.phase}' 2>/dev/null)" == "Bound" ]]; }

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
		|| soft "demo $4 Snapshot Ready != bound content Ready (demo content->snapshot watch lag/regression): snap=[$(ready_triple "$(get_json "$1" "${NS}" "$2")")] content=[$(ready_triple "$(get_json "${CONTENT_RES}" "" "$3")")]"
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
      storage: 1Gi
---
apiVersion: v1
kind: Pod
metadata:
  name: bind-demo-pvc
  namespace: ${ns}
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
        claimName: demo-pvc
---
apiVersion: ${DEMO_API}
kind: DemoVirtualMachine
metadata:
  name: vm-1
  namespace: ${ns}
spec: {}
---
apiVersion: ${DEMO_API}
kind: DemoVirtualDisk
metadata:
  name: disk-vm
  namespace: ${ns}
spec:
  virtualMachineName: vm-1
---
apiVersion: ${DEMO_API}
kind: DemoVirtualDisk
metadata:
  name: disk-standalone
  namespace: ${ns}
spec: {}
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

# apply_csd <vm_priority> <disk_priority>
apply_csd() {
	local vmp="$1" diskp="$2"
	kubectl apply -f - <<EOF
apiVersion: ${SS_API}
kind: CustomSnapshotDefinition
metadata:
  name: ${CSD_NAME}
spec:
  ownerModule: tree-demo-e2e
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

csd_accepted() {
	get_json "${CSD_RES}" "" "${CSD_NAME}" \
		| jq -e '[.status.conditions[]? | select(.type == "Accepted" and .status == "True")] | length >= 1' >/dev/null
}

# RBACReady is set by an external Deckhouse hook in production; demo/smoke sets it manually.
patch_csd_rbac_ready() {
	local gen body
	gen="$(kubectl get "${CSD_RES}" "${CSD_NAME}" -o jsonpath='{.metadata.generation}' 2>/dev/null || echo 0)"
	body="$(get_json "${CSD_RES}" "" "${CSD_NAME}" | jq \
		--arg now "$(now_rfc3339)" --argjson gen "${gen:-0}" \
		'.status.conditions = ((.status.conditions // []) | map(select(.type != "RBACReady")) + [{
			type: "RBACReady", status: "True", reason: "TreeDemoE2E",
			message: "manual tree-demo approval", lastTransitionTime: $now, observedGeneration: $gen
		}])')"
	[[ -n "${body}" ]] || return 0
	printf '%s' "${body}" | kubectl replace --subresource=status -f - >/dev/null 2>&1 && return 0
	printf '%s' "${body}" | kubectl_ctrl replace --subresource=status -f - >/dev/null 2>&1 || true
}

ensure_csd_eligible() {
	wait_until "CSD ${CSD_NAME} Accepted=True" csd_accepted || die "CSD ${CSD_NAME} never Accepted"
	patch_csd_rbac_ready
	wait_until "CSD ${CSD_NAME} RBACReady=True" \
		bash -c "kubectl get '${CSD_RES}' '${CSD_NAME}' -o json | jq -e '[.status.conditions[]?|select(.type==\"RBACReady\" and .status==\"True\")]|length>=1' >/dev/null" \
		|| soft "CSD RBACReady not True (external hook may own it); tree may not build"
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
	dump_kind "${res}" "snapshotcontents" "" "${CONTENT_RES}"
	dump_kind "${res}" "manifestcheckpoints" "" "${MCP_RES}"
	dump_kind "${res}" "volumesnapshotcontents" "" "${VSC_RES}"
	dump_kind "${res}" "customsnapshotdefinitions" "" "${CSD_RES}"
	[[ "${HAS_OBJECTKEEPER}" == "1" ]] && dump_kind "${res}" "objectkeepers" "" "${OK_RES}"
	kubectl get events -n "${ns}" --sort-by=.metadata.creationTimestamp >"${res}/events.txt" 2>/dev/null || true
	condition_table "${ns}" >"${dir}/conditions.txt" 2>/dev/null || true
	ownerref_table "${ns}" >"${dir}/ownerrefs.txt" 2>/dev/null || true
	printf 'namespace=%s\nrun_id=%s\nstage=%s\ncsd=%s\n' "${ns}" "${RUN_ID}" "${stage}" "${CSD_NAME}" >"${dir}/run-context.txt"
}

# condition_table <ns>: Ready/RequestsReady/ChildrenReady/DomainReady for snapshots + contents.
condition_table() {
	local ns="$1"
	printf '%-22s %-34s %-44s\n' "KIND" "NAME" "Ready|RequestsReady|ChildrenReady|DomainReady(obsGen/gen)"
	local res
	for res in "${SNAP_RES}" "${VMSNAP_RES}" "${DISKSNAP_RES}"; do
		kubectl -n "${ns}" get "${res}" -o json 2>/dev/null | jq -r '
			.items[]? | [
				.kind, .metadata.name,
				(([.status.conditions[]?|select(.type=="Ready")][0].status)//"-") + "|" +
				(([.status.conditions[]?|select(.type=="RequestsReady")][0].status)//"-") + "|" +
				(([.status.conditions[]?|select(.type=="ChildrenReady")][0].status)//"-") + "|" +
				(([.status.conditions[]?|select(.type=="DomainReady")][0].status)//"-") + "(" +
				((([.status.conditions[]?|select(.type=="DomainReady")][0].observedGeneration)//0)|tostring) + "/" +
				((.metadata.generation//0)|tostring) + ")"
			] | "\(.[0])\t\(.[1])\t\(.[2])"' 2>/dev/null \
			| while IFS=$'\t' read -r k n c; do printf '%-22s %-34s %-44s\n' "${k}" "${n}" "${c}"; done
	done
	kubectl get "${CONTENT_RES}" -o json 2>/dev/null | jq -r '
		.items[]? | [
			"SnapshotContent", .metadata.name,
			(([.status.conditions[]?|select(.type=="Ready")][0].status)//"-") + "|" +
			(([.status.conditions[]?|select(.type=="RequestsReady")][0].status)//"-") + "|" +
			(([.status.conditions[]?|select(.type=="ChildrenReady")][0].status)//"-") + "|" +
			(([.status.conditions[]?|select(.type=="DomainReady")][0].status)//"-")
		] | "\(.[0])\t\(.[1])\t\(.[2])"' 2>/dev/null \
		| while IFS=$'\t' read -r k n c; do printf '%-22s %-34s %-44s\n' "${k}" "${n}" "${c}"; done
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

# save_graph <stage> <ns> <snap> <name> <mode>
save_graph() {
	local stage="$1" ns="$2" snap="$3" name="$4" mode="${5:-logical}"
	local dir graph_dir
	dir="$(stage_dir "${stage}")"
	graph_dir="${dir}/graph"
	mkdir -p "${graph_dir}"
	[[ -f "${SCRIPT_DIR}/snapshot-graph.sh" ]] || return 0
	bash "${SCRIPT_DIR}/snapshot-graph.sh" --namespace "${ns}" --snapshot "${snap}" \
		--output-dir "${graph_dir}" --name "${name}" --mode "${mode}" \
		--title "tree-demo ${stage}" --chunk-as "${CONTROLLER_SA}" 2>"${dir}/graph.err" \
		|| note "graph render failed for ${stage} (see graph.err)"
}

# ---- cleanup ----------------------------------------------------------------

cleanup_on_exit() {
	local rc=$?
	if [[ "${TREE_DEMO_SKIP_CLEANUP:-0}" == "1" ]]; then
		log "SKIP cleanup: TREE_DEMO_SKIP_CLEANUP=1 (namespaces ${NS} ${NS_PRIORITY_INV} ${NS_DOMAIN_BARRIER}, CSD ${CSD_NAME} kept)"
		return 0
	fi
	kubectl delete "${CSD_RES}" "${CSD_NAME}" --ignore-not-found=true --wait=false 2>/dev/null || true
	kubectl delete namespace "${NS}" "${NS_PRIORITY_INV}" "${NS_DOMAIN_BARRIER}" --ignore-not-found=true --wait=false 2>/dev/null || true
	return "${rc}"
}
trap cleanup_on_exit EXIT

# =============================================================================
need_cmd kubectl
need_cmd jq
mkdir -p "${RUN_ARTIFACT_DIR}"
log "== tree-demo-e2e run_id=${RUN_ID} ns=${NS} sc=${STORAGE_CLASS} module=${MOD_NS}"
log "Artifacts: ${RUN_ARTIFACT_DIR}"

# ---------------------------------------------------------------------------
# 00-preflight
# ---------------------------------------------------------------------------
begin_stage "00-preflight"
kubectl get storageclass "${STORAGE_CLASS}" >/dev/null || die "storageclass ${STORAGE_CLASS} missing"
kubectl get crd \
	snapshots.storage.deckhouse.io snapshotcontents.storage.deckhouse.io \
	manifestcheckpoints.state-snapshotter.deckhouse.io manifestcapturerequests.state-snapshotter.deckhouse.io \
	customsnapshotdefinitions.state-snapshotter.deckhouse.io \
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
# External demo-domain RBAC (granted by Deckhouse RBAC controller in production).
if [[ "$(kubectl auth can-i get "${VM_RES}" --as="system:serviceaccount:${MOD_NS}:webhooks" -n "${NS}" 2>/dev/null || echo no)" != "yes" ]]; then
	soft "webhook SA cannot get demo inventory (${VM_RES}); tree may fail with SubtreeManifestCapturePending — grant external demo-domain RBAC + redeploy"
fi
if [[ "$(kubectl auth can-i create "${MCR_RES}" --as="${CONTROLLER_SA}" -n "${NS}" 2>/dev/null || echo no)" != "yes" ]]; then
	soft "controller SA cannot create ${MCR_RES} in target ns; capture will stall"
fi
# Fresh objects must NOT carry deprecated conditions.
{
	echo "module_ns=${MOD_NS}"
	echo "controller_sa=${CONTROLLER_SA}"
	echo "storage_class=${STORAGE_CLASS}"
	echo "has_objectkeeper=${HAS_OBJECTKEEPER}"
	kubectl api-resources 2>/dev/null | grep -E 'snapshot|manifest|customsnapshot|demovirtual|objectkeeper' || true
} >"$(stage_dir 00-preflight)/preflight.txt"
save_artifacts "00-preflight" "${NS}"
log "00-preflight: PASS"

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
wait_until "demo-pvc Bound in ${NS}" pvc_bound "${NS}" demo-pvc || soft "demo-pvc not Bound; data leg may be empty"
apply_root_snapshot "${NS}"
wait_until "root Snapshot bound in ${NS}" snap_bound "${NS}" "${SNAP}" || die "root Snapshot never bound"
ROOT_CONTENT="$(kubectl -n "${NS}" get "${SNAP_RES}" "${SNAP}" -o jsonpath='{.status.boundSnapshotContentName}')"
note "root content=${ROOT_CONTENT}"
wait_snapshot_ready "${NS}" "${SNAP}" || die "root Snapshot did not become Ready"

# Resolve tree via kind-aware childrenSnapshotRefs.
ROOT_JSON="$(get_json "${SNAP_RES}" "${NS}" "${SNAP}")"
VM_SNAP="$(echo "${ROOT_JSON}" | jq -r '[.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualMachineSnapshot")][0].name // ""')"
N_ROOT_VMSNAP="$(echo "${ROOT_JSON}" | jq '[.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualMachineSnapshot")]|length')"
N_ROOT_DISKSNAP="$(echo "${ROOT_JSON}" | jq '[.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualDiskSnapshot")]|length')"
SIBLING_SNAP="$(echo "${ROOT_JSON}" | jq -r '[.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualDiskSnapshot")][0].name // ""')"
[[ -n "${VM_SNAP}" ]] || die "root has no DemoVirtualMachineSnapshot child (tree shape wrong)"
[[ "${N_ROOT_VMSNAP}" == "1" ]] || soft "expected exactly 1 root VM snapshot child, got ${N_ROOT_VMSNAP}"
[[ "${N_ROOT_DISKSNAP}" == "1" ]] || die "expected exactly 1 root Disk snapshot child (standalone only), got ${N_ROOT_DISKSNAP} (covered VM disk may be duplicated at root)"
[[ -n "${SIBLING_SNAP}" ]] || die "root has no standalone DemoVirtualDiskSnapshot child"

VM_JSON="$(get_json "${VMSNAP_RES}" "${NS}" "${VM_SNAP}")"
LEAF_SNAP="$(echo "${VM_JSON}" | jq -r '[.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualDiskSnapshot")][0].name // ""')"
N_VM_DISK="$(echo "${VM_JSON}" | jq '[.status.childrenSnapshotRefs[]?|select(.kind=="DemoVirtualDiskSnapshot")]|length')"
[[ -n "${LEAF_SNAP}" ]] || die "VM snapshot ${VM_SNAP} has no DemoVirtualDiskSnapshot child (covered disk missing from VM subtree)"
[[ "${N_VM_DISK}" == "1" ]] || soft "expected 1 disk under VM, got ${N_VM_DISK}"

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

# Happy-path conditions: every content RequestsReady=ChildrenReady=Ready=True.
for c in "${LEAF_CONTENT}" "${SIBLING_CONTENT}" "${VM_CONTENT}" "${ROOT_CONTENT}"; do
	[[ -n "${c}" ]] || continue
	wait_until "SnapshotContent ${c} Ready=True" content_ready_true "${c}" || die "content ${c} not Ready"
	cj="$(get_json "${CONTENT_RES}" "" "${c}")"
	[[ "$(cond_field "${cj}" RequestsReady status)" == "True" ]] || soft "content ${c} RequestsReady != True"
	[[ "$(cond_field "${cj}" ChildrenReady status)" == "True" ]] || soft "content ${c} ChildrenReady != True"
done

# ConfigMap captured in a manifest checkpoint but NOT a child snapshot (manifest-only object).
echo "${ROOT_JSON}" | jq -e '[.status.childrenSnapshotRefs[]?|select(.kind=="ConfigMap")]|length==0' >/dev/null \
	|| die "ConfigMap must not be a child snapshot"
if kubectl get --raw "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/${NS}/snapshots/${SNAP}/manifests" \
	>"$(stage_dir 02-tree-ready)/aggregated.json" 2>/dev/null; then
	jq -e '[.[]?|select(.kind=="ConfigMap" and .metadata.name=="demo-snapshot-cm")]|length>=1' \
		"$(stage_dir 02-tree-ready)/aggregated.json" >/dev/null \
		&& note "ConfigMap demo-snapshot-cm present in aggregated manifests (manifest-only)" \
		|| soft "ConfigMap not found in aggregated manifests (may be in a child MCP; check graph)"
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

# ---------------------------------------------------------------------------
# 03-priority-inverted (priority must influence planning, or fail closed)
# ---------------------------------------------------------------------------
begin_stage "03-priority-inverted"
if [[ "${TREE_DEMO_SKIP_INVERSION:-0}" == "1" ]]; then
	note "skipped (TREE_DEMO_SKIP_INVERSION=1)"
elif ! inversion_safe; then
	soft "skipped priority inversion: a global CSD priority flip is unsafe with demo workload outside this run (${INVERSION_BLOCKERS}). Run on an isolated cluster, or set TREE_DEMO_SKIP_INVERSION=1 to acknowledge."
	{ echo "inversion skipped (unsafe global CSD churn)"; echo "blockers: ${INVERSION_BLOCKERS}"; } >"$(stage_dir 03-priority-inverted)/skipped.txt"
	kubectl get "${VMSNAP_RES},${DISKSNAP_RES}" -A -o wide >"$(stage_dir 03-priority-inverted)/demo-snapshots-all-namespaces.txt" 2>/dev/null || true
	kubectl get "${CSD_RES}" -o yaml >"$(stage_dir 03-priority-inverted)/customsnapshotdefinitions.yaml" 2>/dev/null || true
else
	apply_csd 10 100
	ensure_csd_eligible
	apply_source_namespace "${NS_PRIORITY_INV}"
	wait_until "demo-pvc Bound in ${NS_PRIORITY_INV}" pvc_bound "${NS_PRIORITY_INV}" demo-pvc || soft "inverted demo-pvc not Bound"
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
			soft "priority inversion did NOT change the planner decision and gave no fail-closed reason: baseline root(VM=${N_ROOT_VMSNAP},Disk=${N_ROOT_DISKSNAP}) VMdisk=${N_VM_DISK} == inverted root(VM=${INV_VM},Disk=${INV_DISK}) VMdisk=${INV_VM_DISK} (priority appears to NOT affect planning — see expected-vs-actual.txt)"
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
	wait_snapshot_ready "${NS}" "${SNAP}" || soft "main tree did not reconverge to Ready after CSD restore"
fi
log "03-priority-inverted: done"

# ---------------------------------------------------------------------------
# 04-domainready-barrier (domain-owned, generation-gated planning handoff)
# ---------------------------------------------------------------------------
begin_stage "04-domainready-barrier"
# HARD final assertion: every domain snapshot is DomainReady=True at its current generation.
for pair in "${SNAP_RES}|${SNAP}" "${VMSNAP_RES}|${VM_SNAP}" "${DISKSNAP_RES}|${LEAF_SNAP}" "${DISKSNAP_RES}|${SIBLING_SNAP}"; do
	res="${pair%%|*}"; name="${pair##*|}"
	[[ -n "${name}" ]] || die "domain snapshot handle empty (${res}); tree not resolved"
	j="$(get_json "${res}" "${NS}" "${name}")"
	dr_status="$(cond_field "${j}" DomainReady status)"
	[[ "${dr_status}" == "True" ]] || die "${res}/${name} DomainReady=${dr_status:-<none>} (every domain snapshot must publish DomainReady=True)"
	obs="$(jq -r '([.status.conditions[]?|select(.type=="DomainReady")][0].observedGeneration)//0' <<<"${j}")"
	gen="$(jq -r '.metadata.generation//0' <<<"${j}")"
	[[ "${obs}" == "${gen}" ]] || die "${res}/${name} DomainReady observedGeneration(${obs}) != generation(${gen}) (stale barrier)"
	note "${res}/${name} DomainReady=True observedGeneration=${obs}==generation"
done
# HARD: DomainReady is a Snapshot-like planning barrier only and must NEVER appear on a
# SnapshotContent. A DomainReady on content means the common/generic layer self-published it
# (regression of the Slice 2 / snapshotbinding contract: DomainReady is domain-owned).
DR_ON_CONTENT="$(kubectl get "${CONTENT_RES}" -o json 2>/dev/null \
	| jq -r '[.items[]?|select(any(.status.conditions[]?; .type=="DomainReady"))|.metadata.name]|join(",")' 2>/dev/null || true)"
[[ -z "${DR_ON_CONTENT}" ]] || die "DomainReady found on SnapshotContent(s) [${DR_ON_CONTENT}] — common-layer self-publication regression"
note "no SnapshotContent carries DomainReady (barrier remains domain/Snapshot-owned)"
# Timeline probe: confirm content is not bound before current-gen DomainReady on a fresh node.
apply_source_namespace "${NS_DOMAIN_BARRIER}"
wait_until "barrier demo-pvc Bound" pvc_bound "${NS_DOMAIN_BARRIER}" demo-pvc || true
apply_root_snapshot "${NS_DOMAIN_BARRIER}"
TL="$(stage_dir 04-domainready-barrier)/timeline.txt"
: >"${TL}"
deadline=$((SECONDS + 120)); dr_seen=0; bound_before_dr=0
while ((SECONDS < deadline)); do
	bj="$(get_json "${SNAP_RES}" "${NS_DOMAIN_BARRIER}" "${SNAP}")"
	bound="$(jq -r '.status.boundSnapshotContentName // ""' <<<"${bj}")"
	drs="$(cond_field "${bj}" DomainReady status)"
	dro="$(jq -r '([.status.conditions[]?|select(.type=="DomainReady")][0].observedGeneration)//0' <<<"${bj}")"
	bgen="$(jq -r '.metadata.generation//0' <<<"${bj}")"
	printf '%s bound=%s DomainReady=%s(obs=%s/gen=%s)\n' "$(now_rfc3339)" "${bound:-<none>}" "${drs:-<none>}" "${dro}" "${bgen}" >>"${TL}"
	if [[ -n "${bound}" && "${dr_seen}" == "0" && "${drs}" != "True" ]]; then bound_before_dr=1; fi
	if [[ "${drs}" == "True" && "${dro}" == "${bgen}" ]]; then dr_seen=1; fi
	[[ "${dr_seen}" == "1" && -n "${bound}" ]] && break
	sleep 2
done
[[ "${bound_before_dr}" == "0" ]] || soft "barrier: content bound before current-gen DomainReady (barrier may be weak); see timeline.txt"
[[ "${dr_seen}" == "1" ]] && note "barrier: DomainReady=True reached current generation" || soft "barrier: DomainReady=True/current-gen not observed within window"
note "stale-DomainReady injection not performed (out of scope; documented limitation)"
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
		|| soft "leaf MCP ${LEAF_MCP} not owned by SnapshotContent: $(kubectl get "${MCP_RES}" "${LEAF_MCP}" -o jsonpath='{.metadata.ownerReferences}' 2>/dev/null)"
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
			|| soft "VSC ${VSC_NAME} not owned by SnapshotContent (handoff incomplete or different owner)"
	fi
else
	note "no tree content has dataRefs[] (no volume captured in this run); data-leg handoff check skipped"
fi
save_artifacts "05-ownership-handoff" "${NS}"
log "05-ownership-handoff: done"

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

MCP_FAILURE_DONE=0
begin_stage "06-mcp-failure"
if [[ -z "${LEAF_MCP}" ]]; then
	soft "no leaf MCP to fail; skipping 06/07"
else
	if ! patch_mcp_ready "${LEAF_MCP}" "False" "Failed" "tree-demo injected MCP failure"; then
		soft "cannot patch MCP ${LEAF_MCP} status (RBAC); skipping 06/07"
	else
		MCP_FAILURE_DONE=1
		wait_until "leaf content ${LEAF_CONTENT} RequestsReady=False" \
			bash -c "[[ \$(kubectl get '${CONTENT_RES}' '${LEAF_CONTENT}' -o json | jq -r '([.status.conditions[]?|select(.type==\"RequestsReady\")][0].status)//\"\"') == False ]]" \
			|| soft "leaf RequestsReady did not flip False"
		LEAF_RR_REASON="$(cond_field "$(get_json "${CONTENT_RES}" "" "${LEAF_CONTENT}")" RequestsReady reason)"
		[[ "${LEAF_RR_REASON}" == "ManifestCheckpointFailed" ]] || soft "leaf RequestsReady reason=${LEAF_RR_REASON} (expected ManifestCheckpointFailed)"
		wait_until "leaf content ${LEAF_CONTENT} Ready=False" content_ready_false "${LEAF_CONTENT}" || soft "leaf content did not flip Ready=False"
		[[ -n "${VM_CONTENT}" ]] && { wait_until "VM content ${VM_CONTENT} ChildrenReady=False" \
			bash -c "[[ \$(kubectl get '${CONTENT_RES}' '${VM_CONTENT}' -o json | jq -r '([.status.conditions[]?|select(.type==\"ChildrenReady\")][0].status)//\"\"') == False ]]" \
			|| soft "VM content ChildrenReady did not flip False"; }
		wait_until "root content ${ROOT_CONTENT} ChildrenReady=False" \
			bash -c "[[ \$(kubectl get '${CONTENT_RES}' '${ROOT_CONTENT}' -o json | jq -r '([.status.conditions[]?|select(.type==\"ChildrenReady\")][0].status)//\"\"') == False ]]" \
			|| die "root content ChildrenReady did not flip False (propagation broken)"
		ROOT_CR_REASON="$(cond_field "$(get_json "${CONTENT_RES}" "" "${ROOT_CONTENT}")" ChildrenReady reason)"
		[[ "${ROOT_CR_REASON}" == "ChildrenFailed" ]] || soft "root ChildrenReady reason=${ROOT_CR_REASON} (expected ChildrenFailed)"
		wait_until "root Snapshot ${SNAP} Ready=False" \
			bash -c "[[ \$(kubectl -n '${NS}' get '${SNAP_RES}' '${SNAP}' -o json | jq -r '([.status.conditions[]?|select(.type==\"Ready\")][0].status)//\"\"') == False ]]" \
			|| die "root Snapshot did not flip Ready=False"
		# Mirror equality under failure.
		RS="$(ready_triple "$(get_json "${SNAP_RES}" "${NS}" "${SNAP}")")"
		RC="$(ready_triple "$(get_json "${CONTENT_RES}" "" "${ROOT_CONTENT}")")"
		[[ "${RS}" == "${RC}" ]] || die "failure mirror mismatch: snap=[${RS}] content=[${RC}]"
		# Message contains failed-leaf signal (best-effort).
		echo "${RC}" | grep -q "${LEAF_CONTENT}" && note "root Ready message references failed leaf ${LEAF_CONTENT}" \
			|| soft "root Ready message does not name failed leaf (msg=[${RC}])"
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
	patch_mcp_ready "${LEAF_MCP}" "True" "Ready" "tree-demo recovery" || soft "cannot restore MCP ${LEAF_MCP}"
	wait_until "leaf content ${LEAF_CONTENT} Ready=True" content_ready_true "${LEAF_CONTENT}" || soft "leaf did not recover"
	[[ -n "${VM_CONTENT}" ]] && { wait_until "VM content recover Ready=True" content_ready_true "${VM_CONTENT}" || soft "VM content did not recover"; }
	wait_until "root content ${ROOT_CONTENT} Ready=True" content_ready_true "${ROOT_CONTENT}" || die "root content did not recover Ready=True"
	wait_snapshot_ready "${NS}" "${SNAP}" || die "root Snapshot did not recover Ready=True"
	RS="$(ready_triple "$(get_json "${SNAP_RES}" "${NS}" "${SNAP}")")"
	RC="$(ready_triple "$(get_json "${CONTENT_RES}" "" "${ROOT_CONTENT}")")"
	[[ "${RS}" == "${RC}" ]] || die "recovery mirror mismatch: snap=[${RS}] content=[${RC}]"
	[[ -n "${SIBLING_CONTENT}" ]] && { content_ready_true "${SIBLING_CONTENT}" || soft "sibling not Ready after recovery"; }
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
		wait_until "content ${DATA_CONTENT} RequestsReady=False (data pending)" \
			bash -c "[[ \$(kubectl get '${CONTENT_RES}' '${DATA_CONTENT}' -o json | jq -r '([.status.conditions[]?|select(.type==\"RequestsReady\")][0].status)//\"\"') == False ]]" \
			|| soft "content ${DATA_CONTENT} RequestsReady did not flip on VSC not-ready"
		RR_REASON="$(cond_field "$(get_json "${CONTENT_RES}" "" "${DATA_CONTENT}")" RequestsReady reason)"
		[[ "${RR_REASON}" == "DataCapturePending" ]] || soft "data pending reason=${RR_REASON} (expected DataCapturePending)"
		wait_until "root Snapshot Ready=False mirror" \
			bash -c "[[ \$(kubectl -n '${NS}' get '${SNAP_RES}' '${SNAP}' -o json | jq -r '([.status.conditions[]?|select(.type==\"Ready\")][0].status)//\"\"') == False ]]" \
			|| soft "root Snapshot did not flip on VSC pending"
		RS="$(ready_triple "$(get_json "${SNAP_RES}" "${NS}" "${SNAP}")")"
		RC="$(ready_triple "$(get_json "${CONTENT_RES}" "" "${ROOT_CONTENT}")")"
		[[ "${RS}" == "${RC}" ]] || soft "vsc-pending mirror mismatch: snap=[${RS}] content=[${RC}]"
		note "VSC readyToUse=false surfaced DataCapturePending (non-terminal) up to root mirror"
	else
		note "cannot patch VSC ${DATA_VSC} status (admission/RBAC); VSC pending not exercised"
	fi
fi
save_artifacts "08-vsc-pending" "${NS}"
log "08-vsc-pending: done"

begin_stage "09-vsc-recovery"
if [[ "${VSC_PENDING_DONE}" == "1" ]]; then
	patch_vsc_ready_to_use "${DATA_VSC}" true || soft "cannot restore VSC ${DATA_VSC}"
	wait_until "content ${DATA_CONTENT} Ready=True after VSC recovery" content_ready_true "${DATA_CONTENT}" || soft "content did not recover after VSC restore"
	wait_snapshot_ready "${NS}" "${SNAP}" || soft "root Snapshot did not recover after VSC restore"
	note "VSC readyToUse=true restored tree"
else
	note "skipped (no VSC pending injected)"
fi
save_artifacts "09-vsc-recovery" "${NS}"
log "09-vsc-recovery: done"

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
	wait_until "content ${DATA_CONTENT} RequestsReady=False (artifact missing)" \
		bash -c "[[ \$(kubectl get '${CONTENT_RES}' '${DATA_CONTENT}' -o json | jq -r '([.status.conditions[]?|select(.type==\"RequestsReady\")][0].status)//\"\"') == False ]]" \
		|| soft "content RequestsReady did not flip on VSC delete (artifact-missing watch may be limited)"
	RR_REASON="$(cond_field "$(get_json "${CONTENT_RES}" "" "${DATA_CONTENT}")" RequestsReady reason)"
	[[ "${RR_REASON}" == "ArtifactMissing" ]] || soft "missing-artifact reason=${RR_REASON} (expected ArtifactMissing)"
	[[ -n "${SIBLING_CONTENT}" && "${SIBLING_CONTENT}" != "${DATA_CONTENT}" ]] && { content_ready_true "${SIBLING_CONTENT}" \
		&& note "sibling isolation held under VSC delete" || soft "sibling not Ready under VSC delete"; }
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
		wait_until "leaf content ${LEAF_CONTENT} RequestsReady=False after chunk delete + bump" \
			bash -c "[[ \$(kubectl get '${CONTENT_RES}' '${LEAF_CONTENT}' -o json | jq -r '([.status.conditions[]?|select(.type==\"RequestsReady\")][0].status)//\"\"') == False ]]" \
			|| soft "leaf RequestsReady did not flip after chunk delete + bump"
		RR_MSG="$(cond_field "$(get_json "${CONTENT_RES}" "" "${LEAF_CONTENT}")" RequestsReady message)"
		echo "${RR_MSG}" | grep -q "${CHUNK}" && note "RequestsReady message names missing chunk ${CHUNK}" \
			|| soft "RequestsReady message does not name missing chunk (msg=[${RR_MSG}])"
		note "LIMITATION: chunk deletion alone does not wake reconcile; an MCP update/bump is required (no chunk->MCP watch in Phase 2a)"
		fi
		save_artifacts "11-chunk-missing" "${NS}"
	fi
fi
log "11-chunk-missing: done"

# ---------------------------------------------------------------------------
log ""
{
	echo "run_id=${RUN_ID}"
	echo "namespace=${NS}"
	echo "soft_failures=${SOFT_FAILURES}"
} >"${RUN_ARTIFACT_DIR}/SUMMARY.txt"
if [[ "${SOFT_FAILURES}" -gt 0 ]]; then
	log "== tree-demo-e2e COMPLETED with ${SOFT_FAILURES} SOFT finding(s) — review per-stage notes.txt"
else
	log "== tree-demo-e2e PASSED (no soft findings)"
fi
log "Artifacts: ${RUN_ARTIFACT_DIR}"
