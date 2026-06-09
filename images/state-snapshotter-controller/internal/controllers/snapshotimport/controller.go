/*
Copyright 2026 Flant JSC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package snapshotimport implements the SnapshotImportRequest reconciler.
// It re-creates the full snapshot tree (Snapshot/domain snapshots + SnapshotContent +
// ManifestCheckpoint + VolumeCaptureRequest) from an archive uploaded via DataImport,
// so the resulting root Snapshot shows up Ready in `d8 snapshot list` without running workloads.
package snapshotimport

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotcontent"
	vccontroller "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/volumecapture"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	mcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/manifestcheckpoint"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
	liblogger "github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

const (
	// defaultRequeueAfter is the fallback requeue interval while waiting for
	// external progress (VCR, staging PVC, etc.).
	defaultRequeueAfter = 5 * time.Second
)

// SnapshotImportRequestReconciler reconstructs a full snapshot tree from an archive.
type SnapshotImportRequestReconciler struct {
	Client    client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Config    *config.Options
	Logger    liblogger.LoggerInterface
}

// SetupWithManager registers the controller.
func (r *SnapshotImportRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ssv1alpha1.SnapshotImportRequest{}).
		Complete(r)
}

// Reconcile is the main reconciliation loop for SnapshotImportRequest.
func (r *SnapshotImportRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	sir := &ssv1alpha1.SnapshotImportRequest{}
	if err := r.Client.Get(ctx, req.NamespacedName, sir); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if sir.Status.Phase == ssv1alpha1.SnapshotImportPhaseReady {
		return ctrl.Result{}, nil
	}
	if sir.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	r.Logger.Info("Reconciling SnapshotImportRequest", "name", sir.Name, "namespace", sir.Namespace)

	result, err := r.reconcileImport(ctx, sir)
	if err != nil {
		r.Logger.Error(err, "Import reconciliation failed", "name", sir.Name)
		if updateErr := r.setRequestFailed(ctx, sir, err.Error()); updateErr != nil {
			r.Logger.Error(updateErr, "Failed to update request status to Failed")
		}
		return ctrl.Result{}, err
	}
	return result, nil
}

// reconcileImport is the main import lifecycle: leaf→root, then root ObjectKeeper.
func (r *SnapshotImportRequestReconciler) reconcileImport(ctx context.Context, sir *ssv1alpha1.SnapshotImportRequest) (ctrl.Result, error) {
	if sir.Status.Phase == "" || sir.Status.Phase == ssv1alpha1.SnapshotImportPhasePending {
		if err := r.setRequestPhase(ctx, sir, ssv1alpha1.SnapshotImportPhaseImporting); err != nil {
			return ctrl.Result{}, err
		}
	}

	nodeByID := make(map[string]ssv1alpha1.ImportNode, len(sir.Spec.Nodes))
	for _, n := range sir.Spec.Nodes {
		nodeByID[n.ID] = n
	}

	var rootNode *ssv1alpha1.ImportNode
	for i := range sir.Spec.Nodes {
		if sir.Spec.Nodes[i].ParentID == "" {
			rootNode = &sir.Spec.Nodes[i]
			break
		}
	}
	if rootNode == nil {
		return ctrl.Result{}, fmt.Errorf("SnapshotImportRequest %s/%s: no root node found", sir.Namespace, sir.Name)
	}

	ordered := topoSort(sir.Spec.Nodes)

	for _, node := range ordered {
		result, err := r.reconcileNode(ctx, sir, node, nodeByID)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("node %s: %w", node.ID, err)
		}
		if result.Requeue || result.RequeueAfter > 0 {
			return result, nil
		}
	}

	// Root: ensure ObjectKeeper.
	rootSnap, err := r.getSnapshotObject(ctx, sir.Namespace, rootNode)
	if err != nil {
		return ctrl.Result{}, err
	}
	if rootSnap == nil || rootSnap.GetUID() == "" {
		return ctrl.Result{RequeueAfter: defaultRequeueAfter}, nil
	}

	gv, err := schema.ParseGroupVersion(rootNode.APIVersion)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("parse root GV %s: %w", rootNode.APIVersion, err)
	}
	rootGVK := gv.WithKind(rootNode.Kind)

	_, okResult, err := controllercommon.EnsureRootObjectKeeperWithTTL(ctx, r.Client, r.APIReader, r.Config, rootSnap, rootGVK)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("root ObjectKeeper: %w", err)
	}
	if okResult.Requeue || okResult.RequeueAfter > 0 {
		return okResult, nil
	}

	// Poll root Snapshot for Ready.
	ready, err := r.isSnapshotReady(ctx, sir.Namespace, rootNode)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		return ctrl.Result{RequeueAfter: defaultRequeueAfter}, nil
	}

	if err := r.setRequestReady(ctx, sir, rootNode.Name); err != nil {
		return ctrl.Result{}, err
	}
	r.Logger.Info("SnapshotImportRequest completed", "name", sir.Name, "namespace", sir.Namespace, "snapshot", rootNode.Name)
	return ctrl.Result{}, nil
}

// reconcileNode ensures the full state for one import node.
func (r *SnapshotImportRequestReconciler) reconcileNode(
	ctx context.Context,
	sir *ssv1alpha1.SnapshotImportRequest,
	node ssv1alpha1.ImportNode,
	nodeByID map[string]ssv1alpha1.ImportNode,
) (ctrl.Result, error) {
	ns := sir.Namespace

	snapObj, created, err := r.ensureSnapshotObject(ctx, ns, node, sir)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure snapshot object: %w", err)
	}
	if snapObj == nil || snapObj.GetUID() == "" {
		return ctrl.Result{RequeueAfter: defaultRequeueAfter}, nil
	}
	if created {
		return ctrl.Result{Requeue: true}, nil
	}

	contentName := contentNameForNode(node, snapObj.GetUID())

	if err := r.ensureSnapshotContent(ctx, contentName); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure SnapshotContent: %w", err)
	}

	if err := r.bindSnapshotToContent(ctx, ns, node, contentName); err != nil {
		return ctrl.Result{}, fmt.Errorf("bind snapshot to content: %w", err)
	}

	mcpName := mcpkg.ImportCheckpointName(sir.UID, node.ID)
	if result, err := r.ensureManifestCheckpoint(ctx, sir, node, mcpName); err != nil || result.Requeue || result.RequeueAfter > 0 {
		return result, err
	}

	if err := snapshotcontent.PublishSnapshotContentManifestCheckpointName(ctx, r.Client, contentName, mcpName); err != nil {
		return ctrl.Result{}, fmt.Errorf("publish MCP name: %w", err)
	}

	if node.HasData {
		result, err := r.ensureDataRefs(ctx, sir, node, contentName)
		if err != nil || result.Requeue || result.RequeueAfter > 0 {
			return result, err
		}
	}

	if len(node.Children) > 0 {
		if err := r.publishChildren(ctx, ns, node, contentName, nodeByID); err != nil {
			return ctrl.Result{}, fmt.Errorf("publish children: %w", err)
		}
	}

	return ctrl.Result{}, nil
}

// ensureSnapshotObject creates the snapshot CRD object if absent, with the import annotation.
func (r *SnapshotImportRequestReconciler) ensureSnapshotObject(
	ctx context.Context,
	ns string,
	node ssv1alpha1.ImportNode,
	sir *ssv1alpha1.SnapshotImportRequest,
) (*unstructured.Unstructured, bool, error) {
	gv, err := schema.ParseGroupVersion(node.APIVersion)
	if err != nil {
		return nil, false, fmt.Errorf("parse GV %s: %w", node.APIVersion, err)
	}
	gvk := gv.WithKind(node.Kind)
	key := client.ObjectKey{Namespace: ns, Name: node.Name}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(gvk)
	if err := r.Client.Get(ctx, key, existing); err == nil {
		return existing, false, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, false, err
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName(node.Name)
	obj.SetNamespace(ns)
	obj.SetAnnotations(map[string]string{
		ssv1alpha1.AnnotationImported: "true",
	})
	obj.SetOwnerReferences([]metav1.OwnerReference{sirOwnerReference(sir)})

	if err := r.Client.Create(ctx, obj); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if err2 := r.Client.Get(ctx, key, obj); err2 != nil {
				return nil, false, err2
			}
			return obj, false, nil
		}
		return nil, false, err
	}
	r.Logger.Info("Created snapshot object", "kind", node.Kind, "name", node.Name, "namespace", ns)
	return obj, true, nil
}

// getSnapshotObject fetches the snapshot object for a node (returns nil if not found).
func (r *SnapshotImportRequestReconciler) getSnapshotObject(ctx context.Context, ns string, node *ssv1alpha1.ImportNode) (*unstructured.Unstructured, error) {
	gv, err := schema.ParseGroupVersion(node.APIVersion)
	if err != nil {
		return nil, err
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gv.WithKind(node.Kind))
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: node.Name}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return obj, nil
}

// ensureSnapshotContent creates the cluster-scoped SnapshotContent if absent.
func (r *SnapshotImportRequestReconciler) ensureSnapshotContent(ctx context.Context, contentName string) error {
	existing := &storagev1alpha1.SnapshotContent{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: contentName}, existing); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name:        contentName,
			Annotations: map[string]string{ssv1alpha1.AnnotationImported: "true"},
			Finalizers:  []string{snapshotpkg.FinalizerParentProtect},
		},
		Spec: storagev1alpha1.SnapshotContentSpec{
			DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyRetain,
		},
	}
	if err := r.Client.Create(ctx, content); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	r.Logger.Info("Created SnapshotContent", "name", contentName)
	return nil
}

// bindSnapshotToContent sets status.boundSnapshotContentName on the snapshot object.
func (r *SnapshotImportRequestReconciler) bindSnapshotToContent(
	ctx context.Context,
	ns string,
	node ssv1alpha1.ImportNode,
	contentName string,
) error {
	gv, err := schema.ParseGroupVersion(node.APIVersion)
	if err != nil {
		return err
	}
	gvk := gv.WithKind(node.Kind)

	// Use typed path for the native Snapshot.
	if node.APIVersion == storagev1alpha1.SchemeGroupVersion.String() && node.Kind == "Snapshot" {
		return retry.RetryOnConflict(retry.DefaultRetry, func() error {
			snap := &storagev1alpha1.Snapshot{}
			if err := r.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: node.Name}, snap); err != nil {
				return err
			}
			if snap.Status.BoundSnapshotContentName == contentName {
				return nil
			}
			base := snap.DeepCopy()
			snap.Status.BoundSnapshotContentName = contentName
			return r.Client.Status().Patch(ctx, snap, client.MergeFrom(base))
		})
	}

	// Generic unstructured path for domain CRD kinds.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: node.Name}, obj); err != nil {
			return err
		}
		current, _, _ := unstructured.NestedString(obj.Object, "status", "boundSnapshotContentName")
		if current == contentName {
			return nil
		}
		base := obj.DeepCopy()
		if err := unstructured.SetNestedField(obj.Object, contentName, "status", "boundSnapshotContentName"); err != nil {
			return err
		}
		return r.Client.Status().Patch(ctx, obj, client.MergeFrom(base))
	})
}

// ensureManifestCheckpoint builds the MCP and canonical chunks from transport chunks.
func (r *SnapshotImportRequestReconciler) ensureManifestCheckpoint(
	ctx context.Context,
	sir *ssv1alpha1.SnapshotImportRequest,
	node ssv1alpha1.ImportNode,
	mcpName string,
) (ctrl.Result, error) {
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	err := r.Client.Get(ctx, client.ObjectKey{Name: mcpName}, mcp)
	if err == nil {
		if isMCPReady(mcp) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{RequeueAfter: defaultRequeueAfter}, nil
	}
	if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	chunkList := &ssv1alpha1.SnapshotImportManifestChunkList{}
	if err := r.Client.List(ctx, chunkList, client.InNamespace(sir.Namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("list import manifest chunks: %w", err)
	}

	var nodeChunks []ssv1alpha1.SnapshotImportManifestChunk
	for _, c := range chunkList.Items {
		if c.Spec.ImportRequestName == sir.Name && c.Spec.NodeID == node.ID {
			nodeChunks = append(nodeChunks, c)
		}
	}
	sort.Slice(nodeChunks, func(i, j int) bool {
		return nodeChunks[i].Spec.Index < nodeChunks[j].Spec.Index
	})

	newMCP := &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:        mcpName,
			Annotations: map[string]string{ssv1alpha1.AnnotationImported: "true"},
		},
		Spec: ssv1alpha1.ManifestCheckpointSpec{
			SourceNamespace: sir.Namespace,
			ManifestCaptureRequestRef: &ssv1alpha1.ObjectReference{
				Name:      sir.Name,
				Namespace: sir.Namespace,
				UID:       string(sir.UID),
			},
		},
	}
	if err := r.Client.Create(ctx, newMCP); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, fmt.Errorf("create ManifestCheckpoint: %w", err)
	}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: mcpName}, newMCP); err != nil {
		return ctrl.Result{}, err
	}

	chunkInfos := make([]ssv1alpha1.ChunkInfo, 0, len(nodeChunks))
	var totalObjects int
	var totalSizeBytes int64

	if len(nodeChunks) == 0 {
		info, err := r.createEmptyCanonicalChunk(ctx, mcpName, string(newMCP.UID), 0)
		if err != nil {
			return ctrl.Result{}, err
		}
		chunkInfos = append(chunkInfos, info)
	} else {
		for _, tc := range nodeChunks {
			info, err := r.createCanonicalChunkFromTransport(ctx, mcpName, string(newMCP.UID), tc)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("create canonical chunk %d: %w", tc.Spec.Index, err)
			}
			chunkInfos = append(chunkInfos, info)
			totalObjects += tc.Spec.ObjectsCount
			totalSizeBytes += info.SizeBytes
		}
	}

	return ctrl.Result{}, r.setMCPReady(ctx, newMCP, chunkInfos, totalObjects, totalSizeBytes)
}

// createCanonicalChunkFromTransport converts a SnapshotImportManifestChunk into a ManifestCheckpointContentChunk.
func (r *SnapshotImportRequestReconciler) createCanonicalChunkFromTransport(
	ctx context.Context,
	checkpointName, checkpointUID string,
	transport ssv1alpha1.SnapshotImportManifestChunk,
) (ssv1alpha1.ChunkInfo, error) {
	chunkName := mcpkg.ImportChunkName(checkpointName, transport.Spec.Index)
	checksum := mcpkg.CalculateChecksum(transport.Spec.Data)
	base64Len := len(transport.Spec.Data)
	approxGzipBytes := (base64Len * 3) / 4

	chunk := &ssv1alpha1.ManifestCheckpointContentChunk{
		ObjectMeta: metav1.ObjectMeta{
			Name: chunkName,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: ssv1alpha1.SchemeGroupVersion.String(),
					Kind:       "ManifestCheckpoint",
					Name:       checkpointName,
					UID:        types.UID(checkpointUID),
					Controller: boolPtr(true),
				},
			},
		},
		Spec: ssv1alpha1.ManifestCheckpointContentChunkSpec{
			CheckpointName: checkpointName,
			Index:          transport.Spec.Index,
			Data:           transport.Spec.Data,
			ObjectsCount:   transport.Spec.ObjectsCount,
			Checksum:       checksum,
		},
	}
	if err := r.Client.Create(ctx, chunk); err != nil && !apierrors.IsAlreadyExists(err) {
		return ssv1alpha1.ChunkInfo{}, fmt.Errorf("create chunk %s: %w", chunkName, err)
	}
	return ssv1alpha1.ChunkInfo{
		Name:         chunkName,
		Index:        transport.Spec.Index,
		ObjectsCount: transport.Spec.ObjectsCount,
		SizeBytes:    int64(approxGzipBytes),
		Checksum:     checksum,
	}, nil
}

// createEmptyCanonicalChunk creates an empty canonical chunk for nodes with no manifest objects.
func (r *SnapshotImportRequestReconciler) createEmptyCanonicalChunk(
	ctx context.Context,
	checkpointName, checkpointUID string,
	index int,
) (ssv1alpha1.ChunkInfo, error) {
	base64data, gzipBytes, err := mcpkg.CompressToBase64([]byte("[]"))
	if err != nil {
		return ssv1alpha1.ChunkInfo{}, err
	}
	chunkName := mcpkg.ImportChunkName(checkpointName, index)
	checksum := mcpkg.CalculateChecksum(base64data)

	chunk := &ssv1alpha1.ManifestCheckpointContentChunk{
		ObjectMeta: metav1.ObjectMeta{
			Name: chunkName,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: ssv1alpha1.SchemeGroupVersion.String(),
					Kind:       "ManifestCheckpoint",
					Name:       checkpointName,
					UID:        types.UID(checkpointUID),
					Controller: boolPtr(true),
				},
			},
		},
		Spec: ssv1alpha1.ManifestCheckpointContentChunkSpec{
			CheckpointName: checkpointName,
			Index:          index,
			Data:           base64data,
			ObjectsCount:   0,
			Checksum:       checksum,
		},
	}
	if err := r.Client.Create(ctx, chunk); err != nil && !apierrors.IsAlreadyExists(err) {
		return ssv1alpha1.ChunkInfo{}, err
	}
	return ssv1alpha1.ChunkInfo{
		Name:         chunkName,
		Index:        index,
		ObjectsCount: 0,
		SizeBytes:    int64(len(gzipBytes)),
		Checksum:     checksum,
	}, nil
}

// setMCPReady updates the ManifestCheckpoint status to Ready=True.
func (r *SnapshotImportRequestReconciler) setMCPReady(
	ctx context.Context,
	mcp *ssv1alpha1.ManifestCheckpoint,
	chunkInfos []ssv1alpha1.ChunkInfo,
	totalObjects int,
	totalSizeBytes int64,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &ssv1alpha1.ManifestCheckpoint{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: mcp.Name}, fresh); err != nil {
			return err
		}
		if isMCPReady(fresh) {
			return nil
		}
		base := fresh.DeepCopy()
		fresh.Status.Chunks = chunkInfos
		fresh.Status.TotalObjects = totalObjects
		fresh.Status.TotalSizeBytes = totalSizeBytes
		fresh.Status.Conditions = []metav1.Condition{
			{
				Type:               ssv1alpha1.ManifestCheckpointConditionTypeReady,
				Status:             metav1.ConditionTrue,
				Reason:             ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
				Message:            "Imported from SnapshotImportRequest",
				LastTransitionTime: metav1.Now(),
			},
		}
		return r.Client.Status().Patch(ctx, fresh, client.MergeFrom(base))
	})
}

// ensureDataRefs drives VCR creation and dataRefs publication for data nodes.
func (r *SnapshotImportRequestReconciler) ensureDataRefs(
	ctx context.Context,
	sir *ssv1alpha1.SnapshotImportRequest,
	node ssv1alpha1.ImportNode,
	contentName string,
) (ctrl.Result, error) {
	var volumes []ssv1alpha1.ImportVolume
	for _, v := range sir.Spec.Volumes {
		if v.NodeID == node.ID {
			volumes = append(volumes, v)
		}
	}

	for _, v := range volumes {
		if v.StagingPVCName == "" {
			r.Logger.Info("Waiting for staging PVC name", "node", node.ID, "pvcName", v.PVCName)
			return ctrl.Result{RequeueAfter: defaultRequeueAfter}, nil
		}
	}

	content := &storagev1alpha1.SnapshotContent{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
		return ctrl.Result{}, err
	}

	vcrName := vcpkg.SnapshotContentVCRName(content.UID)
	ownerRef := metav1.OwnerReference{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "SnapshotContent",
		Name:       content.Name,
		UID:        content.UID,
		Controller: boolPtr(true),
	}

	targets := make([]vcpkg.Target, 0, len(volumes))
	for _, v := range volumes {
		pvc := &corev1.PersistentVolumeClaim{}
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: sir.Namespace, Name: v.StagingPVCName}, pvc); err != nil {
			if apierrors.IsNotFound(err) {
				r.Logger.Info("Staging PVC not found, waiting", "pvc", v.StagingPVCName)
				return ctrl.Result{RequeueAfter: defaultRequeueAfter}, nil
			}
			return ctrl.Result{}, err
		}
		targets = append(targets, vcpkg.Target{
			UID:        string(pvc.UID),
			APIVersion: "v1",
			Kind:       "PersistentVolumeClaim",
			Name:       pvc.Name,
			Namespace:  pvc.Namespace,
		})
	}

	vcrObj := &unstructured.Unstructured{}
	vcrObj.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: sir.Namespace, Name: vcrName}, vcrObj)
	if apierrors.IsNotFound(err) {
		newVCR := vccontroller.NewVolumeCaptureRequestObject(sir.Namespace, vcrName, ownerRef, targets)
		if err := r.Client.Create(ctx, newVCR); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, fmt.Errorf("create VCR: %w", err)
		}
		return ctrl.Result{RequeueAfter: defaultRequeueAfter}, nil
	} else if err != nil {
		return ctrl.Result{}, err
	}

	dataRefs, err := vccontroller.ParseVolumeCaptureDataRefs(vcrObj)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(dataRefs) == 0 {
		r.Logger.Info("VCR not ready yet, waiting for dataRefs", "vcr", vcrName)
		return ctrl.Result{RequeueAfter: defaultRequeueAfter}, nil
	}

	bindings := vccontroller.SnapshotDataBindingsFromVCRStatus(dataRefs)
	if err := snapshotcontent.EnsureVolumeSnapshotContentsOwnedByContent(ctx, r.Client, content, bindings); err != nil {
		return ctrl.Result{}, err
	}
	if err := snapshotcontent.PublishSnapshotContentDataRefs(ctx, r.Client, contentName, bindings); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// publishChildren sets ChildrenSnapshotRefs on the Snapshot and ChildrenSnapshotContentRefs on the SnapshotContent.
func (r *SnapshotImportRequestReconciler) publishChildren(
	ctx context.Context,
	ns string,
	node ssv1alpha1.ImportNode,
	contentName string,
	nodeByID map[string]ssv1alpha1.ImportNode,
) error {
	childRefs := make([]storagev1alpha1.SnapshotChildRef, 0, len(node.Children))
	for _, childID := range node.Children {
		childNode, ok := nodeByID[childID]
		if !ok {
			continue
		}
		childRefs = append(childRefs, storagev1alpha1.SnapshotChildRef{
			APIVersion: childNode.APIVersion,
			Kind:       childNode.Kind,
			Name:       childNode.Name,
		})
	}

	if node.APIVersion == storagev1alpha1.SchemeGroupVersion.String() && node.Kind == "Snapshot" {
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			snap := &storagev1alpha1.Snapshot{}
			if err := r.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: node.Name}, snap); err != nil {
				return err
			}
			if childRefsEqual(snap.Status.ChildrenSnapshotRefs, childRefs) {
				return nil
			}
			base := snap.DeepCopy()
			snap.Status.ChildrenSnapshotRefs = childRefs
			return r.Client.Status().Patch(ctx, snap, client.MergeFrom(base))
		}); err != nil {
			return fmt.Errorf("set children refs on Snapshot: %w", err)
		}
	}

	_, err := snapshotcontent.PublishSnapshotContentChildrenFromSnapshotRefs(
		ctx, r.Client, nil, ns, contentName, childRefs)
	if err != nil {
		return fmt.Errorf("publish children content refs: %w", err)
	}
	return nil
}

// isSnapshotReady checks if the snapshot object has Ready=True condition.
func (r *SnapshotImportRequestReconciler) isSnapshotReady(ctx context.Context, ns string, node *ssv1alpha1.ImportNode) (bool, error) {
	obj, err := r.getSnapshotObject(ctx, ns, node)
	if err != nil || obj == nil {
		return false, err
	}
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if m["type"] == "Ready" && m["status"] == "True" {
			return true, nil
		}
	}
	return false, nil
}

// setRequestPhase patches the SnapshotImportRequest phase.
func (r *SnapshotImportRequestReconciler) setRequestPhase(ctx context.Context, sir *ssv1alpha1.SnapshotImportRequest, phase ssv1alpha1.SnapshotImportPhase) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &ssv1alpha1.SnapshotImportRequest{}
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: sir.Namespace, Name: sir.Name}, fresh); err != nil {
			return err
		}
		if fresh.Status.Phase == phase {
			return nil
		}
		base := fresh.DeepCopy()
		fresh.Status.Phase = phase
		return r.Client.Status().Patch(ctx, fresh, client.MergeFrom(base))
	})
}

// setRequestReady marks the request as Ready.
func (r *SnapshotImportRequestReconciler) setRequestReady(ctx context.Context, sir *ssv1alpha1.SnapshotImportRequest, createdSnapshotName string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &ssv1alpha1.SnapshotImportRequest{}
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: sir.Namespace, Name: sir.Name}, fresh); err != nil {
			return err
		}
		base := fresh.DeepCopy()
		fresh.Status.Phase = ssv1alpha1.SnapshotImportPhaseReady
		fresh.Status.CreatedSnapshotName = createdSnapshotName
		setImportCondition(&fresh.Status.Conditions,
			ssv1alpha1.SnapshotImportRequestConditionTypeReady,
			metav1.ConditionTrue,
			ssv1alpha1.SnapshotImportRequestConditionReasonCompleted,
			"Import completed successfully",
		)
		return r.Client.Status().Patch(ctx, fresh, client.MergeFrom(base))
	})
}

// setRequestFailed marks the request as Failed.
func (r *SnapshotImportRequestReconciler) setRequestFailed(ctx context.Context, sir *ssv1alpha1.SnapshotImportRequest, msg string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &ssv1alpha1.SnapshotImportRequest{}
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: sir.Namespace, Name: sir.Name}, fresh); err != nil {
			return err
		}
		base := fresh.DeepCopy()
		fresh.Status.Phase = ssv1alpha1.SnapshotImportPhaseFailed
		setImportCondition(&fresh.Status.Conditions,
			ssv1alpha1.SnapshotImportRequestConditionTypeReady,
			metav1.ConditionFalse,
			ssv1alpha1.SnapshotImportRequestConditionReasonFailed,
			msg,
		)
		return r.Client.Status().Patch(ctx, fresh, client.MergeFrom(base))
	})
}

// AddSnapshotImportRequestControllerToManager registers the controller with the manager.
func AddSnapshotImportRequestControllerToManager(
	mgr ctrl.Manager,
	log liblogger.LoggerInterface,
	cfg *config.Options,
) error {
	r := &SnapshotImportRequestReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Config:    cfg,
		Logger:    log,
	}
	return r.SetupWithManager(mgr)
}

// contentNameForNode returns the SnapshotContent name using the same formula as the normal controllers.
func contentNameForNode(node ssv1alpha1.ImportNode, uid types.UID) string {
	switch {
	case node.APIVersion == storagev1alpha1.SchemeGroupVersion.String() && node.Kind == "Snapshot":
		uidStr := strings.ReplaceAll(string(uid), "-", "")
		return fmt.Sprintf("ns-%s", uidStr)
	case node.Kind == "DemoVirtualMachineSnapshot":
		return "demovmc-" + hexHash10([]byte(node.Name))
	case node.Kind == "DemoVirtualDiskSnapshot":
		return "demodiskc-" + hexHash10([]byte(node.Name))
	default:
		uid8 := string(uid)
		if len(uid8) > 8 {
			uid8 = uid8[:8]
		}
		return fmt.Sprintf("%s-content-%s", node.Name, strings.ToLower(uid8))
	}
}

// hexHash10 returns the hex encoding of the first 10 bytes of a SHA-256 hash.
func hexHash10(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h[:10])
}

// topoSort returns nodes in topological order: leaves first, root last.
func topoSort(nodes []ssv1alpha1.ImportNode) []ssv1alpha1.ImportNode {
	childCount := make(map[string]int, len(nodes))
	parentOf := make(map[string]string, len(nodes))
	byID := make(map[string]ssv1alpha1.ImportNode, len(nodes))

	for _, n := range nodes {
		byID[n.ID] = n
		childCount[n.ID] = len(n.Children)
		for _, childID := range n.Children {
			parentOf[childID] = n.ID
		}
	}

	var queue []string
	for _, n := range nodes {
		if childCount[n.ID] == 0 {
			queue = append(queue, n.ID)
		}
	}
	sort.Strings(queue)

	var result []ssv1alpha1.ImportNode
	processed := make(map[string]bool, len(nodes))
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if processed[cur] {
			continue
		}
		processed[cur] = true
		result = append(result, byID[cur])

		if parent, ok := parentOf[cur]; ok {
			childCount[parent]--
			if childCount[parent] == 0 {
				queue = append(queue, parent)
			}
		}
	}

	for _, n := range nodes {
		if !processed[n.ID] {
			result = append(result, n)
		}
	}
	return result
}

func sirOwnerReference(sir *ssv1alpha1.SnapshotImportRequest) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: ssv1alpha1.SchemeGroupVersion.String(),
		Kind:       "SnapshotImportRequest",
		Name:       sir.Name,
		UID:        sir.UID,
		Controller: boolPtr(true),
	}
}

func isMCPReady(mcp *ssv1alpha1.ManifestCheckpoint) bool {
	for _, c := range mcp.Status.Conditions {
		if c.Type == ssv1alpha1.ManifestCheckpointConditionTypeReady && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

func childRefsEqual(a, b []storagev1alpha1.SnapshotChildRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func setImportCondition(conditions *[]metav1.Condition, condType string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i, c := range *conditions {
		if c.Type == condType {
			if c.Status == status {
				return
			}
			(*conditions)[i] = metav1.Condition{
				Type:               condType,
				Status:             status,
				Reason:             reason,
				Message:            message,
				LastTransitionTime: now,
			}
			return
		}
	}
	*conditions = append(*conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}

func boolPtr(b bool) *bool { return &b }
