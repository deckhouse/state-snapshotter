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
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
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
	r.notePublishedIdentityHeld(ctx, contentObj, heldLegData,
		fmt.Sprintf("status.data stays %s, not %s", describeDataBinding(published), describeDataBinding(incoming)))
	if !dataBindingNeedsIdentityEnrichment(published) {
		return true, false, nil
	}
	requeue, err = r.publishDataBindings(ctx, content.Name, []storagev1alpha1.SnapshotDataBinding{*published}, importVolumeMetadataFromPublished(published))
	return true, requeue, err
}

// describeDataBinding names the identity of a data binding for a diagnostic: the artifact and the volume
// it was taken from. nil is the pass that has no candidate binding at all.
func describeDataBinding(d *storagev1alpha1.SnapshotDataBinding) string {
	if d == nil {
		return "a capture that produced no artifact"
	}
	return fmt.Sprintf("%s %q of %s %s/%s", d.ArtifactRef.Kind, d.ArtifactRef.Name, d.SourceRef.Kind, d.SourceRef.Namespace, d.SourceRef.Name)
}

// The two set-once identities a SnapshotContent publishes, as keys of heldIdentityNotes. Both can be held
// in the same pass, and each has its own Event.
const (
	heldLegManifests = "manifests"
	heldLegData      = "data"
)

// notePublishedIdentityHeld counts and logs a projection that refused to rewrite a published identity, and
// records a Warning Event on the SnapshotContent the first time this process sees that refusal.
//
// The refusal is re-decided on every pass while the divergent candidate is around, and a SnapshotContent
// that is not Ready requeues itself every defaultSnapshotContentRequeueAfter: an Event per pass would bury
// the one that says when it began. detail names the published identity and the candidate it refused, so a
// different candidate is a new Event. The metric still counts every refusal.
func (r *SnapshotContentController) notePublishedIdentityHeld(ctx context.Context, contentObj *unstructured.Unstructured, leg, detail string) {
	registerSnapshotContentMetrics()
	publishedIdentityHeldTotal.Inc()
	msg := fmt.Sprintf("refusing to rewrite the published SnapshotContent %q identity: %s", contentObj.GetName(), detail)
	key := types.NamespacedName{Namespace: contentObj.GetNamespace(), Name: contentObj.GetName()}
	if !r.heldNotes.first(key, leg, contentObj.GetUID(), msg) {
		logf.FromContext(ctx).V(1).Info(msg)
		return
	}
	logf.FromContext(ctx).Info(msg)
	if r.Recorder != nil {
		r.Recorder.Eventf(contentObj, nil, corev1.EventTypeWarning, EventReasonPublishedIdentityHeld, "PublishIdentity", "%s", msg)
	}
}

// heldIdentityNotes remembers, per SnapshotContent and identity, the refusal this process last recorded an
// Event for. It lives in memory only: a restarted controller records a refusal that still holds once more.
// The zero value is ready to use.
type heldIdentityNotes struct {
	mu   sync.Mutex
	seen map[heldIdentityKey]heldIdentityNote
}

type heldIdentityKey struct {
	content types.NamespacedName
	leg     string
}

type heldIdentityNote struct {
	uid types.UID
	msg string
}

// first reports whether msg is new for this content and leg, and remembers it. A content recreated under
// the same name has a new uid, so its first refusal is new as well.
func (n *heldIdentityNotes) first(content types.NamespacedName, leg string, uid types.UID, msg string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	key := heldIdentityKey{content: content, leg: leg}
	note := heldIdentityNote{uid: uid, msg: msg}
	if n.seen[key] == note {
		return false
	}
	if n.seen == nil {
		n.seen = make(map[heldIdentityKey]heldIdentityNote)
	}
	n.seen[key] = note
	return true
}

// forget drops what is remembered for a SnapshotContent that is gone.
func (n *heldIdentityNotes) forget(content types.NamespacedName) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.seen, heldIdentityKey{content: content, leg: heldLegManifests})
	delete(n.seen, heldIdentityKey{content: content, leg: heldLegData})
}
