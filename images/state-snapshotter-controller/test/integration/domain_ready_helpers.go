//go:build integration
// +build integration

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

package integration

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// injectDomainPlanned sets status.captureState.domainSpecificController.phase=Planned on obj (in memory).
// Use it inside blocks that drive their own Status().Update; the phase is the planning-done signal the
// generic binder barrier requires (it replaced the former PlanningReady=True condition). Spec is
// immutable, so no observedGeneration gate is needed.
func injectDomainPlanned(obj *unstructured.Unstructured) {
	setDomainPhase(obj, storagev1alpha1.SnapshotCapturePhasePlanned)
}

// setSnapshotDomainPlannedCurrent publishes phase=Planned and updates the status subresource via
// k8sClient — the planning-done signal the generic binder barrier requires.
func setSnapshotDomainPlannedCurrent(ctx context.Context, obj *unstructured.Unstructured) {
	GinkgoHelper()
	setDomainPhase(obj, storagev1alpha1.SnapshotCapturePhasePlanned)
	Expect(k8sClient.Status().Update(ctx, obj)).To(Succeed())
}

// injectDomainFinished sets status.captureState.domainSpecificController.phase=Finished on obj (in memory).
// Finished is capture barrier 2: the domain finished its side (including consistency actions), so the core
// may finalize the aggregate Ready. The post-bind Ready mirror (snapshotcontent.mirrorReadyToOwnerSnapshot)
// holds Ready=False on a domain-capture owner until this phase is reached, so any spec that expects the
// bound content's Ready=True to surface on the Snapshot must advance the phase to Finished (Planned alone
// only clears barrier 1 for the binder). Use inside blocks that drive their own Status().Update.
func injectDomainFinished(obj *unstructured.Unstructured) {
	setDomainPhase(obj, storagev1alpha1.SnapshotCapturePhaseFinished)
}

// setSnapshotDomainFinishedCurrent publishes phase=Finished and updates the status subresource via
// k8sClient — capture barrier 2, the signal the post-bind Ready mirror requires before finalizing a
// domain-capture Snapshot's Ready=True.
func setSnapshotDomainFinishedCurrent(ctx context.Context, obj *unstructured.Unstructured) {
	GinkgoHelper()
	setDomainPhase(obj, storagev1alpha1.SnapshotCapturePhaseFinished)
	Expect(k8sClient.Status().Update(ctx, obj)).To(Succeed())
}

// setDomainPhase sets status.captureState.domainSpecificController.phase on obj (in memory only).
func setDomainPhase(obj *unstructured.Unstructured, phase storagev1alpha1.SnapshotCapturePhase) {
	Expect(unstructured.SetNestedField(obj.Object, string(phase),
		"status", "captureState", "domainSpecificController", "phase")).To(Succeed())
}
