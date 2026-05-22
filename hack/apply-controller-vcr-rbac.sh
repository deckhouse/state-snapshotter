#!/usr/bin/env bash
# Hot-apply N5 VolumeCaptureRequest + VSC handoff RBAC to d8:state-snapshotter:controller.
# Matches templates/controller/rbac-for-us.yaml (until next module helm release).
#
# Usage: ./hack/apply-controller-vcr-rbac.sh

set -euo pipefail

PATCH='[
  {"op": "add", "path": "/rules/-", "value": {"apiGroups": ["storage.deckhouse.io"], "resources": ["volumecapturerequests"], "verbs": ["get", "list", "watch", "create", "update", "patch", "delete"]}},
  {"op": "add", "path": "/rules/-", "value": {"apiGroups": ["storage.deckhouse.io"], "resources": ["volumecapturerequests/status"], "verbs": ["get", "update", "patch"]}},
  {"op": "add", "path": "/rules/-", "value": {"apiGroups": ["snapshot.storage.k8s.io"], "resources": ["volumesnapshotcontents"], "verbs": ["get", "list", "watch", "update", "patch"]}}
]'

kubectl patch clusterrole d8:state-snapshotter:controller \
  --as=system:serviceaccount:d8-system:deckhouse \
  --type=json \
  -p="${PATCH}" 2>/dev/null || true

echo "OK d8:state-snapshotter:controller (check rules if patch was duplicate)"
kubectl auth can-i get volumecapturerequests.storage.deckhouse.io -n default \
  --as=system:serviceaccount:d8-state-snapshotter:controller
