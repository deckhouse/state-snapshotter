#!/usr/bin/env bash
# Verifies snapshot-graph.sh fails when MCP.status.chunks reference missing chunks or chunk get is forbidden.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/snapshot-graph-chunk-verify.XXXXXX")
trap 'rm -rf "$TMP_DIR"' EXIT

FIXTURE_DIR="${TMP_DIR}/fixtures"
BIN_DIR="${TMP_DIR}/bin"
OUT_DIR="${TMP_DIR}/out"
mkdir -p "$FIXTURE_DIR" "$BIN_DIR" "$OUT_DIR"

ready_condition='"conditions":[{"type":"Ready","status":"True","reason":"Completed","message":"ok"}]'

write_obj() {
	local resource="$1" name="$2"
	mkdir -p "${FIXTURE_DIR}/${resource}"
	cat >"${FIXTURE_DIR}/${resource}/${name}.json"
}

write_obj snapshots.storage.deckhouse.io snap <<EOF
{"apiVersion":"storage.deckhouse.io/v1alpha1","kind":"Snapshot","metadata":{"namespace":"ns1","name":"snap","uid":"snap-uid"},"status":{"boundSnapshotContentName":"sc-1",${ready_condition}}}
EOF

write_obj snapshotcontents.storage.deckhouse.io sc-1 <<EOF
{"apiVersion":"storage.deckhouse.io/v1alpha1","kind":"SnapshotContent","metadata":{"name":"sc-1","uid":"sc-1-uid"},"status":{"manifestCheckpointName":"mcp-1",${ready_condition}}}
EOF

write_obj manifestcheckpoints.state-snapshotter.deckhouse.io mcp-1 <<EOF
{"apiVersion":"state-snapshotter.deckhouse.io/v1alpha1","kind":"ManifestCheckpoint","metadata":{"name":"mcp-1","uid":"mcp-1-uid"},"status":{"chunks":[{"name":"chunk-missing-0","index":0,"objectsCount":1}],${ready_condition}}}
EOF

cat >"${BIN_DIR}/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
fixture_dir="${SNAPSHOT_GRAPH_FIXTURE_DIR:?}"
deny_chunk_get="${SNAPSHOT_GRAPH_FIXTURE_DENY_CHUNK_GET:-0}"

if [[ "${1:-}" == "-n" ]]; then
	shift 2
fi

if [[ "${1:-}" == "auth" && "${2:-}" == "can-i" ]]; then
	if [[ "${3:-}" == "get" && "${4:-}" == *manifestcheckpointcontentchunks* ]]; then
		if [[ "$deny_chunk_get" == "1" ]]; then
			echo no
		else
			echo yes
		fi
		exit 0
	fi
	echo yes
	exit 0
fi

if [[ "${1:-}" == "api-resources" ]]; then
	exit 0
fi

[[ "${1:-}" == "get" ]] || { echo "unsupported kubectl args: $*" >&2; exit 1; }
resource="$2"
name="${3:-}"
if [[ -z "$name" || "$name" == "-o" ]]; then
	jq -s '{items: .}' "${fixture_dir}/${resource}"/*.json 2>/dev/null || jq -n '{items: []}'
	exit 0
fi
file="${fixture_dir}/${resource}/${name}.json"
[[ -f "$file" ]] || exit 1
cat "$file"
EOF
chmod +x "${BIN_DIR}/kubectl"

run_graph() {
	local deny="$1" out_name="$2"
	PATH="${BIN_DIR}:$PATH" \
		SNAPSHOT_GRAPH_FIXTURE_DIR="$FIXTURE_DIR" \
		SNAPSHOT_GRAPH_FIXTURE_DENY_CHUNK_GET="$deny" \
		bash "${ROOT_DIR}/hack/snapshot-graph.sh" \
		--namespace ns1 \
		--snapshot snap \
		--output-dir "$OUT_DIR" \
		--name "$out_name" \
		--mode logical \
		--no-include-namespace-resources \
		>"${OUT_DIR}/${out_name}.stdout" 2>"${OUT_DIR}/${out_name}.stderr" && return 0 || return 1
}

if run_graph 0 missing-chunk; then
	echo "expected snapshot-graph to fail when MCP lists a missing chunk" >&2
	exit 1
fi
grep -Fq 'chunk verification failed' "${OUT_DIR}/missing-chunk.stderr" \
	|| grep -Fq 'ManifestCheckpointContentChunk/chunk-missing-0 missing' "${OUT_DIR}/missing-chunk.stderr" \
	|| grep -Fq 'chunk verification failed' "${OUT_DIR}/missing-chunk.stdout" || {
	echo "expected chunk verification failure message" >&2
	cat "${OUT_DIR}/missing-chunk.stderr" >&2
	exit 1
}

if run_graph 1 forbidden-chunk; then
	echo "expected snapshot-graph to fail when chunk get is forbidden" >&2
	exit 1
fi
grep -Fq 'chunk read forbidden' "${OUT_DIR}/forbidden-chunk.stderr" || {
	echo "expected chunk read forbidden message" >&2
	cat "${OUT_DIR}/forbidden-chunk.stderr" >&2
	exit 1
}

printf 'PASS: snapshot-graph chunk verification failures\n'
