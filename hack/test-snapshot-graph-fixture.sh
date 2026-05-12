#!/usr/bin/env bash
# Fixture test for hack/snapshot-graph.sh rendering and diagnostics.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/snapshot-graph-fixture.XXXXXX")
trap 'rm -rf "$TMP_DIR"' EXIT

FIXTURE_DIR="${TMP_DIR}/fixtures"
BIN_DIR="${TMP_DIR}/bin"
OUT_DIR="${TMP_DIR}/out"
mkdir -p "$FIXTURE_DIR" "$BIN_DIR" "$OUT_DIR"

write_obj() {
	local resource="$1" name="$2"
	mkdir -p "${FIXTURE_DIR}/${resource}"
	cat >"${FIXTURE_DIR}/${resource}/${name}.json"
}

ready_condition='"conditions":[{"type":"Ready","status":"True","reason":"Completed","message":"ok"}]'

write_obj snapshots.storage.deckhouse.io root <<EOF
{"apiVersion":"storage.deckhouse.io/v1alpha1","kind":"Snapshot","metadata":{"namespace":"ns1","name":"root","uid":"snap-root","ownerReferences":[]},"status":{"boundSnapshotContentName":"sc-root","manifestCaptureRequestName":"mcr-root","childrenSnapshotRefs":[{"apiVersion":"demo.state-snapshotter.deckhouse.io/v1alpha1","kind":"DemoVirtualDiskSnapshot","name":"disk-snap"}],${ready_condition}}}
EOF

write_obj demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io disk-snap <<EOF
{"apiVersion":"demo.state-snapshotter.deckhouse.io/v1alpha1","kind":"DemoVirtualDiskSnapshot","metadata":{"namespace":"ns1","name":"disk-snap","uid":"snap-disk","ownerReferences":[{"apiVersion":"storage.deckhouse.io/v1alpha1","kind":"Snapshot","name":"root","uid":"snap-root","controller":true}]},"spec":{"sourceRef":{"apiVersion":"demo.state-snapshotter.deckhouse.io/v1alpha1","kind":"DemoVirtualDisk","name":"missing-disk"}},"status":{"boundSnapshotContentName":"sc-disk",${ready_condition}}}
EOF

write_obj snapshotcontents.storage.deckhouse.io sc-root <<EOF
{"apiVersion":"storage.deckhouse.io/v1alpha1","kind":"SnapshotContent","metadata":{"name":"sc-root","uid":"sc-root-uid","ownerReferences":[{"apiVersion":"deckhouse.io/v1alpha1","kind":"ObjectKeeper","name":"ok-root","uid":"ok-root-uid","controller":true}]},"status":{"manifestCheckpointName":"mcp-root","childrenSnapshotContentRefs":[{"name":"sc-disk"},{"name":"sc-bad"},{"name":"sc-missing"}],${ready_condition}}}
EOF

write_obj snapshotcontents.storage.deckhouse.io sc-disk <<EOF
{"apiVersion":"storage.deckhouse.io/v1alpha1","kind":"SnapshotContent","metadata":{"name":"sc-disk","uid":"sc-disk-uid","ownerReferences":[{"apiVersion":"storage.deckhouse.io/v1alpha1","kind":"SnapshotContent","name":"sc-root","uid":"sc-root-uid","controller":true}]},"status":{"manifestCheckpointName":"mcp-disk",${ready_condition}}}
EOF

write_obj snapshotcontents.storage.deckhouse.io sc-bad <<EOF
{"apiVersion":"storage.deckhouse.io/v1alpha1","kind":"SnapshotContent","metadata":{"name":"sc-bad","uid":"sc-bad-uid","ownerReferences":[{"apiVersion":"demo.state-snapshotter.deckhouse.io/v1alpha1","kind":"DemoVirtualDiskSnapshot","name":"disk-snap","uid":"snap-disk","controller":true}]},"status":{"manifestCheckpointName":"mcp-bad",${ready_condition}}}
EOF

write_obj objectkeepers.deckhouse.io ok-root <<EOF
{"apiVersion":"deckhouse.io/v1alpha1","kind":"ObjectKeeper","metadata":{"name":"ok-root","uid":"ok-root-uid","ownerReferences":[]},"spec":{"mode":"FollowObjectWithTTL","ttl":"1m0s","followObjectRef":{"apiVersion":"storage.deckhouse.io/v1alpha1","kind":"Snapshot","namespace":"ns1","name":"root","uid":"snap-root"}}}
EOF

