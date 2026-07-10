//go:build e2e
// +build e2e

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

package e2e

import (
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// injectDomainPlanned publishes the domain planning-done signal
// (status.captureState.domainSpecificController.phase=Planned) on obj (in memory). Use it inside
// blocks that drive their own Status().Update. The generic binder barrier waits for this phase before
// creating SnapshotContent; it replaced the former PlanningReady=True condition. The spec is immutable,
// so no observedGeneration gate is needed.
func injectDomainPlanned(obj *unstructured.Unstructured) {
	Expect(unstructured.SetNestedField(obj.Object, string(storagev1alpha1.SnapshotCapturePhasePlanned),
		"status", "captureState", "domainSpecificController", "phase")).To(Succeed())
}
