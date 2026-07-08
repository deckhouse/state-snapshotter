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

package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	volumecaptureuc "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/volumecapture"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	snapshotpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshotgraphregistry"
)

// buildSnapshotMachineryGVKs builds the mechanism-1 (kind-level dedup) set from the live GVK registry:
// every registered snapshot kind and content kind. These are objects the snapshotter creates itself, so
// they MUST be excluded from namespace capture. Returns snapshotgraphregistry.ErrGraphRegistryNotReady
// when the registry has not been built yet (fail-closed: callers requeue instead of capturing machinery).
func (r *SnapshotReconciler) buildSnapshotMachineryGVKs() (namespacemanifest.SnapshotMachineryGVKs, error) {
	if r.SnapshotGraphRegistry == nil {
		return nil, snapshotgraphregistry.ErrGraphRegistryNotReady
	}
	reg := r.SnapshotGraphRegistry.Current()
	if reg == nil {
		return nil, snapshotgraphregistry.ErrGraphRegistryNotReady
	}
	set := make(namespacemanifest.SnapshotMachineryGVKs)
	for _, kind := range reg.RegisteredSnapshotKinds() {
		gvk, err := reg.ResolveSnapshotGVK(kind)
		if err != nil {
			continue
		}
		set[gvk] = struct{}{}
	}
	for _, gvk := range reg.RegisteredContentGVKs() {
		set[gvk] = struct{}{}
	}
	return set, nil
}

// dataBearingKindFunc returns the coverage data-bearing predicate backed by the live GVK registry
// (CSD spec.requiresDataArtifact via GVKRegistry.RequiresDataArtifact). Coverage keys the decision on
// the owning snapshot kind — NOT the shape of the subtree (Block 5, design §8.5). Returns
// snapshotgraphregistry.ErrGraphRegistryNotReady when the registry is not built yet so callers requeue
// (fail-closed: never under-cover with an empty registry, which would let an already-captured PVC be
// re-captured as orphan).
func (r *SnapshotReconciler) dataBearingKindFunc() (volumecaptureuc.DataBearingKindFunc, error) {
	if r.SnapshotGraphRegistry == nil {
		return nil, snapshotgraphregistry.ErrGraphRegistryNotReady
	}
	reg := r.SnapshotGraphRegistry.Current()
	if reg == nil {
		return nil, snapshotgraphregistry.ErrGraphRegistryNotReady
	}
	return reg.RequiresDataArtifact, nil
}

// namespaceCaptureRBACReady reports whether this controller is already authorized to list every resource
// in the target namespace, i.e. the per-namespace capture RoleBinding (d8-state-snapshotter-capture,
// wildcard get/list) created by hooks/go/040-namespace-capture-rbac has propagated. It issues a
// SelfSubjectAccessReview with Group/Resource "*"; the RBAC authorizer answers allowed for such a request
// only when a rule with resources:["*"] (the capture grant) is in effect — the controller's own narrow
// roles do not match. The review goes through the same authorizer as the subsequent list, so once it
// allows the list is guaranteed readable (no Forbidden race, strictly one list).
//
// When SARClient is nil (tests/envtest without RBAC wiring) the gate is skipped (returns true) so capture
// behaves as before.
func (r *SnapshotReconciler) namespaceCaptureRBACReady(ctx context.Context, namespace string) (bool, error) {
	if r.SARClient == nil {
		return true, nil
	}
	sar := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: namespace,
				Verb:      "list",
				Group:     "*",
				Resource:  "*",
			},
		},
	}
	resp, err := r.SARClient.Create(ctx, sar, metav1.CreateOptions{})
	if err != nil {
		return false, fmt.Errorf("self subject access review (list */* in namespace %s): %w", namespace, err)
	}
	return resp.Status.Allowed, nil
}

// isTransientCaptureTargetError classifies a capture-target build error as a transient apiserver/network
// hiccup (requeue, NOT terminal ListFailed). Discovery lists ALL namespaced types (plus flaky aggregated
// APIServers), so the window for these errors is large; treating them as terminal would stick the snapshot
// (failCapture returns no requeue). Forbidden is NOT classified here — it is collected as unreadable and
// handled as fail-closed transient (ReasonNamespaceCaptureIncomplete) separately.
func isTransientCaptureTargetError(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsServerTimeout(err) ||
		apierrors.IsTimeout(err) ||
		apierrors.IsTooManyRequests(err) ||
		apierrors.IsServiceUnavailable(err) ||
		apierrors.IsInternalError(err) ||
		apierrors.IsUnexpectedServerError(err) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "Informer to sync") ||
		strings.Contains(msg, "failed waiting for") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset")
}

// setSnapshotReadyFalse writes Ready=False with reason/msg on a fresh Snapshot (conflict-retried).
// Used by pre-publish degrade/fail paths where a local Ready reason is allowed (no bound content mirror yet).
func (r *SnapshotReconciler) setSnapshotReadyFalse(ctx context.Context, nsSnap *storagev1alpha1.Snapshot, reason, msg string) error {
	nsKey := types.NamespacedName{Namespace: nsSnap.Namespace, Name: nsSnap.Name}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &storagev1alpha1.Snapshot{}
		if err := r.Client.Get(ctx, nsKey, fresh); err != nil {
			return err
		}
		meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
			Type:               snapshotpkg.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            msg,
			ObservedGeneration: fresh.Generation,
		})
		return r.Client.Status().Update(ctx, fresh)
	})
}

func (r *SnapshotReconciler) failCapture(ctx context.Context, nsSnap *storagev1alpha1.Snapshot, content *storagev1alpha1.SnapshotContent, reason, msg string) (ctrl.Result, error) {
	if err := r.setSnapshotReadyFalse(ctx, nsSnap, reason, msg); err != nil {
		return ctrl.Result{}, err
	}
	_ = content
	return ctrl.Result{}, nil
}
