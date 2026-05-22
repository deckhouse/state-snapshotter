#!/usr/bin/env bash
# Hot-apply storage.deckhouse.io Snapshot RBAC to d8:state-snapshotter:admin-kubeconfig on cluster.
# Matches templates/rbac-for-us.yaml (until next module helm release).
#
# Deckhouse escalation prevention: kubernetes-admin cannot patch rules it does not hold;
# use deckhouse SA impersonation (same rules as module deploy).
#
# Usage: ./hack/apply-admin-kubeconfig-snapshot-rbac.sh

set -euo pipefail

PATCH='[
  {"op": "add", "path": "/rules/-", "value": {"apiGroups": ["storage.deckhouse.io"], "resources": ["snapshots", "snapshotcontents"], "verbs": ["get", "list", "watch", "create", "update", "patch", "delete"]}},
  {"op": "add", "path": "/rules/-", "value": {"apiGroups": ["storage.deckhouse.io"], "resources": ["snapshots/status", "snapshotcontents/status"], "verbs": ["get", "update", "patch"]}}
]'

kubectl patch clusterrole d8:state-snapshotter:admin-kubeconfig \
  --as=system:serviceaccount:d8-system:deckhouse \
  --type=json \
  -p="${PATCH}"

echo "OK patched d8:state-snapshotter:admin-kubeconfig"
kubectl auth can-i create snapshots.storage.deckhouse.io -n default
