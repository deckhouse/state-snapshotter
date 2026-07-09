# DEMO: restore-compiler endpoints. Copy-paste kubectl commands.
# Stand: ns=ss-demo, Snapshot=demo-tree, restore target=ss-demo-restore
# Endpoint base: /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/ss-demo/snapshots/demo-tree

# --- 1. What we captured -----------------------------------------------------
kubectl -n ss-demo get demovirtualmachines.demo.state-snapshotter.deckhouse.io,demovirtualdisks.demo.state-snapshotter.deckhouse.io,pvc,configmap

# --- 2. Snapshot is Ready + content tree -------------------------------------
kubectl -n ss-demo get snapshots.storage.deckhouse.io demo-tree
kubectl -n ss-demo get snapshots.storage.deckhouse.io demo-tree -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\n"}{end}'
kubectl get snapshotcontents.storage.deckhouse.io

# --- 3. Endpoint 1: /manifests = WHAT WAS SAVED ------------------------------
# list
kubectl get --raw "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/ss-demo/snapshots/demo-tree/manifests" | jq -r '(.items?//.)[]|"\(.kind)/\(.metadata.name)"'
# full json
kubectl get --raw "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/ss-demo/snapshots/demo-tree/manifests" | jq .

# --- 4. Endpoint 2: /manifests-with-data-restoration = WHAT TO APPLY ---------
# full apply-ready json
kubectl get --raw "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/ss-demo/snapshots/demo-tree/manifests-with-data-restoration?targetNamespace=ss-demo-restore" | jq .

# (1) post-order: disk-vm before vm-1; all rewritten to ss-demo-restore
kubectl get --raw "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/ss-demo/snapshots/demo-tree/manifests-with-data-restoration?targetNamespace=ss-demo-restore" | jq -r '(.items?//.)[]|"\(.kind)/\(.metadata.name)\tns=\(.metadata.namespace)"'

# (2) orphan PVC demo-pvc -> spec.dataSourceRef -> VolumeSnapshot
kubectl get --raw "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/ss-demo/snapshots/demo-tree/manifests-with-data-restoration?targetNamespace=ss-demo-restore" | jq '(.items?//.)[]|select(.kind=="PersistentVolumeClaim" and .metadata.name=="demo-pvc")|{name:.metadata.name,dataSourceRef:.spec.dataSourceRef}'

# (3) domain disks -> spec.dataSource -> DemoVirtualDiskSnapshot
kubectl get --raw "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/ss-demo/snapshots/demo-tree/manifests-with-data-restoration?targetNamespace=ss-demo-restore" | jq '(.items?//.)[]|select(.kind=="DemoVirtualDisk")|{name:.metadata.name,dataSource:.spec.dataSource}'

# (4) covered PVC demo-pvc-disk suppressed (expect 0)
kubectl get --raw "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/ss-demo/snapshots/demo-tree/manifests-with-data-restoration?targetNamespace=ss-demo-restore" | jq '[(.items?//.)[]|select(.kind=="PersistentVolumeClaim" and .metadata.name=="demo-pvc-disk")]|length'

# --- 5. Prove apply-ready: server-side dry-run apply -------------------------
kubectl get --raw "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/ss-demo/snapshots/demo-tree/manifests-with-data-restoration?targetNamespace=ss-demo-restore" | jq '{apiVersion:"v1",kind:"List",items:(.items?//.)}' | kubectl apply --dry-run=server -f -

# =============================================================================
# 6. BROWSE EVERYTHING WE CREATE
# =============================================================================

# --- 6a. all namespaced objects in ss-demo (one shot) ------------------------
kubectl -n ss-demo get snapshots.storage.deckhouse.io,demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io,demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io,manifestcapturerequests.state-snapshotter.deckhouse.io,volumecapturerequests.storage.deckhouse.io,volumesnapshots.snapshot.storage.k8s.io

# individually:
kubectl -n ss-demo get snapshots.storage.deckhouse.io
kubectl -n ss-demo get demovirtualmachinesnapshots.demo.state-snapshotter.deckhouse.io
kubectl -n ss-demo get demovirtualdisksnapshots.demo.state-snapshotter.deckhouse.io
kubectl -n ss-demo get manifestcapturerequests.state-snapshotter.deckhouse.io
kubectl -n ss-demo get volumecapturerequests.storage.deckhouse.io
kubectl -n ss-demo get volumesnapshots.snapshot.storage.k8s.io