write_obj manifestcapturerequests.state-snapshotter.deckhouse.io mcr-root <<EOF
{"apiVersion":"state-snapshotter.deckhouse.io/v1alpha1","kind":"ManifestCaptureRequest","metadata":{"namespace":"ns1","name":"mcr-root","uid":"mcr-root-uid","ownerReferences":[{"apiVersion":"storage.deckhouse.io/v1alpha1","kind":"Snapshot","name":"root","uid":"snap-root","controller":true}]},"spec":{"targets":[{"apiVersion":"v1","kind":"ConfigMap","name":"cm1"}]}}
EOF

write_obj customsnapshotdefinitions.state-snapshotter.deckhouse.io smoke-demo <<EOF
{"apiVersion":"state-snapshotter.deckhouse.io/v1alpha1","kind":"CustomSnapshotDefinition","metadata":{"name":"smoke-demo","uid":"csd-smoke-uid"},"spec":{"ownerModule":"smoke","snapshotResourceMapping":[{"resourceCRDName":"demovirtualdisks.demo.state-snapshotter.deckhouse.io","snapshotCRDName":"demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io"}]},"status":{"conditions":[{"type":"Accepted","status":"True","reason":"Valid"},{"type":"RBACReady","status":"True","reason":"Smoke"}]}}
EOF

write_obj manifestcheckpoints.state-snapshotter.deckhouse.io mcp-root <<EOF
{"apiVersion":"state-snapshotter.deckhouse.io/v1alpha1","kind":"ManifestCheckpoint","metadata":{"name":"mcp-root","uid":"mcp-root-uid","ownerReferences":[{"apiVersion":"storage.deckhouse.io/v1alpha1","kind":"SnapshotContent","name":"sc-root","uid":"sc-root-uid","controller":true}]},"status":{"chunks":[{"name":"chunk-root-0","index":0,"objectsCount":3}],${ready_condition}}}
EOF

write_obj manifestcheckpoints.state-snapshotter.deckhouse.io mcp-disk <<EOF
{"apiVersion":"state-snapshotter.deckhouse.io/v1alpha1","kind":"ManifestCheckpoint","metadata":{"name":"mcp-disk","uid":"mcp-disk-uid","ownerReferences":[{"apiVersion":"storage.deckhouse.io/v1alpha1","kind":"SnapshotContent","name":"sc-disk","uid":"sc-disk-uid","controller":true}]},"status":{${ready_condition}}}
EOF

write_obj manifestcheckpoints.state-snapshotter.deckhouse.io mcp-bad <<EOF
{"apiVersion":"state-snapshotter.deckhouse.io/v1alpha1","kind":"ManifestCheckpoint","metadata":{"name":"mcp-bad","uid":"mcp-bad-uid","ownerReferences":[{"apiVersion":"storage.deckhouse.io/v1alpha1","kind":"SnapshotContent","name":"sc-bad","uid":"sc-bad-uid","controller":true}]},"status":{${ready_condition}}}
EOF

write_obj manifestcheckpointcontentchunks.state-snapshotter.deckhouse.io chunk-root-0 <<EOF
{"apiVersion":"state-snapshotter.deckhouse.io/v1alpha1","kind":"ManifestCheckpointContentChunk","metadata":{"name":"chunk-root-0","uid":"chunk-root-0-uid","ownerReferences":[{"apiVersion":"state-snapshotter.deckhouse.io/v1alpha1","kind":"ManifestCheckpoint","name":"mcp-root","uid":"mcp-root-uid","controller":true}]},"spec":{"checkpointName":"mcp-root","index":0,"objectsCount":3,"data":"H4sIAAAAAAAA/4pWyslPzy/KSVHSUcpJLC5R0lFKrShQ0lEqLkksSQQA"}}
EOF

