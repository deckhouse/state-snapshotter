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

package snapshotsdk

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// ChildrenCaptureStates reads the snapshot's declared child snapshot refs and reports each child's Ready
// condition plus its capture-leg latch state. It uses the cached client (children are watched by the
// domain's own parent<-child watch), reading each child as an unstructured object by its ref GVK so it
// stays domain-type-agnostic. A child that is not found is reported as still-pending (empty Ready,
// AllLegsCaptured=false) rather than treated as an error, since the child CR may still be materializing.
func (s *sdk) ChildrenCaptureStates(ctx context.Context, t SnapshotAdapter) ([]ChildCaptureState, error) {
	refs := t.GetDomainCaptureState().ChildrenSnapshotRefs
	namespace := t.Object().GetNamespace()
	states := make([]ChildCaptureState, 0, len(refs))
	for _, ref := range refs {
		state := ChildCaptureState{Ref: ref}
		u := &unstructured.Unstructured{}
		u.SetAPIVersion(ref.APIVersion)
		u.SetKind(ref.Kind)
		if err := s.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, u); err != nil {
			if apierrors.IsNotFound(err) {
				states = append(states, state)
				continue
			}
			return nil, err
		}
		state.ReadyStatus, state.ReadyReason, state.ReadyMessage = childReadyCondition(u)
		state.AllLegsCaptured = childCoreCaptureState(u).AllLegsCaptured()
		states = append(states, state)
	}
	return states, nil
}

// childReadyCondition extracts status/reason/message of the Ready condition from an unstructured child
// snapshot. It returns an empty status when the object carries no Ready condition yet.
func childReadyCondition(u *unstructured.Unstructured) (metav1.ConditionStatus, string, string) {
	conditions, found, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil || !found {
		return "", "", ""
	}
	for _, raw := range conditions {
		cond, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if condType, _, _ := unstructured.NestedString(cond, "type"); condType != storagev1alpha1.ConditionReady {
			continue
		}
		status, _, _ := unstructured.NestedString(cond, "status")
		reason, _, _ := unstructured.NestedString(cond, "reason")
		message, _, _ := unstructured.NestedString(cond, "message")
		return metav1.ConditionStatus(status), reason, message
	}
	return "", "", ""
}

// childCoreCaptureState reads the core-written leg latches (status.captureState.commonController) from an
// unstructured child snapshot into the SDK's read-only view. An absent latch stays nil (leg not declared).
func childCoreCaptureState(u *unstructured.Unstructured) CoreCaptureState {
	state := CoreCaptureState{}
	if v, found, err := unstructured.NestedBool(u.Object, "status", "captureState", "commonController", "manifestCaptured"); err == nil && found {
		captured := v
		state.ManifestCaptured = &captured
	}
	if v, found, err := unstructured.NestedBool(u.Object, "status", "captureState", "commonController", "dataCaptured"); err == nil && found {
		captured := v
		state.DataCaptured = &captured
	}
	return state
}