# --- 6b. cluster-scoped objects (ours) ---------------------------------------
# SnapshotContent tree (READY/MANIFESTS/VOLUMES/CHILDREN columns)
kubectl get snapshotcontents.storage.deckhouse.io
# ManifestCheckpoints for this namespace (label-filtered)
kubectl get manifestcheckpoints.state-snapshotter.deckhouse.io -l state-snapshotter.deckhouse.io/source-namespace=ss-demo
# ObjectKeeper (durable retention owner of the tree)
kubectl get objectkeepers.deckhouse.io | grep -E 'NAME|ss-demo'
# VolumeSnapshotContent backing our orphan PVC (filter by source namespace)
kubectl get volumesnapshotcontents.snapshot.storage.k8s.io -o json | jq -r '.items[]|select(.spec.volumeSnapshotRef.namespace=="ss-demo")|.metadata.name'

# --- 6c. manifest checkpoint chunks (raw stored manifest bytes) --------------
# chunks are cluster-scoped and NOT listable by admin; list their names from the MCP status:
kubectl get manifestcheckpoints.state-snapshotter.deckhouse.io -l state-snapshotter.deckhouse.io/source-namespace=ss-demo -o json | jq -r '.items[]|.metadata.name as $m|.status.chunks[]?|"\($m)\tchunk=\(.name)\tobjects=\(.objectsCount)\tbytes=\(.sizeBytes)"'
# get one chunk by name (needs controller SA impersonation):
kubectl get manifestcheckpointcontentchunks.state-snapshotter.deckhouse.io <CHUNK_NAME> --as=system:serviceaccount:d8-state-snapshotter:controller -o yaml

# --- 6d. registry: CustomSnapshotDefinition (CSD) ----------------------------
# list all CSDs
kubectl get customsnapshotdefinitions.state-snapshotter.deckhouse.io
# short alias also works:
kubectl get csd

# conditions per CSD (Accepted / RBACReady / Ready)
kubectl get customsnapshotdefinitions.state-snapshotter.deckhouse.io -o json | jq -r '.items[]|"\(.metadata.name)\t"+([.status.conditions[]?|"\(.type)=\(.status)"]|join(","))'

# full object (our demo CSD)
kubectl get customsnapshotdefinitions.state-snapshotter.deckhouse.io tree-demo-vm-disk-20260616-091311 -o yaml
kubectl describe customsnapshotdefinitions.state-snapshotter.deckhouse.io tree-demo-vm-disk-20260616-091311

# the mapping it declares: source kind -> snapshot kind (+ priority)
kubectl get customsnapshotdefinitions.state-snapshotter.deckhouse.io tree-demo-vm-disk-20260616-091311 -o json | jq -r '.spec.snapshotResourceMapping[]|"\(.source.kind) -> \(.snapshot.kind)\tpriority=\(.priority)"'

# conditions in detail (type/status/reason/message)
kubectl get customsnapshotdefinitions.state-snapshotter.deckhouse.io tree-demo-vm-disk-20260616-091311 -o json | jq -r '.status.conditions[]|"\(.type)\t\(.status)\t\(.reason)\t\(.message)"'

# --- 6e. source workload (what we captured) ----------------------------------
kubectl -n ss-demo get demovirtualmachines.demo.state-snapshotter.deckhouse.io,demovirtualdisks.demo.state-snapshotter.deckhouse.io,pvc,configmap

# --- controller logs ---------------------------------------------------------
kubectl -n d8-state-snapshotter logs deploy/controller --tail=200

# --- rebuild stand (if needed) -----------------------------------------------
# TREE_DEMO_NAMESPACE=ss-demo TREE_DEMO_GROUP=restore TREE_DEMO_SKIP_CLEANUP=1 TREE_DEMO_SKIP_GRAPH=1 ./hack/snapshot-tree-demo-e2e.sh

# --- teardown ----------------------------------------------------------------
# kubectl delete ns ss-demo ss-demo-restore --wait=false
# kubectl delete customsnapshotdefinitions.state-snapshotter.deckhouse.io -l '' $(kubectl get customsnapshotdefinitions.state-snapshotter.deckhouse.io -o name | grep tree-demo)