write_obj demovirtualdisks.demo.state-snapshotter.deckhouse.io live-disk <<EOF
{"apiVersion":"demo.state-snapshotter.deckhouse.io/v1alpha1","kind":"DemoVirtualDisk","metadata":{"namespace":"ns1","name":"live-disk","uid":"disk-live-uid","ownerReferences":[{"apiVersion":"demo.state-snapshotter.deckhouse.io/v1alpha1","kind":"DemoVirtualMachine","name":"vm-live","uid":"vm-live-uid"}]}}
EOF

write_obj demovirtualmachines.demo.state-snapshotter.deckhouse.io vm-live <<EOF
{"apiVersion":"demo.state-snapshotter.deckhouse.io/v1alpha1","kind":"DemoVirtualMachine","metadata":{"namespace":"ns1","name":"vm-live","uid":"vm-live-uid"}}
EOF

cat >"${BIN_DIR}/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

fixture_dir="${SNAPSHOT_GRAPH_FIXTURE_DIR:?}"
namespace=""
if [[ "${1:-}" == "-n" ]]; then
	namespace="$2"
	shift 2
fi

if [[ "${1:-}" == "api-resources" && "$*" == *"--namespaced=true"* ]]; then
	cat <<'RES'
demovirtualdisks.demo.state-snapshotter.deckhouse.io
demovirtualmachines.demo.state-snapshotter.deckhouse.io
RES
	exit 0
fi

if [[ "${1:-}" == "api-resources" ]]; then
	cat <<'RES'
NAME                         APIVERSION                                  NAMESPACED KIND
demovirtualdisksnapshots     demo.state-snapshotter.deckhouse.io/v1alpha1 true       DemoVirtualDiskSnapshot
demovirtualdisks             demo.state-snapshotter.deckhouse.io/v1alpha1 true       DemoVirtualDisk
demovirtualmachines          demo.state-snapshotter.deckhouse.io/v1alpha1 true       DemoVirtualMachine
RES
	exit 0
fi

[[ "${1:-}" == "get" ]] || { echo "unsupported kubectl args: $*" >&2; exit 1; }
resource="$2"

