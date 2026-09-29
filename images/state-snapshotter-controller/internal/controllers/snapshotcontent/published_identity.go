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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// publishedDataIdentityFrozen reports whether status.data already carries a set-once identity
// (artifact name + source name). uid and Size are not identity: they are filled after the first
// publish and must still be allowed to land.
func publishedDataIdentityFrozen(d *storagev1alpha1.SnapshotDataBinding) bool {
	if d == nil {
		return false
	}
	return d.ArtifactRef.Name != "" && d.SourceRef.Name != ""
}

// dataBindingIdentityEqual compares the set-once identity of two bindings: apiVersion/kind/name on
// both refs, plus namespace on sourceRef. uid and Size are ignored.
func dataBindingIdentityEqual(a, b storagev1alpha1.SnapshotDataBinding) bool {
	return a.ArtifactRef.APIVersion == b.ArtifactRef.APIVersion &&
		a.ArtifactRef.Kind == b.ArtifactRef.Kind &&
		a.ArtifactRef.Name == b.ArtifactRef.Name &&
		a.SourceRef.APIVersion == b.SourceRef.APIVersion &&
		a.SourceRef.Kind == b.SourceRef.Kind &&
		a.SourceRef.Name == b.SourceRef.Name &&
		a.SourceRef.Namespace == b.SourceRef.Namespace
}

func dataBindingNeedsIdentityEnrichment(d *storagev1alpha1.SnapshotDataBinding) bool {
	if d == nil {
		return false
	}
	return d.Size == "" || d.ArtifactRef.UID == ""
}

// holdPublishedDataIfDiverged returns held=true when status.data already has a frozen identity that
// does not match incoming. The caller must not publish incoming: a rewrite would move the captured
// artifact (and, once set-once admission is on, wedge the reconcile). Size/uid on the published
// identity may still be backfilled from the live object named by that identity.
//
// incoming nil means there is no candidate binding this pass (a failed leftover request, a missing
// live artifact): the published identity is still held.
func (r *SnapshotContentController) holdPublishedDataIfDiverged(
	ctx context.Context,
	contentObj *unstructured.Unstructured,
	content *storagev1alpha1.SnapshotContent,
	incoming *storagev1alpha1.SnapshotDataBinding,
) (held bool, requeue bool, err error) {
	published := content.Status.Data
	if !publishedDataIdentityFrozen(published) {
		return false, false, nil
	}
	if incoming != nil && dataBindingIdentityEqual(*published, *incoming) {
		return false, false, nil
	}
	r.notePublishedIdentityHeld(ctx, contentObj)
	if !dataBindingNeedsIdentityEnrichment(published) {
		return true, false, nil
	}
	requeue, err = r.publishDataBindings(ctx, content.Name, []storagev1alpha1.SnapshotDataBinding{*published}, importVolumeMetadataFromPublished(published))
	return true, requeue, err
}

func (r *SnapshotContentController) notePublishedIdentityHeld(ctx context.Context, contentObj *unstructured.Unstructured) {
	registerSnapshotContentMetrics()
	publishedIdentityHeldTotal.Inc()
	msg := fmt.Sprintf("refusing to rewrite the published SnapshotContent %q identity", contentObj.GetName())
	logf.FromContext(ctx).Info(msg)
	if r.Recorder != nil {
		r.Recorder.Event(contentObj, corev1.EventTypeWarning, EventReasonPublishedIdentityHeld, msg)
	}
}
