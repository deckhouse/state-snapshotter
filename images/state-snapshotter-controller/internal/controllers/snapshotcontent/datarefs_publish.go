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

package snapshotcontent

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// EnrichDataBindingsWithVolumeMetadata fills volumeMode/fsType/accessModes/storageClassName on each
// PVC-targeted binding by reading the live source PVC (and its bound PV). CSI snapshots are
// mode-agnostic, so this metadata MUST be captured now to faithfully restore the volume on export
// (VolumeRestoreRequest builds CSI VolumeCapabilities from it) and to recreate the PVC on import.
//
// direct is a non-cached reader used for the cluster-scoped PV read: the controller's RBAC grants only
// "get" on persistentvolumes, so a cached read would start a cluster-wide PV informer whose initial
// LIST is Forbidden (and the fsType read would fail). Callers pass their API reader (directReader);
// when nil it falls back to c.
//
// It mutates and returns the same slice. A transient read error is returned so the caller requeues
// instead of publishing partial metadata (which would otherwise be frozen by the steady-state coverage
// gate). Only a genuinely-gone source PVC (NotFound) is tolerated: it is logged and its binding's
// metadata is left empty, since there is nothing left to read.
func EnrichDataBindingsWithVolumeMetadata(ctx context.Context, c client.Client, direct client.Reader, bindings []storagev1alpha1.SnapshotDataBinding) ([]storagev1alpha1.SnapshotDataBinding, error) {
	if direct == nil {
		direct = c
	}
	log := logf.FromContext(ctx)
	for i := range bindings {
		b := &bindings[i]
		// Size lives on the durable data artifact (VolumeSnapshotContent.status.restoreSize, bytes), not
		// on the source PVC: it is the real allocated size the snapshot can be restored to and outlives
		// the PVC. Read it for every artifact-bearing binding (domain data leaves too, which have no PVC
		// target). A not-yet-populated restoreSize is left empty (best-effort) rather than blocking.
		// The same VSC read also backfills the durable artifact uid when an upstream producer (e.g. the
		// import path) referenced the artifact by name only; producers that already know the uid (VCR /
		// orphan paths) keep theirs.
		size, uid, serr := readArtifactRestoreSizeAndUID(ctx, c, b.Artifact)
		if serr != nil {
			return bindings, serr
		}
		if size != "" {
			b.Size = size
		}
		if b.Artifact.UID == "" && uid != "" {
			b.Artifact.UID = uid
		}
		if b.Target.Kind != "PersistentVolumeClaim" || b.Target.Name == "" {
			continue
		}
		pvc := &corev1.PersistentVolumeClaim{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: b.Target.Namespace, Name: b.Target.Name}, pvc); err != nil {
			if apierrors.IsNotFound(err) {
				log.Info("source PVC gone; skipping volume-metadata enrichment",
					"pvc", b.Target.Namespace+"/"+b.Target.Name)
				continue
			}
			return bindings, fmt.Errorf("read source PVC %s/%s for volume metadata: %w", b.Target.Namespace, b.Target.Name, err)
		}
		// PVC.spec.volumeMode defaults to Filesystem when nil (Kubernetes semantics).
		if pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode != "" {
			b.VolumeMode = string(*pvc.Spec.VolumeMode)
		} else {
			b.VolumeMode = string(corev1.PersistentVolumeFilesystem)
		}
		if len(pvc.Spec.AccessModes) > 0 {
			modes := make([]string, 0, len(pvc.Spec.AccessModes))
			for _, am := range pvc.Spec.AccessModes {
				modes = append(modes, string(am))
			}
			b.AccessModes = modes
		}
		if pvc.Spec.StorageClassName != nil {
			b.StorageClassName = *pvc.Spec.StorageClassName
		}
		// fsType lives on the bound PV's CSI source (ignored for Block volumes). Read via the direct
		// reader so the get-only PV RBAC suffices and no cluster-wide PV cache is started.
		if b.VolumeMode != string(corev1.PersistentVolumeBlock) && pvc.Spec.VolumeName != "" {
			pv := &corev1.PersistentVolume{}
			if err := direct.Get(ctx, client.ObjectKey{Name: pvc.Spec.VolumeName}, pv); err != nil {
				return bindings, fmt.Errorf("read bound PV %s for fsType of PVC %s/%s: %w", pvc.Spec.VolumeName, b.Target.Namespace, b.Target.Name, err)
			}
			if pv.Spec.CSI != nil {
				b.FsType = pv.Spec.CSI.FSType
			}
		}
	}
	return bindings, nil
}

