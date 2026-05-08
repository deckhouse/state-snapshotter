#!/usr/bin/env bash
# Build smoke artifacts for a state-snapshotter Snapshot graph.

set -euo pipefail

NS=""
SNAPSHOT=""
SNAPSHOTCONTENT=""
OUTPUT_DIR=""
ARTIFACT_NAME=""
MODE="smoke"
TITLE=""
DESCRIPTION=""
MAX_DEPTH=10
ALLOW_MISSING_ROOT=0
STRICT=0
INCLUDE_NAMESPACE_RESOURCES=1

log() { printf '%s\n' "$*" >&2; }
fail() { log "ERROR: $*"; exit 1; }
warn() { log "WARN: $*"; }

usage() {
	cat >&2 <<'EOF'
Usage:
  hack/snapshot-graph.sh --namespace NS --snapshot NAME --output-dir DIR --name ARTIFACT
  hack/snapshot-graph.sh --snapshotcontent NAME --output-dir DIR --name ARTIFACT

Options:
  --namespace NS          Namespace for live Snapshot traversal.
  --snapshot NAME        Root Snapshot name.
  --snapshotcontent NAME Root SnapshotContent name for retained traversal.
  --output-dir DIR       Directory for .dot/.svg/.objects.yaml/.summary.txt/.details.json.
  --name NAME            Artifact basename.
  --mode MODE            Rendering mode: lifecycle, logical, full, smoke (default: smoke).
  --title TITLE          Human-readable graph title (default: NAME).
  --description TEXT     Stage description saved next to artifacts and rendered in the graph title.
  --max-depth N          Recursion limit (default: 10).
  --allow-missing-root   Do not fail when the live root Snapshot is gone.
  --include-namespace-resources
                         Include all live resources in --namespace as point-in-time inventory (default).
  --no-include-namespace-resources
                         Disable namespace inventory nodes.
  --strict               Return non-zero on invariant violations.
EOF
}

while [[ $# -gt 0 ]]; do
	case "$1" in
	--namespace)
		NS="${2:-}"
		shift 2
		;;
	--snapshot)
		SNAPSHOT="${2:-}"
		shift 2
		;;
	--snapshotcontent)
		SNAPSHOTCONTENT="${2:-}"
		shift 2
		;;
	--output-dir)
		OUTPUT_DIR="${2:-}"
		shift 2
		;;
	--name)
		ARTIFACT_NAME="${2:-}"
		shift 2
		;;
	--mode)
		MODE="${2:-}"
		shift 2
		;;
	--title)
		TITLE="${2:-}"
		shift 2
		;;
	--description)
		DESCRIPTION="${2:-}"
		shift 2
		;;
	--max-depth)
		MAX_DEPTH="${2:-}"
		shift 2
		;;
	--allow-missing-root)
		ALLOW_MISSING_ROOT=1
		shift
		;;
	--include-namespace-resources)
		INCLUDE_NAMESPACE_RESOURCES=1
		shift
		;;
	--no-include-namespace-resources)
		INCLUDE_NAMESPACE_RESOURCES=0
		shift
		;;
	--strict)
		STRICT=1
		shift
		;;
	-h|--help)
		usage
		exit 0
		;;
	*)
		usage
		fail "unknown argument: $1"
		;;
	esac
done

command -v kubectl >/dev/null 2>&1 || fail "missing required command: kubectl"
command -v jq >/dev/null 2>&1 || fail "missing required command: jq"

if [[ -n "$SNAPSHOT" && -n "$SNAPSHOTCONTENT" ]]; then
	fail "use either --snapshot or --snapshotcontent, not both"
fi
if [[ -z "$SNAPSHOT" && -z "$SNAPSHOTCONTENT" ]]; then
	fail "one of --snapshot or --snapshotcontent is required"
fi
if [[ -n "$SNAPSHOT" && -z "$NS" ]]; then
	fail "--namespace is required with --snapshot"
fi
if [[ -z "$OUTPUT_DIR" || -z "$ARTIFACT_NAME" ]]; then
	fail "--output-dir and --name are required"
fi
if ! [[ "$MAX_DEPTH" =~ ^[0-9]+$ ]]; then
	fail "--max-depth must be a number"
fi
case "$MODE" in
lifecycle|logical|full|smoke) ;;
*) fail "--mode must be one of: lifecycle, logical, full, smoke" ;;
esac

mkdir -p "$OUTPUT_DIR"

[[ -n "$TITLE" ]] || TITLE="$ARTIFACT_NAME"
TIMESTAMP=$(date -u +%Y-%m-%dT%H:%M:%SZ)
ARTIFACT_BASE="${OUTPUT_DIR}/${ARTIFACT_NAME}.${MODE}"
DOT_FILE="${ARTIFACT_BASE}.dot"
SVG_FILE="${ARTIFACT_BASE}.svg"
OBJECTS_FILE="${ARTIFACT_BASE}.objects.yaml"
SUMMARY_FILE="${ARTIFACT_BASE}.summary.txt"
DETAILS_FILE="${ARTIFACT_BASE}.details.json"
STAGE_FILE="${ARTIFACT_BASE}.stage.txt"

TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/snapshot-graph.XXXXXX")
trap 'rm -rf "$TMP_DIR"' EXIT

NODES="${TMP_DIR}/nodes.tsv"
EDGES="${TMP_DIR}/edges.tsv"
VISITED="${TMP_DIR}/visited.txt"
DUMPED="${TMP_DIR}/dumped.txt"
CHECKS="${TMP_DIR}/checks.tsv"
DETAILS_RAW="${TMP_DIR}/details.ndjson"
RESOLVE_CACHE="${TMP_DIR}/resource-cache.tsv"
DELIM=$'\034'
: >"$NODES"
: >"$EDGES"
: >"$VISITED"
: >"$DUMPED"
: >"$CHECKS"
: >"$DETAILS_RAW"
: >"$OBJECTS_FILE"

ROOT_KEY=""
ROOT_CONTENT_KEY=""
ROOT_CONTENT_NAME=""
ROOT_OK_NAME=""
VIOLATIONS=0

json_quote() {
	jq -Rn --arg s "$1" '$s'
}

dot_label_quote() {
	json_quote "$1" | sed 's/\\\\n/\\n/g'
}

short_uid() {
	printf '%s' "$1" | cut -c1-6
}

