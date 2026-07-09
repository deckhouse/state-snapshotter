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

# RBAC audit: compare kubectl auth can-i vs templates/rbac-for-us.yaml expectations.
# Usage: ./hack/rbac-can-i-audit.sh [namespace-for-namespaced-checks]
set -uo pipefail

NS="${1:-default}"
MODULE_NS="${D8_MODULE_NS:-d8-state-snapshotter}"
SA="system:serviceaccount:${MODULE_NS}:controller"

log() { printf '%s\n' "$*" >&2; }

can_i() {
	local who="$1" verb="$2" res="$3"
	shift 3
	local out
	if [[ -n "${who}" ]]; then
		out="$(kubectl auth can-i "${verb}" "${res}" --as="${who}" "$@" 2>&1 | tail -1)"
	else
		out="$(kubectl auth can-i "${verb}" "${res}" "$@" 2>&1 | tail -1)"
	fi
	printf '%s\t%s\t%s\t%s\n' "${who:-current}" "${verb}" "${res}" "${out}"
}

log "== Controller SA: ${SA} (ns checks use -n ${NS}) =="
can_i "${SA}" create objectkeepers.deckhouse.io
can_i "${SA}" patch objectkeepers.deckhouse.io
can_i "${SA}" delete objectkeepers.deckhouse.io
can_i "${SA}" create manifestcapturerequests.state-snapshotter.deckhouse.io -n "${NS}"
can_i "${SA}" delete manifestcapturerequests.state-snapshotter.deckhouse.io -n "${NS}"
can_i "${SA}" create manifestcheckpoints.state-snapshotter.deckhouse.io
can_i "${SA}" patch snapshotcontents.state-snapshotter.deckhouse.io/status
can_i "${SA}" create volumecapturerequests.storage-foundation.deckhouse.io -n "${NS}"
can_i "${SA}" delete volumecapturerequests.storage-foundation.deckhouse.io -n "${NS}"
can_i "${SA}" patch volumesnapshotcontents.snapshot.storage.k8s.io
can_i "${SA}" list manifestcheckpointcontentchunks.state-snapshotter.deckhouse.io

log ""
log "== Current kubectl user (admin-kubeconfig path) =="
can_i "" create snapshots.state-snapshotter.deckhouse.io -n "${NS}"
can_i "" get snapshots.state-snapshotter.deckhouse.io -n "${NS}"
can_i "" patch snapshotcontents.state-snapshotter.deckhouse.io/status
can_i "" patch snapshotcontents.state-snapshotter.deckhouse.io
can_i "" get snapshots/manifests.subresources.state-snapshotter.deckhouse.io -n "${NS}"
can_i "" get manifestcheckpoints/manifests.subresources.state-snapshotter.deckhouse.io
can_i "" get manifestcheckpointcontentchunks.state-snapshotter.deckhouse.io
can_i "" get objectkeepers.deckhouse.io
can_i "" patch objectkeepers.deckhouse.io
can_i "" create clusterroles.rbac.authorization.k8s.io
can_i "" create clusterrolebindings.rbac.authorization.k8s.io

log ""
log "== Live ClusterRoles (grep key resources) =="
for cr in "d8:state-snapshotter:controller" "d8:state-snapshotter:admin-kubeconfig"; do
	log "--- ${cr} ---"
	kubectl get clusterrole "${cr}" -o yaml 2>/dev/null | grep -E 'apiGroups:|resources:|verbs:|objectkeeper|manifest|snapshot|volumecapture|subresources' || log "missing ${cr}"
done
