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

package demo

import (
	"testing"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

func newMaterializationFakeClient(t *testing.T, initObjs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, f := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		storagev1alpha1.AddToScheme,
		ssv1alpha1.AddToScheme,
		demov1alpha1.AddToScheme,
		deckhousev1alpha1.AddToScheme,
	} {
		if err := f(scheme); err != nil {
			t.Fatalf("add scheme: %v", err)
		}
	}
	statusSubresources := []client.Object{
		&demov1alpha1.DemoVirtualDisk{},
		&demov1alpha1.DemoVirtualMachine{},
		&demov1alpha1.DemoVirtualDiskSnapshot{},
		&demov1alpha1.DemoVirtualMachineSnapshot{},
		&storagev1alpha1.SnapshotContent{},
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(statusSubresources...).
		WithObjects(initObjs...).
		Build()
}