if [[ "$resource" == "objectkeepers.deckhouse.io" && "${3:-}" == "-o" && "${4:-}" == "json" ]]; then
	jq -s '{items: .}' "${fixture_dir}/${resource}"/*.json
	exit 0
fi

if [[ "${3:-}" == "-o" && "${4:-}" == "json" ]]; then
	jq -s '{items: .}' "${fixture_dir}/${resource}"/*.json 2>/dev/null || jq -n '{items: []}'
	exit 0
fi

name="${3:-}"
out="${5:-json}"
file="${fixture_dir}/${resource}/${name}.json"
[[ -f "$file" ]] || exit 1

case "$out" in
json)
	cat "$file"
	;;
yaml)
	cat "$file"
	;;
jsonpath=*)
	if [[ "$out" == *manifestCheckpointName* ]]; then
		jq -r '.status.manifestCheckpointName // ""' "$file"
	else
		jq -r '.metadata.name' "$file"
	fi
	;;
*)
	cat "$file"
	;;
esac
EOF
chmod +x "${BIN_DIR}/kubectl"

PATH="${BIN_DIR}:$PATH" SNAPSHOT_GRAPH_FIXTURE_DIR="$FIXTURE_DIR" \
	bash "${ROOT_DIR}/hack/snapshot-graph.sh" \
	--namespace ns1 \
	--snapshot root \
	--output-dir "$OUT_DIR" \
	--name fixture \
	--mode full \
	--title "Fixture graph" \
	--no-include-namespace-resources

test -s "${OUT_DIR}/fixture.full.dot"
test -s "${OUT_DIR}/fixture.full.details.json"
test -s "${OUT_DIR}/fixture.full.summary.txt"

grep -Fq 'Snap\nroot\n[Ready]' "${OUT_DIR}/fixture.full.dot"
grep -Fq 'SC\nroot\n[Ready]' "${OUT_DIR}/fixture.full.dot"
grep -Fq 'BAD OWNER' "${OUT_DIR}/fixture.full.dot"
grep -Fq 'MISSING' "${OUT_DIR}/fixture.full.dot"
grep -Fq 'Chunk\nchunk-root-0' "${OUT_DIR}/fixture.full.dot"
! grep -Fq 'Chunk\nchunk-root-0\n[Unknown]' "${OUT_DIR}/fixture.full.dot"
grep -Fq 'status.chunks' "${OUT_DIR}/fixture.full.dot"
grep -Fq 'ownerRef' "${OUT_DIR}/fixture.full.dot"
grep -Fq 'cluster_namespaced' "${OUT_DIR}/fixture.full.dot"
grep -Fq 'cluster_cluster_scoped' "${OUT_DIR}/fixture.full.dot"
jq -e '.nodes[] | select(.kind=="SnapshotContent" and .name=="sc-bad" and (.warnings[]?=="BAD OWNER"))' "${OUT_DIR}/fixture.full.details.json" >/dev/null
jq -e '.nodes[] | select(.kind=="DemoVirtualDisk" and .missing==true)' "${OUT_DIR}/fixture.full.details.json" >/dev/null
jq -e '.nodes[] | select(.kind=="ManifestCheckpointContentChunk" and .refs.checkpointName=="mcp-root" and .refs.objectsCount==3)' "${OUT_DIR}/fixture.full.details.json" >/dev/null
grep -q 'Child SnapshotContent/sc-disk ownerRef -> parent SnapshotContent/sc-root' "${OUT_DIR}/fixture.full.summary.txt"
grep -q 'Chunk/chunk-root-0 spec.checkpointName -> ManifestCheckpoint/mcp-root' "${OUT_DIR}/fixture.full.summary.txt"
grep -q 'Chunk/chunk-root-0 ownerRef -> ManifestCheckpoint/mcp-root' "${OUT_DIR}/fixture.full.summary.txt"
grep -q 'No SnapshotContent ownerRef to Snapshot: sc-bad' "${OUT_DIR}/fixture.full.summary.txt"
grep -Fq 'MCR\nmcr-root' "${OUT_DIR}/fixture.full.dot"
grep -Fq 'status.manifestCaptureRequestName' "${OUT_DIR}/fixture.full.dot"

PATH="${BIN_DIR}:$PATH" SNAPSHOT_GRAPH_FIXTURE_DIR="$FIXTURE_DIR" \
	bash "${ROOT_DIR}/hack/snapshot-graph.sh" \
	--namespace ns1 \
	--snapshot root \
	--output-dir "$OUT_DIR" \
	--name inventory \
	--mode logical \
	--title "Inventory graph" \
	--description "Fixture inventory: Disk ownerRef placeholder for VM must be replaced by the live VM object."

test -s "${OUT_DIR}/inventory.logical.stage.txt"
grep -Fq 'Fixture inventory: Disk ownerRef placeholder for VM must be replaced by the live VM object.' "${OUT_DIR}/inventory.logical.stage.txt"
grep -Fq 'DemoVirtualMachine\nvm-live' "${OUT_DIR}/inventory.logical.dot"
! grep -F 'DemoVirtualMachine\nvm-live' "${OUT_DIR}/inventory.logical.dot" | grep -Fq 'style="dashed"'
grep -Fq 'DemoVirtualDisk\nlive-disk' "${OUT_DIR}/inventory.logical.dot"
grep -Fq 'CSD\nsmoke-demo\n[Ready]' "${OUT_DIR}/inventory.logical.dot"
grep -Fq '"state-snapshotter.deckhouse.io/v1alpha1/CustomSnapshotDefinition//smoke-demo"' "${OUT_DIR}/inventory.logical.dot"

PATH="${BIN_DIR}:$PATH" SNAPSHOT_GRAPH_FIXTURE_DIR="$FIXTURE_DIR" \
	bash "${ROOT_DIR}/hack/snapshot-graph.sh" \
	--namespace ns1 \
	--snapshot root \
	--output-dir "$OUT_DIR" \
	--name lifecycle \
	--mode lifecycle \
	--title "Lifecycle graph" \
	--no-include-namespace-resources

grep -Fq 'MCR\nmcr-root' "${OUT_DIR}/lifecycle.lifecycle.dot"
grep -Fq 'status.manifestCaptureRequestName' "${OUT_DIR}/lifecycle.lifecycle.dot"
grep -Fq 'cluster_namespaced' "${OUT_DIR}/lifecycle.lifecycle.dot"

printf 'PASS: snapshot graph fixture artifacts in %s\n' "$OUT_DIR"
