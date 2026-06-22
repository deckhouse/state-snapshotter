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

package genericbinder

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/manifestcapture"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/snapshotcontent"
	vcctrl "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/volumecapture"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

// ensureDomainContentLinks projects a domain-capture snapshot's planning results (children + data leg)
// onto its bound SnapshotContent and runs the request lifecycle the domain controller no longer owns:
//   - children: SnapshotContent.status.childrenSnapshotContentRefs from demo.status.childrenSnapshotRefs;
//   - data leg: read demo.status.volumeCaptureRequestName -> VCR -> enrich + VSC ownership handoff ->
//     SnapshotContent.status.dataRefs;
//   - cleanup + markers: once a leg's handoff to SnapshotContent is durable, delete the domain MCR/VCR
//     and stamp the domain-only suppression marker (status.manifestCaptured / status.dataCaptured) so the
//     domain controller stops re-creating the request without ever reading SnapshotContent.
//
// mcrName is the snapshot's current status.manifestCaptureRequestName (already used for the MCP publish).
func (r *GenericSnapshotBinderController) ensureDomainContentLinks(
	ctx context.Context,
	obj *unstructured.Unstructured,
	contentName string,
	mcrName string,
) (requeue bool, terminalReason string, terminalMessage string, err error) {
	ns := obj.GetNamespace()

	// Manifest cleanup + marker: once the MCP is published and owned by the SnapshotContent, the domain
	// MCR is stale. Stamp manifestCaptured (so the domain controller stops re-creating it) and delete it.
	manifestCaptured := nestedBool(obj, "manifestCaptured")
	if !manifestCaptured && mcrName != "" {
		safe, sErr := manifestcapture.ManifestCaptureRequestSafeToDelete(ctx, r.APIReader, types.NamespacedName{Namespace: ns, Name: mcrName}, contentName)
		if sErr != nil {
			return false, "", "", sErr
		}
		if safe {
			if mErr := r.markCaptureDone(ctx, obj, "manifestCaptured", "manifestCaptureRequestName"); mErr != nil {
				return false, "", "", mErr
			}
			if dErr := r.deleteManifestCaptureRequest(ctx, types.NamespacedName{Namespace: ns, Name: mcrName}); dErr != nil {
				return false, "", "", dErr
			}
		}
	}

	// Children projection (intermediate nodes, e.g. demo VM). A leaf with no children publishes nothing:
	// SnapshotContentController treats absent childrenSnapshotContentRefs as "no children" (leaf-complete).
	childRefs := parseChildrenSnapshotRefs(obj)
	if len(childRefs) > 0 {
		published, pErr := snapshotcontent.PublishSnapshotContentChildrenFromSnapshotRefs(ctx, r.Client, r.APIReader, ns, contentName, childRefs)
		if pErr != nil {
			return false, "", "", pErr
		}
		if !published {
			requeue = true
		}
	}

	// Data leg (leaf nodes with a PVC, e.g. demo disk). Absent volumeCaptureRequestName means manifest-only.
	vcrName := nestedString(obj, "volumeCaptureRequestName")
	dataCaptured := nestedBool(obj, "dataCaptured")
	if !dataCaptured && vcrName != "" {
		done, treason, tmsg, dErr := r.projectDataLegFromVCR(ctx, obj, contentName, vcrName)
		if dErr != nil {
			return false, "", "", dErr
		}
		if treason != "" {
			return requeue, treason, tmsg, nil
		}
		if !done {
			requeue = true
		} else {
			vcrKey := types.NamespacedName{Namespace: ns, Name: vcrName}
			safe, sErr := vcctrl.VolumeCaptureRequestSafeToDeleteWithHandoff(ctx, r.APIReader, vcrKey, contentName)
			if sErr != nil {
				return false, "", "", sErr
			}
			if safe {
				if mErr := r.markCaptureDone(ctx, obj, "dataCaptured", "volumeCaptureRequestName"); mErr != nil {
					return false, "", "", mErr
				}
				if delErr := r.deleteVolumeCaptureRequest(ctx, vcrKey); delErr != nil {
					return false, "", "", delErr
				}
			} else {
				requeue = true
			}
		}
	}

	return requeue, "", "", nil
}