// readArtifactRestoreSize returns the binding's data artifact size as a resource.Quantity string,
// read from VolumeSnapshotContent.status.restoreSize (an int64 byte count). It returns an empty string
// (no error) when the artifact is not a VSC, is unnamed, is being deleted, or has not published its
// restoreSize yet; a transient read error is returned so the caller can requeue.
func readArtifactRestoreSize(ctx context.Context, c client.Client, artifact storagev1alpha1.SnapshotDataArtifactRef) (string, error) {
	size, _, err := readArtifactRestoreSizeAndUID(ctx, c, artifact)
	return size, err
}

// readArtifactRestoreSizeAndUID reads the durable VolumeSnapshotContent once and returns both its
// restoreSize (see readArtifactRestoreSize) and its uid. The uid is best-effort: it is empty (no error)
// whenever the artifact is not a readable, live VSC, mirroring the size semantics. A single read backs
// both values so enrichment does not double-GET the VSC.
func readArtifactRestoreSizeAndUID(ctx context.Context, c client.Client, artifact storagev1alpha1.SnapshotDataArtifactRef) (string, types.UID, error) {
	if artifact.Kind != "VolumeSnapshotContent" || artifact.Name == "" {
		return "", "", nil
	}
	vsc := &unstructured.Unstructured{}
	vsc.SetGroupVersionKind(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"})
	if err := c.Get(ctx, client.ObjectKey{Name: artifact.Name}, vsc); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("read VolumeSnapshotContent %s for restoreSize: %w", artifact.Name, err)
	}
	if !vsc.GetDeletionTimestamp().IsZero() {
		return "", "", nil
	}
	uid := vsc.GetUID()
	bytes, found, err := unstructured.NestedInt64(vsc.Object, "status", "restoreSize")
	if err != nil {
		// restoreSize present but not an int64 (decoder/schema drift). Leave Size empty (best-effort) but
		// surface it at debug so an unexpected type is observable instead of silently swallowed.
		logf.FromContext(ctx).V(1).Info("VolumeSnapshotContent status.restoreSize is present but not int64; leaving Size empty",
			"vsc", artifact.Name, "error", err.Error())
		return "", uid, nil
	}
	if !found || bytes <= 0 {
		return "", uid, nil
	}
	return resource.NewQuantity(bytes, resource.BinarySI).String(), uid, nil
}

// PublishSnapshotContentDataRefs copies the durable data binding onto a logical SnapshotContent (N5 PR-4).
//
// Variant A (cardinality ≤1): a SnapshotContent holds at most one dataRef. Domain leaves own exactly one
// PVC, so refs carries exactly one binding here; multi-PVC scopes (root residual/orphan) fan out into
// child volume nodes upstream and never publish more than one binding onto a single content. As a
// defensive guard against a foundation VCR returning >1 dataRef for a single logical content, len>1 is a
// terminal programming error rather than a silently-truncated publish.
func PublishSnapshotContentDataRefs(ctx context.Context, c client.Client, contentName string, refs []storagev1alpha1.SnapshotDataBinding) error {
	if contentName == "" || len(refs) == 0 {
		return nil
	}
	if len(refs) > 1 {
		return fmt.Errorf("SnapshotContent %s: cannot publish %d dataRefs onto one content (Variant A cardinality ≤1; multiple volumes must be modeled as child volume nodes)", contentName, len(refs))
	}
	return PublishSnapshotContentDataRef(ctx, c, contentName, &refs[0])
}

// PublishSnapshotContentDataRef writes the single durable data binding onto a logical SnapshotContent.
// A nil binding clears status.dataRef. The write is idempotent under optimistic retry.
func PublishSnapshotContentDataRef(ctx context.Context, c client.Client, contentName string, ref *storagev1alpha1.SnapshotDataBinding) error {
	if contentName == "" {
		return nil
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		content := &storagev1alpha1.SnapshotContent{}
		if err := c.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
			return err
		}
		if snapshotDataRefEqual(content.Status.DataRef, ref) {
			return nil
		}
		base := content.DeepCopy()
		if ref == nil {
			content.Status.DataRef = nil
		} else {
			cp := *ref
			content.Status.DataRef = &cp
		}
		return c.Status().Patch(ctx, content, client.MergeFrom(base))
	})
}