group_from_api() {
	case "$1" in
	*/*) printf '%s\n' "${1%%/*}" ;;
	*) printf '%s\n' "" ;;
	esac
}

kind_to_resource() {
	local api="$1" kind="$2" group cache_key cached name
	case "${api}|${kind}" in
	storage.deckhouse.io/*\|Snapshot)
		printf '%s\n' "snapshots.storage.deckhouse.io"
		return
		;;
	storage.deckhouse.io/*\|SnapshotContent)
		printf '%s\n' "snapshotcontents.storage.deckhouse.io"
		return
		;;
	state-snapshotter.deckhouse.io/*\|CustomSnapshotDefinition)
		printf '%s\n' "customsnapshotdefinitions.state-snapshotter.deckhouse.io"
		return
		;;
	state-snapshotter.deckhouse.io/*\|ManifestCaptureRequest)
		printf '%s\n' "manifestcapturerequests.state-snapshotter.deckhouse.io"
		return
		;;
	state-snapshotter.deckhouse.io/*\|ManifestCheckpoint)
		printf '%s\n' "manifestcheckpoints.state-snapshotter.deckhouse.io"
		return
		;;
	state-snapshotter.deckhouse.io/*\|ManifestCheckpointContentChunk)
		printf '%s\n' "manifestcheckpointcontentchunks.state-snapshotter.deckhouse.io"
		return
		;;
	deckhouse.io/*\|ObjectKeeper)
		printf '%s\n' "objectkeepers.deckhouse.io"
		return
		;;
	esac

	group=$(group_from_api "$api")
	cache_key="${group}|${kind}"
	cached=$(awk -F '\t' -v k="$cache_key" '$1 == k { print $2; exit }' "$RESOLVE_CACHE" 2>/dev/null || true)
	if [[ -n "$cached" ]]; then
		printf '%s\n' "$cached"
		return
	fi

	if [[ -n "$group" ]]; then
		name=$(kubectl api-resources --api-group="$group" -o wide 2>/dev/null \
			| awk -v kind="$kind" 'NR > 1 && $NF == kind { print $1; exit }' || true)
		if [[ -n "$name" ]]; then
			printf '%s\t%s.%s\n' "$cache_key" "$name" "$group" >>"$RESOLVE_CACHE"
			printf '%s.%s\n' "$name" "$group"
			return
		fi
	fi

	name=$(printf '%s' "$kind" | tr '[:upper:]' '[:lower:]')
	printf '%s\t%s\n' "$cache_key" "$name" >>"$RESOLVE_CACHE"
	printf '%s\n' "$name"
}

get_json() {
	local api="$1" kind="$2" ns="$3" name="$4" resource
	resource=$(kind_to_resource "$api" "$kind")
	if [[ -n "$ns" ]]; then
		kubectl -n "$ns" get "$resource" "$name" -o json 2>/dev/null
	else
		kubectl get "$resource" "$name" -o json 2>/dev/null
	fi
}

get_json_flexible() {
	local api="$1" kind="$2" ns="$3" name="$4"
	get_json "$api" "$kind" "" "$name" || {
		if [[ -n "$ns" ]]; then
			get_json "$api" "$kind" "$ns" "$name"
		else
			return 1
		fi
	}
}

get_yaml() {
	local api="$1" kind="$2" ns="$3" name="$4" resource
	resource=$(kind_to_resource "$api" "$kind")
	if [[ -n "$ns" ]]; then
		kubectl -n "$ns" get "$resource" "$name" -o yaml 2>/dev/null
	else
		kubectl get "$resource" "$name" -o yaml 2>/dev/null
	fi
}

key_for() {
	printf '%s/%s/%s/%s\n' "$1" "$2" "$3" "$4"
}

node_exists() {
	local key="$1"
	awk -F "$DELIM" -v k="$key" '$1 == k { found=1; exit } END { exit found ? 0 : 1 }' "$NODES"
}

node_status() {
	local key="$1"
	awk -F "$DELIM" -v k="$key" '$1 == k { print $6; found=1; exit } END { if (!found) exit 1 }' "$NODES"
}

replace_node_row() {
	local key="$1" row="$2" tmp
	tmp="${TMP_DIR}/nodes.new"
	awk -F "$DELIM" -v k="$key" '$1 != k' "$NODES" >"$tmp"
	printf '%s\n' "$row" >>"$tmp"
	mv "$tmp" "$NODES"
}

node_row() {
	local value
	printf '%s' "$1"
	shift
	for value in "$@"; do
		printf '%s%s' "$DELIM" "$value"
	done
}

ready_status() {
	jq -r '
		(.status.conditions // [])
		| map(select(.type == "Ready"))
		| if length == 0 then "Unknown" else .[-1].status end
	'
}

csd_ready_status() {
	jq -r '
		(.status.conditions // []) as $conditions
		| ($conditions | map(select(.type == "Accepted")) | last | .status // "Unknown") as $accepted
		| ($conditions | map(select(.type == "RBACReady")) | last | .status // "Unknown") as $rbac
		| if $accepted == "True" and $rbac == "True" then "True"
		  elif $accepted == "False" or $rbac == "False" then "False"
		  else "Unknown"
		  end
	'
}

condition_reason() {
	jq -r '
		(.status.conditions // [])
		| map(select(.type == "Ready"))
		| if length == 0 then "" else ((.[-1].reason // "") + if (.[-1].message // "") != "" then "/" + .[-1].message else "" end) end
	'
}

csd_condition_reason() {
	jq -r '
		(.status.conditions // []) as $conditions
		| ($conditions | map(select(.type == "Accepted")) | last | .status // "Unknown") as $accepted
		| ($conditions | map(select(.type == "RBACReady")) | last | .status // "Unknown") as $rbac
		| "Accepted=" + $accepted + "/RBACReady=" + $rbac
	'
}

has_failed_condition() {
	jq -e '
		(.status.conditions // [])
		| any(.status == "False" and ((.reason // "") | test("Fail|Error|Invalid|Drift|Conflict"; "i")))
	' >/dev/null
}

node_fill() {
	local kind="$1" status="$2"
	if [[ "$status" == "missing" ]]; then
		printf '%s\n' "#ffeeee"
	elif [[ "$kind" == "Snapshot" || "$kind" == *Snapshot ]]; then
		printf '%s\n' "#d9ecff"
	elif [[ "$kind" == "SnapshotContent" ]]; then
		printf '%s\n' "#ddf4dd"
	elif [[ "$kind" == "CustomSnapshotDefinition" ]]; then
		printf '%s\n' "#eadcff"
	elif [[ "$kind" == "ObjectKeeper" ]]; then
		printf '%s\n' "#ffd6d6"
	elif [[ "$kind" == "ManifestCaptureRequest" || "$kind" == "ManifestCheckpoint" ]]; then
		printf '%s\n' "#fff4c2"
	elif [[ "$kind" == "ManifestCheckpointContentChunk" ]]; then
		printf '%s\n' "#ffe8b3"
	else
		printf '%s\n' "#eeeeee"
	fi
}

kind_short() {
	case "$1" in
	Snapshot) printf '%s\n' "Snap" ;;
	SnapshotContent) printf '%s\n' "SC" ;;
	CustomSnapshotDefinition) printf '%s\n' "CSD" ;;
	ObjectKeeper) printf '%s\n' "OK" ;;
	ManifestCaptureRequest) printf '%s\n' "MCR" ;;
	ManifestCheckpoint) printf '%s\n' "MCP" ;;
	ManifestCheckpointContentChunk) printf '%s\n' "Chunk" ;;
	DemoVirtualMachineSnapshot) printf '%s\n' "VMSnap" ;;
	DemoVirtualDiskSnapshot) printf '%s\n' "DiskSnap" ;;
	*) printf '%s\n' "$1" ;;
	esac
}

ellipsize() {
	local value="$1" limit="${2:-24}"
	if (( ${#value} <= limit )); then
		printf '%s\n' "$value"
	else
		printf '%s…\n' "${value:0:limit}"
	fi
}

display_name() {
	local kind="$1" name="$2"
	if [[ "$kind" == "SnapshotContent" && -n "$SNAPSHOT" && "$name" == "$ROOT_CONTENT_NAME" ]]; then
		printf '%s\n' "$SNAPSHOT"
	elif [[ "$kind" == "ObjectKeeper" && -n "$SNAPSHOT" && "$name" == "$ROOT_OK_NAME" ]]; then
		printf '%s\n' "$SNAPSHOT"
	elif [[ "$kind" == "ManifestCheckpoint" ]]; then
		ellipsize "$name" 14
	elif [[ "$kind" == "ManifestCheckpointContentChunk" ]]; then
		ellipsize "$name" 16
	elif [[ "$kind" == "SnapshotContent" ]]; then
		ellipsize "$name" 18
	elif [[ "$kind" == "ObjectKeeper" ]]; then
		local shortened="$name"
		shortened="${shortened#ret-snap-}"
		shortened="${shortened#ret-mcr-}"
		ellipsize "$shortened" 22
	else
		ellipsize "$name" 24
	fi
}

ready_badge() {
	local ready="$1" node_color="$2"
	if [[ "$node_color" == "red" ]]; then
		printf '%s\n' "[Failed]"
	elif [[ "$ready" == "True" ]]; then
		printf '%s\n' "[Ready]"
	elif [[ "$ready" == "False" ]]; then
		printf '%s\n' "[Pending]"
	else
		printf '%s\n' "[Unknown]"
	fi
}

kind_has_status_badge() {
	case "$1" in
	Snapshot|*Snapshot|SnapshotContent|CustomSnapshotDefinition|ManifestCaptureRequest|ManifestCheckpoint)
		return 0
		;;
	*)
		return 1
		;;
	esac
}

node_diagnostics() {
	local json="$1" kind="$2" ready="$3" diag=""
	if [[ "$kind" == "SnapshotContent" ]]; then
		if printf '%s' "$json" | jq -e 'any(.metadata.ownerReferences[]?; (.kind | endswith("Snapshot")))' >/dev/null; then
			diag="${diag} [BAD OWNER]"
		fi
		if printf '%s' "$json" | jq -e '(.metadata.ownerReferences // []) | length == 0' >/dev/null; then
			diag="${diag} [ORPHAN]"
		fi
	fi
	if printf '%s' "$json" | jq -e '[.metadata.ownerReferences[]? | select(.controller == true)] | length > 1' >/dev/null; then
		diag="${diag} [CONFLICT]"
	fi
	if [[ "$ready" == "False" ]]; then
		diag="${diag} [PENDING]"
	fi
	printf '%s\n' "${diag# }"
}

compact_label() {
	local kind="$1" name="$2" ready="$3" color="$4" diag="$5" short display badge
	short=$(kind_short "$kind")
	display=$(display_name "$kind" "$name")
	if [[ "$ready" == "Unknown" ]] && ! kind_has_status_badge "$kind"; then
		if [[ -n "$diag" ]]; then
			printf '%s\\n%s\\n%s\n' "$short" "$display" "$diag"
		else
			printf '%s\\n%s\n' "$short" "$display"
		fi
		return
	fi
	badge=$(ready_badge "$ready" "$color")
	if [[ -n "$diag" ]]; then
		printf '%s\\n%s\\n%s %s\n' "$short" "$display" "$badge" "$diag"
	else
		printf '%s\\n%s\\n%s\n' "$short" "$display" "$badge"
	fi
}

add_check() {
	local level="$1" message="$2"
	printf '%s\t%s\n' "$level" "$message" >>"$CHECKS"
	if [[ "$level" == "VIOLATION" ]]; then
		VIOLATIONS=$((VIOLATIONS + 1))
	fi
}

add_edge() {
	local from="$1" to="$2" label="$3" color="$4" style="$5" penwidth="${6:-1}"
	printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$from" "$to" "$label" "$color" "$style" "$penwidth" >>"$EDGES"
}

record_detail_json() {
	local json="$1" key="$2" warnings="${3:-}"
	printf '%s' "$json" | jq -c --arg key "$key" --arg warnings "$warnings" '
		{
			key: $key,
			apiVersion: .apiVersion,
			kind: .kind,
			namespace: (.metadata.namespace // ""),
			name: .metadata.name,
			uid: (.metadata.uid // ""),
			ownerReferences: (.metadata.ownerReferences // []),
			conditions: (.status.conditions // []),
			labels: (.metadata.labels // {}),
			annotations: (.metadata.annotations // {}),
			refs: {
				boundSnapshotContentName: (.status.boundSnapshotContentName // null),
				manifestCaptureRequestName: (.status.manifestCaptureRequestName // null),
				manifestCheckpointName: (.status.manifestCheckpointName // null),
				childrenSnapshotRefs: (.status.childrenSnapshotRefs // []),
				childrenSnapshotContentRefs: (.status.childrenSnapshotContentRefs // []),
				chunks: (.status.chunks // []),
				checkpointName: (.spec.checkpointName // null),
				chunkIndex: (.spec.index // null),
				objectsCount: (.spec.objectsCount // null),
				followObjectRef: (.spec.followObjectRef // null),
				sourceRef: (.spec.sourceRef // null),
				dataRef: (.status.dataRef // null)
			},
			warnings: (if $warnings == "" then [] else ($warnings | split("|")) end)
		}
	' >>"$DETAILS_RAW"
}

record_missing_detail() {
	local api="$1" kind="$2" ns="$3" name="$4" key
	key=$(key_for "$api" "$kind" "$ns" "$name")
	jq -nc --arg key "$key" --arg api "$api" --arg kind "$kind" --arg ns "$ns" --arg name "$name" '
		{
			key: $key,
			apiVersion: $api,
			kind: $kind,
			namespace: $ns,
			name: $name,
			missing: true,
			ownerReferences: [],
			conditions: [],
			labels: {},
			annotations: {},
			refs: {},
			warnings: ["MISSING"]
		}
	' >>"$DETAILS_RAW"
}

add_missing_node() {
	local api="$1" kind="$2" ns="$3" name="$4" key label fill
	key=$(key_for "$api" "$kind" "$ns" "$name")
	label="MISSING ${kind}/${name}"
	if [[ -n "$ns" ]]; then
		label="${label}\\nns: ${ns}"
	fi
	fill=$(node_fill "$kind" "missing")
	replace_node_row "$key" "$(node_row "$key" "$api" "$kind" "$ns" "$name" "missing" "Unknown" "$label" "$fill" "red" "dashed" "MISSING")"
	record_missing_detail "$api" "$kind" "$ns" "$name"
}

add_placeholder_node() {
	local api="$1" kind="$2" ns="$3" name="$4" key label fill
	key=$(key_for "$api" "$kind" "$ns" "$name")
	if node_exists "$key"; then
		return
	fi
	label="${kind}/${name}"
	if [[ -n "$ns" ]]; then
		label="${label}\\nns: ${ns}"
	fi
	label="${label}\\nnot traversed"
	fill=$(node_fill "$kind" "placeholder")
	replace_node_row "$key" "$(node_row "$key" "$api" "$kind" "$ns" "$name" "placeholder" "Unknown" "$label" "$fill" "gray" "dashed" "")"
}

dump_object() {
	local api="$1" kind="$2" ns="$3" name="$4" key yaml
	key=$(key_for "$api" "$kind" "$ns" "$name")
	if grep -Fxq "$key" "$DUMPED"; then
		return
	fi
	printf '%s\n' "$key" >>"$DUMPED"
	{
		printf '%s\n' "---"
		if [[ -n "$ns" ]]; then
			printf '# %s/%s namespace=%s\n' "$kind" "$name" "$ns"
		else
			printf '# %s/%s\n' "$kind" "$name"
		fi
	} >>"$OBJECTS_FILE"
	if yaml=$(get_yaml "$api" "$kind" "$ns" "$name"); then
		printf '%s\n' "$yaml" >>"$OBJECTS_FILE"
	else
		printf '# MISSING %s/%s\n' "$kind" "$name" >>"$OBJECTS_FILE"
	fi
}

add_existing_node() {
	local json="$1" key api kind ns name uid short ready reason fill color style label status diag warnings
	api=$(printf '%s' "$json" | jq -r '.apiVersion')
	kind=$(printf '%s' "$json" | jq -r '.kind')
	ns=$(printf '%s' "$json" | jq -r '.metadata.namespace // ""')
	name=$(printf '%s' "$json" | jq -r '.metadata.name')
	uid=$(printf '%s' "$json" | jq -r '.metadata.uid // ""')
	short=$(short_uid "$uid")
	if [[ "$kind" == "CustomSnapshotDefinition" ]]; then
		ready=$(printf '%s' "$json" | csd_ready_status)
		reason=$(printf '%s' "$json" | csd_condition_reason)
	else
		ready=$(printf '%s' "$json" | ready_status)
		reason=$(printf '%s' "$json" | condition_reason)
	fi
	key=$(key_for "$api" "$kind" "$ns" "$name")
	status="existing"
	fill=$(node_fill "$kind" "$status")
	color="black"
	style="solid"
	if [[ "$ready" == "False" ]]; then
		color="orange"
	fi
	if printf '%s' "$json" | has_failed_condition; then
		color="red"
	fi
	diag=$(node_diagnostics "$json" "$kind" "$ready")
	if [[ "$diag" == *"BAD OWNER"* || "$diag" == *"ORPHAN"* || "$diag" == *"CONFLICT"* ]]; then
		color="red"
	fi

	label="${kind}/${name}"
	if [[ -n "$ns" ]]; then
		label="${label}\\nns: ${ns}"
	fi
	label="${label}\\nReady=${ready}"
	if [[ -n "$reason" ]]; then
		label="${label}\\nphase/reason: ${reason}"
	fi
	if [[ -n "$short" ]]; then
		label="${label}\\nuid: ${short}"
	fi
	if [[ "$kind" == "SnapshotContent" ]]; then
		local mcp
		mcp=$(printf '%s' "$json" | jq -r '.status.manifestCheckpointName // ""')
		if [[ -n "$mcp" ]]; then
			label="${label}\\nMCP=${mcp}"
		fi
	elif [[ "$kind" == "ObjectKeeper" ]]; then
		local mode ttl follows
		mode=$(printf '%s' "$json" | jq -r '.spec.mode // ""')
		ttl=$(printf '%s' "$json" | jq -r '.spec.ttl // ""')
		follows=$(printf '%s' "$json" | jq -r 'if .spec.followObjectRef then (.spec.followObjectRef.kind + "/" + .spec.followObjectRef.name) else "" end')
		[[ -n "$mode" ]] && label="${label}\\nmode=${mode}"
		[[ -n "$ttl" ]] && label="${label}\\nttl=${ttl}"
		[[ -n "$follows" ]] && label="${label}\\nfollows=${follows}"
	elif [[ "$kind" == "ManifestCheckpointContentChunk" ]]; then
		local index objects_count
		index=$(printf '%s' "$json" | jq -r '.spec.index // ""')
		objects_count=$(printf '%s' "$json" | jq -r '.spec.objectsCount // ""')
		[[ -n "$index" ]] && label="${label}\\nindex=${index}"
		[[ -n "$objects_count" ]] && label="${label}\\nobjects=${objects_count}"
	fi

	replace_node_row "$key" "$(node_row "$key" "$api" "$kind" "$ns" "$name" "$status" "$ready" "$label" "$fill" "$color" "$style" "$diag")"
	warnings=$(printf '%s' "$diag" | sed -e 's/\] \[/|/g' -e 's/^\[//' -e 's/\]$//')
	record_detail_json "$json" "$key" "$warnings"
}

owner_namespace() {
	local current_ns="$1" owner_kind="$2"
	case "$owner_kind" in
	ObjectKeeper|SnapshotContent|ManifestCheckpoint)
		printf '%s\n' ""
		;;
	*)
		printf '%s\n' "$current_ns"
		;;
	esac
}

add_owner_edges() {
	local json="$1" from_key="$2" current_ns="$3"
	printf '%s' "$json" | jq -r '.metadata.ownerReferences[]? | [.apiVersion, .kind, .name] | @tsv' \
		| while IFS=$'\t' read -r api kind name; do
			[[ -n "$api" && -n "$kind" && -n "$name" ]] || continue
			local owner_ns owner_key
			owner_ns=$(owner_namespace "$current_ns" "$kind")
			owner_key=$(key_for "$api" "$kind" "$owner_ns" "$name")
			add_placeholder_node "$api" "$kind" "$owner_ns" "$name"
			add_edge "$from_key" "$owner_key" "ownerRef" "red" "solid" "2"
		done
}

check_owner_ref() {
	local json="$1" owner_kind="$2" owner_name="$3" message="$4"
	if printf '%s' "$json" | jq -e --arg kind "$owner_kind" --arg name "$owner_name" \
		'any(.metadata.ownerReferences[]?; .kind == $kind and .name == $name)' >/dev/null; then
		add_check "OK" "$message"
	else
		add_check "VIOLATION" "$message"
	fi
}

check_no_content_owner_snapshot() {
	local json="$1" content_name="$2"
	if printf '%s' "$json" | jq -e 'any(.metadata.ownerReferences[]?; (.kind | endswith("Snapshot")))' >/dev/null; then
		add_check "VIOLATION" "No SnapshotContent ownerRef to Snapshot: ${content_name}"
	else
		add_check "OK" "No SnapshotContent ownerRef to Snapshot: ${content_name}"
	fi
}

find_objectkeepers_following() {
	local api="$1" kind="$2" ns="$3" name="$4" uid="$5"
	kubectl get objectkeepers.deckhouse.io -o json 2>/dev/null \
		| jq -r --arg api "$api" --arg kind "$kind" --arg ns "$ns" --arg name "$name" --arg uid "$uid" '
			.items[]?
			| select(.spec.followObjectRef.kind == $kind)
			| select(.spec.followObjectRef.name == $name)
			| select(($ns == "") or (.spec.followObjectRef.namespace == $ns))
			| select(($uid == "") or (.spec.followObjectRef.uid == $uid))
			| .metadata.name
		' || true
}

visited() {
	local key="$1"
	grep -Fxq "$key" "$VISITED"
}

mark_visited() {
	local key="$1"
	printf '%s\n' "$key" >>"$VISITED"
}

visit_objectkeeper() {
	local name="$1" depth="$2" json key uid
	key=$(key_for "deckhouse.io/v1alpha1" "ObjectKeeper" "" "$name")
	if visited "$key"; then
		return
	fi
	if (( depth > MAX_DEPTH )); then
		add_placeholder_node "deckhouse.io/v1alpha1" "ObjectKeeper" "" "$name"
		return
	fi
	mark_visited "$key"
	if json=$(get_json "deckhouse.io/v1alpha1" "ObjectKeeper" "" "$name"); then
		add_existing_node "$json"
		dump_object "deckhouse.io/v1alpha1" "ObjectKeeper" "" "$name"
		uid=$(printf '%s' "$json" | jq -r '.spec.followObjectRef.uid // ""')
		local f_api f_kind f_ns f_name follow_key
		f_api=$(printf '%s' "$json" | jq -r '.spec.followObjectRef.apiVersion // ""')
		f_kind=$(printf '%s' "$json" | jq -r '.spec.followObjectRef.kind // ""')
		f_ns=$(printf '%s' "$json" | jq -r '.spec.followObjectRef.namespace // ""')
		f_name=$(printf '%s' "$json" | jq -r '.spec.followObjectRef.name // ""')
		if [[ -n "$f_kind" && -n "$f_name" ]]; then
			[[ -z "$f_api" ]] && f_api="unknown"
			follow_key=$(key_for "$f_api" "$f_kind" "$f_ns" "$f_name")
			add_placeholder_node "$f_api" "$f_kind" "$f_ns" "$f_name"
			add_edge "$key" "$follow_key" "spec.followObjectRef" "gray" "dashed" "1"
		fi
		: "$uid"
	else
		add_missing_node "deckhouse.io/v1alpha1" "ObjectKeeper" "" "$name"
		dump_object "deckhouse.io/v1alpha1" "ObjectKeeper" "" "$name"
	fi
}

visit_mcp() {
	local name="$1" depth="$2" parent_content="$3" json key
	key=$(key_for "state-snapshotter.deckhouse.io/v1alpha1" "ManifestCheckpoint" "" "$name")
	if visited "$key"; then
		return
	fi
	if (( depth > MAX_DEPTH )); then
		add_placeholder_node "state-snapshotter.deckhouse.io/v1alpha1" "ManifestCheckpoint" "" "$name"
		return
	fi
	mark_visited "$key"
	if json=$(get_json "state-snapshotter.deckhouse.io/v1alpha1" "ManifestCheckpoint" "" "$name"); then
		add_existing_node "$json"
		dump_object "state-snapshotter.deckhouse.io/v1alpha1" "ManifestCheckpoint" "" "$name"
		add_owner_edges "$json" "$key" ""
		if [[ -n "$parent_content" ]]; then
			check_owner_ref "$json" "SnapshotContent" "$parent_content" "ManifestCheckpoint/${name} ownerRef -> SnapshotContent/${parent_content}"
		fi
		printf '%s' "$json" | jq -r '.status.chunks[]? | .name // empty' \
			| while IFS= read -r chunk_name; do
				[[ -n "$chunk_name" ]] || continue
				local chunk_key
				chunk_key=$(key_for "state-snapshotter.deckhouse.io/v1alpha1" "ManifestCheckpointContentChunk" "" "$chunk_name")
				add_edge "$key" "$chunk_key" "status.chunks" "orange" "solid" "1"
				visit_chunk "$chunk_name" $((depth + 1)) "$name"
			done
	else
		add_missing_node "state-snapshotter.deckhouse.io/v1alpha1" "ManifestCheckpoint" "" "$name"
		dump_object "state-snapshotter.deckhouse.io/v1alpha1" "ManifestCheckpoint" "" "$name"
	fi
}

visit_chunk() {
	local name="$1" depth="$2" checkpoint_name="$3" json key
	key=$(key_for "state-snapshotter.deckhouse.io/v1alpha1" "ManifestCheckpointContentChunk" "" "$name")
	if visited "$key"; then
		return
	fi
	if (( depth > MAX_DEPTH )); then
		add_placeholder_node "state-snapshotter.deckhouse.io/v1alpha1" "ManifestCheckpointContentChunk" "" "$name"
		return
	fi
	mark_visited "$key"
	if json=$(get_json "state-snapshotter.deckhouse.io/v1alpha1" "ManifestCheckpointContentChunk" "" "$name"); then
		add_existing_node "$json"
		dump_object "state-snapshotter.deckhouse.io/v1alpha1" "ManifestCheckpointContentChunk" "" "$name"
		add_owner_edges "$json" "$key" ""
		if printf '%s' "$json" | jq -e --arg checkpoint "$checkpoint_name" '.spec.checkpointName == $checkpoint' >/dev/null; then
			add_check "OK" "Chunk/${name} spec.checkpointName -> ManifestCheckpoint/${checkpoint_name}"
		else
			add_check "VIOLATION" "Chunk/${name} spec.checkpointName -> ManifestCheckpoint/${checkpoint_name}"
		fi
		check_owner_ref "$json" "ManifestCheckpoint" "$checkpoint_name" "Chunk/${name} ownerRef -> ManifestCheckpoint/${checkpoint_name}"
	else
		add_missing_node "state-snapshotter.deckhouse.io/v1alpha1" "ManifestCheckpointContentChunk" "" "$name"
		dump_object "state-snapshotter.deckhouse.io/v1alpha1" "ManifestCheckpointContentChunk" "" "$name"
	fi
}

visit_mcr() {
	local ns="$1" name="$2" depth="$3" json key uid ok_name ok_key
	key=$(key_for "state-snapshotter.deckhouse.io/v1alpha1" "ManifestCaptureRequest" "$ns" "$name")
	if visited "$key"; then
		return
	fi
	if (( depth > MAX_DEPTH )); then
		add_placeholder_node "state-snapshotter.deckhouse.io/v1alpha1" "ManifestCaptureRequest" "$ns" "$name"
		return
	fi
	mark_visited "$key"
	if json=$(get_json "state-snapshotter.deckhouse.io/v1alpha1" "ManifestCaptureRequest" "$ns" "$name"); then
		add_existing_node "$json"
		dump_object "state-snapshotter.deckhouse.io/v1alpha1" "ManifestCaptureRequest" "$ns" "$name"
		add_owner_edges "$json" "$key" "$ns"
		uid=$(printf '%s' "$json" | jq -r '.metadata.uid // ""')
		while IFS= read -r ok_name; do
			[[ -n "$ok_name" ]] || continue
			visit_objectkeeper "$ok_name" $((depth + 1))
			ok_key=$(key_for "deckhouse.io/v1alpha1" "ObjectKeeper" "" "$ok_name")
			add_edge "$ok_key" "$key" "spec.followObjectRef" "gray" "dashed" "1"
		done < <(find_objectkeepers_following "state-snapshotter.deckhouse.io/v1alpha1" "ManifestCaptureRequest" "$ns" "$name" "$uid")
	else
		add_missing_node "state-snapshotter.deckhouse.io/v1alpha1" "ManifestCaptureRequest" "$ns" "$name"
		dump_object "state-snapshotter.deckhouse.io/v1alpha1" "ManifestCaptureRequest" "$ns" "$name"
		add_check "WARN" "MCR missing after cleanup: ${ns}/${name}"
	fi
}

visit_source_ref() {
	local api="$1" kind="$2" ns="$3" name="$4" depth="$5" json key
	key=$(key_for "$api" "$kind" "$ns" "$name")
	if visited "$key"; then
		return
	fi
	if (( depth > MAX_DEPTH )); then
		add_placeholder_node "$api" "$kind" "$ns" "$name"
		return
	fi
	mark_visited "$key"
	if json=$(get_json_flexible "$api" "$kind" "$ns" "$name"); then
		add_existing_node "$json"
		dump_object "$api" "$kind" "$(printf '%s' "$json" | jq -r '.metadata.namespace // ""')" "$name"
		add_owner_edges "$json" "$key" "$(printf '%s' "$json" | jq -r '.metadata.namespace // ""')"
	else
		add_missing_node "$api" "$kind" "$ns" "$name"
		dump_object "$api" "$kind" "$ns" "$name"
	fi
}

visit_content() {
	local name="$1" depth="$2" parent_content="$3" json key api kind ns ready mcp child data_api data_kind data_name data_key owner_tmp
	api="storage.deckhouse.io/v1alpha1"
	kind="SnapshotContent"
	ns=""
	key=$(key_for "$api" "$kind" "$ns" "$name")
	if visited "$key"; then
		return
	fi
	if (( depth > MAX_DEPTH )); then
		add_placeholder_node "$api" "$kind" "$ns" "$name"
		return
	fi
	mark_visited "$key"
	if ! json=$(get_json "$api" "$kind" "$ns" "$name"); then
		add_missing_node "$api" "$kind" "$ns" "$name"
		dump_object "$api" "$kind" "$ns" "$name"
		return
	fi
	add_existing_node "$json"
	dump_object "$api" "$kind" "$ns" "$name"
	add_owner_edges "$json" "$key" ""
	owner_tmp="${TMP_DIR}/content-ok-owners.$$"
	printf '%s' "$json" | jq -r '.metadata.ownerReferences[]? | select(.kind == "ObjectKeeper") | .name' >"$owner_tmp"
	while IFS= read -r ok_name; do
		[[ -n "$ok_name" ]] || continue
		if [[ "$depth" == "0" && -z "$ROOT_OK_NAME" ]]; then
			ROOT_OK_NAME="$ok_name"
		fi
		visit_objectkeeper "$ok_name" $((depth + 1))
	done <"$owner_tmp"
	if printf '%s' "$json" | jq -e '(.metadata.ownerReferences // []) | length == 0' >/dev/null; then
		add_check "VIOLATION" "SnapshotContent/${name} has no lifecycle ownerRef"
	fi
	if printf '%s' "$json" | jq -e '[.metadata.ownerReferences[]? | select(.controller == true)] | length > 1' >/dev/null; then
		add_check "VIOLATION" "SnapshotContent/${name} has conflicting controller ownerRefs"
	fi
	check_no_content_owner_snapshot "$json" "$name"
	if [[ -n "$parent_content" ]]; then
		check_owner_ref "$json" "SnapshotContent" "$parent_content" "Child SnapshotContent/${name} ownerRef -> parent SnapshotContent/${parent_content}"
	fi

	if [[ -z "$ROOT_CONTENT_NAME" ]]; then
		ROOT_CONTENT_NAME="$name"
		ROOT_CONTENT_KEY="$key"
	fi

	mcp=$(printf '%s' "$json" | jq -r '.status.manifestCheckpointName // ""')
	if [[ -n "$mcp" ]]; then
		local mcp_key
		mcp_key=$(key_for "state-snapshotter.deckhouse.io/v1alpha1" "ManifestCheckpoint" "" "$mcp")
		add_edge "$key" "$mcp_key" "status.manifestCheckpointName" "blue" "solid" "1"
		visit_mcp "$mcp" $((depth + 1)) "$name"
	fi

	printf '%s' "$json" | jq -r '.status.childrenSnapshotContentRefs[]? | .name // empty' \
		| while IFS= read -r child; do
			[[ -n "$child" ]] || continue
			local child_key
			child_key=$(key_for "$api" "$kind" "" "$child")
			add_edge "$key" "$child_key" "status.childrenSnapshotContentRefs" "green" "solid" "1"
			visit_content "$child" $((depth + 1)) "$name"
		done

	data_api=$(printf '%s' "$json" | jq -r '.status.dataRef.apiVersion // ""')
	data_kind=$(printf '%s' "$json" | jq -r '.status.dataRef.kind // ""')
	data_name=$(printf '%s' "$json" | jq -r '.status.dataRef.name // ""')
	if [[ -n "$data_kind" && -n "$data_name" ]]; then
		data_key=$(key_for "$data_api" "$data_kind" "" "$data_name")
		add_edge "$key" "$data_key" "status.dataRef" "orange" "solid" "1"
		visit_source_ref "$data_api" "$data_kind" "" "$data_name" $((depth + 1))
	fi
}

visit_snapshot() {
	local api="$1" kind="$2" ns="$3" name="$4" depth="$5" parent_snapshot="$6" json key uid content mcr child_api child_kind child_name source_api source_kind source_name ok_name ok_key
	key=$(key_for "$api" "$kind" "$ns" "$name")
	if visited "$key"; then
		return
	fi
	if (( depth > MAX_DEPTH )); then
		add_placeholder_node "$api" "$kind" "$ns" "$name"
		return
	fi
	mark_visited "$key"
	if ! json=$(get_json "$api" "$kind" "$ns" "$name"); then
		add_missing_node "$api" "$kind" "$ns" "$name"
		dump_object "$api" "$kind" "$ns" "$name"
		if [[ "$depth" == "0" && "$ALLOW_MISSING_ROOT" != "1" ]]; then
			fail "root Snapshot ${ns}/${name} not found; use --allow-missing-root or --snapshotcontent for retained graph"
		fi
		return
	fi
	add_existing_node "$json"
	dump_object "$api" "$kind" "$ns" "$name"
	add_owner_edges "$json" "$key" "$ns"
	if [[ -n "$parent_snapshot" ]]; then
		check_owner_ref "$json" "Snapshot" "$parent_snapshot" "Child ${kind}/${name} ownerRef -> parent Snapshot/${parent_snapshot}"
	fi

	uid=$(printf '%s' "$json" | jq -r '.metadata.uid // ""')
	while IFS= read -r ok_name; do
		[[ -n "$ok_name" ]] || continue
		if [[ -z "$ROOT_OK_NAME" && "$depth" == "0" ]]; then
			ROOT_OK_NAME="$ok_name"
		fi
		visit_objectkeeper "$ok_name" $((depth + 1))
		ok_key=$(key_for "deckhouse.io/v1alpha1" "ObjectKeeper" "" "$ok_name")
		add_edge "$ok_key" "$key" "spec.followObjectRef" "gray" "dashed" "1"
	done < <(find_objectkeepers_following "$api" "$kind" "$ns" "$name" "$uid")

	content=$(printf '%s' "$json" | jq -r '.status.boundSnapshotContentName // ""')
	if [[ -n "$content" ]]; then
		local content_key
		content_key=$(key_for "storage.deckhouse.io/v1alpha1" "SnapshotContent" "" "$content")
		if [[ "$depth" == "0" ]]; then
			ROOT_CONTENT_NAME="$content"
			ROOT_CONTENT_KEY="$content_key"
		fi
		add_edge "$key" "$content_key" "status.boundSnapshotContentName" "blue" "solid" "1"
		visit_content "$content" $((depth + 1)) ""
	fi

	mcr=$(printf '%s' "$json" | jq -r '.status.manifestCaptureRequestName // ""')
	if [[ -n "$mcr" ]]; then
		local mcr_key
		mcr_key=$(key_for "state-snapshotter.deckhouse.io/v1alpha1" "ManifestCaptureRequest" "$ns" "$mcr")
		add_edge "$key" "$mcr_key" "status.manifestCaptureRequestName" "blue" "solid" "1"
		visit_mcr "$ns" "$mcr" $((depth + 1))
	fi

	printf '%s' "$json" | jq -r '.status.childrenSnapshotRefs[]? | [.apiVersion, .kind, .name] | @tsv' \
		| while IFS=$'\t' read -r child_api child_kind child_name; do
			[[ -n "$child_api" && -n "$child_kind" && -n "$child_name" ]] || continue
			local child_key
			child_key=$(key_for "$child_api" "$child_kind" "$ns" "$child_name")
			add_edge "$key" "$child_key" "status.childrenSnapshotRefs" "green" "solid" "1"
			visit_snapshot "$child_api" "$child_kind" "$ns" "$child_name" $((depth + 1)) "$name"
		done

	source_api=$(printf '%s' "$json" | jq -r '.spec.sourceRef.apiVersion // ""')
	source_kind=$(printf '%s' "$json" | jq -r '.spec.sourceRef.kind // ""')
	source_name=$(printf '%s' "$json" | jq -r '.spec.sourceRef.name // ""')
	if [[ -n "$source_kind" && -n "$source_name" ]]; then
		local source_key
		source_key=$(key_for "$source_api" "$source_kind" "$ns" "$source_name")
		add_edge "$key" "$source_key" "spec.sourceRef" "purple" "dashed" "1"
		visit_source_ref "$source_api" "$source_kind" "$ns" "$source_name" $((depth + 1))
	fi
}

include_namespace_inventory() {
	local resource resources item_file api kind ns name key previous_status
	[[ -n "$NS" && "$INCLUDE_NAMESPACE_RESOURCES" == "1" ]] || return 0
	resources="${TMP_DIR}/namespaced-resources.txt"
	kubectl api-resources --namespaced=true --verbs=list -o name 2>/dev/null >"$resources" || return
	while IFS= read -r resource; do
		[[ -n "$resource" ]] || continue
		case "$resource" in
		events|events.events.k8s.io)
			continue
			;;
		esac
		: >"${TMP_DIR}/inventory-items.ndjson"
		kubectl -n "$NS" get "$resource" -o json 2>/dev/null \
			| jq -c '.items[]?' >"${TMP_DIR}/inventory-items.ndjson" || true
		while IFS= read -r item_file; do
			[[ -n "$item_file" ]] || continue
			api=$(printf '%s' "$item_file" | jq -r '.apiVersion')
			kind=$(printf '%s' "$item_file" | jq -r '.kind')
			ns=$(printf '%s' "$item_file" | jq -r '.metadata.namespace // ""')
			name=$(printf '%s' "$item_file" | jq -r '.metadata.name')
			[[ -n "$api" && -n "$kind" && -n "$name" ]] || continue
			key=$(key_for "$api" "$kind" "$ns" "$name")
			# A namespace object may first appear as an ownerRef/sourceRef placeholder.
			# Replace that placeholder with the live object so inventory nodes are not dashed.
			previous_status=$(node_status "$key" 2>/dev/null || true)
			if [[ "$previous_status" == "existing" ]]; then
				continue
			fi
			add_existing_node "$item_file"
			dump_object "$api" "$kind" "$ns" "$name"
			add_owner_edges "$item_file" "$key" "$ns"
		done <"${TMP_DIR}/inventory-items.ndjson"
	done <"$resources"
}

include_cluster_context_inventory() {
	local resource item_file api kind ns name key previous_status
	[[ "$INCLUDE_NAMESPACE_RESOURCES" == "1" ]] || return 0
	for resource in customsnapshotdefinitions.state-snapshotter.deckhouse.io; do
		: >"${TMP_DIR}/cluster-context-items.ndjson"
		kubectl get "$resource" -o json 2>/dev/null \
			| jq -c '.items[]?' >"${TMP_DIR}/cluster-context-items.ndjson" || true
		while IFS= read -r item_file; do
			[[ -n "$item_file" ]] || continue
			api=$(printf '%s' "$item_file" | jq -r '.apiVersion')
			kind=$(printf '%s' "$item_file" | jq -r '.kind')
			ns=$(printf '%s' "$item_file" | jq -r '.metadata.namespace // ""')
			name=$(printf '%s' "$item_file" | jq -r '.metadata.name')
			[[ -n "$api" && -n "$kind" && -n "$name" ]] || continue
			key=$(key_for "$api" "$kind" "$ns" "$name")
			previous_status=$(node_status "$key" 2>/dev/null || true)
			if [[ "$previous_status" == "existing" ]]; then
				continue
			fi
			add_existing_node "$item_file"
			dump_object "$api" "$kind" "$ns" "$name"
			add_owner_edges "$item_file" "$key" "$ns"
		done <"${TMP_DIR}/cluster-context-items.ndjson"
	done
}

if [[ -n "$SNAPSHOT" ]]; then
	ROOT_KEY=$(key_for "storage.deckhouse.io/v1alpha1" "Snapshot" "$NS" "$SNAPSHOT")
	visit_snapshot "storage.deckhouse.io/v1alpha1" "Snapshot" "$NS" "$SNAPSHOT" 0 ""
elif [[ -n "$SNAPSHOTCONTENT" ]]; then
	ROOT_KEY=$(key_for "storage.deckhouse.io/v1alpha1" "SnapshotContent" "" "$SNAPSHOTCONTENT")
	ROOT_CONTENT_NAME="$SNAPSHOTCONTENT"
	ROOT_CONTENT_KEY="$ROOT_KEY"
	visit_content "$SNAPSHOTCONTENT" 0 ""
fi
include_namespace_inventory
include_cluster_context_inventory

if [[ -n "$ROOT_OK_NAME" ]]; then
	if ok_json=$(get_json "deckhouse.io/v1alpha1" "ObjectKeeper" "" "$ROOT_OK_NAME"); then
		if printf '%s' "$ok_json" | jq -e '(.metadata.ownerReferences // []) | length == 0' >/dev/null; then
			add_check "OK" "ObjectKeeper has no ownerReferences: ${ROOT_OK_NAME}"
		else
			add_check "VIOLATION" "ObjectKeeper has no ownerReferences: ${ROOT_OK_NAME}"
		fi
		if [[ -n "$SNAPSHOT" ]]; then
			if printf '%s' "$ok_json" | jq -e --arg ns "$NS" --arg name "$SNAPSHOT" \
				'.spec.followObjectRef.kind == "Snapshot" and .spec.followObjectRef.namespace == $ns and .spec.followObjectRef.name == $name' >/dev/null; then
				add_check "OK" "Root ObjectKeeper.spec.followObjectRef -> Snapshot/${NS}/${SNAPSHOT}"
			else
				add_check "VIOLATION" "Root ObjectKeeper.spec.followObjectRef -> Snapshot/${NS}/${SNAPSHOT}"
			fi
		fi
	fi
fi
if [[ -n "$ROOT_CONTENT_NAME" && -n "$ROOT_OK_NAME" ]]; then
	if content_json=$(get_json "storage.deckhouse.io/v1alpha1" "SnapshotContent" "" "$ROOT_CONTENT_NAME"); then
		check_owner_ref "$content_json" "ObjectKeeper" "$ROOT_OK_NAME" "Root SnapshotContent ownerRef -> ObjectKeeper/${ROOT_OK_NAME}"
	fi
fi

edge_group() {
	case "$1" in
	ownerRef) printf '%s\n' "owner" ;;
	status.childrenSnapshotRefs|status.childrenSnapshotContentRefs) printf '%s\n' "child" ;;
	status.chunks) printf '%s\n' "artifact" ;;
	spec.followObjectRef) printf '%s\n' "follow" ;;
	spec.sourceRef) printf '%s\n' "source" ;;
	status.*) printf '%s\n' "status" ;;
	*) printf '%s\n' "status" ;;
	esac
}

render_edge_in_mode() {
	local label="$1" group
	group=$(edge_group "$label")
	case "$MODE" in
	lifecycle)
		[[ "$group" == "owner" || "$group" == "follow" || "$group" == "artifact" || "$label" == "status.boundSnapshotContentName" || "$label" == "status.manifestCaptureRequestName" || "$label" == "status.manifestCheckpointName" || "$label" == "status.dataRef" ]]
		;;
	logical)
		[[ "$group" == "child" || "$group" == "status" || "$group" == "source" || "$group" == "artifact" || "$group" == "follow" ]]
		;;
	full)
		return 0
		;;
	smoke)
		[[ "$group" == "owner" || "$group" == "follow" || "$group" == "child" || "$group" == "artifact" || "$label" == "status.boundSnapshotContentName" || "$label" == "status.manifestCheckpointName" || "$label" == "status.dataRef" || "$group" == "source" ]]
		;;
	esac
}

edge_attrs() {
	local label="$1" group
	group=$(edge_group "$label")
	case "$group" in
	owner) printf '%s\t%s\t%s\t%s\n' "red" "solid" "3" "true" ;;
	child) printf '%s\t%s\t%s\t%s\n' "green4" "solid" "2" "true" ;;
	follow) printf '%s\t%s\t%s\t%s\n' "gray45" "dotted" "1" "true" ;;
	artifact) printf '%s\t%s\t%s\t%s\n' "orange" "solid" "2" "true" ;;
	source) printf '%s\t%s\t%s\t%s\n' "purple" "dashed" "1" "false" ;;
	status) printf '%s\t%s\t%s\t%s\n' "blue" "dashed" "1" "false" ;;
	*) printf '%s\t%s\t%s\t%s\n' "black" "solid" "1" "false" ;;
	esac
}

emit_nodes_for_scope() {
	local scope="$1"
	while IFS="$DELIM" read -r key api kind ns name status ready label fill color style diag; do
		[[ -n "$key" ]] || continue
		if [[ "$scope" == "namespaced" && -z "$ns" ]]; then
			continue
		fi
		if [[ "$scope" == "cluster" && -n "$ns" ]]; then
			continue
		fi
		local_key=$(json_quote "$key")
		local_label=$(dot_label_quote "$(compact_label "$kind" "$name" "$ready" "$color" "$diag")")
		tooltip=$(json_quote "$label")
		penwidth="1"
		[[ "$color" == "red" ]] && penwidth="2"
		printf '    %s [label=%s, tooltip=%s, fillcolor="%s", color="%s", style="filled,%s", penwidth=%s];\n' \
			"$local_key" "$local_label" "$tooltip" "$fill" "$color" "$style" "$penwidth"
		: "$api" "$status"
	done <"$NODES"
}

generate_dot() {
	local graph_label
	graph_label="${TITLE} | ns=${NS:-cluster} | mode=${MODE} | ${TIMESTAMP}"
	if [[ -n "$DESCRIPTION" ]]; then
		graph_label="${graph_label}\\n${DESCRIPTION}"
	fi
	{
		printf '%s\n' "digraph snapshot_graph {"
		printf '  graph [rankdir=TB, compound=true, fontsize=10, label=%s, labelloc="t", nodesep=0.45, ranksep=0.65, splines=polyline];\n' \
			"$(dot_label_quote "$graph_label")"
		printf '%s\n' "  node [shape=box, style=\"rounded,filled\", fontname=\"Helvetica\", fontsize=10, margin=\"0.08,0.05\"];"
		printf '%s\n' "  edge [fontname=\"Helvetica\", fontsize=8];"
		printf '%s\n' "  subgraph cluster_namespaced {"
		printf '%s\n' "    label=\"Namespaced resources\";"
		printf '%s\n' "    style=\"rounded,filled\";"
		printf '%s\n' "    color=\"#d9d9d9\";"
		printf '%s\n' "    fillcolor=\"#fafafa\";"
		emit_nodes_for_scope "namespaced"
		printf '%s\n' "  }"
		printf '%s\n' "  subgraph cluster_cluster_scoped {"
		printf '%s\n' "    label=\"Cluster-scoped resources\";"
		printf '%s\n' "    style=\"rounded,filled\";"
		printf '%s\n' "    color=\"#d9d9d9\";"
		printf '%s\n' "    fillcolor=\"#fbfbf6\";"
		emit_nodes_for_scope "cluster"
		printf '%s\n' "  }"
		sort -u "$EDGES" | while IFS=$'\t' read -r from to label color style penwidth; do
			[[ -n "$from" && -n "$to" ]] || continue
			if ! render_edge_in_mode "$label"; then
				continue
			fi
			IFS=$'\t' read -r color style penwidth constraint <<<"$(edge_attrs "$label")"
			printf '  %s -> %s [label=%s, color="%s", style="%s", penwidth=%s, constraint=%s];\n' \
				"$(json_quote "$from")" "$(json_quote "$to")" "$(json_quote "$label")" "$color" "$style" "$penwidth" "$constraint"
		done
		cat <<'EOF'
  legend [shape=plain, margin=0, label=<
    <TABLE BORDER="0" CELLBORDER="1" CELLSPACING="0" CELLPADDING="3" COLOR="#cccccc">
      <TR><TD><B>Legend</B></TD></TR>
      <TR><TD ALIGN="LEFT"><FONT COLOR="red">━━</FONT> ownerRef</TD></TR>
      <TR><TD ALIGN="LEFT"><FONT COLOR="blue">- -</FONT> statusRef</TD></TR>
      <TR><TD ALIGN="LEFT"><FONT COLOR="green4">━━</FONT> childRef</TD></TR>
      <TR><TD ALIGN="LEFT"><FONT COLOR="orange">━━</FONT> artifactRef</TD></TR>
      <TR><TD ALIGN="LEFT"><FONT COLOR="gray45">···</FONT> followRef</TD></TR>
    </TABLE>
  >];
}
EOF
	} >"$DOT_FILE"
}

generate_summary() {
	local snapshots contents mcrs mcps chunks missing ready_false
	snapshots=$(awk -F "$DELIM" '$3 ~ /Snapshot$/ && $3 != "SnapshotContent" && $6 == "existing" { c++ } END { print c + 0 }' "$NODES")
	contents=$(awk -F "$DELIM" '$3 == "SnapshotContent" && $6 == "existing" { c++ } END { print c + 0 }' "$NODES")
	mcrs=$(awk -F "$DELIM" '$3 == "ManifestCaptureRequest" && $6 == "existing" { c++ } END { print c + 0 }' "$NODES")
	mcps=$(awk -F "$DELIM" '$3 == "ManifestCheckpoint" && $6 == "existing" { c++ } END { print c + 0 }' "$NODES")
	chunks=$(awk -F "$DELIM" '$3 == "ManifestCheckpointContentChunk" && $6 == "existing" { c++ } END { print c + 0 }' "$NODES")
	missing=$(awk -F "$DELIM" '$6 == "missing" { c++ } END { print c + 0 }' "$NODES")
	ready_false=$(awk -F "$DELIM" '$7 == "False" { c++ } END { print c + 0 }' "$NODES")
	{
		if [[ -n "$SNAPSHOT" ]]; then
			printf 'Root: Snapshot %s/%s\n' "$NS" "$SNAPSHOT"
		else
			printf 'Root: SnapshotContent/%s\n' "$SNAPSHOTCONTENT"
		fi
		printf 'Root content: %s\n' "${ROOT_CONTENT_NAME:-}"
		printf 'Root ObjectKeeper: %s\n' "${ROOT_OK_NAME:-}"
		printf 'Snapshots: %s\n' "$snapshots"
		printf 'SnapshotContents: %s\n' "$contents"
		printf 'MCRs: %s\n' "$mcrs"
		printf 'MCPs: %s\n' "$mcps"
		printf 'Chunks: %s\n' "$chunks"
		printf 'Missing objects: %s\n' "$missing"
		printf 'Ready false: %s\n' "$ready_false"
		printf 'Invariant checks:\n'
		if [[ -s "$CHECKS" ]]; then
			while IFS=$'\t' read -r level message; do
				case "$level" in
				OK) printf '[OK] %s\n' "$message" ;;
				WARN) printf '[WARN] %s\n' "$message" ;;
				VIOLATION) printf '[WARN] %s\n' "$message" ;;
				esac
			done <"$CHECKS"
		else
			printf '[WARN] No invariant checks were applicable\n'
		fi
	} >"$SUMMARY_FILE"
}

generate_stage_text() {
	{
		printf 'Title: %s\n' "$TITLE"
		printf 'Mode: %s\n' "$MODE"
		printf 'Namespace: %s\n' "${NS:-cluster}"
		printf 'Timestamp: %s\n' "$TIMESTAMP"
		if [[ -n "$DESCRIPTION" ]]; then
			printf '\n%s\n' "$DESCRIPTION"
		fi
	} >"$STAGE_FILE"
}

generate_details() {
	local checks_json
	checks_json=$(jq -Rn '
		[inputs | select(length > 0) | split("\t") | {level: .[0], message: .[1]}]
	' <"$CHECKS")
	jq -s \
		--arg title "$TITLE" \
		--arg mode "$MODE" \
		--arg namespace "${NS:-}" \
		--arg snapshot "${SNAPSHOT:-}" \
		--arg snapshotcontent "${SNAPSHOTCONTENT:-}" \
		--arg timestamp "$TIMESTAMP" \
		--argjson checks "$checks_json" '
		{
			title: $title,
			mode: $mode,
			namespace: $namespace,
			root: {
				snapshot: (if $snapshot == "" then null else $snapshot end),
				snapshotContent: (if $snapshotcontent == "" then null else $snapshotcontent end)
			},
			timestamp: $timestamp,
			nodes: (unique_by(.key) | sort_by(.kind, .namespace, .name)),
			invariantChecks: $checks
		}
	' "$DETAILS_RAW" >"$DETAILS_FILE"
}

generate_dot
generate_summary
generate_stage_text
generate_details
VIOLATIONS=$(awk -F '\t' '$1 == "VIOLATION" { c++ } END { print c + 0 }' "$CHECKS")

if command -v dot >/dev/null 2>&1; then
	dot -Tsvg "$DOT_FILE" -o "$SVG_FILE"
else
	warn "graphviz dot not found; saved DOT only: ${DOT_FILE}"
fi

log "wrote ${DOT_FILE}"
[[ -f "$SVG_FILE" ]] && log "wrote ${SVG_FILE}"
log "wrote ${OBJECTS_FILE}"
log "wrote ${SUMMARY_FILE}"
log "wrote ${DETAILS_FILE}"
log "wrote ${STAGE_FILE}"

if [[ "$STRICT" == "1" && "$VIOLATIONS" -gt 0 ]]; then
	fail "strict mode: ${VIOLATIONS} invariant violations; see ${SUMMARY_FILE}"
fi