// projectDataLegFromVCR reads the domain-created VolumeCaptureRequest, validates its result against its
// own spec.targets, enriches volume metadata, transfers VolumeSnapshotContent ownership to the
// SnapshotContent, and publishes dataRefs. Returns done=true once dataRefs cover the VCR targets.
func (r *GenericSnapshotBinderController) projectDataLegFromVCR(
	ctx context.Context,
	obj *unstructured.Unstructured,
	contentName string,
	vcrName string,
) (done bool, terminalReason string, terminalMessage string, err error) {
	vcrKey := client.ObjectKey{Namespace: obj.GetNamespace(), Name: vcrName}
	vcr := &unstructured.Unstructured{}
	vcr.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
	if getErr := r.Get(ctx, vcrKey, vcr); getErr != nil {
		if errors.IsNotFound(getErr) {
			// Domain controller has not (re)created the VCR yet; wait for it.
			return false, "", "", nil
		}
		return false, "", "", getErr
	}

	expectedTargets, parseErr := vcctrl.ParseVolumeCaptureTargets(vcr)
	if parseErr != nil {
		return false, "", "", parseErr
	}

	content := &storagev1alpha1.SnapshotContent{}
	if cErr := r.Get(ctx, client.ObjectKey{Name: contentName}, content); cErr != nil {
		return false, "", "", cErr
	}
	if vcctrl.ContentDataRefsCoverExpectedTargets(content.DataRefList(), expectedTargets) {
		return true, "", "", nil
	}

	if failed, reason, msg := vcctrl.VolumeCaptureRequestFailed(vcr); failed {
		detail := msg
		if reason != "" {
			detail = fmt.Sprintf("%s: %s", reason, msg)
		}
		return false, snapshot.ReasonVolumeCaptureFailed, fmt.Sprintf("data-leg volume capture failed: %s", detail), nil
	}
	if !vcctrl.VolumeCaptureRequestReady(vcr) {
		return false, "", "", nil
	}

	vcrRefs, refErr := vcctrl.ParseVolumeCaptureDataRefs(vcr)
	if refErr != nil {
		return false, "", "", refErr
	}
	if validateErr := vcctrl.ValidateDataRefsForPublish(expectedTargets, vcrRefs); validateErr != nil {
		// Ready VCR whose dataRefs are not yet consistent: retry without a terminal condition.
		return false, "", "", nil
	}

	bindings := vcctrl.SnapshotDataBindingsFromVCRStatus(vcrRefs)
	// Variant A: a domain volume leaf owns exactly one PVC, so its content holds ≤1 dataRef. A ready VCR
	// that returned >1 data artifact for this single logical content cannot be represented (status.dataRef
	// is singular) and is a domain decomposition fault — fail terminally instead of silently publishing
	// dataRefs[0] (which would drop the others) or looping forever. Real multi-volume scopes must fan out
	// into child volume nodes upstream, never a list on one content.
	if len(bindings) > 1 {
		return false, snapshot.ReasonVolumeCaptureFailed,
			fmt.Sprintf("data-leg volume capture returned %d data artifacts for a single SnapshotContent %q; Variant A allows at most one PVC per domain volume node (decompose multiple volumes into child volume nodes)", len(bindings), contentName), nil
	}
	bindings, enrichErr := snapshotcontent.EnrichDataBindingsWithVolumeMetadata(ctx, r.Client, r.APIReader, bindings)
	if enrichErr != nil {
		return false, "", "", enrichErr
	}
	if cErr := r.Get(ctx, client.ObjectKey{Name: contentName}, content); cErr != nil {
		return false, "", "", cErr
	}
	if handoffErr := snapshotcontent.EnsureVolumeSnapshotContentsOwnedByContent(ctx, r.Client, content, bindings); handoffErr != nil {
		// Retryable handoff; coverage still holds via the pending VCR until dataRefs are published.
		return false, "", "", nil
	}
	if pubErr := snapshotcontent.PublishSnapshotContentDataRefs(ctx, r.Client, contentName, bindings); pubErr != nil {
		return false, "", "", pubErr
	}
	return true, "", "", nil
}

// markCaptureDone sets a domain-only boolean capture marker to true and clears the matching request-name
// field in the same status patch, under an optimistic retry. The marker MUST be set before the request is
// deleted so the domain controller (which gates re-creation on the marker) never re-creates it.
func (r *GenericSnapshotBinderController) markCaptureDone(ctx context.Context, obj *unstructured.Unstructured, markerField, clearNameField string) error {
	gvk := obj.GetObjectKind().GroupVersionKind()
	key := client.ObjectKey{Namespace: obj.GetNamespace(), Name: obj.GetName()}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &unstructured.Unstructured{}
		fresh.SetGroupVersionKind(gvk)
		if err := r.Get(ctx, key, fresh); err != nil {
			return err
		}
		marker := nestedBool(fresh, markerField)
		name := nestedString(fresh, clearNameField)
		if marker && name == "" {
			return nil
		}
		base := fresh.DeepCopy()
		if err := unstructured.SetNestedField(fresh.Object, true, "status", markerField); err != nil {
			return err
		}
		if err := unstructured.SetNestedField(fresh.Object, "", "status", clearNameField); err != nil {
			return err
		}
		// D4a: demo.status is co-owned (domain writes conditions/capture fields), so use an optimistic-lock
		// merge patch — a concurrent demo status write yields 409 and RetryOnConflict re-reads, instead of
		// this patch silently racing on a stale resourceVersion.
		return r.Status().Patch(ctx, fresh, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}

func (r *GenericSnapshotBinderController) deleteManifestCaptureRequest(ctx context.Context, key types.NamespacedName) error {
	mcr := &ssv1alpha1.ManifestCaptureRequest{}
	if err := r.Get(ctx, key, mcr); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if err := r.Delete(ctx, mcr); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

func (r *GenericSnapshotBinderController) deleteVolumeCaptureRequest(ctx context.Context, key types.NamespacedName) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
	if err := r.Get(ctx, key, obj); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if err := r.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

func nestedString(obj *unstructured.Unstructured, field string) string {
	v, _, _ := unstructured.NestedString(obj.Object, "status", field)
	return v
}

func nestedBool(obj *unstructured.Unstructured, field string) bool {
	v, _, _ := unstructured.NestedBool(obj.Object, "status", field)
	return v
}

// parseChildrenSnapshotRefs reads status.childrenSnapshotRefs into typed child refs (APIVersion/Kind/Name).
func parseChildrenSnapshotRefs(obj *unstructured.Unstructured) []storagev1alpha1.SnapshotChildRef {
	raw, _, err := unstructured.NestedSlice(obj.Object, "status", "childrenSnapshotRefs")
	if err != nil || len(raw) == 0 {
		return nil
	}
	out := make([]storagev1alpha1.SnapshotChildRef, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		ref := storagev1alpha1.SnapshotChildRef{}
		if v, ok := m["apiVersion"].(string); ok {
			ref.APIVersion = v
		}
		if v, ok := m["kind"].(string); ok {
			ref.Kind = v
		}
		if v, ok := m["name"].(string); ok {
			ref.Name = v
		}
		if ref.Name == "" {
			continue
		}
		out = append(out, ref)
	}
	return out
}