// volumeSnapshotContentRetainPolicy keeps the bound VSC durable after the per-run VolumeSnapshot /
// VolumeCaptureRequest is deleted (durable-artifact contract, ADR 2026-06-09 / spec §3.9.6, §3.9.11).
const volumeSnapshotContentRetainPolicy = "Retain"

// EnsureVolumeSnapshotContentsOwnedByContent performs the durable-artifact handoff for each bound VSC:
// it re-parents the VSC ownerReference to the bound SnapshotContent AND forces spec.deletionPolicy to
// Retain so the artifact survives deletion of the per-run VolumeSnapshot/VCR. This mirrors the orphan
// PVC path (ensureVolumeSnapshotContentRetain): without the Retain step a class-default Delete policy
// would let the underlying snapshot be garbage-collected, breaking durability (the domain VCR path
// previously left the VSC at deletionPolicy=Delete).
func EnsureVolumeSnapshotContentsOwnedByContent(
	ctx context.Context,
	c client.Client,
	content *storagev1alpha1.SnapshotContent,
	refs []storagev1alpha1.SnapshotDataBinding,
) error {
	if content == nil {
		return nil
	}
	for _, binding := range refs {
		if binding.Artifact.Kind != "VolumeSnapshotContent" || binding.Artifact.Name == "" {
			continue
		}
		if err := ensureVolumeSnapshotContentOwnedByContent(ctx, c, binding.Artifact.Name, content); err != nil {
			return err
		}
	}
	return nil
}

func ensureVolumeSnapshotContentOwnedByContent(
	ctx context.Context,
	c client.Client,
	vscName string,
	content *storagev1alpha1.SnapshotContent,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"})
		if err := c.Get(ctx, client.ObjectKey{Name: vscName}, obj); err != nil {
			return err
		}
		// A VSC that is being deleted MUST NOT be patched (spec §3.9.10): touching ownerRef or
		// deletionPolicy on an object with a deletionTimestamp is pointless (it is going away) and could
		// race finalizer removal. Data readiness already treats a deleting VSC as ArtifactMissing. The
		// self-heal caller pre-checks this too; the guard here makes the publish-path handoff equally safe.
		if !obj.GetDeletionTimestamp().IsZero() {
			return nil
		}
		ownerRef := metav1.OwnerReference{
			APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
			Kind:       "SnapshotContent",
			Name:       content.Name,
			UID:        content.UID,
			Controller: func() *bool { b := true; return &b }(),
		}
		refs, ownerChanged, err := snapshotContentControllerOwnerRefsForHandoff(obj.GetOwnerReferences(), ownerRef)
		if err != nil {
			return fmt.Errorf("VolumeSnapshotContent %s: %w", vscName, err)
		}
		policy, _, perr := unstructured.NestedString(obj.Object, "spec", "deletionPolicy")
		if perr != nil {
			return fmt.Errorf("read VolumeSnapshotContent %s deletionPolicy: %w", vscName, perr)
		}
		policyChanged := policy != volumeSnapshotContentRetainPolicy
		if !ownerChanged && !policyChanged {
			return nil
		}
		base := obj.DeepCopy()
		if ownerChanged {
			obj.SetOwnerReferences(refs)
		}
		if policyChanged {
			if serr := unstructured.SetNestedField(obj.Object, volumeSnapshotContentRetainPolicy, "spec", "deletionPolicy"); serr != nil {
				return fmt.Errorf("set VolumeSnapshotContent %s deletionPolicy=Retain: %w", vscName, serr)
			}
		}
		return c.Patch(ctx, obj, client.MergeFrom(base))
	})
}

// snapshotDataRefEqual compares two optional singular dataRef bindings (Variant A).
func snapshotDataRefEqual(a, b *storagev1alpha1.SnapshotDataBinding) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return dataBindingEqual(*a, *b)
}

func dataBindingEqual(x, y storagev1alpha1.SnapshotDataBinding) bool {
	if x.TargetUID != y.TargetUID {
		return false
	}
	if x.Target.APIVersion != y.Target.APIVersion || x.Target.Kind != y.Target.Kind ||
		x.Target.Name != y.Target.Name || x.Target.Namespace != y.Target.Namespace ||
		string(x.Target.UID) != string(y.Target.UID) {
		return false
	}
	if x.Artifact != y.Artifact {
		return false
	}
	if x.VolumeMode != y.VolumeMode || x.FsType != y.FsType || x.StorageClassName != y.StorageClassName || x.Size != y.Size {
		return false
	}
	return stringSlicesEqual(x.AccessModes, y.AccessModes)
}

func stringSlicesEqual(a, b []string) bool {
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
